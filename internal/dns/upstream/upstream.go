// Package upstream forwards DNS queries to upstream resolvers (UDP, TCP, DoT,
// DoH) with a response cache, serve-stale, in-flight de-duplication and the
// upstream modes load_balance/parallel/strict/fastest_addr
// (docs/ARCHITECTURE.md 7.4).
//
// The default set (Resolve, LookupIP) is the configured upstreams, the
// clock-guard set while the clock guard is active, and the fallbacks, asked
// only when no default upstream replied at all (while every default
// upstream is unhealthy: when none replied within 500 ms, exchangeHedged).
// Its answers are classified
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
// set DO or dns.dnssecMode is passthrough or validate; the encrypted
// transports (DoT, DoH, DoQ, HTTP/3) add EDNS padding to a multiple of 128
// bytes and remove the padding of the reply. Client EDNS options, the CD
// bit and the client's ID never reach an upstream.
//
// DNSSEC (docs/ARCHITECTURE.md 7.6; mode validate): the answers of the
// validated routes (the default route, the group sets, the validating
// forwarders registered with SetValidatingForwarders) are validated once
// per fetch by internal/dns/dnssec (rcode NOERROR or NXDOMAIN, not
// classified as a block; after the classification, before the
// fastest-address order) and cached with their verdict (Info.DNSSEC);
// their Answer section keeps only the answer to the question (the chain
// from the question name). Stale hits are indeterminate. The cache key
// holds whether the fetch validates. Chain lookups (DS, DNSKEY; DO=1,
// CD=0, fresh ID) use the route that answered (only its fallbacks when
// they answered the fetch), bypass the response cache and are counted in
// the upstream statistics; a fetch started for a client (WithClient: its
// device key) draws on that device's share of them. In validate
// mode every returned message carries AD exactly when its verdict is
// secure and it is not stale; the upstream's AD is discarded on every
// route. Modes off and passthrough pass the upstream's AD on. Probes
// (". DNSKEY" and ". SOA" straight through the transport) judge each
// upstream of a validated route (the clock-guard sets only while the clock
// guard is active): capable, no-dnssec, anchor-mismatch or unknown; data
// from a no-dnssec or anchor-mismatch upstream is indeterminate. Time
// checks (RRSIG validity) are suspended while the clock guard is active,
// the host clock (read through SetClockReader, at most once a second) is
// readable and not synchronised, or, while it is not known to be
// synchronised, the probes of the capable upstreams agree that the root's
// signatures are outside their period now; they resume after 60 s without
// any of these, which empties the caches. Every reply must echo the question
// (qname case-insensitively, qtype, qclass); UDP replies that do not are
// discarded and the exchange keeps waiting, TCP/DoT/DoH replies fail the
// attempt.
//
// Clock guard: while time.Now() is before the binary's build date
// (version.Date; ignored when "unknown"), encrypted upstreams are skipped and
// queries go over plain UDP/TCP to the bootstrap IPs (logged once at WARN,
// reported by ClockGuard). Certificate errors alone never trigger this.
// A stale host clock (not synchronised, behind a genuine certificate of an
// encrypted upstream that is not yet valid by it) is handled by checking
// the certificates at the start of that certificate's validity instead
// (certTime, learnCertFloor), never by plain DNS.
//
// Bounds: response cache ≤ dns.cacheSize entries and 64 MiB of wire data
// (responses above 16 KiB are not cached; TTLs capped at 7 days), 4096
// distinct in-flight queries, 16 upstreams per set, 64 ResolveVia sets,
// 256 queued stale refreshes, 4 idle DoT connections per upstream, 4 DoH
// connections per upstream (each dial bounded by the attempt timeout, a
// silent HTTP/2 connection pinged after 10 s and closed 5 s later), DoH
// bodies ≤ 64 KiB, 64 bootstrap hostnames, 1024 LookupIP results; the
// duplicate check covers sections of at most 256 records; 16 EDE options
// examined per reply, EDE texts ≤ 200 bytes; fastest-address probes: 8
// addresses and 300 ms per answer, 32 dials at a time, 100 new targets per
// second, 4096 cached results (10 minutes). DNSSEC: the bounds of the
// dnssec package per validation context (32 chain lookups, 4 s, 16
// signature verifications of which 2 failed, 256 NSEC3 hashes, …), 1024
// chain steps in flight, 50 new chain exchanges per second (burst 200) of
// which 25 per second (burst 200) for one client key (4096 keys tracked;
// a lookup waits for its turn of both within its context's deadline, else
// it is refused at once), GOMAXPROCS concurrent signature
// verifications, the key cache (16 384 zone states, 8 MiB) and the
// failure cache (4096 entries); a bogus answer is cached min(its TTL,
// 30 s) after a cryptographic failure and 5 s otherwise, never served
// stale, and not at all after a local refusal (a chain rate limit, the
// flight cap); a secure one at most until its earliest RRSIG expiration;
// probes: 2 queries of 2 s per upstream every 30 minutes (30 s while
// unknown or while its probes get no reply), 16 at a time; one bogus WARN
// per zone per 10 minutes (1024 zones remembered).
package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/version"
)

