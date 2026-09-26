// Package dnsserver serves DNS over UDP and TCP and implements the request
// pipeline of docs/ARCHITECTURE.md 7.1: ACL, blocked clients, rate limit,
// hardening, client identity (with EDNS from trusted forwarders),
// special-use names, local records (and the names of DHCP leases,
// Deps.Leases), parental controls and protection lists, dropped domains,
// safe search, download cache answers, special domains, filtering (with
// the filtering groups: a paused group's lists and rules stop),
// single-label names, conditional forwarding /
// router resolver, upstream resolution, upstream blocks, bogus NXDOMAIN,
// CNAME inspection, DNS rebinding protection, reply shaping and logging.
// It also owns local DNS records and conditional forwarders. Health probes
// from this machine (HealthProbeName) are answered before the pipeline and
// never counted or logged.
//
// Two addresses describe a query: the source (the transport peer: ACL,
// blocked sources, rate limit, loop guard, this server's names, ECS) and
// the client (the identity: the source, or the address a trusted forwarder
// named in EDNS: groups, blocked identities, query log, statistics).
//
// Serving: UDP with (&dns.Server{PacketConn: pc, Handler: h}).ActivateAndServe()
// (miekg replies from the query's destination address via IP_PKTINFO) and TCP
// with Listener: ln, MaxTCPQueries 128, IdleTimeout 8 s. No custom
// ReadFrom/WriteTo loops; a reader decorator drops UDP packets from sources
// outside the ACL and from blocked sources before they are parsed. The rate
// limiter is swept every 10 s.
//
// Tables (picache.db, component "dns"): dns_records, dns_forwarders,
// dns_forwarder_domains.
//
// Bounds: CNAME chains (local and upstream) are followed at most 8 hops with
// a visited set (else SERVFAIL, status "error"); records that reference
// themselves are rejected. At most 4096 queries are processed concurrently.
// The sources of queries dropped by the ACL are counted per address in
// memory for the network check (at most 256, the least recently refused is
// evicted; loopback excluded; nothing is logged per packet).
package dnsserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/parental"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Query statuses (stable strings stored in logs.db and used by the API/UI).
const (
	StatusForwarded      = "forwarded"
	StatusCached         = "cached"
	StatusStale          = "stale"
	StatusLocal          = "local"
	StatusSpecial        = "special"
	StatusOverride       = "override"
	StatusBlockedList    = "blocked-list"
	StatusBlockedRule    = "blocked-rule"
	StatusBlockedRegex   = "blocked-regex"
	StatusBlockedCNAME   = "blocked-cname"
	StatusBlockedSpecial = "blocked-special"
	// StatusBlockedSchedule: parental controls, a block-all schedule or a
	// block override (step 7a).
	StatusBlockedSchedule = "blocked-schedule"
	// StatusBlockedService: parental controls, a blocked service (always or
	// by a schedule).
	StatusBlockedService = "blocked-service"
	// StatusBlockedUpstream: a default upstream blocked the name itself
	// (step 13a).
	StatusBlockedUpstream = "blocked-upstream"
	// StatusBlockedRebind: DNS rebinding protection (step 14c).
	StatusBlockedRebind = "blocked-rebind"
	// StatusSafeSearch: safe search answered the name with a CNAME to the
	// search engine's restricted host (step 7c); not a blocked status.
	StatusSafeSearch = "safesearch"
	StatusRefused    = "refused"
	StatusError      = "error"
	// StatusDropped: the query gets no answer (a blocked client, a dropped
	// domain); reported by Lookup only, never logged.
	StatusDropped = "dropped"
)

// Background intervals.
const (
	maintainEvery = 10 * time.Second // rate-limiter sweep, QPS sample, pause expiry
	refreshEvery  = time.Minute      // router resolver, cache IPs, host addresses
	bucketIdle    = time.Minute      // idle rate-limit buckets are dropped
	shutdownWait  = 5 * time.Second
	udpReadSize   = dns.DefaultMsgSize
	tcpIdle       = 8 * time.Second
	maxTCPQueries = 128
)

// Consumer-side interfaces (implemented by the concrete packages; fakes in tests).

// Filter is the part of *filter.Engine the server uses.
type Filter interface {
	Check(qname string, groups []int64) filter.Decision
	CheckRules(qname string, groups []int64) filter.Decision
	// CheckProtection evaluates only the protection lists (step 7a).
	CheckProtection(qname string, groups []int64) filter.Decision
	Explain(ctx context.Context, qname string, groups []int64) ([]filter.Match, error)
}

