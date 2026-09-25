// Package proxy is the download cache's HTTP cache on :80
// (docs/ARCHITECTURE.md 8.2): heartbeat, ACL, loop detection, host
// classification, special paths, slice cache with streaming request
// collapsing, read-ahead, no-range handling, redirects and SSRF-safe
// upstream fetching.
//
// Request flow (handler.go): ACL → heartbeat → download cache enabled → loop
// detection → host → classification → special paths → method → canonical
// path → nocache → cache path (serve.go) or pass-through (passthrough.go).
//
// The cache path plans the response itself (ranges.go) and serves it
// position by position from three kinds of sources: cached slices
// (SliceReader.WriteRange on the unwrapped ResponseWriter, so net/http can
// use sendfile), in-flight fills (fill.go: one upstream range request per
// slice, shared by every concurrent reader through a sync.Cond-signalled
// growing buffer) and direct upstream bodies (direct.go: range failures,
// hosts without range support, fallbacks). Requests for an object whose
// upstream ignores Range collapse too: one request leads and captures the
// slices of the whole body as fills the others stream (collapse.go). A
// failing source is retried once through the store and fills, then the
// rest of the range is fetched directly; only a failure of that aborts the
// response. Fills hold a global and a per-client slot for as long as their
// buffer lives, which bounds fill memory to min(maxConcurrentFills, 1 GiB
// / slice size) buffers. The object being served is marked in use in the
// store (never evicted).
//
// Table (picache.db, component "proxy"): proxy_noslice_hosts.
package proxy

