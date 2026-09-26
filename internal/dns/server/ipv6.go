package dnsserver

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/dns/filter"
)

// ReasonAAAADisabled is the query-log reason of AAAA queries answered with
// NODATA because dns.disableAAAA is on; ReasonDNS64 marks synthesised
// answers (ARCHITECTURE 7.5).
const (
	ReasonAAAADisabled = "aaaa-disabled"
	ReasonDNS64        = "dns64"
)

// dns64Excluded are the IPv4 ranges never embedded in synthesised AAAA
// records (RFC 6147 5.1.4).
var dns64Excluded = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("255.255.255.255/32"),
}

// aaaaDisabled answers an AAAA query that reached the forwarding stage with
// NOERROR/NODATA while dns.disableAAAA is on (step 12a).
func (s *Server) aaaaDisabled(qc *qctx) (result, bool) {
	if qc.qtype != dns.TypeAAAA || !qc.set.DNS.DisableAAAA {
		return result{}, false
	}
	qc.note("IPv6 addresses are not answered (dns.disableAAAA): NODATA")
	return s.negative(qc, dns.RcodeSuccess, StatusSpecial, ReasonAAAADisabled), true
}

// forwardedStatus reports whether a result is an upstream answer.
func forwardedStatus(status string) bool {
	return status == StatusForwarded || status == StatusCached || status == StatusStale
}

// stripIPv6Hints removes the ipv6hint parameter from forwarded HTTPS and
// SVCB answers while dns.disableAAAA is on (step 14a); the reply is no
// longer the signed data, so AD is cleared when something was removed.
func stripIPv6Hints(qc *qctx, r *result) {
	if !qc.set.DNS.DisableAAAA || r.msg == nil || !forwardedStatus(r.status) {
		return
	}
	removed := false
	for _, rr := range r.msg.Answer {
		var svcb *dns.SVCB
		switch v := rr.(type) {
		case *dns.HTTPS:
			svcb = &v.SVCB
		case *dns.SVCB:
			svcb = v
		default:
			continue
		}
		n := len(svcb.Value)
		svcb.Value = slices.DeleteFunc(svcb.Value, func(kv dns.SVCBKeyValue) bool { return kv.Key() == dns.SVCB_IPV6HINT })
		removed = removed || len(svcb.Value) != n
	}
	if removed {
		qc.note("ipv6hint removed from the answer (dns.disableAAAA)")
		r.msg.AuthenticatedData = false
	}
}

// synthesizeAAAA answers an allowed AAAA query whose forwarded answer has no
// AAAA record (NOERROR/NODATA, also at the end of a CNAME chain) with AAAA
// records synthesised from the A records of the final name (DNS64, RFC
// 6147; step 14b). The A records are resolved through the same upstreams
// (via, ips) and embedded in the /96 prefix; excluded ranges and names that
// are blocked for the client are never synthesised. TTL: the A record's,
// at most the negative TTL of the AAAA answer's SOA. The answer never
// carries AD; the status stays, the reason becomes "dns64".
func (s *Server) synthesizeAAAA(qc *qctx, r *result, via []string, ips []netip.Addr) {
	d := &qc.set.DNS
	if !d.DNS64.Enabled || qc.qtype != dns.TypeAAAA || r.msg == nil || r.msg.Rcode != dns.RcodeSuccess || !forwardedStatus(r.status) {
		return
	}
	prefix := d.DNS64Prefix()
	if !prefix.IsValid() || prefix.Bits() != 96 {
		return
	}
	final, chain := answerChain(r.msg.Answer, qc.q.Name)
	if slices.ContainsFunc(r.msg.Answer, func(rr dns.RR) bool {
		return rr.Header().Rrtype == dns.TypeAAAA && chain[strings.ToLower(rr.Header().Name)]
	}) {
		return // existing AAAA records are answered as they are
	}
	if normalizeName(final) != qc.qname && s.dns64Blocked(qc, normalizeName(final), true) {
		qc.note("DNS64: " + normalizeName(final) + " is blocked: no synthesis")
		return
	}
	resp, info, err := s.exchange(qc, dns.Question{Name: final, Qtype: dns.TypeA, Qclass: dns.ClassINET}, via, ips)
	if err != nil || resp.Rcode != dns.RcodeSuccess || (len(via) == 0 && info.Block != nil) {
		qc.note("DNS64: no A records of " + normalizeName(final) + ": NODATA")
		return
	}
	maxTTL := ^uint32(0)
	for _, rr := range r.msg.Ns {
		if soa, ok := rr.(*dns.SOA); ok {
			maxTTL = min(soa.Hdr.Ttl, soa.Minttl)
		}
	}
	base := prefix.Addr().As16()
	var add []dns.RR
	synthesised := 0
	for _, rr := range resp.Answer {
		switch v := rr.(type) {
		case *dns.CNAME:
			if s.dns64Blocked(qc, normalizeName(v.Target), qc.set.Filter.CNAMEInspection) {
				qc.note("DNS64: CNAME target " + normalizeName(v.Target) + " is blocked: no synthesis")
				return
			}
			if !slices.ContainsFunc(r.msg.Answer, func(x dns.RR) bool { return dns.IsDuplicate(x, v) }) {
				add = append(add, v) // the A answer continues the chain
			}
		case *dns.A:
			v4, ok := netip.AddrFromSlice(v.A.To4())
			if !ok || slices.ContainsFunc(dns64Excluded, func(p netip.Prefix) bool { return p.Contains(v4) }) {
				continue
			}
			b := base
			copy(b[12:], v4.AsSlice())
			add = append(add, &dns.AAAA{Hdr: rrHeader(v.Hdr.Name, dns.TypeAAAA, min(v.Hdr.Ttl, maxTTL)), AAAA: netip.AddrFrom16(b).AsSlice()})
			synthesised++
		}
	}
	if synthesised == 0 {
		qc.note("DNS64: no usable A records of " + normalizeName(final) + ": NODATA")
		return
	}
	if qc.tracing() {
		qc.note(fmt.Sprintf("DNS64: %d AAAA records synthesised in %s", synthesised, prefix))
	}
	r.msg.Answer = append(r.msg.Answer, add...)
	r.msg.Ns = nil
	r.msg.AuthenticatedData = false
	r.reason = ReasonDNS64
}

