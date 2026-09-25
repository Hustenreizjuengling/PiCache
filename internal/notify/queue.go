package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// worker is the queue and rate limit of one channel. Its goroutine
// (runWorker) delivers the queued messages one after the other.
type worker struct {
	wake   chan struct{}      // 1-buffered: a message was queued
	cancel context.CancelFunc // stops the goroutine (guarded by Service.mu)

	mu      sync.Mutex // guards the fields below
	ch      Channel
	sealed  string      // sealed secret ("" = none)
	queue   []Message   // at most queueSize
	window  []time.Time // accept times within the last rateWindow, oldest first
	dropped int         // messages dropped since the last summary

	// Used by the worker goroutine only.
	lastErr   string    // last logged delivery error
	lastErrAt time.Time // when it was logged
}

func newWorker(c Channel, sealed string) *worker {
	return &worker{wake: make(chan struct{}, 1), ch: c, sealed: sealed}
}

// snapshot returns the channel and its sealed secret.
func (w *worker) snapshot() (Channel, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ch, w.sealed
}

// update replaces the channel; a disabled channel drops its queue.
func (w *worker) update(c Channel, sealed string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ch, w.sealed = c, sealed
	if !c.Enabled {
		w.queue, w.dropped = nil, 0
	}
}

// offer queues m if the channel's filter accepts it and the rate limit
// allows; otherwise an accepted message is counted as dropped. It never
// blocks.
func (w *worker) offer(m Message, now time.Time) {
	w.mu.Lock()
	if !w.ch.accepts(m) {
		w.mu.Unlock()
		return
	}
	w.flushDroppedLocked(now) // the summary goes before newer messages
	if len(w.window) < rateLimit && len(w.queue) < queueSize {
		w.window = append(w.window, now)
		w.queue = append(w.queue, m)
	} else {
		w.dropped++
	}
	w.mu.Unlock()
	w.signal()
}

func (w *worker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// pruneLocked forgets accept times older than the rate window.
func (w *worker) pruneLocked(now time.Time) {
	i := 0
	for i < len(w.window) && now.Sub(w.window[i]) >= rateWindow {
		i++
	}
	w.window = w.window[i:]
}

// flushDroppedLocked queues the summary of dropped messages once the rate
// window has room again.
func (w *worker) flushDroppedLocked(now time.Time) {
	w.pruneLocked(now)
	if w.dropped == 0 || len(w.window) >= rateLimit || len(w.queue) >= queueSize {
		return
	}
	n := w.dropped
	w.dropped = 0
	w.window = append(w.window, now)
	noun := "notifications were"
	if n == 1 {
		noun = "notification was"
	}
	w.queue = append(w.queue, Message{Event: EventDropped, Severity: SeverityWarning, Time: now.UTC(),
		Title: fmt.Sprintf("%d notifications dropped", n),
		Message: fmt.Sprintf("%d %s not sent to this channel: PiCache sends at most %d notifications per channel in %d minutes. "+
			"See the delivery log or the health page for what happened.", n, noun, rateLimit, int(rateWindow/time.Minute))})
}

// summaryAt returns when the summary of dropped messages can be queued
// (zero: nothing was dropped).
func (w *worker) summaryAt() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.dropped == 0:
		return time.Time{}
	case len(w.window) < rateLimit:
		return time.Unix(0, 0) // now
	}
	return w.window[0].Add(rateWindow)
}

// pop takes the next queued message.
func (w *worker) pop() (Message, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.queue) == 0 {
		return Message{}, false
	}
	m := w.queue[0]
	w.queue[0] = Message{}
	w.queue = w.queue[1:]
	return m, true
}

// runWorker delivers the queued messages of w until ctx ends.
func (s *Service) runWorker(ctx context.Context, w *worker) {
	for {
		if m, ok := w.pop(); ok {
			s.deliver(ctx, w, m)
			if ctx.Err() != nil {
				return
			}
			continue
		}
		var timer *time.Timer
		var fire <-chan time.Time
		if at := w.summaryAt(); !at.IsZero() {
			timer = time.NewTimer(max(at.Sub(s.now()), 0))
			fire = timer.C
		}
		select {
		case <-ctx.Done():
		case <-w.wake:
		case <-fire:
			w.mu.Lock()
			w.flushDroppedLocked(s.now())
			w.mu.Unlock()
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// deliver sends m with up to maxAttempts attempts. Each attempt uses the
// channel as it is configured then; a disabled or deleted channel stops.
func (s *Service) deliver(ctx context.Context, w *worker, m Message) {
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		c, sealed := w.snapshot()
		if !c.Enabled || ctx.Err() != nil {
			return
		}
		rctx, cancel := context.WithTimeout(s.sendCtx, requestTimeout)
		status, err := s.send(rctx, c, sealed, m)
		cancel()
		s.record(c, m, attempt, err)
		if err == nil {
			if w.lastErr != "" {
				s.log.Info("notifications are delivered again", slog.String("channel", c.ID), slog.String("name", c.Name))
				w.lastErr = ""
			}
			return
		}
		if msg := err.Error(); msg != w.lastErr || s.now().Sub(w.lastErrAt) >= time.Hour {
			w.lastErr, w.lastErrAt = msg, s.now()
			s.log.Warn("notification not delivered", slog.String("channel", c.ID), slog.String("name", c.Name),
				slog.String("event", m.Event), slog.Int("attempt", attempt), slog.String("err", msg))
		}
		if attempt == maxAttempts || !retryable(status, err) {
			return
		}
		t := time.NewTimer(backoff[attempt-1])
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// retryable reports whether another attempt may succeed: network errors,
// timeouts, 408, 429 and 5xx. Other answers (wrong token, unknown topic,
// a redirect) and configuration errors do not change by retrying.
func retryable(status int, err error) bool {
	var perm *permanentError
	switch {
	case errors.As(err, &perm):
		return false
	case status == 0:
		return true
	}
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

// permanentError is a delivery error that no retry fixes (configuration).
type permanentError struct{ msg string }

func (e *permanentError) Error() string { return e.msg }
