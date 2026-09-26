// Package dhcp is PiCache's optional DHCP server (docs/ARCHITECTURE.md
// 18): DHCPv4 on one interface, PiCache's IPv6 DNS announcements (router
// advertisements with RDNSS/DNSSL only, package ra; stateless DHCPv6,
// package dhcpv6), the DNS names of leases and the safety gates that keep
// it from serving where it would do harm.
//
// Privileges and sockets: nothing is held while DHCP is switched off. The
// app opens at start, before the privilege drop, what the markers in the
// data directory ask for (OpenAtStart): UDP 67 and 547 while dhcp.enabled
// is true, the raw ICMPv6 socket (CAP_NET_RAW, dropped for good right
// after start) while router advertisements are on too. The service keeps
// the markers, opens UDP 67 and 547 itself when DHCP is switched on while
// it runs (systemd keeps CAP_NET_BIND_SERVICE; in Docker the ports open
// only at start: restart-required) and closes every socket the settings no
// longer need. With PICACHE_DHCP=off nothing is ever opened.
//
// States: unavailable (PICACHE_DHCP=off, not Linux, container bridge
// network, UDP 67 failed while switched on), off (dhcp.enabled false),
// blocked (a gate), serving, error (no router address, a failed probe).
// Gates: PiCache's own IPv4 is dynamic (a hard blocker), another DHCP
// server answered a probe within 10 minutes or was named by a client's
// DHCPREQUEST within 24 hours (unless dhcp.ignoreOtherServers; checked
// before serving starts: while serving a new server only raises a health
// warning), the interface is missing or has no single RFC 1918 IPv4
// address, the range does not fit the subnet. Before serving starts the
// service probes for other servers (a relay-style DISCOVER answered to
// PiCache's port 67) and repeats the probe every 10 minutes while serving.
//
// Every packet is untrusted: DHCPv4 packets of 240 to 1500 bytes with
// bounds-checked options, from the configured interface only (IP_PKTINFO),
// BOOTREQUESTs with Ethernet addresses and giaddr 0 (relayed requests are
// never answered); at most 50 packets per second and 5 per second and
// client MAC; malformed packets are dropped and counted. A failure while
// handling one packet drops that packet (DNS keeps running whatever
// happens here).
//
// Tables (picache.db, component "dhcp"): dhcp_static(mac PRIMARY KEY, ip
// UNIQUE, hostname, comment, client_id (unique when set), lease_seconds,
// created_at, updated_at) with at most 1024 rows; dhcp_leases(mac PRIMARY
// KEY, ip UNIQUE, hostname, client_id, expires_at, updated_at) with at
// most 4096 rows, expired leases kept 24 h to hand the address back.
// Timestamps are unix milliseconds.
package dhcp

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/dhcp/ra"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Reasons for the unavailable state and of the router advertisements.
const (
	reasonOptOut   = "PICACHE_DHCP=off is set"
	reasonNotLinux = "the DHCP server needs Linux"
	reasonBridge   = "PiCache runs in a container bridge network, where DHCP broadcasts do not reach it; DHCP needs host networking"
	reasonRestart  = "PiCache opens the DHCP ports only at start in this installation: restart PiCache once to start the DHCP server"
	reasonPending  = "the DHCP ports are opened when the DHCP server starts"
	noCapNetRaw    = "PiCache did not hold CAP_NET_RAW at start, which it needs to open the raw ICMPv6 socket for router advertisements"
	raRestart      = "the raw ICMPv6 socket for router advertisements opens only at start: restart PiCache once"
)

// Intervals.
const (
	reconcileEvery = 30 * time.Second
	purgeEvery     = 10 * time.Minute
	logEvery       = time.Hour // repeated errors are logged at most hourly
)

// Deps are the collaborators of the service.
type Deps struct {
	DB       *db.DB
	Settings *settings.Store
	// Sockets are the sockets opened at start (OpenAtStart); nil: none
	// (the opt-out).
	Sockets *Sockets
	// DataDir holds the markers (MarkerSockets, MarkerRA); "": no markers.
	DataDir string
	// Bridge reports whether PiCache runs in a container bridge network.
	Bridge func() bool
	// Deployment reports docker, systemd or other (nil: other).
	Deployment func() string
	// Neighbour returns the MAC of ip from a fresh read of the kernel's
	// neighbour table (nil: no neighbour table, no conflict check).
	Neighbour func(ip netip.Addr) (mac string, ok bool)
	// OnNames is called (not concurrently) with the addresses whose lease
	// name changed, so client names follow the leases.
	OnNames func(ips []netip.Addr)
	Log     *slog.Logger
}

