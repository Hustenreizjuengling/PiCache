// Package upstream forwards DNS queries to upstream resolvers (UDP, TCP, DoT,
// DoH) with a response cache, serve-stale, in-flight de-duplication and the
// upstream modes load_balance/parallel/strict/fastest_addr
// (docs/ARCHITECTURE.md 7.4).
//
// The default set (Resolve, LookupIP) is the configured upstreams, the
// clock-guard set while the clock guard is active, and the fallbacks, asked
// only when no default upstream replied at all. Its answers are classified
// at fetch time when the upstream blocked the name itself (Info.Block: EDE
// 15–17, 0.0.0.0/::, block pages, Quad9's NXDOMAIN without RA), may carry
// the client subnet of the query (part of the cache key) and are ordered by
// the fastest address in mode fastest_addr. ResolveVia sets get none of
// this. The EDE of every reply is returned (Info.EDE, sanitised).
//
// LookupIP resolves names for PiCache itself (cache proxy, SNI, list and
// cache-domains downloads) and deliberately bypasses local records, the
// download cache DNS answers and filtering.
//
// Reply contract: the returned message's Question equals req.Question byte
// for byte (original case) and its Id equals req.Id; every RR TTL in Answer,
// Ns and Extra (except OPT) is reduced by the seconds since the entry was
// cached (minimum 0; stale answers use 30). Duplicate records (RFC 2181 5)
// are removed before a reply is cached or returned. De-duplicated waiters
// each get their own copy. The cache key uses the DO bit sent upstream.
//
// Upstream queries are built fresh for every exchange: random ID (0 for
// DoH), RD=1, AD=1, our OPT with a 1232-byte buffer and DO=1 if the client
// set DO or dns.dnssec is on. Client EDNS options, the CD bit and the
// client's ID never reach an upstream. Every reply must echo the question
// (qname case-insensitively, qtype, qclass); UDP replies that do not are
// discarded and the exchange keeps waiting, TCP/DoT/DoH replies fail the
// attempt.
//
// Clock guard: while time.Now() is before the binary's build date
// (version.Date; ignored when "unknown"), encrypted upstreams are skipped and
// queries go over plain UDP/TCP to the bootstrap IPs (logged once at WARN,
// reported by ClockGuard). Certificate errors alone never trigger this.
//
// Bounds: response cache ≤ dns.cacheSize entries and 64 MiB of wire data
// (responses above 16 KiB are not cached; TTLs capped at 7 days), 4096
// distinct in-flight queries, 16 upstreams per set, 64 ResolveVia sets,
// 256 queued stale refreshes, 4 idle DoT connections per upstream, DoH
// bodies ≤ 64 KiB, 64 bootstrap hostnames, 1024 LookupIP results; the
// duplicate check covers sections of at most 256 records; 16 EDE options
// examined per reply, EDE texts ≤ 200 bytes; fastest-address probes: 8
// addresses and 300 ms per answer, 32 dials at a time, 100 new targets per
// second, 4096 cached results (10 minutes).
package upstream

import (
	"context"
	"crypto/x509"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/version"
)

// Info describes how a response was obtained. Block, EDE and Fallback are
// stored with a cached answer and returned by cache and stale hits too.
type Info struct {
	Upstream string        // upstream that answered ("" for cache hits)
	Cached   bool          // served from the response cache
	Stale    bool          // served stale (refresh in background)
	RTT      time.Duration // upstream round-trip time (0 for cache hits)
	// Block is set when an upstream of the default set blocked the name
	// itself (EDE 15–17, 0.0.0.0/::, a block page, Quad9's NXDOMAIN
	// without RA); never for ResolveVia answers.
	Block *BlockInfo
	// EDE is the Extended DNS Error of the upstream reply (any set), nil
	// if it carried none.
	EDE *EDE
	// Fallback reports that a fallback upstream answered (Resolve only).
	Fallback bool
}

// UpstreamStat is per-upstream health for the UI.
type UpstreamStat struct {
	Upstream    string    `json:"upstream"`
	Queries     int64     `json:"queries"`
	Errors      int64     `json:"errors"`
	AvgRTTMs    float64   `json:"avgRttMs"` // EWMA
	LastError   string    `json:"lastError,omitempty"`
	LastErrorAt time.Time `json:"lastErrorAt,omitzero"`
	Healthy     bool      `json:"healthy"`
}

