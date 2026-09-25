package app

import (
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/clients"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// The network check (docs/ARCHITECTURE.md 17) answers "do all devices use
// PiCache?": it compares the kernel's neighbour table with the DNS
// activity of the last 24 hours and looks for the two router set-ups that
// hide devices from PiCache (the router forwards all queries; the router
// announces itself as IPv6 DNS server). Everything is read locally and
// unprivileged; the optional discovery scan sends one empty UDP datagram per
// address of this machine's private IPv4 subnets so that the devices
// answer ARP and appear in the neighbour table.

// Network check bounds and thresholds.
const (
	netCheckMaxAge     = 30 * time.Second // cache lifetime of a computed check
	netCheckScanMaxAge = 2 * time.Second  // … while a scan runs
	netActivityWindow  = 24 * time.Hour
	netKnownWindow     = 30 * 24 * time.Hour
	routerNameTTL      = 10 * time.Minute
	routerNameTimeout  = 2 * time.Second
	maxNetDevices      = 1024
	maxRefusedShown    = 20

	forwardingMinQueries = 200 // below this the shares are not judged
	forwardingWarnShare  = 0.8
	forwardingInfoShare  = 0.2

	scanMaxAddrs = 512
	scanRate     = 200 // packets per second
	scanCooldown = time.Minute
	scanPort     = 9 // discard
)

// scanSettle is how long a scan waits after its last packet for the
// answers (a variable for tests).
var scanSettle = 3 * time.Second

// Address classes, in the order device addresses are listed.
const (
	classIPv4 = iota
	classULA
	classGlobal
	classLinkLocal
)

func addrClass(a netip.Addr) int {
	switch {
	case a.Is4():
		return classIPv4
	case a.IsLinkLocalUnicast():
		return classLinkLocal
	case netutil.IsULA(a):
		return classULA
	}
	return classGlobal
}

// sortAddrs orders addresses by class (IPv4, ULA, global, link-local),
// then by value.
func sortAddrs(as []netip.Addr) {
	slices.SortFunc(as, func(a, b netip.Addr) int { return cmp.Or(cmp.Compare(addrClass(a), addrClass(b)), a.Compare(b)) })
}

func addrStrings(as []netip.Addr) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.String())
	}
	return out
}

// isFritzBox reports whether a host or domain name is the FRITZ!Box's own
// domain ("fritz.box" or below it).
func isFritzBox(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	return name == "fritz.box" || strings.HasSuffix(name, ".fritz.box")
}

// netSources are the facts the network check reads (replaced in tests).
type netSources struct {
	now        func() time.Time
	since      time.Time // start of the process (refused sources are counted since then)
	host       func() dnsserver.HostNetwork
	gateway4   func() (netip.Addr, error)
	gateway6   func() (netip.Addr, error)
	ptr        func(ctx context.Context, ip netip.Addr) (string, error)
	domain     func() string // effective local domain
	neighbours func(ctx context.Context) ([]clients.Neighbour, error)
	known      func(ctx context.Context, within time.Duration) ([]clients.Known, error)
	describe   func(ip netip.Addr, mac string) (clientID int64, name, hostname string)
	// lookupNames schedules PTR lookups for addresses without a recent
	// name, so devices that never asked PiCache get names too.
	lookupNames func(ips []netip.Addr)
	// stats returns the per-client statistics of [from, to), ok = false
	// when they are unavailable or keyed by anonymised addresses.
	stats   func(ctx context.Context, from, to time.Time) (st []logs.ClientStat, ok bool)
	refused func() []dnsserver.RefusedSource
	dnsIPv6 func() bool     // a DNS listener serves IPv6
	ownMACs func() []string // MAC addresses of this machine's interfaces
	// trustConnected reports the setting dns.trustConnectedNetworks.
	trustConnected func() bool
	// ignoresRA reports whether this machine ignores IPv6 router
	// advertisements (hostIgnoresRA; nil: unknown).
	ignoresRA func() bool
	// scanSupported: the neighbour table can be read (Linux).
	scanSupported bool
	send          func(ctx context.Context, addrs []netip.Addr) // sends the scan's datagrams
}