// view is the published state of one evaluation: read by the packet
// handlers and the status (immutable once stored).
type view struct {
	available  bool
	reason     string
	reasonCode string
	enabled    bool
	state      string
	blockers   []string
	err        string
	// probePending: the only reason not to serve is the probe that has
	// not run yet (the start of the control loop runs it).
	probePending bool
	gate         gate
	poolSize     int
	serving      bool
	leaseTime    time.Duration
	opts         []option // static reply options: 1, 3, 6, 15, 28, 119, 42, 26, 252
	onlyReserved bool
	rapidCommit  bool
	ignoreOthers bool

	// IPv6
	raEnabled, raAvailable bool
	raReason, raReasonCode string
	raState                string
	raBlockers             []string
	adv                    ra.Advertisement
	v6Enabled              bool
	v6State                string
	v6Blockers             []string
	v6Err                  string
	duid                   []byte
	domainWire             []byte
}

// announcing reports whether PiCache announces itself over IPv6 (router
// advertisements sent or DHCPv6 answered).
func (v *view) announcing() bool { return v.raState == StateSending || v.v6State == StateServing }

// counters of DHCPv4 packets.
type counters struct {
	received, offers, acks, naks, declines, releases, informs, dropped atomic.Int64
}

// otherServer is another DHCP server seen on an interface.
type otherServer struct {
	iface    string
	address  netip.Addr
	serverID netip.Addr
	source   string
	lastSeen time.Time
}

// raRuntime is the state of the router advertisements (Service.raMu).
type raRuntime struct {
	active   bool
	ifIndex  int
	adv      ra.Advertisement
	sched    ra.Schedule
	joined   int // interface index with ff02::2 joined (0: none)
	lastSent time.Time
	sent     int64
	solicits int64
	err      string
}

// Service is the DHCP server.
type Service struct {
	d   Deps
	log *slog.Logger
	env env

	goos      string // runtime.GOOS (replaced in tests)
	now       func() time.Time
	sleep     func(time.Duration)
	prime     func(netip.Addr) // makes the kernel resolve a neighbour (nil: never)
	neighbour func(netip.Addr) (string, bool)
	// listen4 and listen6 open UDP 67 and 547 while the process runs
	// (replaced in tests).
	listen4 func() (v4Conn, error)
	listen6 func() (v6Conn, error)

	// sockMu serialises opening and closing sockets. live is set by Start:
	// from then on evaluate opens and closes sockets and keeps the
	// markers (before, the other components may not be built yet).
	sockMu  sync.Mutex
	live    bool
	runCtx  context.Context
	readers sync.WaitGroup // one reader per open socket
	// markerErr is why a marker could not be written ("" when fine).
	markerErr atomic.Pointer[string]

	xlog     exchangeLog
	ann      *announcers
	annKick  chan struct{}
	searchMu sync.Mutex // one search for other IPv6 announcers at a time

	mu        sync.Mutex // guards the fields below and the database writes of leases
	t         *table
	holders   map[string]string // host name → MAC holding it
	others    []otherServer
	lastGW    netip.Addr
	lastProbe *LastProbe
	probeFor  map[string]time.Time // interface → time of its last completed probe
	// startAt is when the current attempt to start serving began (zero
	// while serving or not trying): only a probe that ended after it may
	// let serving start, so a clean probe from before switching on (or
	// before the last stop) never lets it start without a fresh one.
	startAt  time.Time
	apiProbe time.Time            // start of the last probe (rate limit of the API)
	v6Joined int                  // interface with ff02::1:2 joined
	logged   map[string]time.Time // last time a repeated message was logged

	view  atomic.Pointer[view]
	names atomic.Pointer[names]
	c     counters

	limit4    *windowLimiter
	limit6    *windowLimiter
	v6Replies atomic.Int64
	v6Ignored atomic.Int64
	v6SendErr atomic.Pointer[string]

	probeMu sync.Mutex // one probe at a time
	probe   atomic.Pointer[probeRun]

	raMu   sync.Mutex
	ra     raRuntime
	raKick chan struct{}

	kick chan struct{}
	// stateLogged: the state was logged once by the control loop (read
	// and written by evaluate only, which never runs concurrently).
	stateLogged bool
	namesM      sync.Mutex // serialises OnNames calls
}

