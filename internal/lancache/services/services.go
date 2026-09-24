// Package services manages the LanCache service catalogue: the
// uklans/cache-domains source, enable/disable state, custom services and
// hosts, the anchored host matchers used by DNS/proxy/SNI, the special
// pass-through path rules, and content grouping/labels
// (docs/ARCHITECTURE.md 8.1, 8.2 steps 5–6, 8.4).
//
// Tables (picache.db, component "services"): services_custom,
// services_extra_domains, services_labels.
//
// The source is untrusted network input:
//   - domain_files entries must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}\.txt$;
//     URLs are built with url.JoinPath(domainsSource, name); snapshots are
//     written only through os.Root (temp + rename) into the snapshot dir.
//   - cache_domains.json ≤ 1 MiB and ≤ 128 services; each .txt ≤ 4 MiB and
//     ≤ 50 000 lines (io.LimitReader(max+1) → error); at most 16 files per
//     service and 100 000 patterns in total.
//   - service IDs must match ^[a-z0-9][a-z0-9_-]{0,31}$.
//   - every host pattern (source, custom, extra) passes ValidatePattern;
//     rejected ones are reported in SourceStatus.Skipped.
//   - no redirects are followed; never private destinations (the fetch
//     client dials through netutil.SafeDialer).
//
// The registry subscribes to settings: it rebuilds the matcher snapshot when
// lancache.disabledServices changes and refetches when domainsSource changes.
package services

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// SteamUserAgentSuffix identifies Steam CDN requests.
const SteamUserAgentSuffix = "Valve/Steam HTTP Client 1.0"

// SteamTrigger is the name Steam resolves to detect a LanCache.
const SteamTrigger = "lancache.steamcontent.com"

// steamID is the service that the Steam User-Agent rule and the trigger
// name belong to.
const steamID = "steam"

// Refresh scheduling.
const (
	refreshTimeout = 5 * time.Minute
	minRetry       = time.Minute
	maxRetry       = time.Hour
	errorLogEvery  = time.Hour
)

// Service is one LanCache service.
type Service struct {
	ID           string   `json:"id"` // cache-domains "name", or "custom-<slug>"
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Notes        string   `json:"notes,omitempty"`
	MixedContent bool     `json:"mixedContent"`
	Custom       bool     `json:"custom"`
	Enabled      bool     `json:"enabled"`
	Domains      []string `json:"domains"`      // effective host patterns (source + extra)
	ExtraDomains []string `json:"extraDomains"` // user-added host patterns
	DomainCount  int      `json:"domainCount"`
}

// ServiceInput creates or updates a custom service.
type ServiceInput struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Domains     []string `json:"domains"` // exact hosts or *.suffix
}

// SourceStatus describes the cache-domains source.
type SourceStatus struct {
	Source       string    `json:"source"`
	LastFetched  time.Time `json:"lastFetched,omitzero"` // last successful fetch
	LastAttempt  time.Time `json:"lastAttempt,omitzero"`
	Error        string    `json:"error,omitempty"`
	ServiceCount int       `json:"serviceCount"`
	DomainCount  int       `json:"domainCount"`
	Skipped      []string  `json:"skipped,omitempty"` // rejected patterns/files with reason
	Ready        bool      `json:"ready"`             // a snapshot is loaded
}

// Group is the content group of a request (what was downloaded).
type Group struct {
	Key     string `json:"key"`     // e.g. "steam:depot:228990", "blizzard:wow"
	Label   string `json:"label"`   // default display label
	Version string `json:"version"` // e.g. Steam manifest id, package version
}

// Registry is safe for concurrent use; matchers are immutable snapshots.
type Registry struct {
	db    *db.DB
	set   *settings.Store
	fetch *http.Client
	dir   string
	log   *slog.Logger

	snap   atomic.Pointer[snapshot]
	labels atomic.Pointer[map[string]string] // user labels (copy on write)

	// mu guards the fields below and serialises rebuilds and DB writes. It
	// is never held while calling settings.Update (whose listeners take it).
	mu       sync.Mutex
	src      *sourceSnapshot
	custom   []customService // sorted by ID
	extras   map[string][]string
	status   SourceStatus
	failures int     // consecutive failed fetches (retry backoff)
	jitter   float64 // factor applied to the refresh interval (0.9–1.1)
	lastErr  string  // last logged fetch error
	lastErrT time.Time

	fetchSem chan struct{} // one fetch at a time
	refetch  chan struct{} // domainsSource changed
	resched  chan struct{} // updateIntervalHours changed
	unsub    func()
}