// Info describes how a response was obtained. Block, EDE and Fallback are
// stored with a cached answer and returned by cache and stale hits too.
type Info struct {
	Upstream string        // display name of the upstream that answered ("" for cache hits)
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
	// DNSSEC is the verdict of a validated answer (mode validate, a
	// validated route); nil when it was not validated. Stored with the
	// answer; a stale hit is indeterminate.
	DNSSEC *Verdict
}

// UpstreamStat is per-upstream health for the UI. Upstream is the
// configured string (the UI matches it with the settings); Name is its
// display name (settings.UpstreamSpec.Display: a DNS stamp as
// sdns:<protocol>:<host>, never the stamp).
type UpstreamStat struct {
	Upstream    string    `json:"upstream"`
	Name        string    `json:"name"`
	Queries     int64     `json:"queries"`
	Errors      int64     `json:"errors"`
	AvgRTTMs    float64   `json:"avgRttMs"` // EWMA
	LastError   string    `json:"lastError,omitempty"`
	LastErrorAt time.Time `json:"lastErrorAt,omitzero"`
	Healthy     bool      `json:"healthy"`
	// DNSSEC is the probe state (capable, no-dnssec, anchor-mismatch,
	// unknown), DNSSECError why it is not capable (≤ 200 bytes) and
	// DNSSECCheckedAt when a probe last answered: only in validate mode
	// for upstreams of validated routes.
	DNSSEC          string    `json:"dnssec,omitempty"`
	DNSSECError     string    `json:"dnssecError,omitempty"`
	DNSSECCheckedAt time.Time `json:"dnssecCheckedAt,omitzero"`
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
	// Validation describes the DNSSEC key and failure caches (validate
	// mode only).
	Validation *ValidationStat `json:"validation,omitempty"`
}

// ValidationStat describes the DNSSEC caches: the validated zone states
// kept (per route), their bytes and the chain failures kept.
type ValidationStat struct {
	Zones    int `json:"zones"`
	Bytes    int `json:"bytes"`
	Failures int `json:"failures"`
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
	errInternal    = errors.New("upstream: internal error")
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
	// quicIdle is the idle timeout of QUIC connections (DoQ, HTTP/3).
	quicIdle time.Duration
	// dohPingAfter and dohPingTimeout are the HTTP/2 health check of the
	// DoH connections (0: dohPingAfter, dohPingTimeout); dohDial dials one
	// address of a DoH upstream (nil: net.Dialer).
	dohPingAfter, dohPingTimeout time.Duration
	dohDial                      func(ctx context.Context, network, address string) (net.Conn, error)
	// DNSSEC timings (0: the defaults): the probe interval, the retry
	// while an upstream is unknown, the time of one probe query, the
	// quiet time before time checks resume and the maintenance tick.
	probeEvery, probeRetry, probeTimeout, resumeAfter, dnssecTick time.Duration
	// anchors replace the root trust anchors (tests).
	anchors []*dns.DS
}

func defaultOptions() options {
	return options{
		plainPort: 53,
		attempt:   defaultAttemptTimeout,
		quicIdle:  quicIdleTimeout,
		buildDate: parseBuildDate(version.Date),
	}
}

// withDefaults fills the DNSSEC timings and the DoH health check left at
// zero.
func (o options) withDefaults() options {
	for _, v := range []struct {
		p *time.Duration
		d time.Duration
	}{{&o.probeEvery, probeEvery}, {&o.probeRetry, probeRetry}, {&o.probeTimeout, probeQueryTimeout},
		{&o.resumeAfter, resumeAfter}, {&o.dnssecTick, dnssecTick}, {&o.dohPingAfter, dohPingAfter},
		{&o.dohPingTimeout, dohPingTimeout}} {
		if *v.p <= 0 {
			*v.p = v.d
		}
	}
	return o
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

	mu  sync.Mutex // serialises rebuilds and guards via, groupCfg and fwdCfg
	def atomic.Pointer[defaultSets]
	via map[string]*upstreamSet
	// groups are the group sets built from groupCfg (SetGroupUpstreams).
	groups   atomic.Pointer[groupSets]
	groupCfg []GroupUpstreams
	// viaOrder is the insertion order of via (oldest first) for eviction.
	viaOrder []string
	// fwds are the target sets of the validating forwarders built from
	// fwdCfg (SetValidatingForwarders), pinned outside the via LRU.
	fwds   atomic.Pointer[fwdSets]
	fwdCfg [][]string

	cache    respCache
	flight   flightGroup
	ips      ipCache
	refreshQ chan refreshJob
	workers  atomic.Bool // refresh workers are running
	prober   *prober     // fastest-address probes

	guardLogged  atomic.Bool
	lastFallback atomic.Int64 // unix nanoseconds a fallback last answered a fetch; 0 = never
	// fallbackAt is a ring of the times (unix nanoseconds, 0 = none) the
	// fallbacks answered the last FallbackTimesKept fetches, written at
	// fallbackSeq (FallbacksSince).
	fallbackAt  [FallbackTimesKept]atomic.Int64
	fallbackSeq atomic.Uint64
	// certFloor is a lower bound of the real time learned from an
	// upstream's certificate (learnCertFloor; unix nanoseconds, 0 = none).
	certFloor atomic.Int64

	// val is the DNSSEC state: the validator, the time checks, the probes
	// and the validating forwarders (dnssec.go).
	val *validation
}