// New loads the leases and static leases and publishes the first state.
// The tables are migrated whether or not DHCP is available, so backups
// and restores are the same on every installation.
func New(ctx context.Context, d Deps) (*Service, error) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Sockets == nil {
		d.Sockets = OptOutSockets()
	}
	s := &Service{
		d: d, log: d.Log.With(slog.String("component", "dhcp")), env: liveEnv(d.Bridge),
		goos: runtime.GOOS, now: time.Now, sleep: time.Sleep, neighbour: d.Neighbour,
		listen4: platformListen4, listen6: platformListen6,
		t: newTable(), holders: map[string]string{}, probeFor: map[string]time.Time{}, logged: map[string]time.Time{},
		limit4: newWindowLimiter(limitTotal, limitPerMAC), limit6: newWindowLimiter(limitV6, 0),
		raKick: make(chan struct{}, 1), kick: make(chan struct{}, 1), annKick: make(chan struct{}, 1),
		ann: newAnnouncers(),
	}
	s.ra.sched.Rand = rand.Int64N
	if runtime.GOOS == "linux" && d.Neighbour != nil {
		s.prime = primeNeighbour
	}
	s.names.Store(emptyNames)
	s.view.Store(&view{state: StateUnavailable, reason: reasonPending, reasonCode: ReasonSocket, raState: StateOff, v6State: StateOff})
	if err := d.DB.Migrate(ctx, "dhcp", migrations); err != nil {
		return nil, err
	}
	skipped, err := load(ctx, d.DB, s.t)
	if err != nil {
		return nil, err
	}
	if skipped > 0 {
		s.log.Warn("skipped invalid DHCP lease rows", slog.Int("rows", skipped))
	}
	s.refreshNames()
	d.Settings.Subscribe(func(o, n *settings.All) {
		if !o.DHCP.Equal(n.DHCP) || o.DNS.LocalDomain != n.DNS.LocalDomain {
			s.Kick()
		}
	})
	s.evaluate(ctx, false)
	return s, nil
}

// Kick re-evaluates the gates now (settings changed).
func (s *Service) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Start serves until ctx is done: the readers of the open sockets, the
// control loop (gates every 30 s and on changes, probes, lease purging,
// sockets and markers), the router advertisements and the search for
// other IPv6 announcers. At the end the announcements are withdrawn (best
// effort) and the sockets closed.
func (s *Service) Start(ctx context.Context) {
	s.sockMu.Lock()
	s.live, s.runCtx = true, ctx
	v4, v6, icmp, _, _, _ := s.d.Sockets.get()
	if v4 != nil {
		s.startReader4(v4)
	}
	if v6 != nil {
		s.startReader6(v6)
	}
	if icmp != nil {
		s.startReaderICMP(icmp)
	}
	s.sockMu.Unlock()
	var ctl sync.WaitGroup
	ctl.Go(func() { s.raLoop(ctx) })
	ctl.Go(func() { s.searchLoop(ctx) })
	ctl.Go(func() { s.controlLoop(ctx) })
	<-ctx.Done()
	ctl.Wait()
	s.sockMu.Lock()
	s.d.Sockets.Close()
	s.sockMu.Unlock()
	s.readers.Wait()
}

func (s *Service) controlLoop(ctx context.Context) {
	s.evaluate(ctx, true)
	tick := time.NewTicker(reconcileEvery)
	defer tick.Stop()
	purge := time.NewTicker(purgeEvery)
	defer purge.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.kick:
		case <-tick.C:
		case <-purge.C:
			s.purge(ctx)
			if v := s.view.Load(); v.serving {
				// While serving: look for servers that appeared meanwhile
				// (a new one raises a health warning, serving goes on).
				if _, err := s.runProbe(ctx, v.gate.iface); err != nil {
					s.logRepeated("probe", "periodic DHCP probe failed", err)
				}
			}
		}
		s.evaluate(ctx, true)
	}
}

// support reports whether this installation can serve DHCP at all: not
// with PICACHE_DHCP=off, only on Linux and not in a container bridge
// network.
func (s *Service) support() (ok bool, code, reason string) {
	st := s.d.Sockets.state()
	switch {
	case st.optOut:
		return false, ReasonOptOut, reasonOptOut
	case s.goos != "linux" || st.unsupported != "":
		return false, ReasonNotLinux, reasonNotLinux
	case s.env.bridge():
		return false, ReasonBridge, reasonBridge
	}
	return true, "", ""
}

// availability reports whether the service can serve: supported, and
// while switched on with UDP 67 open.
func (s *Service) availability(enabled bool) (ok bool, code, reason string) {
	if ok, code, reason := s.support(); !ok {
		return false, code, reason
	}
	if !enabled {
		return true, "", ""
	}
	if st := s.d.Sockets.state(); !st.v4Open {
		if st.v4Code == "" {
			return false, ReasonSocket, reasonPending
		}
		return false, st.v4Code, st.v4Err
	}
	return true, "", ""
}

