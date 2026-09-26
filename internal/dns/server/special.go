package dnsserver

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// specialTTL is the TTL of locally generated special-use answers.
const specialTTL uint32 = 60

// localhostV4 and localhostV6 answer localhost and health probes (read-only).
var (
	localhostV4 = []netip.Addr{netip.AddrFrom4([4]byte{127, 0, 0, 1})}
	localhostV6 = []netip.Addr{netip.IPv6Loopback()}
)

// serverName reports whether name is one of this server's names
// (dns.serverNames, also below the local domain).
func serverName(set *settings.All, name string) bool {
	ld := set.DNS.LocalDomain
	for _, n := range set.DNS.ServerNames {
		if name == n || (ld != "" && name == n+"."+ld) {
			return true
		}
	}
	return false
}

// ownPTRName is the PTR target for this server's own addresses.
func ownPTRName(set *settings.All) (string, bool) {
	if len(set.DNS.ServerNames) == 0 {
		return "", false
	}
	n := set.DNS.ServerNames[0]
	if !strings.Contains(n, ".") && set.DNS.LocalDomain != "" {
		n += "." + set.DNS.LocalDomain
	}
	return n, true
}

// specialUse handles special-use names (ARCHITECTURE 7.1 step 6): they are
// answered locally, never forwarded to the default upstreams and exempt
// from blocking.
func (s *Server) specialUse(qc *qctx) (result, bool) {
	name := qc.qname
	if v4, v6, what, ok := s.specialAddrs(qc, name); ok {
		if len(v4) == 0 && len(v6) == 0 {
			qc.note("special-use name " + what + ": NODATA")
			return s.negative(qc, dns.RcodeSuccess, StatusSpecial, what), true
		}
		qc.note("special-use name: " + what)
		return s.addrAnswer(qc, v4, v6), true
	}
	if zone, ok := s.privateReverseZone(name); ok {
		return s.reverseZone(qc, zone), true
	}
	if zone, router, ok := s.localZone(qc.set, name); ok {
		return s.localZoneAnswer(qc, zone, router), true
	}
	return result{}, false
}

// specialAddrs returns the local answer addresses of the special-use names
// that are answered from this machine (localhost, this server's names,
// resolver.arpa = no addresses) for qc's client. ok is false for other names.
func (s *Server) specialAddrs(qc *qctx, name string) (v4, v6 []netip.Addr, what string, ok bool) {
	switch {
	case inZone(name, "localhost"):
		return localhostV4, localhostV6, "localhost", true
	case serverName(qc.set, name):
		if st := s.cacheIPs.Load(); st.bridge && !qc.source.IsLoopback() {
			// Bridge addresses are unreachable for clients: answer with the
			// configured cache addresses (NODATA until they are set).
			return st.v4, st.v6, "this server's own name", true
		}
		h := s.host.Load()
		return h.addrsFor(qc.source, false), h.addrsFor(qc.source, true), "this server's own name", true
	case inZone(name, "resolver.arpa"):
		return nil, nil, "resolver.arpa", true
	}
	return nil, nil, "", false
}

// addrAnswer answers A/AAAA with the given addresses (NODATA for other
// types or when no address of the family exists).
func (s *Server) addrAnswer(qc *qctx, v4, v6 []netip.Addr) result {
	m := newReply(qc.req)
	m.Authoritative = true
	var ips []netip.Addr
	switch qc.qtype {
	case dns.TypeA:
		ips = v4
	case dns.TypeAAAA:
		ips = v6
	}
	m.Answer = addrRRs(qc.q.Name, ips, specialTTL)
	if len(m.Answer) == 0 {
		m.Ns = []dns.RR{syntheticSOA(qc.q.Name, specialTTL)}
	}
	return result{msg: m, status: StatusSpecial}
}

// addrRRs returns A/AAAA records for ips owned by owner.
func addrRRs(owner string, ips []netip.Addr, ttl uint32) []dns.RR {
	var out []dns.RR
	for _, ip := range ips {
		if ip.Is4() {
			out = append(out, &dns.A{Hdr: rrHeader(owner, dns.TypeA, ttl), A: ip.AsSlice()})
		} else {
			out = append(out, &dns.AAAA{Hdr: rrHeader(owner, dns.TypeAAAA, ttl), AAAA: ip.AsSlice()})
		}
	}
	return out
}

// negative returns an authoritative NXDOMAIN or NODATA with the synthetic SOA.
func (s *Server) negative(qc *qctx, rcode int, status, reason string) result {
	m := newReply(qc.req)
	m.Authoritative = true
	m.Rcode = rcode
	m.Ns = []dns.RR{syntheticSOA(qc.q.Name, specialTTL)}
	return result{msg: m, status: status, reason: reason}
}