// New creates a resolver from the current settings and subscribes to changes.
func New(set *settings.Store, log *slog.Logger) (*Resolver, error) {
	return newResolver(set, log, defaultOptions()), nil
}

func newResolver(set *settings.Store, log *slog.Logger, opts options) *Resolver {
	opts = opts.withDefaults()
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
	r.val = newValidation(r)
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
	workers.Go(func() { r.dnssecLoop(ctx) })
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
		if fs := r.fwds.Swap(nil); fs != nil {
			fs.close()
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
// false (and does not run fn) after shutdown. A panic in fn is logged and
// ends only fn, never the process (these goroutines exchange, validate and
// probe with untrusted data; the DNS server's handler has the same guard);
// fn cleans up with defers, so its waiters still get an answer.
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
		defer func() {
			if v := recover(); v != nil {
				r.log.Error("panic in a DNS background task", slog.Any("panic", v), slog.String("stack", string(debug.Stack())))
			}
		}()
		fn()
	}()
	return true
}

// onSettings follows settings changes: a new upstream, fallback or
// bootstrap list (or bootstrap order) rebuilds the upstream sets; cache
// changes resize or empty the cache; a new DNSSEC mode empties the
// response, LookupIP, key and failure caches (switching to validate
// starts the probes).
func (r *Resolver) onSettings(old, next settings.DNS) {
	if !slices.Equal(old.Upstreams, next.Upstreams) || !slices.Equal(old.FallbackUpstreams, next.FallbackUpstreams) ||
		!slices.Equal(old.Bootstrap, next.Bootstrap) || old.BootstrapPreferIPv6 != next.BootstrapPreferIPv6 {
		r.rebuild(next)
	}
	switch {
	case !next.CacheEnabled || next.CacheSize == 0:
		r.flushResponses()
	case next.CacheSize < old.CacheSize:
		r.cache.trim(next.CacheSize)
	}
	if old.DNSSECMode != next.DNSSECMode {
		r.FlushCache()
		if next.DNSSECMode == settings.DNSSECValidate {
			r.val.probeAllSoon()
		}
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
	if len(r.fwdCfg) > 0 {
		r.rebuildForwardersLocked(boot)
	}
	r.val.probeAllSoon() // the rebuilt sets are probed again
	var fallbacks []string
	if ds.fallback != nil {
		fallbacks = ds.fallback.displays()
	}
	r.log.Info("upstreams configured", slog.Any("upstreams", normal.displays()), slog.Any("fallbacks", fallbacks),
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
		return route{set: ds.guard, def: true, validate: true}
	}
	return route{set: ds.normal, fallback: ds.fallback, def: true, validate: true}
}

func (r *Resolver) clockBehind() bool {
	return !r.opts.buildDate.IsZero() && time.Now().Before(r.opts.buildDate)
}

// certTime is the clock of the certificate checks of the encrypted
// upstreams: the host clock, or the lower bound of the real time learned
// from an upstream's certificate while the host clock is behind it
// (learnCertFloor). A later clock never accepts a certificate that has
// expired by the host clock; it only accepts one whose validity started
// before the bound.
func (r *Resolver) certTime() time.Time {
	now := time.Now()
	if f := r.certFloor.Load(); f != 0 && now.UnixNano() < f {
		return time.Unix(0, f)
	}
	return now
}

// learnCertFloor lets a host whose clock is stale (restored from the last
// shutdown on a device without a real-time clock, and not synchronised
// yet) reach its encrypted upstreams once their certificates were renewed
// while it was off. It learns a lower bound of the real time from err, a
// failed exchange with u: the TLS handshake failed because a certificate
// was not yet valid by the host clock, the host clock is readable and not
// synchronised (SetClockReader), and the chain verifies against the roots
// for u's host name at the start of that certificate's validity (a CA
// never dates a certificate ahead, so the real time is at least that).
// certTime then checks certificates at that time while the host clock is
// behind it. It reports whether the bound was raised (the attempt is worth
// repeating). A certificate that has expired, an unverifiable chain, a
// synchronised or unreadable host clock never change anything: with a
// correct clock nothing is relaxed.
func (r *Resolver) learnCertFloor(err error, u *upstream) bool {
	var cve *tls.CertificateVerificationError
	var cie x509.CertificateInvalidError
	if err == nil || !errors.As(err, &cve) || !errors.As(cve.Err, &cie) || cie.Reason != x509.Expired ||
		cie.Cert == nil || len(cve.UnverifiedCertificates) == 0 {
		return false
	}
	now, from := time.Now(), cie.Cert.NotBefore
	if !now.Before(from) || from.UnixNano() <= r.certFloor.Load() {
		return false // expired, or not later than the bound in use
	}
	if unsynced, _ := r.val.hostClock(now); !unsynced {
		return false
	}
	inter := x509.NewCertPool()
	for _, c := range cve.UnverifiedCertificates[1:] {
		inter.AddCert(c)
	}
	if _, err := cve.UnverifiedCertificates[0].Verify(x509.VerifyOptions{DNSName: u.host, Roots: r.opts.rootCAs,
		Intermediates: inter, CurrentTime: from}); err != nil {
		return false
	}
	for {
		old := r.certFloor.Load()
		if from.UnixNano() <= old {
			return false
		}
		if r.certFloor.CompareAndSwap(old, from.UnixNano()) {
			break
		}
	}
	r.log.Warn("the system clock is behind the certificate of an encrypted upstream and not synchronised: certificates are checked at the start of its validity until the clock has passed it",
		slog.String("upstream", u.display), slog.Time("validFrom", from), slog.Time("now", now))
	return true
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
// plain fallback while the clock guard is active), with their DNSSEC probe
// state in validate mode.
func (r *Resolver) Stats() []UpstreamStat { return setStats(r.defaultSet(), r.validating()) }

// FallbackStats returns per-upstream health of the fallback upstreams
// (their own statistics; empty, never nil, without fallbacks).
func (r *Resolver) FallbackStats() []UpstreamStat {
	return setStats(r.def.Load().fallback, r.validating())
}

// LastFallback returns the time a fallback upstream last answered a fetch
// (zero if never since the start).
func (r *Resolver) LastFallback() time.Time {
	if ns := r.lastFallback.Load(); ns != 0 {
		return time.Unix(0, ns).UTC()
	}
	return time.Time{}
}

// FallbackTimesKept is how many of the latest fallback answers
// FallbacksSince remembers.
const FallbackTimesKept = 8

// noteFallback records that a fallback answered a fetch.
func (r *Resolver) noteFallback() {
	now := time.Now().UnixNano()
	r.lastFallback.Store(now)
	r.fallbackAt[r.fallbackSeq.Add(1)%FallbackTimesKept].Store(now)
}

// FallbacksSince returns how many fetches the fallbacks answered after
// since, counting only the latest FallbackTimesKept.
func (r *Resolver) FallbacksSince(since time.Time) int {
	n := 0
	for i := range r.fallbackAt {
		if ns := r.fallbackAt[i].Load(); ns != 0 && ns > since.UnixNano() {
			n++
		}
	}
	return n
}

// setStats returns the statistics of a set's upstreams; withDNSSEC adds
// their probe state.
func setStats(set *upstreamSet, withDNSSEC bool) []UpstreamStat {
	if set == nil {
		return []UpstreamStat{}
	}
	out := make([]UpstreamStat, 0, len(set.ups))
	for _, u := range set.ups {
		out = append(out, u.st.snapshot(u.name, u.display, withDNSSEC))
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
	if d.Validating() {
		zones, bytes, failures := r.val.v.Stats()
		st.Validation = &ValidationStat{Zones: zones, Bytes: bytes, Failures: failures}
	}
	return st
}

// ClockGuard reports whether the clock guard is active (plain DNS fallback).
func (r *Resolver) ClockGuard() bool {
	ds := r.def.Load()
	return ds != nil && ds.guard != nil && r.clockBehind()
}

// FlushCache empties the response cache, the LookupIP cache and the
// DNSSEC key and failure caches (POST /dns/cache/flush, a new DNSSEC
// mode).
func (r *Resolver) FlushCache() {
	r.flushResponses()
	r.val.v.Flush()
}

// flushResponses empties the response cache and the LookupIP cache.
func (r *Resolver) flushResponses() {
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