// evaluate computes and publishes the state for the current settings. With
// probe it may run the probe that must precede serving (up to 5 s). While
// live it also opens UDP 67 and 547 when DHCP is switched on, and after
// publishing (announcements withdrawn) closes what is no longer needed and
// keeps the markers in step.
func (s *Service) evaluate(ctx context.Context, probe bool) {
	set := s.d.Settings.Get()
	h := set.DHCP
	now := s.now()
	prev := s.view.Load()
	supported, _, _ := s.support()
	live := s.isLive()
	if live && supported && h.Enabled {
		s.openRuntime()
	}
	v := &view{enabled: h.Enabled, leaseTime: time.Duration(h.LeaseSeconds) * time.Second, onlyReserved: h.OnlyReserved,
		rapidCommit: h.RapidCommit, ignoreOthers: h.IgnoreOtherServers}
	v.available, v.reasonCode, v.reason = s.availability(h.Enabled)
	s.mu.Lock()
	lastGW := s.lastGW
	s.mu.Unlock()
	if h.Interface != "" {
		v.gate = s.env.evalGate(set, lastGW)
		if h.Router == "" && v.gate.router.IsValid() {
			s.mu.Lock()
			s.lastGW = v.gate.router
			s.mu.Unlock()
		}
	} else {
		v.gate = gate{iface: ifaceState{problem: "no interface is configured"}, blockers: []string{BlockerNoInterface}}
	}
	if v.gate.pool != nil {
		v.poolSize = v.gate.pool.size()
	}
	switch {
	case !v.available:
		v.state = StateUnavailable
	case !h.Enabled:
		v.state = StateOff
	default:
		name := v.gate.iface.name
		v.blockers = slices.Clone(v.gate.blockers)
		v.err = v.gate.err
		// Serving on the same interface goes on when another server
		// appears (a health warning); starting to serve needs a probe
		// without an answer first.
		sameIface := prev.serving && prev.gate.iface.name == name && prev.gate.iface.index == v.gate.iface.index
		ready := len(v.blockers) == 0 && v.err == ""
		since := s.startAttempt(!sameIface, now)
		if ready && !sameIface && probe && !h.IgnoreOtherServers {
			if _, probed := s.gateOthers(name, now, since); !probed {
				if _, err := s.runProbe(ctx, v.gate.iface); err != nil && ctx.Err() == nil {
					v.err = "the search for other DHCP servers failed: " + err.Error()
				}
			}
		}
		others, probed := s.gateOthers(name, s.now(), since)
		v.probePending = !sameIface && !h.IgnoreOtherServers && ready && !probed && !others && v.err == ""
		if !sameIface && !h.IgnoreOtherServers && (others || (ready && !probed)) {
			v.blockers = insertBlocker(v.blockers, BlockerOtherServer)
		}
		switch {
		case len(v.blockers) > 0:
			v.state = StateBlocked
		case v.err != "":
			v.state = StateError
		default:
			v.state, v.serving = StateServing, true
		}
	}
	if v.serving || v.state == StateOff || v.state == StateUnavailable {
		s.startAttempt(false, now) // the next start needs a fresh probe
	}
	if v.serving {
		v.opts = replyOptions(v.gate, h.Options)
	}
	s.evaluateIPv6(set, v)
	if v.announcing() {
		s.ann.setOwn(s.env.own()) // to ignore PiCache's own addresses in other announcers
	}
	s.view.Store(v)
	if prev.serving && !v.serving {
		s.mu.Lock()
		for mac := range s.t.offers {
			s.t.dropOffer(mac)
		}
		s.mu.Unlock()
	}
	s.refreshNames()
	s.applyIPv6(v)
	if live {
		s.closeUnneeded(h, supported)
		s.reconcileMarkers(h, supported)
	}
	if !prev.announcing() && v.announcing() {
		s.kickSearch() // look for other IPv6 announcers now (never delays the own)
	}
	if probe && (!s.stateLogged || prev.state != v.state || !slices.Equal(prev.blockers, v.blockers) || prev.err != v.err) {
		s.stateLogged = true
		s.logState(v)
	}
}

// insertBlocker adds b keeping the documented order of the blockers.
func insertBlocker(bs []string, b string) []string {
	order := []string{BlockerDynamicAddress, BlockerOtherServer, BlockerNoInterface, BlockerRange}
	if slices.Contains(bs, b) {
		return bs
	}
	bs = append(bs, b)
	slices.SortFunc(bs, func(x, y string) int { return slices.Index(order, x) - slices.Index(order, y) })
	return bs
}

func (s *Service) logState(v *view) {
	attrs := []any{slog.String("state", v.state)}
	if len(v.blockers) > 0 {
		attrs = append(attrs, slog.Any("blockers", v.blockers))
	}
	if v.err != "" {
		attrs = append(attrs, slog.String("error", v.err))
	}
	if v.serving {
		attrs = append(attrs, slog.String("interface", v.gate.iface.name),
			slog.String("range", v.gate.pool.start.String()+"-"+v.gate.pool.end.String()))
	}
	if v.state == StateUnavailable && v.enabled {
		attrs = append(attrs, slog.String("reason", v.reason))
	}
	s.log.Info("DHCP server state", attrs...)
}

