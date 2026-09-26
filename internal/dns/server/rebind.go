package dnsserver

import (
	"net"
	"net/netip"
	"slices"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// Query-log reasons of steps 11a, 13b and of fallback answers.
const (
	ReasonSingleLabel   = "single-label"
	ReasonBogusNXDomain = "bogus-nxdomain"
	ReasonFallback      = "fallback"
)

// upstreamBlock turns an answer the default upstreams blocked themselves
// (upstream.Info.Block, classified at fetch time) into the blocking reply
// (step 13a): status blocked-upstream, reason "<host>: <kind>" (the host
// only, never a DoH path). Clients get the EDE text "blocked by upstream
// (<kind>)": a host name can carry an account or profile ID. It applies
// whether or not blocking is active and for allowlisted names: the
// upstream answered nothing usable.
func (s *Server) upstreamBlock(qc *qctx, r *result) {
	if r.block == nil || !r.def || !forwardedStatus(r.status) {
		return
	}
	reason := r.block.Reason()
	qc.note("upstream block: " + reason)
	upstreamName, ede := r.upstream, r.ede
	*r = result{msg: s.globalReply(qc), status: StatusBlockedUpstream, reason: reason, blocked: true,
		edeText: r.block.ClientText(), upstream: upstreamName, ede: ede, def: true}
}

// bogusNXDomain answers NXDOMAIN (step 13b) when an A or AAAA record of a
// default-set answer lies in dns.bogusNxdomain: status special, reason
// bogus-nxdomain, synthetic SOA with filter.blockedTtl. It applies whether
// or not blocking is active.
func (s *Server) bogusNXDomain(qc *qctx, r *result) {
	if !r.def || !forwardedStatus(r.status) || r.msg == nil {
		return
	}
	bogus := s.lists.Load().bogus
	if len(bogus) == 0 {
		return
	}
	for _, rr := range r.msg.Answer {
		ip, ok := rrAddr(rr)
		if !ok || !slices.ContainsFunc(bogus, func(p netip.Prefix) bool { return p.Contains(netutil.Canon(ip)) }) {
			continue
		}
		qc.note("bogus NXDOMAIN: " + ip.String() + " is in dns.bogusNxdomain")
		ttl := qc.set.Filter.BlockedTTL
		m := newReply(qc.req)
		m.Rcode = dns.RcodeNameError
		m.Ns = []dns.RR{syntheticSOA(qc.q.Name, ttl)}
		*r = result{msg: m, status: StatusSpecial, reason: ReasonBogusNXDomain, upstream: r.upstream, ede: r.ede, def: true}
		return
	}
}

// rebindCheck protects against DNS rebinding (step 14c, dns.rebindProtection;
// default-set answers only, whether or not blocking is active): an A or
// AAAA record in the answer that points at a rebind target
// (netutil.RebindTarget) makes the whole answer the blocking reply (status
// blocked-rebind, reason "rebind: <address>"); otherwise target addresses
// are removed from the ipv4hint/ipv6hint of HTTPS/SVCB records and from the
// additional section (AD cleared when something was removed). Exempt: the
// local domain, home.arpa and the search domains, dns.rebindAllow
// (subtrees), the host names of web.allowedHosts and names a user allow
// rule applies to for the client.
func (s *Server) rebindCheck(qc *qctx, r *result) {
	d := &qc.set.DNS
	if !d.RebindProtection || !r.def || r.msg == nil || !forwardedStatus(r.status) {
		return
	}
	prefix := d.DNS64Prefix()
	target := func(ip netip.Addr) bool { return netutil.RebindTarget(ip, prefix) }
	first := netip.Addr{}
	for _, rr := range r.msg.Answer {
		if ip, ok := rrAddr(rr); ok && target(ip) {
			first = ip
			break
		}
	}
	hints := first.IsValid() || svcbHintTargets(r.msg.Answer, target) || slices.ContainsFunc(r.msg.Extra, func(rr dns.RR) bool {
		ip, ok := rrAddr(rr)
		return ok && target(ip)
	})
	if !hints {
		return
	}
	if why, exempt := s.rebindExempt(qc); exempt {
		qc.note("rebind protection: exempt (" + why + ")")
		return
	}
	if first.IsValid() {
		qc.note("rebind protection: " + netutil.Canon(first).String() + " blocked")
		upstreamName, ede := r.upstream, r.ede
		*r = result{msg: s.globalReply(qc), status: StatusBlockedRebind,
			reason: "rebind: " + netutil.Canon(first).String(), blocked: true, upstream: upstreamName, ede: ede, def: true}
		return
	}
	removed := stripSVCBHints(r.msg.Answer, target)
	extra := slices.DeleteFunc(r.msg.Extra, func(rr dns.RR) bool {
		ip, ok := rrAddr(rr)
		return ok && target(ip)
	})
	if removed || len(extra) != len(r.msg.Extra) {
		qc.note("rebind protection: private addresses removed from the hints and the additional section")
		r.msg.Extra = extra
		r.msg.AuthenticatedData = false
	}
}

// rebindExempt reports whether the query name may resolve to private
// addresses and why.
func (s *Server) rebindExempt(qc *qctx) (string, bool) {
	name := qc.qname
	for _, z := range append([]string{qc.set.DNS.LocalDomain, "home.arpa"}, s.host.Load().search...) {
		if z != "" && inZone(name, z) {
			return "local zone " + z, true
		}
	}
	for _, a := range qc.set.DNS.RebindAllow {
		if inZone(name, a) {
			return "dns.rebindAllow " + a, true
		}
	}
	for _, h := range qc.set.Web.AllowedHosts {
		if _, err := netip.ParseAddr(h); err != nil && normalizeName(h) == name {
			return "web.allowedHosts", true
		}
	}
	if s.d.Filter != nil {
		if d := s.d.Filter.CheckRules(name, qc.qtype, qc.id.GroupIDs); d.Action == filter.ActionAllow {
			return "allow rule " + d.Name, true
		}
	}
	return "", false
}

// svcbHintTargets reports whether an HTTPS/SVCB record of rrs carries a
// hint address that is a target.
func svcbHintTargets(rrs []dns.RR, target func(netip.Addr) bool) bool {
	found := false
	eachSVCB(rrs, func(svcb *dns.SVCB) {
		for _, kv := range svcb.Value {
			for _, ip := range svcbHints(kv) {
				if target(ip) {
					found = true
				}
			}
		}
	})
	return found
}

// stripSVCBHints removes target addresses from the ipv4hint and ipv6hint
// values of HTTPS/SVCB records (a hint left empty is removed); it reports
// whether anything was removed.
func stripSVCBHints(rrs []dns.RR, target func(netip.Addr) bool) bool {
	removed := false
	eachSVCB(rrs, func(svcb *dns.SVCB) {
		out := svcb.Value[:0]
		for _, kv := range svcb.Value {
			switch v := kv.(type) {
			case *dns.SVCBIPv4Hint:
				n := len(v.Hint)
				v.Hint = slices.DeleteFunc(v.Hint, func(ip net.IP) bool { return target(netipFrom(ip)) })
				removed = removed || len(v.Hint) != n
				if len(v.Hint) == 0 {
					continue
				}
			case *dns.SVCBIPv6Hint:
				n := len(v.Hint)
				v.Hint = slices.DeleteFunc(v.Hint, func(ip net.IP) bool { return target(netipFrom(ip)) })
				removed = removed || len(v.Hint) != n
				if len(v.Hint) == 0 {
					continue
				}
			}
			out = append(out, kv)
		}
		svcb.Value = out
	})
	return removed
}

func eachSVCB(rrs []dns.RR, fn func(*dns.SVCB)) {
	for _, rr := range rrs {
		switch v := rr.(type) {
		case *dns.HTTPS:
			fn(&v.SVCB)
		case *dns.SVCB:
			fn(v)
		}
	}
}

// svcbHints returns the addresses of an ipv4hint or ipv6hint value.
func svcbHints(kv dns.SVCBKeyValue) []netip.Addr {
	var raw [][]byte
	switch v := kv.(type) {
	case *dns.SVCBIPv4Hint:
		for _, ip := range v.Hint {
			raw = append(raw, ip)
		}
	case *dns.SVCBIPv6Hint:
		for _, ip := range v.Hint {
			raw = append(raw, ip)
		}
	}
	out := make([]netip.Addr, 0, len(raw))
	for _, b := range raw {
		if ip := netipFrom(b); ip.IsValid() {
			out = append(out, ip)
		}
	}
	return out
}

// netipFrom converts a net.IP (4 or 16 bytes) canonically.
func netipFrom(b []byte) netip.Addr {
	ip, _ := netip.AddrFromSlice(b)
	return netutil.Canon(ip)
}

// rrAddr returns the address of an A or AAAA record.
func rrAddr(rr dns.RR) (netip.Addr, bool) {
	switch v := rr.(type) {
	case *dns.A:
		return netip.AddrFromSlice(v.A.To4())
	case *dns.AAAA:
		return netip.AddrFromSlice(v.AAAA.To16())
	}
	return netip.Addr{}, false
}