// New creates the registry and loads the last cache-domains snapshot from
// dir (offline start). fetch downloads cache-domains (SafeDialer over the
// bypass resolver); redirects are never followed.
func New(ctx context.Context, d *db.DB, set *settings.Store, fetch *http.Client, dir string, log *slog.Logger) (*Registry, error) {
	r := &Registry{
		db:       d,
		set:      set,
		dir:      dir,
		log:      log.With(slog.String("component", "services")),
		extras:   map[string][]string{},
		jitter:   1,
		fetchSem: make(chan struct{}, 1),
		refetch:  make(chan struct{}, 1),
		resched:  make(chan struct{}, 1),
	}
	if fetch != nil {
		c := *fetch
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		r.fetch = &c
	}
	if err := d.Migrate(ctx, "services", migrations); err != nil {
		return nil, err
	}
	if err := r.loadDB(ctx); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	r.mu.Lock()
	src, err := loadSnapshot(dir)
	switch {
	case err != nil:
		r.log.Warn("ignoring unreadable cache-domains snapshot; it is fetched again", slog.Any("err", err))
		r.status.Error = "the stored snapshot is unreadable; waiting for a new download"
	case src == nil:
		r.status.Error = "waiting for the first download"
	default:
		r.applySourceLocked(src)
	}
	r.rebuildLocked(set.Get())
	r.mu.Unlock()

	r.unsub = set.Subscribe(r.onSettings)
	return r, nil
}