// logRepeated logs a recurring problem at most hourly per key.
func (s *Service) logRepeated(key, msg string, err error) {
	s.mu.Lock()
	last, seen := s.logged[key]
	if seen && s.now().Sub(last) < logEvery {
		s.mu.Unlock()
		return
	}
	if len(s.logged) > 64 {
		clear(s.logged)
	}
	s.logged[key] = s.now()
	s.mu.Unlock()
	s.log.Warn(msg, slog.Any("err", err))
}

// replyOptions returns the options every reply of a serving view carries:
// subnet mask, router, DNS server, domain name, broadcast address and, on
// request only, the domain search list (the domain first, then the extra
// search domains; extras that do not fit are dropped from the end), NTP
// servers, the interface MTU and the WPAD URL.
func replyOptions(g gate, o settings.DHCPOptions) []option {
	subnet := g.iface.self.Masked()
	mask := net.CIDRMask(subnet.Bits(), 32)
	opts := []option{{code: optSubnetMask, data: []byte(mask)}}
	if g.router.IsValid() {
		opts = append(opts, option{code: optRouter, data: g.router.AsSlice()})
	}
	opts = append(opts, option{code: optDNS, data: g.dns.AsSlice()})
	if g.domain != "" {
		opts = append(opts, option{code: optDomainName, data: []byte(g.domain)})
	}
	opts = append(opts, option{code: optBroadcast, data: lastAddr(subnet).AsSlice()})
	if lists := searchLists(g.domain, o.ExtraSearchDomains); len(lists) > 0 {
		opts = append(opts, option{code: optDomainSearch, data: lists[0], alts: lists[1:], onRequest: true})
	}
	var ntp []byte
	for _, s := range o.NTPServers {
		if ip, err := netip.ParseAddr(s); err == nil && ip.Is4() {
			ntp = append(ntp, ip.AsSlice()...)
		}
	}
	if len(ntp) > 0 {
		opts = append(opts, option{code: optNTPServers, data: ntp, onRequest: true})
	}
	if o.MTU > 0 && o.MTU <= 0xffff {
		opts = append(opts, option{code: optInterfaceMTU, data: []byte{byte(o.MTU >> 8), byte(o.MTU)}, onRequest: true})
	}
	if o.WPADURL != "" && len(o.WPADURL) <= 255 {
		opts = append(opts, option{code: optWPAD, data: []byte(o.WPADURL), onRequest: true})
	}
	return opts
}

// searchLists returns the encodings of option 119 from the longest to the
// shortest: the domain followed by all extra search domains, then with one
// extra less each time down to the domain alone (the domain is never
// dropped because of the extras). Extras equal to the domain, invalid ones
// and lists longer than one option (255 bytes) are left out.
func searchLists(domain string, extras []string) [][]byte {
	names := []string{}
	if encodeDomain(domain) != nil {
		names = append(names, domain)
	}
	for _, d := range extras {
		if d != domain && encodeDomain(d) != nil && !slices.Contains(names, d) {
			names = append(names, d)
		}
	}
	first := 0
	if len(names) > 0 && names[0] == domain {
		first = 1
	}
	var out [][]byte
	for n := len(names); n >= max(first, 1); n-- {
		var w []byte
		for _, d := range names[:n] {
			w = append(w, encodeDomain(d)...)
		}
		if len(w) <= 255 {
			out = append(out, w)
		}
	}
	return out
}

// purge drops expired offers, quarantine entries and leases that expired
// more than 24 hours ago.
func (s *Service) purge(ctx context.Context) {
	now := s.now()
	s.mu.Lock()
	gone := s.t.expire(now)
	err := deleteLeases(ctx, s.d.DB, gone...)
	if err == nil {
		wctx, cancel := context.WithTimeout(ctx, writeBudget)
		_, err = s.d.DB.W.ExecContext(wctx, `DELETE FROM dhcp_leases WHERE expires_at < ?`, db.Ms(now.Add(-retention)))
		cancel()
	}
	s.mu.Unlock()
	if err != nil && ctx.Err() == nil {
		s.logRepeated("purge", "purging expired DHCP leases failed", err)
	}
	s.refreshNames()
}

// refreshNames rebuilds the lease names and tells the clients registry
// which addresses changed.
func (s *Service) refreshNames() {
	set := s.d.Settings.Get()
	domain := settings.EffectiveDomain(set)
	s.namesM.Lock()
	defer s.namesM.Unlock()
	s.mu.Lock()
	now := s.now()
	s.holders = s.t.assignNames(s.holders, now)
	next := s.t.buildNames(s.holders, domain, set.DHCP.RegisterHostnames, set.DHCP.GenerateNames, now)
	prev := s.names.Swap(next)
	s.mu.Unlock()
	if s.d.OnNames != nil {
		if changed := changedDisplay(prev, next); len(changed) > 0 {
			s.d.OnNames(changed)
		}
	}
}

