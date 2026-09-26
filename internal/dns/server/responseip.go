package dnsserver

import (
	"fmt"
	"net/netip"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// maxAnswerAddrs bounds the addresses step 14d examines per answer (the
// rest is not examined).
const maxAnswerAddrs = 256

// responseIPCheck blocks an answer by its addresses (step 14d; after 14c):
// while blocking is active for the query, its name is not allowed (step
// 11) and the status is still forwarded, cached or stale, the A and AAAA
// records of the answer and additional sections and the ipv4hint/ipv6hint
// of HTTPS/SVCB records of an answer of the default set or a group set are
// checked (Filter.CheckIP with the filtering groups; forwarder, router and
// local-PTR answers are the admin's own network and never checked). An
// IPv4-mapped address is judged as IPv4, an address that embeds IPv4
// (NAT64, the DNS64 prefix, 6to4, IPv4-compatible) by the IP rules as
// itself and as the embedded IPv4 address, by the lists only as the
// embedded IPv4 address; an address of this machine is never blocked. The
// first blocked address turns the whole answer into the blocking reply of
// the blocking mode (status blocked-ip).
func (s *Server) responseIPCheck(qc *qctx, r *result) {
	if !qc.blocking || s.d.Filter == nil || !r.def || r.msg == nil || !forwardedStatus(r.status) ||
		qc.dec.Action == filter.ActionAllow {
		return
	}
	prefix := qc.set.DNS.DNS64Prefix()
	h := s.host.Load()
	examined := 0
	var (
		dec     filter.Decision
		blocked netip.Addr
	)
	// judge checks one address; true stops the scan (blocked or bound hit).
	judge := func(ip netip.Addr) bool {
		if examined >= maxAnswerAddrs {
			return true
		}
		examined++
		ip = netutil.Canon(ip)
		if !ip.IsValid() || ip.IsLoopback() || h.isOwn(ip) {
			return false
		}
		d := s.d.Filter.CheckIP(ip, qc.groups)
		if v4, ok := embeddedIPv4(ip, prefix); ok && d.Source != "ip-rule" {
			// Lists judge an embedding address only by its IPv4 address: a
			// list entry around the NAT64 or DNS64 prefix would decide every
			// synthesised answer (the IP guard is static and cannot know the
			// configured prefix). The admin's IP rules may name the IPv6 form.
			d = filter.Decision{}
			if !v4.IsLoopback() && !h.isOwn(v4) {
				d = s.d.Filter.CheckIP(v4, qc.groups)
			}
		}
		if d.Blocked() {
			dec, blocked = d, ip
			return true
		}
		return false
	}
	scan := func(rrs []dns.RR, hints bool) bool {
		for _, rr := range rrs {
			if ip, ok := rrAddr(rr); ok {
				if judge(ip) {
					return true
				}
				continue
			}
			if !hints {
				continue
			}
			stop := false
			eachSVCB([]dns.RR{rr}, func(svcb *dns.SVCB) {
				for _, kv := range svcb.Value {
					for _, ip := range svcbHints(kv) {
						if !stop && judge(ip) {
							stop = true
						}
					}
				}
			})
			if stop {
				return true
			}
		}
		return false
	}
	if !scan(r.msg.Answer, true) {
		scan(r.msg.Extra, false)
	}
	if !blocked.IsValid() {
		return
	}
	what, purpose := "list", decisionPurpose(dec)
	if dec.Source == "ip-rule" {
		what, purpose = "IP rule", PurposeRule
	}
	if qc.tracing() {
		qc.note(fmt.Sprintf("response IP %s blocked by %s %q", blocked, what, dec.Name))
	}
	upstreamName, ede := r.upstream, r.ede
	*r = result{msg: s.globalReply(qc), status: StatusBlockedIP, reason: dec.Name + ": " + blocked.String(),
		listID: dec.ListID, ruleID: dec.RuleID, blocked: true, edeText: dec.Name, purpose: purpose,
		upstream: upstreamName, ede: ede, def: true}
}

// embeddedIPv4 returns the IPv4 address an IPv6 address carries: NAT64
// 64:ff9b::/96, 6to4 2002::/16, IPv4-compatible ::/96 (netutil.EmbeddedIPv4)
// or the configured DNS64 prefix (a /96).
func embeddedIPv4(ip netip.Addr, dns64 netip.Prefix) (netip.Addr, bool) {
	if v4, ok := netutil.EmbeddedIPv4(ip); ok {
		return v4, true
	}
	if ip.Is6() && dns64.IsValid() && dns64.Bits() == 96 && dns64.Addr().Is6() && dns64.Contains(ip) {
		b := ip.As16()
		return netip.AddrFrom4([4]byte(b[12:16])), true
	}
	return netip.Addr{}, false
}