// netInputs are the facts one check is computed from.
type netInputs struct {
	now        time.Time
	since      time.Time
	host       dnsserver.HostNetwork
	gw4, gw6   netip.Addr
	routerName string
	domain     string
	neighbours []clients.Neighbour
	known      []clients.Known
	stats      []logs.ClientStat
	statsOK    bool
	refused    []dnsserver.RefusedSource
	dnsIPv6    bool
	ownMACs    []string
	describe   func(ip netip.Addr, mac string) (int64, string, string)

	// trustConnected: dns.trustConnectedNetworks; ignoresRA: this machine
	// ignores router advertisements (read only without a ULA or global
	// address).
	trustConnected, ignoresRA bool
}

// netChecker computes and caches the network check and runs the discovery
// scan. It implements api.Network.
type netChecker struct {
	src netSources
	log *slog.Logger

	computeMu sync.Mutex // one computation at a time

	mu        sync.Mutex // guards the fields below
	cached    *api.NetworkCheck
	cachedAt  time.Time
	cachedGen uint64
	gen       uint64 // incremented when a scan starts or ends (invalidates the cache)
	scan      api.NetworkScan
	router    struct {
		ip   netip.Addr
		name string
		at   time.Time
	}
	runCtx  context.Context // set by Start
	stopped bool
	scans   sync.WaitGroup
}

func newNetChecker(src netSources, log *slog.Logger) *netChecker {
	n := &netChecker{src: src, log: log.With(slog.String("component", "network"))}
	if n.src.send == nil {
		n.src.send = n.sendProbes
	}
	return n
}

// netSources returns the live sources of the network check.
func (a *App) netSources() netSources {
	return netSources{
		now: time.Now, since: a.started,
		host: a.dns.HostNetwork, gateway4: netutil.DefaultGatewayIPv4, gateway6: netutil.DefaultGatewayIPv6,
		ptr:        a.lookupClientName,
		domain:     func() string { return a.dns.Router().Domain },
		neighbours: a.clients.Neighbours, known: a.clients.Known, describe: a.clients.Describe,
		lookupNames: a.clients.LookupNames,
		stats: func(ctx context.Context, from, to time.Time) ([]logs.ClientStat, bool) {
			if a.logs.Metrics().Disabled != "" || a.set.Get().Logs.AnonymizeClientIPs {
				return nil, false
			}
			st, err := a.logs.ClientStats(ctx, from, to)
			if err != nil {
				a.log.Debug("network check: client statistics unavailable", slog.Any("err", err))
				return nil, false
			}
			return st, true
		},
		refused:        a.dns.RefusedSources,
		dnsIPv6:        a.dnsServesIPv6,
		ownMACs:        interfaceMACs,
		trustConnected: func() bool { return a.set.Get().DNS.TrustConnectedNetworks },
		ignoresRA:      hostIgnoresRA,
		scanSupported:  runtime.GOOS == "linux",
	}
}

// hostIgnoresRA reports whether this machine ignores IPv6 router
// advertisements: its default-route interface does, or (without a default
// route) every interface that is up and neither loopback nor virtual does.
// False when this cannot be read (systems other than Linux).
func hostIgnoresRA() bool {
	if iface := netutil.DefaultRouteInterface(); iface != "" {
		ignores, ok := netutil.IgnoresRouterAdvertisements(iface)
		return ok && ignores
	}
	ifs, err := net.Interfaces()
	if err != nil {
		return false
	}
	n := 0
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || netutil.VirtualInterface(ifc.Name) {
			continue
		}
		if ignores, ok := netutil.IgnoresRouterAdvertisements(ifc.Name); !ok || !ignores {
			return false
		}
		n++
	}
	return n > 0
}

