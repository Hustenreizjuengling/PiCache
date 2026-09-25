package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// fakeTransport answers requests from a list of statuses (-1: a network
// error, -2: hang until the request is cancelled; 200 once the list is
// used up) and records when they came.
type fakeTransport struct {
	mu      sync.Mutex
	answers []int
	calls   []fakeCall
}

type fakeCall struct {
	at    time.Time
	title string // the Title header (ntfy)
}

func (f *fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	_, _ = io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	status := http.StatusOK
	if len(f.answers) > 0 {
		status, f.answers = f.answers[0], f.answers[1:]
	}
	f.calls = append(f.calls, fakeCall{at: time.Now(), title: r.Header.Get("Title")})
	f.mu.Unlock()
	switch status {
	case -1:
		return nil, errors.New("connect: connection refused")
	case -2:
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}, Request: r}, nil
}

func (f *fakeTransport) got() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

func (f *fakeTransport) answer(statuses ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, statuses...)
}

// startFake runs s with a fake transport until the test ends.
func startFake(t *testing.T, s *Service) (*fakeTransport, func()) {
	t.Helper()
	ft := &fakeTransport{}
	s.client = &http.Client{Transport: ft, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Start(ctx); close(done) }()
	return ft, func() { cancel(); <-done }
}

func msg(title string) Message { return Message{Event: EventHealthWarning, Title: title} }

// Failed deliveries are retried twice, 10 s and 60 s later; network
// errors and 5xx are retried, a 401 is not. Every attempt is logged.
func TestRetriesAndBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newTestService(t)
		if _, err := s.Create(t.Context(), ChannelInput{Name: "phone", Kind: KindNtfy, URL: "https://ntfy.sh/t", Enabled: true}); err != nil {
			t.Fatal(err)
		}
		ft, stop := startFake(t, s)
		defer stop()
		ft.answer(500, -1, 200)
		start := time.Now()
		s.Emit(msg("disk full"))
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		calls := ft.got()
		if len(calls) != 3 || calls[0].at.Sub(start) != 0 || calls[1].at.Sub(start) != 10*time.Second ||
			calls[2].at.Sub(start) != 70*time.Second {
			t.Fatalf("attempts %+v", calls)
		}
		log := s.Log(maxLogEntries)
		if len(log) != 3 || !log[0].OK || log[0].Attempt != 3 || log[1].OK || log[1].Attempt != 2 ||
			log[1].Error != "connect: connection refused" || log[2].Error != "HTTP 500 Internal Server Error" ||
			log[0].Title != "disk full" || log[0].Severity != SeverityWarning || log[0].ChannelName != "phone" {
			t.Fatalf("log %+v", log)
		}

		ft.answer(401)
		s.Emit(msg("unauthorized"))
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if n := len(ft.got()); n != 4 {
			t.Fatalf("a 401 was retried: %d calls", n)
		}
		ft.answer(503, 503, 503, 503)
		s.Emit(msg("down"))
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if n := len(ft.got()); n != 7 {
			t.Fatalf("%d calls, want 3 attempts at most", n)
		}
	})
}

// At most 20 messages per channel in 10 minutes; further ones are
// dropped and one summary follows as soon as the window has room.
func TestRateLimitSummary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newTestService(t)
		if _, err := s.Create(t.Context(), ChannelInput{Name: "p", Kind: KindNtfy, URL: "https://ntfy.sh/t", Enabled: true,
			MinSeverity: SeverityInfo}); err != nil {
			t.Fatal(err)
		}
		ft, stop := startFake(t, s)
		defer stop()
		for range 25 {
			s.Emit(msg("flood"))
		}
		synctest.Wait()
		if n := len(ft.got()); n != rateLimit {
			t.Fatalf("%d delivered, want %d", n, rateLimit)
		}
		time.Sleep(5 * time.Minute)
		s.Emit(msg("still flooding"))
		synctest.Wait()
		if n := len(ft.got()); n != rateLimit {
			t.Fatalf("delivered within the window: %d", n)
		}
		time.Sleep(5*time.Minute - time.Second)
		synctest.Wait()
		if n := len(ft.got()); n != rateLimit {
			t.Fatalf("summary before the window allows: %d", n)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		calls := ft.got()
		if len(calls) != rateLimit+1 || calls[rateLimit].title != "6 notifications dropped" {
			t.Fatalf("summary %+v", calls[len(calls)-1])
		}
		log := s.Log(1)
		if log[0].Event != EventDropped || log[0].Severity != SeverityWarning {
			t.Fatalf("summary log %+v", log)
		}
		s.Emit(msg("after the window"))
		synctest.Wait()
		if calls := ft.got(); len(calls) != rateLimit+2 || calls[rateLimit+1].title != "after the window" {
			t.Fatalf("after the window %+v", calls)
		}
	})
}