// CacheStat describes the response cache. The counters count since the
// start.
type CacheStat struct {
	Entries    int   `json:"entries"`
	Capacity   int   `json:"capacity"` // 0 while the cache is disabled
	Hits       int64 `json:"hits"`     // answers from the cache, stale ones included
	Misses     int64 `json:"misses"`
	StaleHits  int64 `json:"staleHits"`
	Insertions int64 `json:"insertions"` // answers stored
	Evictions  int64 `json:"evictions"`  // entries removed for the capacity
	Expired    int64 `json:"expired"`    // entries removed after their TTL and the serve-stale window
	// Types are the current entries by record type: the 16 largest, most
	// entries first, and "OTHER" for the rest. Never null.
	Types []CacheTypeStat `json:"types"`
}

// CacheTypeStat is the number of cached answers of one record type.
type CacheTypeStat struct {
	Type    string `json:"type"`
	Entries int    `json:"entries"`
}

// TestResult is the outcome of testing one upstream string.
type TestResult struct {
	Upstream string  `json:"upstream"`
	OK       bool    `json:"ok"`
	RTTMs    float64 `json:"rttMs"`
	Answer   string  `json:"answer,omitempty"`
	Error    string  `json:"error,omitempty"`
}

const (
	defaultAttemptTimeout = 3 * time.Second // per upstream attempt
	probeTimeout          = time.Second
	testTimeout           = 5 * time.Second
	refreshWorkers        = 4
	refreshQueueSize      = 256
	maxInflight           = 4096
	maxViaSets            = 64
)

var (
	errClosed      = errors.New("upstream: resolver is closed")
	errBusy        = errors.New("upstream: too many concurrent queries")
	errBadRequest  = errors.New("upstream: request must have exactly one question")
	errNoUpstreams = errors.New("upstream: no usable upstream configured")
	errNoPlainPTR  = errors.New("upstream: no plain DNS server given for the PTR lookup")
	errInvalidAddr = errors.New("upstream: invalid address")
)

// options are internal knobs; tests override them through newResolver.
type options struct {
	rootCAs   *x509.CertPool // nil = system roots
	plainPort int            // port for bootstrap servers and probes (53)
	attempt   time.Duration  // per-attempt timeout
	buildDate time.Time      // clock guard threshold; zero disables it
	// transport, if set and returning non-nil, replaces the real transport
	// for an upstream (fakes in tests).
	transport func(spec settings.UpstreamSpec) transport
	// publicFilter keeps the addresses that named plain upstreams and the
	// fastest-address probes may dial: public unicast, not this machine
	// (netutil.SafeDialer's filter). nil = that filter.
	publicFilter func(ctx context.Context, addrs []netip.Addr) ([]netip.Addr, error)
	// probeDial dials a fastest-address probe (nil = net.Dialer).
	probeDial func(ctx context.Context, network, address string) (net.Conn, error)
}

func defaultOptions() options {
	return options{
		plainPort: 53,
		attempt:   defaultAttemptTimeout,
		buildDate: parseBuildDate(version.Date),
	}
}

// safeFilter is netutil.SafeDialer's destination filter without private
// destinations: public unicast addresses that are not this machine's
// (embedded IPv4 addresses judged too).
func safeFilter(ctx context.Context, addrs []netip.Addr) ([]netip.Addr, error) {
	d := netutil.SafeDialer{AllowPrivate: func(context.Context) bool { return false }}
	return d.Filter(ctx, addrs)
}