// dnsServesIPv6 reports whether a DNS listener accepts IPv6 queries from
// the network (bound to "::" or a non-loopback IPv6 address).
func (a *App) dnsServesIPv6() bool {
	for _, pc := range a.ln.dnsUDP {
		if ip := netutil.AddrFromNet(pc.LocalAddr()); ip.Is6() && !ip.IsLoopback() {
			return true
		}
	}
	return false
}

// interfaceMACs returns the Ethernet addresses of this machine's interfaces.
func interfaceMACs() []string {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifs {
		if len(ifc.HardwareAddr) == 6 {
			out = append(out, ifc.HardwareAddr.String())
		}
	}
	return out
}

// Start keeps the context the scans run in and waits for them when ctx
// ends (blocks).
func (n *netChecker) Start(ctx context.Context) {
	n.mu.Lock()
	n.runCtx = ctx
	n.mu.Unlock()
	<-ctx.Done()
	n.mu.Lock()
	n.stopped = true
	n.mu.Unlock()
	n.scans.Wait()
}

// Check returns the network check (api.Network): the cached one while it
// is younger than 30 s (2 s while a scan runs) and no scan started or
// ended since, else a fresh one.
func (n *netChecker) Check(ctx context.Context) api.NetworkCheck {
	n.computeMu.Lock()
	defer n.computeMu.Unlock()
	now := n.src.now()
	n.mu.Lock()
	maxAge := netCheckMaxAge
	if n.scan.Running {
		maxAge = netCheckScanMaxAge
	}
	if c := n.cached; c != nil && n.cachedGen == n.gen && !now.Before(n.cachedAt) && now.Sub(n.cachedAt) < maxAge {
		out := *c
		out.Scan = n.scan
		n.mu.Unlock()
		return out
	}
	gen := n.gen
	n.mu.Unlock()

	nc := computeNetworkCheck(n.gather(ctx, now))
	n.mu.Lock()
	defer n.mu.Unlock()
	if ctx.Err() == nil { // a cancelled request may have read partial data
		c := nc
		n.cached, n.cachedAt, n.cachedGen = &c, now, gen
	}
	nc.Scan = n.scan
	return nc
}

// gather reads the inputs of one check. Unreadable sources count as empty.
func (n *netChecker) gather(ctx context.Context, now time.Time) netInputs {
	s := n.src
	in := netInputs{now: now, since: s.since, host: s.host(), domain: s.domain(), refused: s.refused(),
		dnsIPv6: s.dnsIPv6(), ownMACs: s.ownMACs(), describe: s.describe}
	if s.trustConnected != nil {
		in.trustConnected = s.trustConnected()
	}
	if s.ignoresRA != nil && !in.host.Bridge && !slices.ContainsFunc(in.host.Prefixes, func(p netip.Prefix) bool {
		c := addrClass(netutil.Canon(p.Addr()))
		return c == classULA || c == classGlobal
	}) {
		in.ignoresRA = s.ignoresRA()
	}
	if ip, err := s.gateway4(); err == nil {
		in.gw4 = netutil.Canon(ip)
	}
	if ip, err := s.gateway6(); err == nil {
		in.gw6 = netutil.Canon(ip)
	}
	var err error
	if in.neighbours, err = s.neighbours(ctx); err != nil {
		n.log.Debug("network check: neighbour table unavailable", slog.Any("err", err))
	}
	if s.lookupNames != nil && !in.host.Bridge {
		ips := make([]netip.Addr, 0, len(in.neighbours))
		for _, nb := range in.neighbours {
			ips = append(ips, nb.IP)
		}
		s.lookupNames(ips)
	}
	if in.known, err = s.known(ctx, netKnownWindow); err != nil {
		n.log.Debug("network check: client activity unavailable", slog.Any("err", err))
	}
	in.stats, in.statsOK = s.stats(ctx, now.Add(-netActivityWindow), now)
	gw := in.gw4
	if !gw.IsValid() {
		gw = in.gw6
	}
	if !in.host.Bridge && gw.IsValid() {
		in.routerName = n.routerName(ctx, gw, now)
	}
	return in
}