// Services is the part of *services.Registry the server uses.
type Services interface {
	MatchDNS(qname string) (serviceID string, ok bool)
}

// Clients is the part of *clients.Registry the server uses.
type Clients interface {
	Identify(ip netip.Addr) *clients.Identity
	// IdentifyDerived identifies a client behind a trusted forwarder by
	// the address and/or MAC of its EDNS options (step 4a).
	IdentifyDerived(ip netip.Addr, mac string) *clients.Identity
	Seen(ip netip.Addr)          // activity, persisted to logs.db
	SeenTransient(ip netip.Addr) // activity kept in memory only (client addresses anonymised)
}

// Upstream is the part of *upstream.Resolver the server uses.
type Upstream interface {
	// Resolve answers through the default set; ecs is the client subnet
	// to send (invalid = none).
	Resolve(ctx context.Context, req *dns.Msg, ecs netip.Prefix) (*dns.Msg, upstream.Info, error)
	ResolveVia(ctx context.Context, req *dns.Msg, upstreams []string) (*dns.Msg, upstream.Info, error)
	Probe(ctx context.Context, server netip.Addr) bool
}

// Parental is the part of *parental.Engine the server uses.
type Parental interface {
	Check(qname string, groups []int64, now time.Time) parental.Decision
	// SafeSearch returns the safe search rewrite of a name (step 7c).
	SafeSearch(qname string, groups []int64, now time.Time) (parental.SafeSearchRewrite, bool)
	// FilterGroups returns the groups whose filtering is not paused
	// (groups itself when none is).
	FilterGroups(groups []int64, now time.Time) []int64
	// PausedGroups lists the paused groups (for the lookup trace).
	PausedGroups(groups []int64, now time.Time) []parental.GroupPause
}

// QueryLogger is the part of *logs.Store the server uses.
type QueryLogger interface {
	LogQuery(e logs.QueryEvent)
}

// Deps are the collaborators of the server.
type Deps struct {
	DB       *db.DB
	Settings *settings.Store
	Upstream Upstream
	Filter   Filter
	Clients  Clients
	Services Services
	Parental Parental // nil: no parental controls
	Logs     QueryLogger
	ACL      *netutil.ACLWatcher
	// DownloadCacheReady reports whether the download cache DNS answers may
	// be given: the cache HTTP listener is bound. (The server itself checks
	// that a valid cache IPv4 is known.) When false, the answers are skipped
	// (never answer with an unusable address).
	DownloadCacheReady func() (ok bool, reason string)
	// Container is the detected container type ("docker", "podman", "lxc",
	// "") for cache-IP auto-detection (Docker bridge IPs are never used).
	Container string
	// Neighbours reads the kernel's neighbour table (clients.Registry
	// .Neighbours): the router's other addresses and its address over IPv6.
	// nil: only the gateway addresses are known.
	Neighbours func(ctx context.Context) ([]clients.Neighbour, error)
	// Leases answers the DNS names of DHCP leases in step 7 (nil: none).
	Leases LeaseNames
	Log    *slog.Logger
}

// LeaseNames is the part of the DHCP server (package dhcp) the server
// uses: the names of active leases, <host>.<domain> → address and the
// address's PTR, each with the TTL to answer with. Local records win.
type LeaseNames interface {
	LeaseAddr(name string) (ip netip.Addr, ttl uint32, ok bool)
	LeasePTR(ip netip.Addr) (name string, ttl uint32, ok bool)
}

// Record is a local DNS record.
type Record struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"` // lower-case FQDN without trailing dot; "*.x" = subdomains of x
	Type      string    `json:"type"` // A | AAAA | CNAME | TXT
	Value     string    `json:"value"`
	TTL       uint32    `json:"ttl"`
	Enabled   bool      `json:"enabled"`
	Comment   string    `json:"comment"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// RecordInput creates or updates a record (TTL 0 → 300). A CNAME may not
// share its name with any other record (apperr.Conflict).
type RecordInput struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Value   string `json:"value"`
	TTL     uint32 `json:"ttl"`
	Enabled bool   `json:"enabled"`
	Comment string `json:"comment"`
}

// Forwarder sends domains (each apex + subdomains; "*.x" = subdomains
// only; Unqualified = single-label names) to specific upstreams, e.g.
// "fritz.box" → 192.168.178.1 or "178.168.192.in-addr.arpa" →
// 192.168.178.1, or to the default upstreams (the single target
// DefaultTarget). Domain is the first of Domains.
type Forwarder struct {
	ID        int64     `json:"id"`
	Domain    string    `json:"domain"`
	Domains   []string  `json:"domains"`
	Upstreams []string  `json:"upstreams"`
	Enabled   bool      `json:"enabled"`
	Comment   string    `json:"comment"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ForwarderInput creates or updates a forwarder: Domains (1–16) wins,
