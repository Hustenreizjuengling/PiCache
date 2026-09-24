package dnsserver

import (
	"context"
	"log/slog"
	"net/netip"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// routerState is the router resolver (dns.routerResolver).
type routerState struct {
	mode    string // off | auto | manual
	addr    netip.Addr
	answers bool // DNS probe succeeded
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
		return &routerState{mode: "manual", addr: ip.Unmap()}
	}
}

// routerAddr returns the router resolver address if one is usable.
func (s *Server) routerAddr() (netip.Addr, bool) { return s.router.Load().usable() }

// routerUpstream formats a router address as a plain DNS upstream.
func routerUpstream(ip netip.Addr) string { return netip.AddrPortFrom(ip, 53).String() }

// detectRouter re-reads the default gateway (auto mode) and probes the
// router resolver. A gateway that is this machine is never used (loop).
func (s *Server) detectRouter(ctx context.Context) {
	set := s.d.Settings.Get()
	cfg := set.DNS.RouterResolver
	st := configuredRouter(set)
	if st.mode == "auto" {
		gw, err := s.env.gateway()
		if err == nil && !s.host.Load().isOwn(gw) {
			st.addr = gw
		}
	}
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
		s.reconfigureLimiter()
	}
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