// routerName returns the PTR name of the gateway, cached for 10 minutes
// (also when the lookup fails).
func (n *netChecker) routerName(ctx context.Context, gw netip.Addr, now time.Time) string {
	n.mu.Lock()
	r := n.router
	n.mu.Unlock()
	if r.ip == gw && !now.Before(r.at) && now.Sub(r.at) < routerNameTTL {
		return r.name
	}
	cctx, cancel := context.WithTimeout(ctx, routerNameTimeout)
	name, err := n.src.ptr(cctx, gw)
	cancel()
	if err != nil {
		n.log.Debug("network check: router name lookup failed", slog.String("router", gw.String()), slog.Any("err", err))
		name = ""
	}
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	if _, _, ok := netutil.NormalizeHost(name); !ok {
		name = ""
	}
	if ctx.Err() != nil {
		return name // the caller went away: do not cache a failed lookup
	}
	n.mu.Lock()
	n.router.ip, n.router.name, n.router.at = gw, name, now
	n.mu.Unlock()
	return name
}

// computeNetworkCheck derives the check from its inputs (pure).
func computeNetworkCheck(in netInputs) api.NetworkCheck {
	nc := api.NetworkCheck{CheckedAt: in.now.UTC(), Mode: "host", StatsAvailable: in.statsOK,
		Checks: []api.NetworkItem{}, Devices: []api.NetworkDevice{}}
	if in.host.Bridge {
		nc.Mode = "bridge"
	}
	var own map[netip.Addr]bool
	nc.Self, own = in.self()
	var router map[netip.Addr]bool
	var routerMAC string
	nc.Router, router, routerMAC = in.router(own)
	act, lastByMAC := in.activity()

	// Queries of the last 24 h from other devices.
	var q api.NetworkQueries
	routerQueries := map[netip.Addr]int64{}
	var v6Queries int64
	v6Clients := 0
	for ip, a := range act {
		if ip.IsLoopback() || own[ip] || a.queries <= 0 {
			continue
		}
		q.Total += a.queries
		if ip.Is4() {
			q.IPv4 += a.queries
		} else {
			q.IPv6 += a.queries
		}
		if router[ip] {
			q.FromRouter += a.queries
			routerQueries[ip] = a.queries
		} else if ip.Is6() {
			v6Queries += a.queries
			v6Clients++
		}
	}
	nc.Queries24h = q

	nc.Checks = append(nc.Checks, forwardingCheck(q, router, routerQueries, in.host.Bridge))
	lanV6 := len(nc.Self.ULA)+len(nc.Self.Global) > 0
	for _, nb := range in.neighbours {
		if nb.IP.Is6() && !nb.IP.IsLinkLocalUnicast() {
			lanV6 = true
		}
	}
	ignoresRA := in.ignoresRA && len(nc.Self.ULA)+len(nc.Self.Global) == 0
	nc.Checks = append(nc.Checks, ipv6Checks(nc.Self, lanV6, v6Queries, v6Clients, ignoresRA)...)

	status := "ok"
	if len(in.refused) > 0 {
		status = "warn"
	}
	nc.Checks = append(nc.Checks, api.NetworkItem{ID: "refused", Status: status,
		Data: api.NetworkRefused{Sources: in.refusedSources(), Since: in.since.UTC(), TrustConnectedNetworks: in.trustConnected}})

	// A bridge network shows only the bridge: no device list.
	if in.host.Bridge {
		return nc
	}
	exclude := func(ip netip.Addr, mac string) bool { return mac == routerMAC || own[ip] || router[ip] }
	devices, counts := in.devices(exclude, act, lastByMAC)
	nc.Devices = devices[:min(len(devices), maxNetDevices)]
	status = "ok"
	if counts.Inactive+counts.Never > 0 {
		status = "info"
	}
	nc.Checks = append(nc.Checks, api.NetworkItem{ID: "devices", Status: status, Data: counts})
	return nc
}

