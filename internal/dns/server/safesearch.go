package dnsserver

import (
	"fmt"
	"strings"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/dns/filter"
)

// safeSearchTTL caps the TTL of the CNAME to a restricted host, and is the
// TTL of the synthetic SOA of HTTPS, SVCB and ANY answers.
const safeSearchTTL uint32 = 300

// safeSearch rewrites the names of search engines to their restricted
// hosts for the client's groups (step 7c; parental.Engine.SafeSearch). It
// runs after the parental controls and the dropped domains (a blocked
// service and a dropped domain win) and whether blocking is enabled,
// disabled or paused; the allow override does not lift it. While blocking
// is active for the query, a block of the name for the client (user deny
// rule, list, regex) wins: the query takes the normal path and gets the
// blocking reply. Allow rules do not lift safe search.
//
// A and AAAA: the CNAME plus the target's answer, which runs steps 12a–14c
// for the target with the client's context (forwarders, the default set
// with its client subnet, upstream blocks, bogus NXDOMAIN, DNS64, rebind
// protection) but is not filtered. CNAME: the CNAME alone. HTTPS, SVCB and
// ANY: NODATA, so clients get no ECH or address hints of the unrestricted
// host. Every other type takes the normal path (mail and validating
// resolvers keep working). The combined answer is never cached; only the
// target's own answer is, under the target's key. A failed target lookup
// is SERVFAIL (status error): the unrestricted answer is never returned.
func (s *Server) safeSearch(qc *qctx) (result, bool) {
	if s.d.Parental == nil {
		return result{}, false
	}
	switch qc.qtype {
	case dns.TypeA, dns.TypeAAAA, dns.TypeCNAME, dns.TypeHTTPS, dns.TypeSVCB, dns.TypeANY:
	default:
		return result{}, false
	}
	rw, ok := s.d.Parental.SafeSearch(qc.qname, qc.id.GroupIDs, qc.start)
	if !ok {
		return result{}, false
	}
	if qc.blocking && s.d.Filter != nil {
		qc.dec, qc.decided = s.d.Filter.Check(qc.qname, qc.qtype, qc.groups), true
		if qc.dec.Blocked() {
			qc.note("safe search: not applied, " + qc.qname + " is blocked for the client")
			return result{}, false
		}
	}
	qc.note(fmt.Sprintf("safe search: %s → CNAME %s (%s)", qc.qname, rw.Target, rw.Group))
	target := fqdn(rw.Target)
	m := newReply(qc.req)
	res := result{msg: m, status: StatusSafeSearch, reason: rw.Reason()}
	cname := func(ttl uint32) dns.RR {
		return &dns.CNAME{Hdr: rrHeader(qc.q.Name, dns.TypeCNAME, ttl), Target: target}
	}
	switch qc.qtype {
	case dns.TypeCNAME:
		m.Answer = []dns.RR{cname(safeSearchTTL)}
		return res, true
	case dns.TypeHTTPS, dns.TypeSVCB, dns.TypeANY:
		m.Ns = []dns.RR{syntheticSOA(qc.q.Name, safeSearchTTL)}
		return res, true
	}

	sub := *qc
	sub.req = qc.req.Copy()
	sub.req.Question = []dns.Question{{Name: target, Qtype: qc.qtype, Qclass: dns.ClassINET}}
	sub.q, sub.qname = sub.req.Question[0], rw.Target
	// The target is not filtered: steps 11, 14, 14d and the blocked-name check
	// of 14b depend on an active blocking.
	sub.blocking, sub.dec, sub.decided = false, filter.Decision{}, false
	r := s.forward(&sub)
	switch {
	case r.status == StatusError:
		fail := s.servfail(qc, "safe search: "+r.reason)
		fail.upstream, fail.ede = r.upstream, r.ede
		return fail, true
	case r.msg == nil:
		return s.servfail(qc, "safe search: no answer for "+rw.Target), true
	case forwardedStatus(r.status) && r.msg.Rcode != dns.RcodeSuccess && r.msg.Rcode != dns.RcodeNameError:
		qc.note("safe search: the target " + rw.Target + " answered " + rcodeString(r.msg.Rcode) + ": SERVFAIL")
		fail := s.servfail(qc, "safe search: "+rw.Target+" answered "+rcodeString(r.msg.Rcode))
		fail.upstream, fail.ede = r.upstream, r.ede
		return fail, true
	case !forwardedStatus(r.status) && r.reason != ReasonAAAADisabled:
		// 13a, 13b or 14c turned the target's answer into a blocking or a
		// bogus-NXDOMAIN reply: that reply and its status stand.
		retarget(r.msg, sub.q.Name, qc.q)
		return r, true
	}
	ttl := safeSearchTTL
	for _, rr := range r.msg.Answer {
		ttl = min(ttl, rr.Header().Ttl)
	}
	if len(r.msg.Answer) == 0 {
		for _, rr := range r.msg.Ns {
			if soa, ok := rr.(*dns.SOA); ok {
				ttl = min(ttl, soa.Hdr.Ttl, soa.Minttl)
			}
		}
		m.Ns = r.msg.Ns // NXDOMAIN or NODATA keep the target's SOA
	}
	m.Rcode = r.msg.Rcode
	m.Answer = append([]dns.RR{cname(ttl)}, r.msg.Answer...)
	m.AuthenticatedData = false
	res.upstream, res.ede = r.upstream, r.ede
	return res, true
}

// retarget turns a reply to a question about from into a reply to q: the
// records owned by from (case insensitive) get q's name and the question
// becomes q.
func retarget(m *dns.Msg, from string, q dns.Question) {
	for _, rrs := range [][]dns.RR{m.Answer, m.Ns} {
		for _, rr := range rrs {
			if h := rr.Header(); strings.EqualFold(h.Name, from) {
				h.Name = q.Name
			}
		}
	}
	m.Question = []dns.Question{q}
	m.AuthenticatedData = false
}
