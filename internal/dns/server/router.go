package dnsserver

import (
	"cmp"
	"context"
	"log/slog"
	"net/netip"
	"slices"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// routerState is the router resolver (dns.routerResolver).
type routerState struct {
	mode    string // off | auto | manual
	addr    netip.Addr
	answers bool // DNS probe succeeded
	// addrs are all addresses of the router (canonical): addr, the
	// detected gateways and every neighbour address with the MAC of addr
	// or the gateway. The loop guard and the rate-limit exemption apply to
	// all of them.
	addrs []netip.Addr
	// protect are the router's addresses whatever the mode (addr, the
	// IPv4 and IPv6 default gateways and every neighbour address with the
	// MAC of one of them), macs those MACs and trustedMACs the neighbour
	// MACs of the trusted EDNS forwarders: dns.blockedClients never drops
	// them. A settings change keeps them until the next detection.
	protect     []netip.Addr
	macs        []string
	trustedMACs []string
}

// usable reports the address to use: a manual address always, an
// auto-detected one only when it answers DNS.
func (r *routerState) usable() (netip.Addr, bool) {
	switch r.mode {
	case "manual":
		return r.addr, r.addr.IsValid()
	case "auto":
		return r.addr, r.addr.IsValid() && r.answers
	}
	return netip.Addr{}, false
}

// configuredRouter returns the router state implied by the settings alone
// (before detection and probing).
func configuredRouter(set *settings.All) *routerState {
	switch v := set.DNS.RouterResolver; v {
	case "":
		return &routerState{mode: "off"}
	case "auto":
		return &routerState{mode: "auto"}
	default:
		ip, _ := netip.ParseAddr(v)
		st := &routerState{mode: "manual", addr: ip.Unmap()}
		if st.addr.IsValid() {
			st.addrs = []netip.Addr{netutil.Canon(st.addr)}
			st.protect = st.addrs
		}
		return st
	}
}

// startRouter returns the router state before the first detection: the
// configured one, with the default gateways already protected from
// dns.blockedClients (the detection adds the neighbour table).
func (s *Server) startRouter(set *settings.All) *routerState {
	st := configuredRouter(set)
	st.protect, _ = routerAddrSet(append(slices.Clone(st.protect), s.gateways(s.host.Load())...), nil, false)
	return st
}

// routerAddr returns the router resolver address if one is usable.
func (s *Server) routerAddr() (netip.Addr, bool) { return s.router.Load().usable() }

// routerAddrs returns all addresses of the router resolver (loop guard).
func (s *Server) routerAddrs() []netip.Addr { return s.router.Load().addrs }

// routerUpstream formats a router address as a plain DNS upstream
// ("[fe80::1%eth0]:53" for a link-local address).
func routerUpstream(ip netip.Addr) string { return netip.AddrPortFrom(ip, 53).String() }

// detectRouter re-reads the default gateways (auto mode: the IPv4 gateway,
// else the IPv6 one) and the router's other addresses from the neighbour
// table, and probes the router resolver. A gateway that is this machine is
// never used (loop). The addresses and MACs dns.blockedClients never drops
// (every gateway, the trusted EDNS forwarders' MACs) are read in every
// mode and apply before the probe.
func (s *Server) detectRouter(ctx context.Context) {
	set := s.d.Settings.Get()
	cfg := set.DNS.RouterResolver
	st := configuredRouter(set)
	var nbs []clients.Neighbour
	if s.env.neighbours != nil {
		var err error
		if nbs, err = s.env.neighbours(ctx); err != nil {
			s.log.Debug("router resolver: neighbour table unavailable", slog.Any("err", err))
		}
	}
	host := s.host.Load()
	gateways := s.gateways(host)
	if st.mode == "auto" && len(gateways) > 0 {
		st.addr = gateways[0]
		if st.addr.Is6() {
			st.addr = routerV6Addr(st.addr, nbs, host)
		}
	}
	seeds := append([]netip.Addr{st.addr}, gateways...)
	if st.mode == "auto" {
		st.addrs, _ = routerAddrSet(seeds, nbs, true)
	} else {
		st.addrs, _ = routerAddrSet(seeds[:1], nbs, true)
	}
	st.protect, st.macs = routerAddrSet(seeds, nbs, false)
	for _, ip := range s.lists.Load().trusted {
		if mac := neighbourMAC(ip, nbs); mac != "" && !slices.Contains(st.trustedMACs, mac) {
			st.trustedMACs = append(st.trustedMACs, mac)
		}
	}
	// The probe takes up to a second: protect the router meanwhile.
	s.routerMu.Lock()
	if ctx.Err() == nil && s.d.Settings.Get().DNS.RouterResolver == cfg {
		cur := *s.router.Load()
		cur.protect, cur.macs, cur.trustedMACs = st.protect, st.macs, st.trustedMACs
		s.router.Store(&cur)
	}
	s.routerMu.Unlock()
	if st.addr.IsValid() && s.d.Upstream != nil {
		st.answers = s.d.Upstream.Probe(ctx, st.addr)
	}
	s.routerMu.Lock()
	defer s.routerMu.Unlock()
	if ctx.Err() != nil || s.d.Settings.Get().DNS.RouterResolver != cfg {
		return // settings changed meanwhile; the pending kick re-detects
	}
	old := s.router.Swap(st)
	if old.mode != st.mode || old.addr != st.addr || old.answers != st.answers {
		s.log.Info("router resolver", slog.String("mode", st.mode), slog.String("address", addrOrNone(st.addr)),
			slog.Bool("answers", st.answers))
	}
	if !slices.Equal(old.addrs, st.addrs) || old.addr != st.addr {
		s.reconfigureLimiter()
	}
}

// routerV6Addr returns the address to reach an IPv6 gateway at: a
// non-link-local address of the router from the neighbour table (same
// MAC; ULA before global, the smallest first), else the gateway's
// link-local address with its zone.
func routerV6Addr(gw netip.Addr, nbs []clients.Neighbour, host *hostInfo) netip.Addr {
	mac := neighbourMAC(netutil.Canon(gw), nbs)
	if mac == "" {
		return gw
	}
	var cands []netip.Addr
	for _, nb := range nbs {
		ip := netutil.Canon(nb.IP)
		if nb.MAC == mac && ip.Is6() && !ip.IsLinkLocalUnicast() && !ip.IsLoopback() && !host.isOwn(ip) {
			cands = append(cands, ip)
		}
	}
	if len(cands) == 0 {
		return gw
	}
	slices.SortFunc(cands, func(a, b netip.Addr) int { return cmp.Or(cmp.Compare(v6Rank(a), v6Rank(b)), a.Compare(b)) })
	return cands[0]
}

// gateways returns the IPv4 and IPv6 default gateways that are not this
// machine (IPv4 first; link-local ones with their zone).
func (s *Server) gateways(host *hostInfo) []netip.Addr {
	var out []netip.Addr
	for _, get := range []func() (netip.Addr, error){s.env.gateway, s.env.gateway6} {
		if get == nil {
			continue
		}
		if gw, err := get(); err == nil && gw.IsValid() && !host.isOwn(netutil.Canon(gw)) {
			out = append(out, gw)
		}
	}
	return out
}

// routerAddrSet returns the router's addresses: the seeds (its address and
// the gateways; invalid ones skipped) and every neighbour address with the
// MAC of a seed (sorted, canonical), and those MACs. With first only the
// MAC of the first seed that has one counts (one router: the loop guard);
// otherwise every seed's (dns.blockedClients protects a second gateway,
// e.g. a separate IPv6 router, too).
func routerAddrSet(seeds []netip.Addr, nbs []clients.Neighbour, first bool) ([]netip.Addr, []string) {
	var out []netip.Addr
	add := func(ip netip.Addr) {
		if ip = netutil.Canon(ip); ip.IsValid() && !slices.Contains(out, ip) {
			out = append(out, ip)
		}
	}
	for _, ip := range seeds {
		add(ip)
	}
	var macs []string
	for _, ip := range out {
		if mac := neighbourMAC(ip, nbs); mac != "" && !slices.Contains(macs, mac) {
			macs = append(macs, mac)
			if first {
				break
			}
		}
	}
	for _, nb := range nbs {
		if slices.Contains(macs, nb.MAC) {
			add(nb.IP)
		}
	}
	slices.SortFunc(out, func(a, b netip.Addr) int { return a.Compare(b) })
	return out, macs
}

// neighbourMAC returns the MAC of ip in the neighbour table ("" if absent).
func neighbourMAC(ip netip.Addr, nbs []clients.Neighbour) string {
	for _, nb := range nbs {
		if netutil.Canon(nb.IP) == ip {
			return nb.MAC
		}
	}
	return ""
}

// Router returns the router-resolver state.
func (s *Server) Router() RouterStatus {
	st := s.router.Load()
	out := RouterStatus{Mode: st.mode, Answers: st.answers, Domain: s.d.Settings.Get().DNS.LocalDomain}
	if ip, ok := st.usable(); ok {
		out.Address = ip.String()
	}
	return out
}

func addrOrNone(ip netip.Addr) string {
	if !ip.IsValid() {
		return "none"
	}
	return ip.String()
}