// refusedSources returns the refused sources shown (newest first, at most
// 20), each marked whether it is inside a network this machine is
// connected to (the networks dns.trustConnectedNetworks would allow).
func (in *netInputs) refusedSources() []api.NetworkRefusedSource {
	var nets []netip.Prefix
	for _, p := range in.host.Prefixes {
		if n, ok := netutil.ConnectedPrefix(p); ok {
			nets = append(nets, n)
		}
	}
	out := make([]api.NetworkRefusedSource, 0, min(len(in.refused), maxRefusedShown))
	for _, r := range in.refused[:min(len(in.refused), maxRefusedShown)] {
		src := api.NetworkRefusedSource{Address: r.Address, Count: r.Count, Last: r.Last}
		if ip, err := netip.ParseAddr(r.Address); err == nil {
			ip = netutil.Canon(ip)
			src.OnLink = slices.ContainsFunc(nets, func(p netip.Prefix) bool { return p.Contains(ip) })
		}
		out = append(out, src)
	}
	return out
}

// self classifies this machine's addresses (link-local ones are left out)
// and returns them as a set too.
func (in *netInputs) self() (api.NetworkSelf, map[netip.Addr]bool) {
	own := map[netip.Addr]bool{}
	var addrs []netip.Addr
	for _, p := range in.host.Prefixes {
		a := netutil.Canon(p.Addr())
		if a.IsValid() && !own[a] {
			own[a] = true
			addrs = append(addrs, a)
		}
	}
	sortAddrs(addrs)
	s := api.NetworkSelf{IPv4: []string{}, ULA: []string{}, Global: []string{}, DNSIPv6: in.dnsIPv6}
	for _, a := range addrs {
		switch addrClass(a) {
		case classIPv4:
			if !a.IsLinkLocalUnicast() {
				s.IPv4 = append(s.IPv4, a.String())
			}
		case classULA:
			s.ULA = append(s.ULA, a.String())
		case classGlobal:
			s.Global = append(s.Global, a.String())
		}
	}
	return s, own
}

// router describes the default gateway and returns its addresses (the
// gateways and every neighbour address with the gateway's MAC) and MAC. In
// a bridge network the gateway is the bridge, which forwards the queries
// of every LAN device; the router itself is not visible.
func (in *netInputs) router(own map[netip.Addr]bool) (*api.NetworkRouter, map[netip.Addr]bool, string) {
	gw4, gw6 := in.gw4, in.gw6
	if own[gw4] {
		gw4 = netip.Addr{}
	}
	if own[gw6] {
		gw6 = netip.Addr{}
	}
	r := &api.NetworkRouter{IPv6: []string{}, Kind: "unknown"}
	addrs := map[netip.Addr]bool{}
	if in.host.Bridge {
		if gw4.IsValid() {
			addrs[gw4] = true
		}
		if isFritzBox(in.domain) {
			r.Kind = "fritzbox"
		}
		return r, addrs, ""
	}
	mac := ""
	for _, gw := range []netip.Addr{gw4, gw6} {
		if !gw.IsValid() {
			continue
		}
		addrs[gw] = true
		for _, nb := range in.neighbours {
			if mac == "" && nb.IP == gw {
				mac = nb.MAC
			}
		}
	}
	var v6 []netip.Addr
	for _, nb := range in.neighbours {
		if mac != "" && nb.MAC == mac {
			addrs[nb.IP] = true
		}
	}
	for ip := range addrs {
		if ip.Is6() {
			v6 = append(v6, ip)
		}
	}
	sortAddrs(v6)
	r.IPv6, r.MAC, r.Name = addrStrings(v6), mac, in.routerName
	if gw4.IsValid() {
		r.IPv4 = gw4.String()
	}
	switch {
	case !gw4.IsValid() && !gw6.IsValid():
	case isFritzBox(in.routerName) || isFritzBox(in.domain):
		r.Kind = "fritzbox"
	default:
		r.Kind = "generic"
	}
	return r, addrs, mac
}