// parseBuildDate parses version.Date (RFC 3339 or YYYY-MM-DD); anything else
// ("unknown") disables the clock guard.
func parseBuildDate(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// defaultSets is the upstream configuration derived from one settings
// snapshot. It is replaced as a whole when the upstreams change.
type defaultSets struct {
	normal   *upstreamSet
	guard    *upstreamSet // plain fallback for the clock guard; nil if none
	fallback *upstreamSet // dns.fallbackUpstreams; nil if none
	boot     *bootstrap
}

// Resolver is safe for concurrent use. It follows settings changes.
type Resolver struct {
	set  *settings.Store
	log  *slog.Logger
	opts options

	life      context.Context // cancelled on shutdown; parent of all exchanges
	stop      context.CancelFunc
	unsub     func()
	closeOnce sync.Once

	lifeMu sync.Mutex // guards closed and wg.Add
	closed bool
	wg     sync.WaitGroup // exchange and refresh goroutines

	mu  sync.Mutex // serialises rebuilds and guards via and groupCfg
	def atomic.Pointer[defaultSets]
	via map[string]*upstreamSet
	// groups are the group sets built from groupCfg (SetGroupUpstreams).
	groups   atomic.Pointer[groupSets]
	groupCfg []GroupUpstreams
	// viaOrder is the insertion order of via (oldest first) for eviction.
	viaOrder []string

	cache    respCache
	flight   flightGroup
	ips      ipCache
	refreshQ chan refreshJob
	workers  atomic.Bool // refresh workers are running
	prober   *prober     // fastest-address probes

	guardLogged  atomic.Bool
	lastFallback atomic.Int64 // unix nanoseconds a fallback last answered a fetch; 0 = never
}

// New creates a resolver from the current settings and subscribes to changes.
func New(set *settings.Store, log *slog.Logger) (*Resolver, error) {
	return newResolver(set, log, defaultOptions()), nil
}

func newResolver(set *settings.Store, log *slog.Logger, opts options) *Resolver {
	if opts.publicFilter == nil {
		opts.publicFilter = safeFilter
	}
	if opts.probeDial == nil {
		var d net.Dialer
		opts.probeDial = d.DialContext
	}
	r := &Resolver{
		set:      set,
		log:      log.With(slog.String("component", "upstream")),
		opts:     opts,
		via:      map[string]*upstreamSet{},
		refreshQ: make(chan refreshJob, refreshQueueSize),
		prober:   newProber(opts.probeDial, opts.publicFilter),
	}
	r.life, r.stop = context.WithCancel(context.Background())
	r.cache.init()
	r.flight.m = map[cacheKey]*call{}
	r.ips.m = map[ipKey]ipEntry{}
	r.rebuild(set.Get().DNS)
	r.unsub = set.Subscribe(func(old, next *settings.All) { r.onSettings(old.DNS, next.DNS) })
	return r
}

// Start runs background maintenance (stale refresh workers). Blocks until
// ctx is done and its goroutines have exited; afterwards no new upstream
// exchanges are started (cache hits are still answered).
func (r *Resolver) Start(ctx context.Context) {
	var workers sync.WaitGroup
	for range refreshWorkers {
		workers.Go(func() { r.refreshLoop(ctx) })
	}
	r.workers.Store(true)
	<-ctx.Done()
	r.workers.Store(false)
	r.shutdown()
	workers.Wait()
	r.wg.Wait()
}

// Close releases connections. In-flight exchanges are cancelled.
func (r *Resolver) Close() error {
	r.closeOnce.Do(func() {
		r.shutdown()
		r.unsub()
		r.mu.Lock()
		if ds := r.def.Load(); ds != nil {
			ds.close()
		}
		r.dropViaLocked()
		if gs := r.groups.Swap(nil); gs != nil {
			gs.close()
		}
		r.mu.Unlock()
		r.wg.Wait()
	})
	return nil
}

func (r *Resolver) shutdown() {
	r.lifeMu.Lock()
	r.closed = true
	r.lifeMu.Unlock()
	r.stop()
}

// goTracked runs fn in a goroutine that Start and Close wait for. It returns
// false (and does not run fn) after shutdown.
func (r *Resolver) goTracked(fn func()) bool {
	r.lifeMu.Lock()
	if r.closed {
		r.lifeMu.Unlock()
		return false
	}
	r.wg.Add(1)
	r.lifeMu.Unlock()
	go func() {
		defer r.wg.Done()
		fn()
	}()
	return true
}

// onSettings follows settings changes: a new upstream, fallback or
// bootstrap list (or bootstrap order) rebuilds the upstream sets; cache
// changes resize or empty the cache.
func (r *Resolver) onSettings(old, next settings.DNS) {
	if !slices.Equal(old.Upstreams, next.Upstreams) || !slices.Equal(old.FallbackUpstreams, next.FallbackUpstreams) ||
		!slices.Equal(old.Bootstrap, next.Bootstrap) || old.BootstrapPreferIPv6 != next.BootstrapPreferIPv6 {
		r.rebuild(next)
	}
	switch {
	case !next.CacheEnabled || next.CacheSize == 0:
		r.FlushCache()
	case next.CacheSize < old.CacheSize:
		r.cache.trim(next.CacheSize)
	}
}

// rebuild replaces the default upstream sets and drops all ResolveVia sets
// (their DoT/DoH hostnames depend on the bootstrap servers). Per-upstream
// statistics survive for upstreams that stay configured.
func (r *Resolver) rebuild(d settings.DNS) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lifeMu.Lock()
	closed := r.closed
	r.lifeMu.Unlock()
	if closed {
		return
	}
	prev, prevFallback := map[string]*upstreamStats{}, map[string]*upstreamStats{}
	old := r.def.Load()
	if old != nil {
		old.collectStats(prev, prevFallback)
	}
	boot := newBootstrap(d.Bootstrap, r.opts.plainPort, d.BootstrapPreferIPv6)
	normal, errs := r.buildSet("default", d.Upstreams, boot, prev)
	for _, err := range errs {
		r.log.Warn("ignoring upstream", slog.Any("err", err))
	}
	ds := &defaultSets{normal: normal, boot: boot, guard: r.buildGuardSet(d, boot, prev)}
	if len(d.FallbackUpstreams) > 0 {
		// Fallback answers are cached under the default set's key, so the
		// fallback set needs no cache namespace of its own; its upstreams
		// keep their own statistics.
		fb, errs := r.buildSet("fallback", d.FallbackUpstreams, boot, prevFallback)
		for _, err := range errs {
			r.log.Warn("ignoring fallback upstream", slog.Any("err", err))
		}
		if len(fb.ups) > 0 {
			ds.fallback = fb
		}
	}
	r.def.Store(ds)
	if old != nil {
		old.close()
	}
	r.dropViaLocked()
	if len(r.groupCfg) > 0 {
		r.rebuildGroupsLocked(boot) // named group upstreams depend on the bootstrap servers
	}
	var fallbacks []string
	if ds.fallback != nil {
		fallbacks = ds.fallback.names()
	}
	r.log.Info("upstreams configured", slog.Any("upstreams", normal.names()), slog.Any("fallbacks", fallbacks),
		slog.Int("bootstrap", len(boot.servers)))
}