// --- status ---

// Status returns the state of the DHCP server (GET /dhcp).
func (s *Service) Status() Status {
	v := s.view.Load()
	now := s.now()
	st := Status{Available: v.available, State: v.state, Blockers: nonNil(v.blockers), Error: v.err,
		OtherServers: []OtherServer{}, Deployment: s.deployment()}
	e := s.markerErr.Load()
	if e != nil {
		st.MarkerError = *e
	}
	if v.state == StateUnavailable {
		st.Reason, st.ReasonCode = v.reason, v.reasonCode
	} else if e != nil {
		st.Reason = *e
	}
	g := v.gate
	if g.iface.name != "" && g.iface.problem == "" {
		mac, _ := macString(g.iface.mac)
		st.Interface = &StatusInterface{Name: g.iface.name, MAC: mac, IPv4: g.iface.self.Addr().String(),
			PrefixLen: g.iface.self.Bits(), Dynamic: g.iface.dynamic}
		st.DNSServer = g.dns.String()
		if g.router.IsValid() {
			st.Router = g.router.String()
		}
		st.Domain = g.domain
	}
	s.mu.Lock()
	if g.pool != nil {
		used := 0
		for _, l := range s.t.leases {
			if l.active(now) && g.pool.dynamic(l.ip) {
				used++
			}
		}
		st.Pool = &StatusPool{Start: g.pool.start.String(), End: g.pool.end.String(), Size: v.poolSize, Used: used, Static: len(s.t.statics)}
	}
	for _, o := range s.otherServers(g.iface.name, now) {
		st.OtherServers = append(st.OtherServers, OtherServer{Address: o.address.String(), ServerID: o.serverID.String(),
			Source: o.source, LastSeen: o.lastSeen.UTC()})
	}
	if s.lastProbe != nil {
		lp := *s.lastProbe
		st.LastProbe = &lp
	}
	s.mu.Unlock()
	st.Counters = Counters{Received: s.c.received.Load(), Offers: s.c.offers.Load(), Acks: s.c.acks.Load(), Naks: s.c.naks.Load(),
		Declines: s.c.declines.Load(), Releases: s.c.releases.Load(), Informs: s.c.informs.Load(), Dropped: s.c.dropped.Load()}

	r := RAStatus{Enabled: v.raEnabled, Available: v.raAvailable, Reason: v.raReason, ReasonCode: v.raReasonCode, State: v.raState,
		Blockers: nonNil(v.raBlockers)}
	if v.raState == StateSending {
		r.Address = v.adv.DNS.String()
	}
	s.raMu.Lock()
	r.LastSent, r.Sent, r.Solicitations = s.ra.lastSent, s.ra.sent, s.ra.solicits
	if !r.LastSent.IsZero() {
		r.LastSent = r.LastSent.UTC()
	}
	if s.ra.err != "" && v.raState == StateSending {
		r.State, r.Error = StateError, s.ra.err
	}
	s.raMu.Unlock()
	st.IPv6.RouterAdvertisements = r
	d6 := DHCPv6Status{Enabled: v.v6Enabled, State: v.v6State, Blockers: nonNil(v.v6Blockers), Replies: s.v6Replies.Load(),
		Ignored: s.v6Ignored.Load(), Error: v.v6Err}
	if e := s.v6SendErr.Load(); e != nil && *e != "" && v.v6State == StateServing {
		d6.State, d6.Error = StateError, *e
	}
	st.IPv6.DHCPv6 = d6
	st.IPv6.OtherAnnouncers, st.IPv6.LastSearch = s.ann.status(now, v.announcing())
	return st
}

// deployment reports docker, systemd or other.
func (s *Service) deployment() string {
	if s.d.Deployment != nil {
		if d := s.d.Deployment(); d != "" {
			return d
		}
	}
	return DeploymentOther
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return slices.Clone(s)
}

// Serving reports whether DHCPv4 is served, on which interface, and
// whether router advertisements are sent (network check).
func (s *Service) Serving() (serving bool, iface string, routerAdvertisements bool) {
	v := s.view.Load()
	return v.serving, v.gate.iface.name, v.raState == StateSending
}