// netActivity is the DNS activity of one address.
type netActivity struct {
	queries int64     // in the last 24 h
	last    time.Time // last query (up to 30 days back)
}

// activity returns the activity per address (the statistics of the last
// 24 h, or the in-memory activity of addresses seen in the last 24 h when
// they are unavailable) and the last query per MAC.
func (in *netInputs) activity() (map[netip.Addr]*netActivity, map[string]time.Time) {
	act := map[netip.Addr]*netActivity{}
	get := func(s string) *netActivity {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return nil
		}
		ip = netutil.Canon(ip)
		a := act[ip]
		if a == nil {
			a = &netActivity{}
			act[ip] = a
		}
		return a
	}
	if in.statsOK {
		for _, st := range in.stats {
			if a := get(st.ClientIP); a != nil {
				a.queries += st.Queries
				if st.LastSeen.After(a.last) {
					a.last = st.LastSeen
				}
			}
		}
	}
	lastByMAC := map[string]time.Time{}
	for _, k := range in.known {
		a := get(k.IP)
		if a == nil {
			continue
		}
		if k.LastSeen.After(a.last) {
			a.last = k.LastSeen
		}
		if !in.statsOK && in.now.Sub(k.LastSeen) < netActivityWindow {
			a.queries += k.Queries
		}
		if k.MAC != "" && k.LastSeen.After(lastByMAC[k.MAC]) {
			lastByMAC[k.MAC] = k.LastSeen
		}
	}
	return act, lastByMAC
}

// forwardingCheck is router-forwarding (container-nat in a bridge
// network): warn from 80 % of at least 200 queries from the router's
// addresses, info from 20 %.
func forwardingCheck(q api.NetworkQueries, router map[netip.Addr]bool, perAddr map[netip.Addr]int64, bridge bool) api.NetworkItem {
	var addrs []netip.Addr
	for ip := range router {
		addrs = append(addrs, ip)
	}
	slices.SortFunc(addrs, func(a, b netip.Addr) int {
		return cmp.Or(cmp.Compare(perAddr[b], perAddr[a]), cmp.Compare(addrClass(a), addrClass(b)), a.Compare(b))
	})
	data := api.NetworkForwarding{RouterQueries: q.FromRouter, TotalQueries: q.Total, RouterAddresses: addrStrings(addrs)}
	status := "ok"
	if q.Total > 0 {
		share := float64(q.FromRouter) / float64(q.Total)
		data.Share = math.Round(share*1000) / 1000
		switch {
		case q.Total < forwardingMinQueries:
		case share >= forwardingWarnShare:
			status = "warn"
		case share >= forwardingInfoShare:
			status = "info"
		}
	}
	id := "router-forwarding"
	if bridge {
		id = "container-nat"
	}
	return api.NetworkItem{ID: id, Status: status, Data: data}
}

// ipv6Checks are ipv6-dns (the LAN has IPv6 but no LAN device asked over
// IPv6: they probably use the router as IPv6 DNS server) and ipv6-address
// (PiCache has no stable address to announce: warn without any IPv6
// address, info with global addresses only, which change with the prefix).
// ignoresRA is reported with ipv6-dns (hostIgnoresRA).
func ipv6Checks(self api.NetworkSelf, lanV6 bool, queries int64, clients int, ignoresRA bool) []api.NetworkItem {
	dns := "ok"
	if lanV6 && queries == 0 {
		dns = "warn"
	}
	address := "ok"
	switch {
	case !lanV6 || len(self.ULA) > 0:
	case len(self.Global) > 0:
		address = "info"
	default:
		address = "warn"
	}
	return []api.NetworkItem{
		{ID: "ipv6-dns", Status: dns, Data: api.NetworkIPv6DNS{LANHasIPv6: lanV6, IPv6Queries: queries, IPv6Clients: clients,
			ULA: self.ULA, Global: self.Global, HostIgnoresRA: ignoresRA}},
		{ID: "ipv6-address", Status: address, Data: api.NetworkIPv6Address{ULA: self.ULA, Global: self.Global}},
	}
}