// buildGuardSet returns the clock-guard fallback: the configured plain
// upstreams given by IP address followed by the bootstrap servers over
// plain DNS; nil if there is neither. Plain upstreams given by name are
// left out (their names would need the upstreams the guard replaces).
func (r *Resolver) buildGuardSet(d settings.DNS, boot *bootstrap, prev map[string]*upstreamStats) *upstreamSet {
	var list []string
	seen := map[string]bool{}
	for _, u := range append(slices.Clone(d.Upstreams), boot.servers...) {
		spec, err := settings.ParseUpstream(u)
		if err == nil && spec.IsIPLit && (spec.Proto == "udp" || spec.Proto == "tcp") && !seen[spec.Addr()] {
			seen[spec.Addr()] = true
			list = append(list, u)
		}
	}
	if len(list) == 0 {
		return nil
	}
	s, _ := r.buildSet("guard", list, boot, prev)
	if len(s.ups) == 0 {
		return nil
	}
	return s
}

// defaultSet returns the set used by Resolve and LookupIP: the configured
// upstreams, or the plain fallback while the clock guard is active.
func (r *Resolver) defaultSet() *upstreamSet { return r.defaultRoute().set }

// defaultRoute returns the route of Resolve and LookupIP: the configured
// upstreams with the fallbacks, or the clock-guard set (without fallbacks)
// while the clock guard is active.
func (r *Resolver) defaultRoute() route {
	ds := r.def.Load()
	if ds.guard != nil && r.clockBehind() {
		if !r.guardLogged.Swap(true) {
			r.log.Warn("system clock is before the build date: encrypted upstreams are skipped and plain DNS to the bootstrap servers is used until the clock is set",
				slog.Time("buildDate", r.opts.buildDate), slog.Time("now", time.Now()))
		}
		return route{set: ds.guard, def: true}
	}
	return route{set: ds.normal, fallback: ds.fallback, def: true}
}