// onSettings reacts to settings changes (called synchronously by
// settings.Update).
func (r *Registry) onSettings(old, n *settings.All) {
	if !slices.Equal(old.LanCache.DisabledServices, n.LanCache.DisabledServices) {
		r.mu.Lock()
		r.rebuildLocked(n)
		r.mu.Unlock()
	}
	if old.LanCache.DomainsSource != n.LanCache.DomainsSource {
		signal(r.refetch)
	}
	if old.LanCache.UpdateIntervalHours != n.LanCache.UpdateIntervalHours {
		signal(r.resched)
	}
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Start fetches the source if it is missing or stale, refreshes it every
// lancache.updateIntervalHours (±10 %, 0 = manual only) and refetches when
// the source URL changes. Failed fetches are retried with backoff (1 min to
// 1 h). Blocks until ctx is done.
func (r *Registry) Start(ctx context.Context) {
	defer r.unsub()
	for {
		var timer *time.Timer
		var fire <-chan time.Time
		if d, ok := r.nextFetch(time.Now()); ok {
			timer = time.NewTimer(d)
			fire = timer.C
		}
		fetch := false
		select {
		case <-ctx.Done():
		case <-fire:
			fetch = true
		case <-r.refetch:
			fetch = true
		case <-r.resched:
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
		if fetch {
			r.refreshAndLog(ctx)
		}
	}
}

// nextFetch returns the delay until the next scheduled fetch; ok=false
// means none is scheduled (manual updates only).
func (r *Registry) nextFetch(now time.Time) (time.Duration, bool) {
	lc := r.set.Get().LanCache
	hours := lc.UpdateIntervalHours
	r.mu.Lock()
	defer r.mu.Unlock()
	// current: a snapshot of the configured source is loaded (the source
	// may have been changed while PiCache was not running).
	current := r.src != nil && r.src.Source == lc.DomainsSource
	var due time.Time
	switch {
	case current && hours > 0:
		interval := time.Duration(float64(time.Duration(hours)*time.Hour) * r.jitter)
		due = r.status.LastFetched.Add(interval)
	case !current && r.failures == 0:
		return 0, true
	}
	if r.failures > 0 && (!current || hours > 0) {
		// Retry failed fetches with backoff, also after a failed manual
		// refresh or source change while the old snapshot stays active.
		if retry := r.status.LastAttempt.Add(retryDelay(r.failures)); due.IsZero() || retry.Before(due) {
			due = retry
		}
	}
	if due.IsZero() {
		return 0, false // manual updates only
	}
	return max(due.Sub(now), 0), true
}

// retryDelay is the backoff after n consecutive failures.
func retryDelay(n int) time.Duration {
	d := minRetry
	for i := 1; i < n && d < maxRetry; i++ {
		d *= 2
	}
	return min(d, maxRetry)
}

// refreshAndLog runs a background refresh; repeated identical errors are
// logged at most hourly.
func (r *Registry) refreshAndLog(ctx context.Context) {
	err := r.Refresh(ctx)
	if ctx.Err() != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		r.lastErr = ""
		r.log.Info("cache-domains updated", slog.Int("services", r.status.ServiceCount),
			slog.Int("domains", r.status.DomainCount), slog.Int("skipped", len(r.status.Skipped)))
		return
	}
	msg := r.status.Error
	if msg != r.lastErr || time.Since(r.lastErrT) >= errorLogEvery {
		r.lastErr, r.lastErrT = msg, time.Now()
		r.log.Warn("cache-domains update failed", slog.String("err", msg), slog.Bool("snapshot", r.src != nil))
	}
}

// Refresh fetches the cache-domains source now. On failure the last good
// snapshot stays active and the error is recorded in Status.
func (r *Registry) Refresh(ctx context.Context) error {
	select {
	case r.fetchSem <- struct{}{}:
	case <-ctx.Done():
		return apperr.Unavailable("another cache-domains update is still running")
	}
	defer func() {
		<-r.fetchSem
		signal(r.resched) // the schedule depends on the outcome
	}()
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()

	base := r.set.Get().LanCache.DomainsSource
	started := time.Now().UTC()
	src, err := fetchSource(ctx, r.fetch, base)

	r.mu.Lock()
	r.status.LastAttempt = started
	if err != nil {
		r.failures++
		r.status.Error = err.Error()
		r.mu.Unlock()
		return apperr.Wrap(apperr.KindUnavailable, err, "cache-domains update failed: %s", err.Error())
	}
	src.FetchedAt = time.Now().UTC()
	r.failures = 0
	r.jitter = 0.9 + 0.2*rand.Float64()
	r.applySourceLocked(src)
	r.rebuildLocked(r.set.Get())
	r.mu.Unlock()

	if err := saveSnapshot(r.dir, src); err != nil {
		r.log.Warn("could not save the cache-domains snapshot; the next start needs Internet access", slog.Any("err", err))
	}
	return nil
}

// applySourceLocked makes src the active source.
func (r *Registry) applySourceLocked(src *sourceSnapshot) {
	r.src = src
	r.status.LastFetched = src.FetchedAt
	r.status.Error = ""
	r.status.ServiceCount = len(src.Services)
	r.status.DomainCount = src.domainCount()
	r.status.Skipped = src.Skipped
	r.status.Ready = true
}

// Status returns the source status.
func (r *Registry) Status() SourceStatus {
	r.mu.Lock()
	st := r.status
	st.Skipped = slices.Clone(st.Skipped)
	r.mu.Unlock()
	st.Source = redactURL(r.set.Get().LanCache.DomainsSource)
	return st
}

// MatchDNS returns the enabled service whose host patterns match qname
// (lower-case, no trailing dot). The Steam trigger matches when steam is
// enabled. Nothing matches before a cache-domains snapshot is loaded.
func (r *Registry) MatchDNS(qname string) (serviceID string, ok bool) {
	s := r.snap.Load()
	if !s.ready {
		return "", false
	}
	sv, found := s.lookup(normalizeHost(qname))
	if !found || !sv.Enabled {
		return "", false
	}
	return sv.ID, true
}

// Classify maps an HTTP request to a service:
//   - User-Agent ending in SteamUserAgentSuffix AND (path matches
//     ^/depot/[0-9]+/ or path == "/server-status") AND method GET/HEAD
//     (checked by the caller) → "steam" (Steam sends the real CDN host).
//   - otherwise by host (exact or *.suffix, anchored, case-insensitive).
//
// known=false means the host belongs to no service (the proxy refuses it).
func (r *Registry) Classify(host, userAgent, path string) (serviceID string, enabled, known bool) {
	s := r.snap.Load()
	if strings.HasSuffix(userAgent, SteamUserAgentSuffix) && isSteamPath(path) {
		return steamID, s.services[s.steam].Enabled, true
	}
	sv, found := s.lookup(normalizeHost(host))
	if !found {
		return "", false, false
	}
	return sv.ID, sv.Enabled, true
}

// SNIAllowed reports whether sni belongs to an enabled service.
func (r *Registry) SNIAllowed(sni string) (serviceID string, ok bool) {
	sv, found := r.snap.Load().lookup(normalizeHost(sni))
	if !found || !sv.Enabled {
		return "", false
	}
	return sv.ID, true
}

// Services lists all services with effective state: the source services
// in source order, then the custom services by ID. The Domains and
// ExtraDomains slices are shared and must not be modified.
func (r *Registry) Services(ctx context.Context) ([]Service, error) {
	return slices.Clone(r.snap.Load().services), nil
}

// Service returns one service.
func (r *Registry) Service(ctx context.Context, id string) (Service, error) {
	s := r.snap.Load()
	i, ok := s.index[id]
	if !ok {
		return Service{}, apperr.NotFound("service", id)
	}
	return s.services[i], nil
}