// devices groups the neighbours by MAC (excluded: the router, this machine
// and whatever exclude says) and sorts them: never, inactive, active, then
// by address.
func (in *netInputs) devices(exclude func(netip.Addr, string) bool, act map[netip.Addr]*netActivity,
	lastByMAC map[string]time.Time) ([]api.NetworkDevice, api.NetworkDeviceCounts) {
	ownMAC := map[string]bool{}
	for _, m := range in.ownMACs {
		ownMAC[strings.ToLower(m)] = true
	}
	byMAC := map[string][]netip.Addr{}
	var macs []string
	for _, nb := range in.neighbours {
		ip := netutil.Canon(nb.IP)
		if nb.MAC == "" || ownMAC[nb.MAC] || exclude(ip, nb.MAC) {
			continue
		}
		if _, ok := byMAC[nb.MAC]; !ok {
			macs = append(macs, nb.MAC)
		}
		if !slices.Contains(byMAC[nb.MAC], ip) {
			byMAC[nb.MAC] = append(byMAC[nb.MAC], ip)
		}
	}
	var counts api.NetworkDeviceCounts
	out := make([]api.NetworkDevice, 0, len(macs))
	first := map[string]netip.Addr{}
	for _, mac := range macs {
		ips := byMAC[mac]
		sortAddrs(ips)
		first[mac] = ips[0]
		d := api.NetworkDevice{MAC: mac, IPs: addrStrings(ips)}
		hostname, last := "", lastByMAC[mac]
		for _, ip := range ips {
			if in.describe != nil {
				id, name, host := in.describe(ip, mac)
				if d.ClientID == 0 && id != 0 {
					d.ClientID, d.Name = id, name
				}
				if hostname == "" {
					hostname = host
				}
			}
			if a := act[ip]; a != nil {
				d.Queries24h += a.queries
				if a.last.After(last) {
					last = a.last
				}
			}
		}
		if d.Name == "" {
			d.Name = hostname
		}
		switch {
		case last.IsZero():
			d.Status = "never"
			counts.Never++
		case in.now.Sub(last) < netActivityWindow:
			d.Status, d.LastQuery = "active", last.UTC()
			counts.Active++
		default:
			d.Status, d.LastQuery = "inactive", last.UTC()
			counts.Inactive++
		}
		out = append(out, d)
	}
	counts.Total = len(out)
	rank := map[string]int{"never": 0, "inactive": 1, "active": 2}
	slices.SortFunc(out, func(a, b api.NetworkDevice) int {
		return cmp.Or(cmp.Compare(rank[a.Status], rank[b.Status]), cmp.Compare(addrClass(first[a.MAC]), addrClass(first[b.MAC])),
			first[a.MAC].Compare(first[b.MAC]), strings.Compare(a.MAC, b.MAC))
	})
	return out, counts
}

// networkHealth is the health check "network": a warning while most
// queries come from the router or the container network's gateway.
func networkHealth(nc api.NetworkCheck) (status, msg, hint string) {
	for _, c := range nc.Checks {
		if c.Status != "warn" || (c.ID != "router-forwarding" && c.ID != "container-nat") {
			continue
		}
		ip := "?"
		if d, ok := c.Data.(api.NetworkForwarding); ok && len(d.RouterAddresses) > 0 {
			ip = d.RouterAddresses[0]
		}
		if c.ID == "container-nat" {
			return "warn", fmt.Sprintf("Most DNS queries come from the container network's gateway (%s): PiCache cannot tell devices apart", ip),
				"Run PiCache with host networking. See DNS → Network check."
		}
		return "warn", fmt.Sprintf("Most DNS queries come from the router (%s): PiCache cannot tell devices apart", ip),
			"Set PiCache as the DNS server in the router's DHCP settings instead of forwarding. See DNS → Network check."
	}
	return "ok", "", ""
}

// --- discovery scan ---