// reverseZone answers names in locally served reverse zones: local records
// (auto-PTR), this server's addresses, the most specific conditional
// forwarder, dns.localPtrUpstreams, the router resolver, else NXDOMAIN.
// Nothing here reaches the default upstreams.
func (s *Server) reverseZone(qc *qctx, zone string) result {
	if qc.tracing() {
		qc.note(fmt.Sprintf("private reverse zone %s: never sent to public upstreams", zone))
	}
	if r, ok := s.localAnswer(qc); ok {
		return r
	}
	if qc.qtype == dns.TypePTR {
		if ip, ok := parseReverse(qc.qname); ok && s.host.Load().isOwn(ip) {
			if name, ok := ownPTRName(qc.set); ok {
				qc.note("address of this server")
				m := newReply(qc.req)
				m.Authoritative = true
				m.Answer = []dns.RR{&dns.PTR{Hdr: rrHeader(qc.q.Name, dns.TypePTR, specialTTL), Ptr: fqdn(name)}}
				return result{msg: m, status: StatusSpecial}
			}
		}
	}
	if f := s.fwd.Load().match(qc.qname, true); f != nil {
		return s.resolveVia(qc, f.upstreams, f.ips, "conditional forwarder "+f.domain)
	}
	if ups := qc.set.DNS.LocalPTRUpstreams; len(ups) > 0 {
		return s.resolveVia(qc, ups, upstreamIPs(ups), "local PTR upstreams")
	}
	if ip, ok := s.routerAddr(); ok {
		return s.resolveVia(qc, []string{routerUpstream(ip)}, s.routerAddrs(), "router resolver")
	}
	qc.note("no local resolver for this reverse zone: NXDOMAIN")
	return s.negative(qc, dns.RcodeNameError, StatusSpecial, zone)
}

// localZone returns the most specific special-use or local zone containing
// name and whether the router resolver may answer it. The local domain,
// home.arpa and the search domains may be below a special-use zone (e.g.
// "home.internal", "corp.local") and then win, so their names still reach
// the router resolver; a local domain equal to a special-use zone (e.g.
// "local") wins too. Names below "onion" and "invalid" are never resolved,
// whatever is configured.
func (s *Server) localZone(set *settings.All, name string) (zone string, router, ok bool) {
	consider := func(z string, r bool) {
		if z == "" || !inZone(name, z) || (r && neverResolvedZone(z)) {
			return
		}
		// All candidates contain name, so a longer zone is a more specific one.
		if !ok || len(z) > len(zone) || (len(z) == len(zone) && r) {
			zone, router, ok = z, r, true
		}
	}
	for _, z := range specialZones {
		consider(z, false)
	}
	consider("home.arpa", true)
	consider(set.DNS.LocalDomain, true)
	for _, d := range s.host.Load().search {
		consider(d, true)
	}
	return zone, router, ok
}

// neverResolvedZone reports zones whose names must never be sent to any
// resolver (RFC 6761 invalid, RFC 7686 onion).
func neverResolvedZone(z string) bool { return inZone(z, "invalid") || inZone(z, "onion") }

// localZoneAnswer answers names in special-use and local zones: local
// records and forwarders first, then (if allowed) the router resolver,
// otherwise NXDOMAIN.
func (s *Server) localZoneAnswer(qc *qctx, zone string, router bool) result {
	if qc.tracing() {
		qc.note(fmt.Sprintf("local zone %s: never sent to the default upstreams", zone))
	}
	if r, ok := s.localAnswer(qc); ok {
		return r
	}
	if f := s.fwd.Load().match(qc.qname, true); f != nil {
		return s.resolveVia(qc, f.upstreams, f.ips, "conditional forwarder "+f.domain)
	}
	if router {
		if ip, ok := s.routerAddr(); ok {
			return s.resolveVia(qc, []string{routerUpstream(ip)}, s.routerAddrs(), "router resolver")
		}
	}
	qc.note("no local data or resolver for this name: NXDOMAIN")
	return s.negative(qc, dns.RcodeNameError, StatusSpecial, zone)
}

// routeName resolves a name on behalf of qc without filtering (CNAME targets
// of local records): conditional forwarder, local zones (router resolver
// only), otherwise the default upstreams. Forwarders are matched like the
// pipeline does: private and local names skip those with the target
// default as if absent (step 6); for other names such a forwarder means
// the default upstreams (step 12), never a less specific forwarder. A nil
// reply means the name has no resolver (local zone without router).
func (s *Server) routeName(qc *qctx, name string, q dns.Question) (*dns.Msg, upstream.Info, error) {
	_, private := s.privateReverseZone(name)
	_, router, local := s.localZone(qc.set, name)
	special := inZone(name, "localhost") || inZone(name, "resolver.arpa") || serverName(qc.set, name)
	if f := s.fwd.Load().match(name, private || local || special); f != nil && !f.def {
		return s.exchange(qc, q, f.upstreams, f.ips)
	}
	switch {
	case private && len(qc.set.DNS.LocalPTRUpstreams) > 0:
		ups := qc.set.DNS.LocalPTRUpstreams
		return s.exchange(qc, q, ups, upstreamIPs(ups))
	case private || router:
		if ip, ok := s.routerAddr(); ok {
			return s.exchange(qc, q, []string{routerUpstream(ip)}, s.routerAddrs())
		}
		return nil, upstream.Info{}, nil
	case local || special:
		return nil, upstream.Info{}, nil
	}
	return s.exchange(qc, q, nil, nil)
}