// Domain alone means [Domain]; with both, Domains[0] must be Domain.
type ForwarderInput struct {
	Domain    string   `json:"domain,omitempty"`
	Domains   []string `json:"domains,omitempty"`
	Upstreams []string `json:"upstreams"`
	Enabled   bool     `json:"enabled"`
	Comment   string   `json:"comment"`
}

// LookupRequest is a test query from the UI.
type LookupRequest struct {
	Name     string `json:"name"`
	Type     string `json:"type"`     // default "A"
	ClientIP string `json:"clientIp"` // evaluate as this client (default: the caller)
}

// LookupResult explains how PiCache would answer. Status "dropped" (a
// blocked client or a dropped domain: no answer at all) comes with an
// empty RCode and no answers.
type LookupResult struct {
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	Status     string         `json:"status"`
	RCode      string         `json:"rcode"`
	Answers    []string       `json:"answers"` // RR strings
	Reason     string         `json:"reason,omitempty"`
	Upstream   string         `json:"upstream,omitempty"`
	DurationUs int64          `json:"durationUs"`
	GroupIDs   []int64        `json:"groupIds"`
	Steps      []string       `json:"steps"`   // pipeline trace, human-readable
	Matches    []filter.Match `json:"matches"` // all filter matches
}

// BlockingStatus is the global blocking state. TimeZone and
// UTCOffsetMinutes describe the host's clock ("CEST", 120), so the UI can
// offer "until 06:00" on the host's clock.
type BlockingStatus struct {
	Enabled          bool       `json:"enabled"` // effective now
	PausedUntil      *time.Time `json:"pausedUntil,omitempty"`
	Permanent        bool       `json:"permanent"` // disabled until re-enabled
	TimeZone         string     `json:"timeZone"`
	UTCOffsetMinutes int        `json:"utcOffsetMinutes"`
}

// CacheIPStatus describes the addresses of the download cache DNS answers.
// IPv4/IPv6 are the addresses download service names are (or, while the
// download cache is disabled or not ready, would be) answered with.
type CacheIPStatus struct {
	IPv4   []string `json:"ipv4"`
	IPv6   []string `json:"ipv6"`
	Auto   bool     `json:"auto"`             // auto-detected (not configured)
	Ready  bool     `json:"ready"`            // overrides are being answered
	Reason string   `json:"reason,omitempty"` // why not ready; while ready: the warning (compatibility)
	// Warning is the address detection warning, reported whether or not
	// the download cache is enabled: public or CGNAT primary address,
	// container bridge network, no private address, no valid configured
	// address.
	Warning string `json:"warning,omitempty"`
}

// RouterStatus describes the router resolver.
type RouterStatus struct {
	Mode    string `json:"mode"`    // off | auto | manual
	Address string `json:"address"` // effective resolver IP ("" if none)
	Answers bool   `json:"answers"` // probe succeeded
	Domain  string `json:"domain"`  // effective local domain
}

// Stats are live counters.
type Stats struct {
	Queries        int64                 `json:"queries"`
	QPS            float64               `json:"qps"` // 1-minute average
	Refused        int64                 `json:"refused"`
	RateLimited    int64                 `json:"rateLimited"`
	InFlight       int64                 `json:"inFlight"`
	Overloaded     int64                 `json:"overloaded"` // dropped because too many queries were in flight
	TopRateLimited []netutil.RateLimited `json:"topRateLimited"`
	BlockedClients int64                 `json:"blockedClients"` // queries of dns.blockedClients dropped (UDP) or closed (TCP)
	Dropped        int64                 `json:"dropped"`        // queries of dns.droppedDomains dropped (UDP) or closed (TCP)
}

// Server is the DNS server.
type Server struct {
	d   Deps
	log *slog.Logger
	env hostEnv

	writeMu  sync.Mutex // records/forwarders writes with their snapshot reload
	zone     atomic.Pointer[zone]
	fwd      atomic.Pointer[fwdTable]
	cacheIPs atomic.Pointer[cacheIPState]
	routerMu sync.Mutex // orders router state updates from settings and detection
	router   atomic.Pointer[routerState]
	host     atomic.Pointer[hostInfo]

	limMu   sync.Mutex // serialises limiter reconfiguration
	limKey  string     // configuration the limiter was last set to (under limMu)
	limiter *netutil.RateLimiter

	routerKick  chan struct{}
	rotate      atomic.Uint32
	refusedSrc  refusedTable // sources dropped by the ACL
	lists       atomic.Pointer[dnsLists]
	protectWarn protectWarnings

	queries, refused, rateLimited, inFlight, overloaded, blockedClients, dropped atomic.Int64

	qpsMu      sync.Mutex
	qpsSamples []qpsSample // the last 7 (time, queries) samples, 10 s apart
}