// The window with an explicit clock: 20 accepts in 10 minutes, the 21st
// is counted; the summary takes the first free slot.
func TestRateWindow(t *testing.T) {
	w := newWorker(Channel{Enabled: true, MinSeverity: SeverityInfo}, "")
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	m := Message{Event: EventBackupSucceeded, Severity: SeverityInfo}
	for i := range rateLimit {
		w.offer(m, t0.Add(time.Duration(i)*time.Second))
	}
	w.offer(m, t0.Add(30*time.Second))
	for range rateLimit {
		if _, ok := w.pop(); !ok {
			t.Fatal("queue shorter than the limit")
		}
	}
	if _, ok := w.pop(); ok || w.dropped != 1 {
		t.Fatalf("21st message queued (dropped %d)", w.dropped)
	}
	w.offer(m, t0.Add(10*time.Minute-time.Second))
	if at := w.summaryAt(); !at.Equal(t0.Add(10*time.Minute)) || w.dropped != 2 {
		t.Fatalf("summary at %v, dropped %d", at, w.dropped)
	}
	// At t0+10m one slot is free: the summary takes it, the new message is
	// dropped (and counted for the next summary).
	w.offer(m, t0.Add(10*time.Minute))
	if got, _ := w.pop(); got.Event != EventDropped || !strings.HasPrefix(got.Title, "2 notifications dropped") {
		t.Fatalf("summary %+v", got)
	}
	if _, ok := w.pop(); ok || w.dropped != 1 {
		t.Fatalf("dropped %d", w.dropped)
	}
	// A disabled channel forgets its queue and its count.
	w.update(Channel{Enabled: false}, "")
	if w.dropped != 0 || w.summaryAt() != (time.Time{}) {
		t.Fatal("disabled channel keeps its drops")
	}
}

// Filters: enabled, minimum severity, events (empty = all).
func TestEmitFilters(t *testing.T) {
	s, _ := newTestService(t)
	mk := func(name string, enabled bool, sev Severity, evs ...string) *worker {
		c, err := s.Create(t.Context(), ChannelInput{Name: name, Kind: KindWebhook, URL: "http://10.0.0.2/h", Enabled: enabled,
			MinSeverity: sev, Events: evs})
		if err != nil {
			t.Fatal(err)
		}
		return s.worker(c.ID)
	}
	all := mk("all", true, SeverityInfo)
	off := mk("off", false, SeverityInfo)
	errs := mk("errors", true, SeverityError)
	backups := mk("backups", true, SeverityInfo, EventBackupFailed, EventBackupSucceeded)
	s.Emit(Message{Event: EventBackupSucceeded, Title: "ok"})                          // info
	s.Emit(Message{Event: EventBackupFailed, Message: "disk full"})                    // error
	s.Emit(Message{Event: EventHealthWarning, Title: "warn"})                          // warning
	s.Emit(Message{Event: EventStorageOffline, Severity: SeverityError, Title: "sev"}) // explicit severity
	s.Emit(Message{Event: "dns.unknown", Title: "x"})                                  // unknown: dropped
	s.Emit(Message{Event: EventTest, Title: "x"})                                      // only through Test
	for w, want := range map[*worker]int{all: 4, off: 0, errs: 2, backups: 2} {
		if n := len(w.queue); n != want {
			c, _ := w.snapshot()
			t.Errorf("%s: %d queued, want %d", c.Name, n, want)
		}
	}
	if m := errs.queue[0]; m.Event != EventBackupFailed || m.Severity != SeverityError || m.Title != "Scheduled backup failed" ||
		m.Time.IsZero() || m.Time.Location() != time.UTC {
		t.Fatalf("defaults %+v", m)
	}
	var nilService *Service
	nilService.Emit(msg("nothing happens"))
}

// Messages are cleaned: no control characters, bounded length.
func TestCleanText(t *testing.T) {
	if got := cleanText(" a\x00b\r\nc d\te ", 100, false); got != "ab  cd e" {
		t.Fatalf("single line %q", got)
	}
	if got := cleanText("a\nb\x1b[31m", 100, true); got != "a\nb[31m" {
		t.Fatalf("multi line %q", got)
	}
	if got := cleanText(strings.Repeat("ü", 300), maxTitle, false); len([]rune(got)) != maxTitle || !strings.HasSuffix(got, "…") {
		t.Fatalf("long %q", got)
	}
}

// At shutdown a request in flight gets shutdownGrace; nothing else is sent.
func TestShutdownGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newTestService(t)
		if _, err := s.Create(t.Context(), ChannelInput{Name: "p", Kind: KindNtfy, URL: "https://ntfy.sh/t", Enabled: true}); err != nil {
			t.Fatal(err)
		}
		ft, stop := startFake(t, s)
		ft.answer(-2)
		s.Emit(msg("hangs"))
		s.Emit(msg("queued"))
		synctest.Wait()
		start := time.Now()
		stop()
		if d := time.Since(start); d != shutdownGrace {
			t.Fatalf("shutdown took %s", d)
		}
		if n := len(ft.got()); n != 1 {
			t.Fatalf("%d requests", n)
		}
		s.Emit(msg("after shutdown"))
		if n := len(ft.got()); n != 1 {
			t.Fatal("sent after shutdown")
		}
	})
}

// A channel created while running gets a worker; a deleted one stops.
func TestWorkersFollowChannels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newTestService(t)
		ft, stop := startFake(t, s)
		defer stop()
		c, err := s.Create(t.Context(), ChannelInput{Name: "p", Kind: KindNtfy, URL: "https://ntfy.sh/t", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		s.Emit(msg("one"))
		synctest.Wait()
		ft.answer(500, 500)
		s.Emit(msg("two"))
		synctest.Wait()
		if err := s.Delete(t.Context(), c.ID); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if n := len(ft.got()); n != 2 {
			t.Fatalf("%d requests; the deleted channel must not retry", n)
		}
	})
}
