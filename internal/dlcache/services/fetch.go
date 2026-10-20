package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Name lookup bounds shared by the sources (steam.go, catalog.go).
const (
	lookupGap     = time.Second
	lookupPerDay  = 2000
	lookupTimeout = 15 * time.Second
	lookupPause   = 10 * time.Minute
)

var (
	errLookupBudget = errors.New("the daily budget of name lookups is used up")
	errNotFound     = errors.New("not found")
)

// fetcher makes the requests of one source of names: at most one per gap
// and perDay a day, a pause after a failure, no redirects, bounded answers.
// Requests go out with the app's lookup client (SetLookupClient).
type fetcher struct {
	name string // in logs: "Steam name lookup failed"
	log  *slog.Logger

	client    atomic.Pointer[http.Client]
	userAgent atomic.Pointer[string]

	// Replaced in tests.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) bool

	bmu      sync.Mutex // guards the budget below
	last     time.Time  // the last request
	dayStart time.Time
	dayCount int
	pausedTo time.Time
	lastErr  time.Time // the last logged error
	requests atomic.Int64
}

func newFetcher(name string, log *slog.Logger) fetcher {
	return fetcher{name: name, log: log, now: time.Now, sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// setClient sets the HTTP client (nil: no requests are made) and the
// User-Agent; redirects are never followed.
func (f *fetcher) setClient(c *http.Client, userAgent string) {
	if c != nil {
		cc := *c
		cc.Timeout = lookupTimeout
		cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		c = &cc
	}
	f.client.Store(c)
	f.userAgent.Store(&userAgent)
}

// fail pauses the requests and logs at most hourly.
func (f *fetcher) fail(err error) {
	now := f.now()
	f.bmu.Lock()
	f.pausedTo = now.Add(lookupPause)
	logIt := now.Sub(f.lastErr) >= time.Hour
	if logIt {
		f.lastErr = now
	}
	f.bmu.Unlock()
	if logIt && !errors.Is(err, context.Canceled) {
		f.log.Warn(f.name+" lookup failed; retrying later", slog.Any("err", err))
	}
}

// wait keeps the request budget: one per lookupGap, lookupPerDay per day,
// and the pause after a failure.
func (f *fetcher) wait(ctx context.Context) error {
	f.bmu.Lock()
	now := f.now()
	if now.Sub(f.dayStart) >= 24*time.Hour {
		f.dayStart, f.dayCount = now, 0
	}
	if f.dayCount >= lookupPerDay {
		f.bmu.Unlock()
		return errLookupBudget
	}
	next := f.last.Add(lookupGap)
	if f.pausedTo.After(next) {
		next = f.pausedTo
	}
	f.dayCount++
	if next.Before(now) {
		next = now
	}
	f.last = next
	f.bmu.Unlock()
	if !f.sleep(ctx, next.Sub(now)) {
		return ctx.Err()
	}
	f.requests.Add(1)
	return nil
}

// get fetches url (at most limit bytes); a 404 is errNotFound.
func (f *fetcher) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	c := f.client.Load()
	if c == nil {
		return nil, errors.New("no HTTP client")
	}
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if ua := f.userAgent.Load(); ua != nil && *ua != "" {
		req.Header.Set("User-Agent", *ua)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("%s: %w", req.URL.Host, errNotFound)
	default:
		return nil, fmt.Errorf("%s: HTTP %d", req.URL.Host, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: the answer is larger than %d bytes", req.URL.Host, limit)
	}
	return b, nil
}