// Health returns the health check "dhcp": fail when dropping CAP_NET_RAW
// could not be verified (always shown), and while DHCP is switched on but
// UDP 67 could not be opened; warn while it is switched on but its ports
// open only at start (restart-required), blocked or not available, when
// another DHCP server was seen while serving, when router advertisements
// are switched on but not sent, or when another router announces its own
// DNS server while PiCache announces itself. show is false when there is
// nothing to report (neither enabled nor available).
func (s *Service) Health() (status, msg, hint string, show bool) {
	v := s.view.Load()
	if st := s.d.Sockets.state(); st.dropUnverified != "" {
		return "fail", "could not verify that CAP_NET_RAW was dropped: " + st.dropUnverified,
			"PiCache closed the raw socket and sends no router advertisements; check the kernel and the service unit", true
	}
	if !v.enabled {
		return "ok", "", "", v.available
	}
	iface := v.gate.iface.name
	switch v.state {
	case StateUnavailable:
		switch v.reasonCode {
		case ReasonSocket:
			return "fail", "the DHCP server could not open its port: " + v.reason,
				"stop the other DHCP server on this host, or switch PiCache's DHCP server off", true
		case ReasonRestartRequired:
			if e := s.markerErr.Load(); e != nil {
				return "fail", "DHCP is switched on but its ports open only at start, and a restart will not open them: " + *e,
					"free space in the data directory (or fix its permissions), then restart PiCache under DNS → DHCP", true
			}
			return "warn", "DHCP is switched on but its ports open only at start: restart PiCache", "restart PiCache under DNS → DHCP", true
		}
		return "warn", "DHCP is enabled but not available: " + v.reason, "see DNS → DHCP", true
	case StateBlocked:
		if v.probePending {
			return "ok", "searching for other DHCP servers before serving", "", true
		}
		return "warn", "the DHCP server does not serve: " + s.blockerText(v), blockerHint(v.blockers), true
	case StateError:
		return "warn", "the DHCP server does not serve: " + v.err, "see DNS → DHCP", true
	}
	if others := s.otherServersNow(iface); len(others) > 0 {
		return "warn", fmt.Sprintf("another DHCP server (%s) was seen on %s while PiCache serves", strings.Join(others, ", "), iface),
			"switch off the other DHCP server (the router's), or DHCP in PiCache", true
	}
	if v.raEnabled && v.raState != StateSending {
		why, hint := v.raReason, "see DNS → DHCP (IPv6)"
		switch {
		case len(v.raBlockers) > 0 && v.raBlockers[0] != BlockerNoRawSocket:
			why = raBlockerText(v.raBlockers[0], iface)
		case v.raReasonCode == RAReasonRestart:
			hint = "restart PiCache under DNS → DHCP (IPv6)"
		case v.raReasonCode == RAReasonNoCapNetRaw:
			hint = "PiCache needs CAP_NET_RAW at start: see DNS → DHCP (IPv6)"
		}
		return "warn", "IPv6 DNS announcements are enabled but not sent: " + why, hint, true
	}
	if addrs := s.ann.conflicts(s.now(), v.announcing()); len(addrs) > 0 {
		return "warn", fmt.Sprintf("another router announces its own DNS server (%s) on %s, so devices may ask it instead of PiCache",
				strings.Join(addrs, ", "), iface),
			"the fix is on the router: switch off its DNS announcement or let it announce PiCache (see DNS → Network check, FRITZ!Box steps)", true
	}
	if e := s.markerErr.Load(); e != nil {
		return "warn", *e, "free space in the data directory (or fix its permissions)", true
	}
	return "ok", "", "", true
}