import (
	"cmp"
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
	cachestore "github.com/hustenreizjuengling/picache/internal/dlcache/store"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Consumer-side interfaces (implemented by the concrete packages; fakes in tests).

// SliceStore is the part of *cachestore.Store the proxy uses.
type SliceStore interface {
	ID() string
	SliceSize() int64
	Head(ctx context.Context, id string) (cachestore.ObjectHead, bool, error)
	ReadSlice(ctx context.Context, id string, gen uint64, idx int64) (cachestore.SliceReader, error)
	SetMeta(ctx context.Context, id string, m cachestore.Meta) (uint64, error)
	WriteSlice(ctx context.Context, id string, gen uint64, idx int64, data []byte) error
	Invalidate(ctx context.Context, id string, reason string) error
	// Touch records an access; bytesServed are the bytes served from the
	// cache (a hit when > 0).
	Touch(id string, bytesServed int64)
	// Use marks an object as being served (never evicted) until release.
	Use(id string) (release func())
}

// Classifier is the part of *services.Registry the proxy uses.
type Classifier interface {
	Classify(host, userAgent, path string) (serviceID string, enabled, known bool)
	Label(groupKey string) string
}

// Clients is the part of *clients.Registry the proxy uses.
type Clients interface {
	Identify(ip netip.Addr) *clients.Identity
}

// CacheLogger is the part of *logs.Store the proxy uses.
type CacheLogger interface {
	LogCache(e logs.CacheEvent)
}

// Deps are the collaborators of the proxy.
type Deps struct {
	DB       *db.DB // for persisted no-slice hosts
	Settings *settings.Store
	Services Classifier
	Lookup   netutil.Resolver // bypass resolver (IPv4)
	// Store returns the current store (nil → pass-through mode), the same
	// value for as long as that store is active: a request whose store is
	// no longer returned stops using it.
	Store func() SliceStore
	// StoreFull reports that the store cannot free space (no new fills).
	StoreFull  func() bool
	Clients    Clients
	Logs       CacheLogger
	ACL        *netutil.ACLWatcher
	InstanceID string // value of the processed-by response header (processedByHeader)
	Log        *slog.Logger
}

// Transfer is a live client request.
type Transfer struct {
	ID         string    `json:"id"`
	ClientIP   string    `json:"clientIp"`
	ClientName string    `json:"clientName,omitempty"`
	Service    string    `json:"service"`
	Host       string    `json:"host"`
	Path       string    `json:"path"` // no query
	GroupKey   string    `json:"groupKey"`
	Label      string    `json:"label"`
	Started    time.Time `json:"started"`
	BytesSent  int64     `json:"bytesSent"`
	BytesHit   int64     `json:"bytesHit"`
	BytesWAN   int64     `json:"bytesWan"`
	Total      int64     `json:"total"` // expected bytes for this response (0 = unknown)
	RateBps    float64   `json:"rateBps"`
}

// ActiveDownload aggregates live requests per (client, service, group) with
// a 10 s rate window; entries stay 30 s after the last request.
type ActiveDownload struct {
	ClientIP   string    `json:"clientIp"`
	ClientName string    `json:"clientName,omitempty"`
	Service    string    `json:"service"`
	GroupKey   string    `json:"groupKey"`
	Label      string    `json:"label"`
	Started    time.Time `json:"started"`
	LastSeen   time.Time `json:"lastSeen"`
	InFlight   int       `json:"inFlight"`
	BytesSent  int64     `json:"bytesSent"`
	BytesHit   int64     `json:"bytesHit"`
	BytesWAN   int64     `json:"bytesWan"`
	RateBps    float64   `json:"rateBps"`
}

// Stats are live counters.
type Stats struct {
	Requests          int64    `json:"requests"`
	ActiveClients     int64    `json:"activeClients"`
	ActiveFills       int64    `json:"activeFills"`
	BytesHit          int64    `json:"bytesHit"`
	BytesWAN          int64    `json:"bytesWan"`
	Refused           int64    `json:"refused"`
	Errors            int64    `json:"errors"`
	PassThrough       bool     `json:"passThrough"`                 // no store available
	SteamHostsRefused []string `json:"steamHostsRefused,omitempty"` // recent refused hosts with Steam UA (last 20)
}

// NoSliceHost is a host that answered range requests without range support.
type NoSliceHost struct {
	Host     string    `json:"host"`
	Failures int       `json:"failures"`
	Marked   bool      `json:"marked"` // requests go without Range
	Since    time.Time `json:"since"`
}

// timings are the proxy's time limits (ARCHITECTURE 8.2); tests shorten them.
type timings struct {
	demandWait time.Duration // demand fill waits this long for a slot, then streams directly
	stall      time.Duration // a fill without progress for this long is stalled
	idleRead   time.Duration // upstream body read idle timeout
	write      time.Duration // client write deadline, set before each write
	storeWrite time.Duration // store writes (incl. waiting for the NAS I/O semaphore)
}

var defaultTimings = timings{
	demandWait: 2 * time.Second,
	stall:      15 * time.Second,
	idleRead:   60 * time.Second,
	write:      60 * time.Second,
	storeWrite: 10 * time.Second,
}

// Server is the HTTP cache.
type Server struct {
	d        Deps
	log      *slog.Logger
	defaults *settings.All // used when Deps.Settings is nil
	tm       timings

	safe      *netutil.SafeDialer
	transport *http.Transport

	// ctx scopes fills and background store writes; it is canceled when
	// Start returns.
	ctx      context.Context
	cancel   context.CancelFunc
	goMu     sync.Mutex
	stopping bool
	wg       sync.WaitGroup

	fills   fillTable
	leaders objFetches // whole-object bodies being streamed (collapse.go)
	slots   fillSlots
	bufs    bufPool
	noslice *noSliceTracker
	https   httpsOnlyHosts
	live    liveState
	stats   counters
	warns   warnLimiter
	own     ownAddrs
	nocache atomic.Pointer[nocacheSet]
}

// New creates the proxy (migrates its table).
func New(ctx context.Context, d Deps) (*Server, error) {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	def := settings.Defaults()
	s := &Server{d: d, log: log.With(slog.String("component", "proxy")), defaults: &def, tm: defaultTimings}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.safe = &netutil.SafeDialer{
		Resolve:      d.Lookup,
		AllowPrivate: func(context.Context) bool { return s.settings().DownloadCache.AllowPrivateUpstreams },
		Timeout:      connectTimeout,
		OwnAddrs:     s.own.get,
	}
	s.transport = newTransport(s.safe.DialContext, nil)
	ns, err := newNoSliceTracker(ctx, d.DB, s.log)
	if err != nil {
		s.cancel()
		return nil, err
	}
	s.noslice = ns
	return s, nil
}

// Handler returns the :80 handler. The caller wraps the listener with
// netutil.LimitListener; the handler sets a 60 s write deadline before
// each write to the client.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

// Start runs housekeeping (active-download expiry, no-slice decay). Blocks
// until ctx is done; then it stops fills and background store writes, waits
// for them and persists pending no-slice state.
func (s *Server) Start(ctx context.Context) {
	second := time.NewTicker(time.Second)
	defer second.Stop()
	minute := time.NewTicker(time.Minute)
	defer minute.Stop()
	for {
		select {
		case <-ctx.Done():
			s.shutdown()
			return
		case now := <-second.C:
			s.live.tick(now)
		case now := <-minute.C:
			s.noslice.decay(now)
			s.https.expire(now)
			s.noslice.flush(ctx)
		case <-s.noslice.dirtySignal():
			s.noslice.flush(ctx)
		}
	}
}

// shutdown stops new background work, cancels running fills and waits for
// every goroutine the proxy started.
func (s *Server) shutdown() {
	s.goMu.Lock()
	s.stopping = true
	s.goMu.Unlock()
	s.cancel()
	s.wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.noslice.flush(ctx)
	s.noslice.close()
	s.transport.CloseIdleConnections()
}

// goTracked runs fn in a goroutine that Start waits for. It returns false
// (and does not run fn) once shutdown has begun.
func (s *Server) goTracked(fn func()) bool {
	s.goMu.Lock()
	if s.stopping {
		s.goMu.Unlock()
		return false
	}
	s.wg.Add(1)
	s.goMu.Unlock()
	go func() {
		defer s.wg.Done()
		fn()
	}()
	return true
}

// Active returns live transfers (oldest first).
func (s *Server) Active() []Transfer { return s.live.transfersSnapshot(time.Now()) }

// ActiveDownloads returns live downloads aggregated per client and content
// (fastest first).
func (s *Server) ActiveDownloads() []ActiveDownload { return s.live.downloadsSnapshot(time.Now()) }

// Stats returns live counters.
func (s *Server) Stats() Stats {
	return Stats{
		Requests:          s.stats.requests.Load(),
		ActiveClients:     int64(s.live.activeClients()),
		ActiveFills:       s.stats.activeFills.Load(),
		BytesHit:          s.stats.bytesHit.Load(),
		BytesWAN:          s.stats.bytesWAN.Load(),
		Refused:           s.stats.refused.Load(),
		Errors:            s.stats.errors.Load(),
		PassThrough:       s.store() == nil,
		SteamHostsRefused: s.stats.steamHosts(),
	}
}

// NoSliceHosts lists hosts detected without range support (by host name).
func (s *Server) NoSliceHosts(ctx context.Context) ([]NoSliceHost, error) {
	out := s.noslice.list()
	slices.SortFunc(out, func(a, b NoSliceHost) int { return cmp.Compare(a.Host, b.Host) })
	return out, nil
}

// ResetNoSlice clears the no-slice state of a host (apperr.Invalid for a
// malformed host, apperr.NotFound for an unknown one).
func (s *Server) ResetNoSlice(ctx context.Context, host string) error {
	return s.noslice.reset(ctx, host)
}

// settings returns the current settings snapshot.
func (s *Server) settings() *settings.All {
	if s.d.Settings == nil {
		return s.defaults
	}
	return s.d.Settings.Get()
}

// store returns the current store (nil: pass-through mode).
func (s *Server) store() SliceStore {
	if s.d.Store == nil {
		return nil
	}
	return s.d.Store()
}

// storeFull reports whether new content must not be stored.
func (s *Server) storeFull() bool { return s.d.StoreFull != nil && s.d.StoreFull() }

// nocacheSet caches the parsed downloadCache.nocacheClients of one settings snapshot.
type nocacheSet struct {
	src      *settings.All
	prefixes []netip.Prefix
}

// nocacheAllowed reports whether ip may force a refetch with ?nocache.
func (s *Server) nocacheAllowed(ip netip.Addr) bool {
	all := s.settings()
	ns := s.nocache.Load()
	if ns == nil || ns.src != all {
		ns = &nocacheSet{src: all, prefixes: settings.ParsePrefixes(all.DownloadCache.NocacheClients)}
		s.nocache.Store(ns)
	}
	ip = netutil.Canon(ip)
	for _, p := range ns.prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// ownAddrs caches this machine's addresses for the SSRF guard (refreshed
// at most once a minute instead of on every dial).
type ownAddrs struct {
	mu      sync.Mutex
	addrs   []netip.Addr
	fetched time.Time
}

func (o *ownAddrs) get() []netip.Addr {
	o.mu.Lock()
	defer o.mu.Unlock()
	if now := time.Now(); now.Sub(o.fetched) > time.Minute {
		o.addrs, o.fetched = netutil.LocalAddrs(), now
	}
	return o.addrs
}

// warnLimiter lets a warning through at most once per hour per key
// (ARCHITECTURE 5: repeated errors are logged at most hourly).
type warnLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

const maxWarnKeys = 256

func (l *warnLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.last[key]; ok && now.Sub(t) < time.Hour {
		return false
	}
	if l.last == nil || len(l.last) >= maxWarnKeys {
		l.last = make(map[string]time.Time)
	}
	l.last[key] = now
	return true
}
