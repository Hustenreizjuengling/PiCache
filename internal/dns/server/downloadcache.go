package dnsserver

import (
	"fmt"
	"net/netip"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// cacheIPState is the effective address set of the download cache DNS
// answers. It is computed whether or not the download cache is enabled, so
// the UI can show the would-be addresses and warnings before it is turned
// on.
type cacheIPState struct {
	v4, v6 []netip.Addr
	auto   bool
	// bridge: PiCache runs in a container bridge network, so its interface
	// addresses are unreachable for clients; server names are then answered
	// with the configured cache addresses (the host's LAN address).
	bridge bool
	// warning explains why no IPv4 is known (then it is also the reason
	// overrides are inactive) or warns about the chosen address.
	warning string
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// describeNonPrivate names the kind of a non-RFC 1918 primary address.
func describeNonPrivate(ip netip.Addr) string {
	switch {
	case cgnat.Contains(ip):
		return fmt.Sprintf("the primary address %s is a carrier-grade NAT (100.64.0.0/10) address, which game clients do not accept as a cache", ip)
	case netutil.IsPublicUnicast(ip):
		return fmt.Sprintf("the primary address %s is a public address, which game clients do not accept as a cache", ip)
	}
	return fmt.Sprintf("the primary address %s is not a private (RFC 1918) address", ip)
}

// computeCacheIPs derives the answer addresses from the settings or, when
// none are configured, from the host: the primary IPv4 if it is RFC 1918,
// else the first RFC 1918 interface address; never in Docker bridge mode.
func computeCacheIPs(l *settings.DownloadCache, env hostEnv) *cacheIPState {
	primary, perr := env.primary()
	private, docker0 := env.ifaces()
	container := env.container == "docker" || env.container == "podman"
	st := &cacheIPState{bridge: container && perr == nil && netip.MustParsePrefix("172.16.0.0/12").Contains(primary) && !docker0}
	for _, s := range l.CacheIPv6 {
		if ip, err := netip.ParseAddr(s); err == nil && netutil.IsULA(ip) {
			st.v6 = append(st.v6, ip)
		}
	}
	if len(l.CacheIPv4) > 0 {
		for _, s := range l.CacheIPv4 {
			if ip, err := netip.ParseAddr(s); err == nil && netutil.IsRFC1918(ip) {
				st.v4 = append(st.v4, ip.Unmap())
			}
		}
		if len(st.v4) == 0 {
			st.warning = "none of the configured cache IPv4 addresses is a private (RFC 1918) address"
		}
		return st
	}
	st.auto = true
	if st.bridge {
		st.warning = "PiCache runs in a container bridge network, so its own address (" + primary.String() +
			") is not reachable by clients; set the host's LAN IPv4 address as cache IP (download cache settings)"
		return st
	}
	if perr == nil && netutil.IsRFC1918(primary) {
		st.v4 = []netip.Addr{primary}
		return st
	}
	if len(private) > 0 {
		st.v4 = []netip.Addr{private[0].ip}
		if perr == nil {
			st.warning = fmt.Sprintf("%s; answering with %s (%s); set the cache IP explicitly if clients cannot reach it",
				describeNonPrivate(primary), private[0].ip, private[0].iface)
		}
		return st
	}
	st.warning = "no private (RFC 1918) IPv4 address found on this machine; set the cache IP in the download cache settings"
	if perr == nil {
		st.warning = describeNonPrivate(primary) + " and " + st.warning
	}
	return st
}

// updateCacheIPs recomputes the cache IPs from the current settings.
func (s *Server) updateCacheIPs(set *settings.All) {
	s.cacheIPs.Store(computeCacheIPs(&set.DownloadCache, s.env))
}

// downloadCacheReady reports whether the download cache DNS answers may be
// given and why not.
func (s *Server) downloadCacheReady(set *settings.All) (bool, string) {
	if !set.DownloadCache.Enabled {
		return false, "the download cache is disabled"
	}
	if s.d.DownloadCacheReady != nil {
		if ok, why := s.d.DownloadCacheReady(); !ok {
			if why == "" {
				why = "the cache is not ready"
			}
			return false, why
		}
	}
	if st := s.cacheIPs.Load(); len(st.v4) == 0 {
		return false, st.warning
	}
	return true, ""
}

// CacheIPs returns the addresses of the download cache DNS answers
// (auto-detection is recomputed every minute). While the download cache
// is disabled it reports the addresses and warnings that would apply once it
// is enabled.
func (s *Server) CacheIPs() CacheIPStatus {
	st := s.cacheIPs.Load()
	out := CacheIPStatus{IPv4: addrStrings(st.v4), IPv6: addrStrings(st.v6), Auto: st.auto, Warning: st.warning}
	out.Ready, out.Reason = s.downloadCacheReady(s.d.Settings.Get())
	if out.Ready {
		out.Reason = st.warning // compatibility: warnings were reported as the reason while ready
	}
	return out
}

// downloadCacheOverride answers the names of download services with the
// cache IPs (step 9): A → cache IPv4s (rotated), AAAA → configured ULAs or
// NODATA, every other type → NODATA. Before an override is answered, the
// user block rules are checked (step 8), so a group can still block a
// download service name.
// Only override candidates are checked here: every other name gets its
// verdict from Filter.Check (step 11) with the full precedence of
// ARCHITECTURE 7.2, where list allow entries beat user regex denies.
func (s *Server) downloadCacheOverride(qc *qctx) (result, bool) {
	l := &qc.set.DownloadCache
	if !l.Enabled || s.d.Services == nil {
		return result{}, false
	}
	svc, ok := s.d.Services.MatchDNS(qc.qname)
	if !ok {
		return result{}, false
	}
	if qc.id.DownloadCacheBypass {
		qc.note("download service " + svc + " matched, but this client bypasses the download cache")
		return result{}, false
	}
	if ready, why := s.downloadCacheReady(qc.set); !ready {
		qc.note("download service " + svc + " matched, but the download cache DNS answers are inactive: " + why)
		return result{}, false
	}
	if qc.blocking && s.d.Filter != nil { // 8
		if d := s.d.Filter.CheckRules(qc.qname, qc.qtype, qc.groups); d.Blocked() {
			qc.note("download service " + svc + " matched, but a user rule blocks it")
			return s.blocked(qc, d, statusFor(d)), true
		}
	}
	st := s.cacheIPs.Load()
	m := newReply(qc.req)
	m.Authoritative = true
	ttl := l.DNSTTL
	var ips []netip.Addr
	switch qc.qtype {
	case dns.TypeA:
		ips = s.rotated(st.v4)
	case dns.TypeAAAA:
		ips = s.rotated(st.v6)
	}
	for _, ip := range ips {
		if ip.Is4() {
			m.Answer = append(m.Answer, &dns.A{Hdr: rrHeader(qc.q.Name, dns.TypeA, ttl), A: ip.AsSlice()})
		} else {
			m.Answer = append(m.Answer, &dns.AAAA{Hdr: rrHeader(qc.q.Name, dns.TypeAAAA, ttl), AAAA: ip.AsSlice()})
		}
	}
	if len(m.Answer) == 0 {
		m.Ns = []dns.RR{syntheticSOA(qc.q.Name, ttl)}
	}
	qc.note("download cache answer for service " + svc)
	return result{msg: m, status: StatusOverride, service: svc}, true
}

// rotated returns ips starting at a rotating offset (round robin).
func (s *Server) rotated(ips []netip.Addr) []netip.Addr {
	if len(ips) < 2 {
		return ips
	}
	off := int(s.rotate.Add(1) % uint32(len(ips)))
	return append(append(make([]netip.Addr, 0, len(ips)), ips[off:]...), ips[:off]...)
}

func addrStrings(ips []netip.Addr) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}