// Scan starts a discovery scan (api.Network).
func (n *netChecker) Scan() (int, error) {
	if !n.src.scanSupported {
		return 0, apperr.Unavailable("the network scan needs Linux (PiCache reads the kernel's neighbour table)")
	}
	host := n.src.host()
	if host.Bridge {
		return 0, apperr.Unavailable("the network scan is not available in a container bridge network; run PiCache with host networking")
	}
	addrs := scanTargets(host.Prefixes)
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.src.now()
	switch {
	case n.runCtx == nil || n.stopped:
		return 0, apperr.Unavailable("the network scan is not available while PiCache starts or stops")
	case n.scan.Running:
		return 0, apperr.Conflict("a network scan is already running")
	case !n.scan.StartedAt.IsZero() && now.Sub(n.scan.StartedAt) < scanCooldown:
		wait := int((scanCooldown - now.Sub(n.scan.StartedAt) + time.Second - 1) / time.Second)
		return 0, apperr.TooMany("a network scan started less than a minute ago; try again in %d s", wait)
	case len(addrs) == 0:
		return 0, apperr.Unavailable("no private IPv4 network of this machine to scan")
	}
	n.scan = api.NetworkScan{Running: true, StartedAt: now.UTC(), Addresses: len(addrs)}
	n.gen++
	ctx := n.runCtx
	n.log.Info("network scan started", slog.Int("addresses", len(addrs)))
	n.scans.Go(func() {
		n.src.send(ctx, addrs)
		t := time.NewTimer(scanSettle)
		select {
		case <-ctx.Done():
		case <-t.C:
		}
		t.Stop()
		n.mu.Lock()
		n.scan.Running = false
		n.scan.FinishedAt = n.src.now().UTC()
		n.gen++
		n.mu.Unlock()
	})
	return len(addrs), nil
}

// scanTargets returns the addresses a scan probes: the hosts of this
// machine's private (RFC 1918) IPv4 subnets, subnets of /24 or smaller in
// full and larger ones only the /24 around this machine's address, without
// the subnet's network and broadcast addresses and this machine's
// addresses, at most 512.
func scanTargets(prefixes []netip.Prefix) []netip.Addr {
	own := map[netip.Addr]bool{}
	for _, p := range prefixes {
		own[netutil.Canon(p.Addr())] = true
	}
	var out []netip.Addr
	seen := map[netip.Addr]bool{}
	for _, p := range prefixes {
		a := netutil.Canon(p.Addr())
		if !a.Is4() || !netutil.IsRFC1918(a) || p.Bits() > 30 {
			continue // /31 and /32 links have no other hosts to find
		}
		subnet := netip.PrefixFrom(a, p.Bits()).Masked()
		span := subnet
		if p.Bits() < 24 {
			span = netip.PrefixFrom(a, 24).Masked()
		}
		network, broadcast := subnet.Addr(), lastAddr(subnet)
		for x := span.Addr(); span.Contains(x); x = x.Next() {
			if x == network || x == broadcast || own[x] || seen[x] {
				continue
			}
			seen[x] = true
			out = append(out, x)
			if len(out) == scanMaxAddrs {
				return out
			}
		}
	}
	return out
}

// lastAddr returns the broadcast address of an IPv4 prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	v := binary.BigEndian.Uint32(b[:]) | uint32(1<<(32-p.Bits())-1)
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

// sendProbes sends one empty UDP datagram to the discard port of each
// address from one unconnected, unprivileged socket, at most scanRate per
// second. The kernel resolves each address with ARP first; devices that
// answer appear in the neighbour table. Nothing is read back.
func (n *netChecker) sendProbes(ctx context.Context, addrs []netip.Addr) {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		n.log.Warn("network scan: cannot open a UDP socket", slog.Any("err", err))
		return
	}
	defer conn.Close()
	tick := time.NewTicker(time.Second / scanRate)
	defer tick.Stop()
	for _, a := range addrs {
		_, _ = conn.WriteToUDPAddrPort(nil, netip.AddrPortFrom(a, scanPort))
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