func (s *Service) otherServersNow(iface string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, o := range s.otherServers(iface, s.now()) {
		if a := o.serverID.String(); !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

func (s *Service) blockerText(v *view) string {
	var parts []string
	for _, b := range v.blockers {
		switch b {
		case BlockerDynamicAddress:
			parts = append(parts, fmt.Sprintf("PiCache's own address %s on %s comes from a DHCP client", v.gate.iface.self.Addr(), v.gate.iface.name))
		case BlockerOtherServer:
			if others := s.otherServersNow(v.gate.iface.name); len(others) > 0 {
				parts = append(parts, fmt.Sprintf("another DHCP server answers on %s (%s)", v.gate.iface.name, strings.Join(others, ", ")))
			} else {
				parts = append(parts, "the search for other DHCP servers has not finished")
			}
		case BlockerNoInterface:
			parts = append(parts, v.gate.iface.problem)
		case BlockerRange:
			parts = append(parts, fmt.Sprintf("the address range does not fit the subnet %s", v.gate.iface.self.Masked()))
		}
	}
	return strings.Join(parts, "; ")
}

func blockerHint(bs []string) string {
	if len(bs) == 0 {
		return ""
	}
	switch bs[0] {
	case BlockerDynamicAddress:
		return "give this machine a fixed address (static configuration on the host or container; a router reservation is not enough)"
	case BlockerOtherServer:
		return "switch off the router's DHCP server, then search again under DNS → DHCP"
	case BlockerNoInterface:
		return "choose an interface with exactly one private IPv4 address under DNS → DHCP"
	}
	return "adjust the address range under DNS → DHCP"
}

func raBlockerText(b, iface string) string {
	switch b {
	case BlockerNoULA:
		return "PiCache has no stable unique local IPv6 address (ULA) on " + iface
	case BlockerNoRawSocket:
		return "PiCache has no raw ICMPv6 socket (it opens only at start, with CAP_NET_RAW)"
	}
	return "the interface cannot be served"
}

// --- API ---

// Interfaces lists the interfaces that are up (not loopback), for the
// interface selection: served interfaces must not be virtual and need
// exactly one private IPv4 address.
func (s *Service) Interfaces() []Interface {
	ifs, err := s.env.interfaces()
	if err != nil {
		return []Interface{}
	}
	addrs := s.env.addrs()
	out := []Interface{}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		it := Interface{Name: ifc.Name, IPv4: []string{}, IPv6: []InterfaceIPv6{}, Virtual: netutil.VirtualInterface(ifc.Name)}
		if len(ifc.HardwareAddr) == 6 {
			it.MAC = ifc.HardwareAddr.String()
		}
		for _, a := range addrs {
			if a.Iface != ifc.Name {
				continue
			}
			ip := netutil.Canon(a.Prefix.Addr())
			switch {
			case ip.Is4():
				it.IPv4 = append(it.IPv4, netip.PrefixFrom(ip, a.Prefix.Bits()).String())
				it.Dynamic4 = it.Dynamic4 || a.Dynamic
			case ip.IsLinkLocalUnicast():
				it.IPv6 = append(it.IPv6, InterfaceIPv6{Address: ip.String(), Kind: "link-local", Temporary: a.Temporary, Deprecated: a.Deprecated})
			case netutil.IsULA(ip):
				it.IPv6 = append(it.IPv6, InterfaceIPv6{Address: ip.String(), Kind: "ula", Temporary: a.Temporary, Deprecated: a.Deprecated})
			case ip.IsGlobalUnicast():
				it.IPv6 = append(it.IPv6, InterfaceIPv6{Address: ip.String(), Kind: "global", Temporary: a.Temporary, Deprecated: a.Deprecated})
			}
		}
		out = append(out, it)
	}
	slices.SortFunc(out, func(a, b Interface) int {
		if a.Virtual != b.Virtual {
			if a.Virtual {
				return 1
			}
			return -1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// CheckSettings checks enabled DHCP settings against the live interface
// (the settings package checks their form): the interface must be usable,
// the range inside its subnet with at least one address to hand out, the
// router known, and no NTP server the subnet's network or broadcast
// address. Disabled settings are not checked (the interface may
// be absent while the server is off).
func (s *Service) CheckSettings(in *settings.All) error {
	if !in.DHCP.Enabled {
		return nil
	}
	// The API calls this before the settings are normalised.
	c := *in
	a := &c
	h := &a.DHCP
	for _, p := range []*string{&h.Interface, &h.RangeStart, &h.RangeEnd, &h.Router, &h.DNSServer} {
		*p = strings.TrimSpace(*p)
	}
	h.Domain = strings.Trim(strings.ToLower(strings.TrimSpace(h.Domain)), ".")
	st := s.env.lookupIface(h.Interface)
	if st.problem != "" {
		return apperr.Invalid("dhcp.interface", "%s", st.problem)
	}
	subnet := st.self.Masked()
	if subnet.Bits() > 30 {
		return apperr.Invalid("dhcp.interface", "the subnet %s of %s is too small", subnet, h.Interface)
	}
	for _, f := range []struct{ field, v string }{{"dhcp.rangeStart", h.RangeStart}, {"dhcp.rangeEnd", h.RangeEnd}} {
		if ip, err := netip.ParseAddr(f.v); err != nil || !subnet.Contains(ip) {
			return apperr.Invalid(f.field, "must be an address of %s (the subnet of %s)", subnet, h.Interface)
		}
	}
	s.mu.Lock()
	last := s.lastGW
	s.mu.Unlock()
	g := s.env.evalGate(a, last)
	if g.err != "" {
		return apperr.Invalid("dhcp.router", "%s", g.err)
	}
	if g.pool == nil || g.pool.size() == 0 {
		return apperr.Invalid("dhcp.rangeEnd", "the range holds no address that can be handed out")
	}
	i := 0 // the index after normalisation, which drops empty entries
	for _, n := range h.Options.NTPServers {
		if n = strings.TrimSpace(n); n == "" {
			continue
		}
		if ip, err := netip.ParseAddr(n); err == nil && (ip.Unmap() == subnet.Addr() || ip.Unmap() == lastAddr(subnet)) {
			return apperr.Invalid("dhcp.options.ntpServers["+strconv.Itoa(i)+"]", "%s is the network or broadcast address of %s", ip.Unmap(), subnet)
		}
		i++
	}
	return nil
}

// randomXID returns a transaction ID for the probe.
func randomXID() uint32 {
	var b [4]byte
	_, _ = cryptorand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}