// dns64Blocked reports whether name would be blocked for the client (never
// while blocking is off or the query name is allowlisted, like step 14).
// check=false skips the check.
func (s *Server) dns64Blocked(qc *qctx, name string, check bool) bool {
	if !check || !qc.blocking || s.d.Filter == nil || qc.dec.Action == filter.ActionAllow {
		return false
	}
	return s.d.Filter.Check(name, qc.qtype, qc.groups).Blocked()
}

// answerChain follows the CNAME chain of owner through rrs (at most
// maxCNAMEHops) and returns its last name and the set of lower-case names
// on it.
func answerChain(rrs []dns.RR, owner string) (string, map[string]bool) {
	chain := map[string]bool{strings.ToLower(owner): true}
	name := owner
	for range maxCNAMEHops {
		next := ""
		for _, rr := range rrs {
			if c, ok := rr.(*dns.CNAME); ok && strings.EqualFold(c.Hdr.Name, name) {
				next = c.Target
				break
			}
		}
		if next == "" || chain[strings.ToLower(next)] {
			break
		}
		chain[strings.ToLower(next)] = true
		name = next
	}
	return name, chain
}

// dns64PTR answers PTR queries for addresses inside the DNS64 prefix
// (step 5a): the query is rewritten to the in-addr.arpa name of the
// embedded IPv4 address, answered by the normal pipeline, and the answer is
// renamed back. The answer never carries AD.
func (s *Server) dns64PTR(qc *qctx) (result, bool) {
	d := &qc.set.DNS
	if !d.DNS64.Enabled || qc.qtype != dns.TypePTR {
		return result{}, false
	}
	prefix := d.DNS64Prefix()
	ip, ok := parseReverse(qc.qname)
	if !ok || !ip.Is6() || !prefix.IsValid() || prefix.Bits() != 96 || !prefix.Contains(ip) {
		return result{}, false
	}
	b := ip.As16()
	v4 := netip.AddrFrom4([4]byte(b[12:16]))
	name := reverseName(v4)
	qc.note("DNS64: PTR of the embedded IPv4 address " + v4.String() + " (" + name + ")")
	req := qc.req.Copy()
	req.Question = []dns.Question{{Name: fqdn(name), Qtype: dns.TypePTR, Qclass: dns.ClassINET}}
	sub := *qc
	sub.req, sub.q, sub.qname, sub.dec, sub.decided = req, req.Question[0], name, filter.Decision{}, false
	r := s.process(&sub)
	if r.msg == nil {
		return r, true
	}
	for _, rr := range append(slices.Clone(r.msg.Answer), r.msg.Ns...) {
		if h := rr.Header(); strings.EqualFold(h.Name, sub.q.Name) {
			h.Name = qc.q.Name
		}
	}
	r.msg.Question = []dns.Question{qc.q}
	r.msg.AuthenticatedData = false
	if r.reason == "" {
		r.reason = ReasonDNS64
	}
	return r, true
}
