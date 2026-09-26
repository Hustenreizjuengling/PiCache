package dnsserver

import (
	"net/netip"
	"strings"

	"github.com/miekg/dns"
)

// singleLabelType reports the query types of single-label names that step
// 11a answers from the local domain; every other type (NS, DS, DNSKEY,
// SOA, …) takes the normal path, so validating resolvers behind PiCache
// keep working.
func singleLabelType(t uint16) bool {
	switch t {
	case dns.TypeA, dns.TypeAAAA, dns.TypeHTTPS, dns.TypeSVCB, dns.TypeANY:
		return true
	}
	return false
}

// singleLabelQuery reports a query for a single-label name (not the root)
// of one of the types of singleLabelType.
func singleLabelQuery(qc *qctx) bool {
	return qc.qname != "" && !strings.Contains(qc.qname, ".") && singleLabelType(qc.qtype)
}

// singleLabel answers single-label names while dns.domainNeeded is on
// (step 11a): as <name>.<localDomain> from the local records and the DHCP
// lease names, the most specific forwarder with explicit targets, the
// router resolver (an NXDOMAIN or a failure counts as no answer, NODATA is
// an answer), then the (unqualified) forwarder, else NXDOMAIN. wpad and
// isatap are answered from local records only. Answers are returned with
// the queried owner name for the records of <name>.<localDomain>.
func (s *Server) singleLabel(qc *qctx) (result, bool) {
	if !qc.set.DNS.DomainNeeded || !singleLabelQuery(qc) {
		return result{}, false
	}
	name := qc.qname
	recordsOnly := name == "wpad" || name == "isatap"
	if ld := qc.set.DNS.LocalDomain; ld != "" {
		full := name + "." + ld
		if r, what, ok := s.singleLabelLocal(qc, full, recordsOnly); ok { // 1
			qc.note("single-label: answered as " + full + " by " + what)
			return r, true
		}
		if !recordsOnly {
			if f := s.fwd.Load().match(full, true); f != nil { // 2
				if r, ok := s.singleLabelVia(qc, full, f.upstreams, f.ips); ok {
					qc.note("single-label: answered as " + full + " by forwarder " + f.domain)
					return r, true
				}
			}
			if ip, ok := s.routerAddr(); ok { // 3
				if r, ok := s.singleLabelVia(qc, full, []string{routerUpstream(ip)}, s.routerAddrs()); ok {
					qc.note("single-label: answered as " + full + " by router")
					return r, true
				}
			}
		}
	}
	if recordsOnly {
		qc.note("single-label: " + name + " only from local records")
	} else if f := s.fwd.Load().unqualified; f != nil && !f.def { // 4
		qc.note("single-label: (unqualified) forwarder")
		r := s.resolveVia(qc, f.upstreams, f.ips, "conditional forwarder "+Unqualified)
		if forwardedStatus(r.status) {
			r.reason = ReasonSingleLabel
		}
		return r, true
	}
	qc.note("single-label: not forwarded (dns.domainNeeded), NXDOMAIN") // 5
	return s.negative(qc, dns.RcodeNameError, StatusSpecial, ReasonSingleLabel), true
}

// singleLabelLocal answers full (<name>.<localDomain>) from the local
// records and, unless recordsOnly, the DHCP lease names; what names the
// source for the trace.
func (s *Server) singleLabelLocal(qc *qctx, full string, recordsOnly bool) (result, string, bool) {
	what := "local record"
	if _, ok := s.zone.Load().lookup(full); !ok {
		if recordsOnly || s.d.Leases == nil {
			return result{}, "", false
		}
		what = "DHCP lease name"
	}
	sub := *qc
	sub.req = qc.req.Copy()
	sub.req.Question = []dns.Question{{Name: fqdn(full), Qtype: qc.qtype, Qclass: qc.q.Qclass}}
	sub.q, sub.qname, sub.recordsOnly = sub.req.Question[0], full, recordsOnly
	r, ok := s.localAnswer(&sub)
	if !ok || r.msg == nil {
		return result{}, "", false
	}
	renameOwner(r.msg, sub.q.Name, qc.q)
	r.reason = ReasonSingleLabel
	return r, what, true
}

// singleLabelVia asks a forwarder or the router for full; an NXDOMAIN, an
// rcode other than NOERROR or a failure is no answer.
func (s *Server) singleLabelVia(qc *qctx, full string, via []string, ips []netip.Addr) (result, bool) {
	q := dns.Question{Name: fqdn(full), Qtype: qc.qtype, Qclass: dns.ClassINET}
	resp, info, err := s.exchange(qc, q, via, ips)
	if err != nil || resp.Rcode != dns.RcodeSuccess {
		return result{}, false
	}
	renameOwner(resp, q.Name, qc.q)
	status := StatusForwarded
	switch {
	case info.Stale:
		status = StatusStale
	case info.Cached:
		status = StatusCached
	}
	return result{msg: resp, status: status, reason: ReasonSingleLabel, upstream: info.Upstream, ede: info.EDE}, true
}

// renameOwner gives the records of m's answer section owned by from (case
// insensitive) the name of the client's question and sets the question;
// CNAME targets and their records keep their names, the authority section
// is kept.
func renameOwner(m *dns.Msg, from string, q dns.Question) {
	for _, rr := range m.Answer {
		if h := rr.Header(); strings.EqualFold(h.Name, from) {
			h.Name = q.Name
		}
	}
	m.Question = []dns.Question{q}
}