type qpsSample struct {
	at time.Time
	n  int64
}

// New creates the server and migrates its tables (records, forwarders).
func New(ctx context.Context, d Deps) (*Server, error) {
	if d.DB == nil || d.Settings == nil {
		return nil, errors.New("dnsserver: DB and Settings are required")
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.ACL == nil {
		d.ACL = netutil.NewACLWatcher(d.Settings)
	}
	s := &Server{
		d:          d,
		log:        d.Log.With(slog.String("component", "dns")),
		env:        defaultHostEnv(d.Container, d.Neighbours),
		routerKick: make(chan struct{}, 1),
		limiter:    netutil.NewRateLimiter(0, 0, nil), // configured by reloadConfig
	}
	if err := d.DB.Migrate(ctx, "dns", migrations); err != nil {
		return nil, err
	}
	set := d.Settings.Get()
	s.lists.Store(newDNSLists(&set.DNS))
	s.host.Store(s.env.host())
	s.router.Store(s.startRouter(set))
	s.updateCacheIPs(set)
	if err := s.reloadConfig(ctx); err != nil {
		return nil, err
	}
	d.Settings.Subscribe(s.settingsChanged)
	return s, nil
}

// settingsChanged applies settings live: the compiled DNS lists, rate
// limits, cache IPs and the router resolver. Everything else is read per
// query.
func (s *Server) settingsChanged(old, cur *settings.All) {
	s.lists.Store(newDNSLists(&cur.DNS))
	routerChanged := old.DNS.RouterResolver != cur.DNS.RouterResolver
	if routerChanged {
		// Until the detection has run again, the router's known addresses
		// and MACs stay protected from dns.blockedClients.
		s.routerMu.Lock()
		prev := s.router.Load()
		st := configuredRouter(cur)
		st.protect, _ = routerAddrSet(append(slices.Clone(st.protect), prev.protect...), nil, false)
		st.macs, st.trustedMACs = prev.macs, prev.trustedMACs
		s.router.Store(st)
		s.routerMu.Unlock()
	}
	// A changed trusted-forwarder list needs their MACs (routerState.trustedMACs).
	if routerChanged || !slices.Equal(old.DNS.EDNSClientTrusted, cur.DNS.EDNSClientTrusted) {
		select {
		case s.routerKick <- struct{}{}:
		default:
		}
	}
	s.reconfigureLimiter()
	s.updateCacheIPs(cur)
}

// reconfigureLimiter applies the rate-limit configuration when it changed.
// The limiter keeps its buckets and drop statistics. Exempt:
// dns.rateLimitExempt, loopback, the router resolver (all of its addresses,
// routerState.addrs), local PTR upstreams, conditional forwarder targets
// and the trusted EDNS forwarders (a whole LAN behind one source). Public
// sources are keyed by dns.rateLimitIpv4Prefix/rateLimitIpv6Prefix
// (netutil.RateKey).
func (s *Server) reconfigureLimiter() {
	s.limMu.Lock()
	defer s.limMu.Unlock()
	set := s.d.Settings.Get()
	exempt := settings.ParsePrefixes(set.DNS.RateLimitExempt)
	exempt = append(exempt, netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128"))
	ips := slices.Clone(s.router.Load().addrs)
	ips = append(ips, upstreamIPs(set.DNS.LocalPTRUpstreams)...)
	if t := s.fwd.Load(); t != nil {
		ips = append(ips, t.ips...)
	}
	for _, v := range set.DNS.EDNSClientTrusted {
		if ip, err := netip.ParseAddr(v); err == nil {
			ips = append(ips, netutil.Canon(ip))
		}
	}
	for _, ip := range ips {
		exempt = append(exempt, netip.PrefixFrom(ip, ip.BitLen()))
	}
	keys := make([]string, 0, len(exempt))
	for _, p := range exempt {
		keys = append(keys, p.String())
	}
	slices.Sort(keys)
	v4, v6 := set.DNS.RateLimitIPv4Prefix, set.DNS.RateLimitIPv6Prefix
	key := fmt.Sprintf("%d/%d/%d/%d/%s", set.DNS.RateLimitQPS, set.DNS.RateLimitBurst, v4, v6, strings.Join(slices.Compact(keys), ","))
	if s.limKey == key {
		return
	}
	s.limKey = key
	s.limiter.Reconfigure(set.DNS.RateLimitQPS, set.DNS.RateLimitBurst, exempt, v4, v6)
}

// Serve answers queries on the pre-bound sockets until ctx ends (blocks).
// It also runs the background refreshes (rate-limiter sweep, router
// resolver, cache IPs, pause expiry) and returns after all of them exited.
func (s *Server) Serve(ctx context.Context, udp []net.PacketConn, tcp []net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	h := &dnsHandler{s: s, ctx: ctx}
	var servers []*dns.Server
	for _, pc := range udp {
		servers = append(servers, &dns.Server{PacketConn: pc, Handler: h, UDPSize: udpReadSize, DecorateReader: s.decorateReader})
	}
	for _, ln := range tcp {
		servers = append(servers, &dns.Server{Listener: ln, Handler: h, MaxTCPQueries: maxTCPQueries,
			IdleTimeout: func() time.Duration { return tcpIdle }})
	}

	var bg sync.WaitGroup
	bg.Go(func() { s.maintain(ctx) })
	bg.Go(func() { s.refresh(ctx) })

	errc := make(chan error, len(servers))
	var running []*dns.Server
	var wg sync.WaitGroup
	var runErr error
	for _, srv := range servers {
		started := make(chan struct{})
		srv.NotifyStartedFunc = func() { close(started) }
		wg.Go(func() {
			if err := srv.ActivateAndServe(); err != nil {
				errc <- err
			}
		})
		select {
		case <-started:
			running = append(running, srv)
		case runErr = <-errc:
		}
		if runErr != nil {
			break
		}
	}
	if runErr == nil {
		select {
		case <-ctx.Done():
		case runErr = <-errc:
		}
	}
	cancel()
	for _, srv := range running {
		sctx, scancel := context.WithTimeout(context.Background(), shutdownWait)
		_ = srv.ShutdownContext(sctx)
		scancel()
	}
	wg.Wait()
	bg.Wait()
	if runErr != nil {
		return fmt.Errorf("dns: %w", runErr)
	}
	return nil
}

// maintain sweeps the rate limiter, samples the query rate and ends elapsed
// blocking pauses.
func (s *Server) maintain(ctx context.Context) {
	t := time.NewTicker(maintainEvery)
	defer t.Stop()
	s.sampleQPS(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.limiter.Sweep(bucketIdle)
			s.sampleQPS(now)
			s.expirePause(ctx, now)
		}
	}
}

// refresh re-detects the router resolver, the host addresses and the
// automatic cache IPs every minute (router also on settings changes).
func (s *Server) refresh(ctx context.Context) {
	t := time.NewTicker(refreshEvery)
	defer t.Stop()
	for {
		s.host.Store(s.env.host())
		s.updateCacheIPs(s.d.Settings.Get())
		s.detectRouter(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.routerKick:
		}
	}
}

func (s *Server) sampleQPS(now time.Time) {
	s.qpsMu.Lock()
	defer s.qpsMu.Unlock()
	s.qpsSamples = append(s.qpsSamples, qpsSample{at: now, n: s.queries.Load()})
	if len(s.qpsSamples) > 7 { // 7 samples × 10 s cover the last minute
		s.qpsSamples = slices.Delete(s.qpsSamples, 0, len(s.qpsSamples)-7)
	}
}

// qps returns the average query rate over the sampled window (≤ 1 minute).
func (s *Server) qps() float64 {
	s.qpsMu.Lock()
	defer s.qpsMu.Unlock()
	if len(s.qpsSamples) == 0 {
		return 0
	}
	first := s.qpsSamples[0]
	secs := time.Since(first.at).Seconds()
	if secs < 1 {
		return 0
	}
	return float64(s.queries.Load()-first.n) / secs
}

// Stats returns live counters.
func (s *Server) Stats() Stats {
	top := s.limiter.Top(10)
	if top == nil {
		top = []netutil.RateLimited{}
	}
	return Stats{
		Queries:        s.queries.Load(),
		QPS:            s.qps(),
		Refused:        s.refused.Load(),
		RateLimited:    s.rateLimited.Load(),
		InFlight:       s.inFlight.Load(),
		Overloaded:     s.overloaded.Load(),
		TopRateLimited: top,
		BlockedClients: s.blockedClients.Load(),
		Dropped:        s.dropped.Load(),
	}
}