func (r *Resolver) clockBehind() bool {
	return !r.opts.buildDate.IsZero() && time.Now().Before(r.opts.buildDate)
}

// viaSet returns the (cached) upstream set for ResolveVia and LookupPTR.
func (r *Resolver) viaSet(upstreams []string) (*upstreamSet, error) {
	if len(upstreams) == 0 {
		return nil, errNoUpstreams
	}
	key := joinKey(upstreams)
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.via[key]; ok {
		return s, nil
	}
	s, errs := r.buildSet("via", upstreams, r.def.Load().boot, nil)
	if len(s.ups) == 0 {
		return nil, errors.Join(append([]error{errNoUpstreams}, errs...)...)
	}
	if len(r.viaOrder) >= maxViaSets {
		oldest := r.viaOrder[0]
		r.viaOrder = r.viaOrder[1:]
		r.via[oldest].close()
		delete(r.via, oldest)
	}
	r.via[key] = s
	r.viaOrder = append(r.viaOrder, key)
	return s, nil
}

func (r *Resolver) dropViaLocked() {
	for _, s := range r.via {
		s.close()
	}
	clear(r.via)
	r.viaOrder = nil
}

func (ds *defaultSets) close() {
	for _, s := range []*upstreamSet{ds.normal, ds.guard, ds.fallback} {
		if s != nil {
			s.close()
		}
	}
}

func (ds *defaultSets) collectStats(into, fallbacks map[string]*upstreamStats) {
	for _, s := range []*upstreamSet{ds.normal, ds.guard, ds.fallback} {
		if s == nil {
			continue
		}
		m := into
		if s == ds.fallback {
			m = fallbacks
		}
		for _, u := range s.ups {
			m[u.name] = u.st
		}
	}
}

// Stats returns per-upstream health of the upstreams currently in use (the
// plain fallback while the clock guard is active).
func (r *Resolver) Stats() []UpstreamStat { return setStats(r.defaultSet()) }

// FallbackStats returns per-upstream health of the fallback upstreams
// (their own statistics; empty, never nil, without fallbacks).
func (r *Resolver) FallbackStats() []UpstreamStat {
	return setStats(r.def.Load().fallback)
}

// LastFallback returns the time a fallback upstream last answered a fetch
// (zero if never since the start).
func (r *Resolver) LastFallback() time.Time {
	if ns := r.lastFallback.Load(); ns != 0 {
		return time.Unix(0, ns).UTC()
	}
	return time.Time{}
}

func setStats(set *upstreamSet) []UpstreamStat {
	if set == nil {
		return []UpstreamStat{}
	}
	out := make([]UpstreamStat, 0, len(set.ups))
	for _, u := range set.ups {
		out = append(out, u.st.snapshot(u.name))
	}
	return out
}

// CacheStats returns response cache counters.
func (r *Resolver) CacheStats() CacheStat {
	d := r.set.Get().DNS
	st := CacheStat{
		Entries:    r.cache.len(),
		Hits:       r.cache.hits.Load(),
		Misses:     r.cache.misses.Load(),
		StaleHits:  r.cache.staleHits.Load(),
		Insertions: r.cache.insertions.Load(),
		Evictions:  r.cache.evictions.Load(),
		Expired:    r.cache.expired.Load(),
		Types:      r.cache.typeStats(),
	}
	if d.CacheEnabled {
		st.Capacity = d.CacheSize
	}
	return st
}

// ClockGuard reports whether the clock guard is active (plain DNS fallback).
func (r *Resolver) ClockGuard() bool {
	ds := r.def.Load()
	return ds != nil && ds.guard != nil && r.clockBehind()
}

// FlushCache empties the response cache and the LookupIP cache.
func (r *Resolver) FlushCache() {
	r.cache.flush()
	r.ips.flush()
}

func joinKey(list []string) string {
	n := 0
	for _, s := range list {
		n += len(s) + 1
	}
	b := make([]byte, 0, n)
	for i, s := range list {
		if i > 0 {
			b = append(b, '\n')
		}
		b = append(b, s...)
	}
	return string(b)
}

// msFloat converts d to milliseconds rounded to 0.1 ms.
func msFloat(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Millisecond)*10) / 10
}
