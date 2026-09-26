package dnsserver

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/parental"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// MaxPause bounds a timed blocking pause.
const MaxPause = 7 * 24 * time.Hour

// syntheticSOA is the SOA of locally generated negative answers:
// picache.invalid. hostmaster.picache.invalid. 1 1800 900 604800 <ttl>.
func syntheticSOA(owner string, ttl uint32) *dns.SOA {
	return &dns.SOA{
		Hdr:     rrHeader(owner, dns.TypeSOA, ttl),
		Ns:      "picache.invalid.",
		Mbox:    "hostmaster.picache.invalid.",
		Serial:  1,
		Refresh: 1800,
		Retry:   900,
		Expire:  604800,
		Minttl:  ttl,
	}
}

// statusFor maps a blocking decision to a query status.
func statusFor(d filter.Decision) string {
	switch {
	case d.Kind == "regex":
		return StatusBlockedRegex
	case d.Source == "rule":
		return StatusBlockedRule
	}
	return StatusBlockedList
}

// blocked builds the blocking reply for decision d (ARCHITECTURE 7.3): a
// user rule's own reply, else the blocking mode.
func (s *Server) blocked(qc *qctx, d filter.Decision, status string) result {
	f := &qc.set.Filter
	mode, v4, v6 := f.BlockingMode, f.BlockingIPv4, f.BlockingIPv6
	if d.Source == "rule" && d.Reply != "" {
		mode, v4, v6 = d.Reply, d.ReplyIPv4, d.ReplyIPv6
	}
	if qc.tracing() {
		qc.note(fmt.Sprintf("blocked by %s %q (%s): %s reply", d.Source, d.Name, d.Kind, mode))
	}
	return result{
		msg:     s.blockReply(qc, mode, v4, v6),
		status:  status,
		reason:  d.Name,
		listID:  d.ListID,
		ruleID:  d.RuleID,
		blocked: true,
		purpose: decisionPurpose(d),
	}
}

// decisionPurpose is the statistics purpose of a filter decision: the
// list's category ("other" when it has none) or "rule".
func decisionPurpose(d filter.Decision) string {
	if d.Source == "rule" {
		return PurposeRule
	}
	if d.Category == "" {
		return filter.CategoryOther
	}
	return d.Category
}

// parentalBlock applies the parental controls of all the client's groups
// (step 7a): Parental.Check (block override, block-all schedule, blocked
// service), then the protection lists (Filter.CheckProtection). It runs
// whether or not blocking is active (pausing the lists and rules does not
// lift a bedtime or a protection list) and before the download cache
// answer (a blocked Steam or a bedtime also stops cached downloads). A
// user allow rule that applies to the client lifts the block.
func (s *Server) parentalBlock(qc *qctx) (result, bool) {
	var (
		reason, status, until, purpose, trace string
		listID                                int64
	)
	if s.d.Parental != nil {
		if d := s.d.Parental.Check(qc.qname, qc.id.GroupIDs, qc.start); d.Blocked {
			reason, status, purpose = d.Reason(), StatusBlockedSchedule, PurposeSchedule
			if d.Kind == parental.KindService {
				status, purpose = StatusBlockedService, PurposeService
			}
			if !d.Until.IsZero() {
				until = " until " + d.Until.UTC().Format(time.RFC3339)
			}
			trace = "parental: blocked by " + reason + until
		}
	}
	if reason == "" && s.d.Filter != nil {
		if d := s.d.Filter.CheckProtection(qc.qname, qc.qtype, qc.id.GroupIDs); d.Blocked() {
			reason, status, listID, purpose = d.Name, StatusBlockedList, d.ListID, decisionPurpose(d)
			trace = fmt.Sprintf("parental: blocked by list %q (%s)", d.Name, d.Category)
		}
	}
	if reason == "" {
		qc.note("parental: no restriction")
		return result{}, false
	}
	if s.d.Filter != nil {
		if a := s.d.Filter.CheckRules(qc.qname, qc.qtype, qc.id.GroupIDs); a.Action == filter.ActionAllow {
			if qc.tracing() {
				qc.note(fmt.Sprintf("parental block lifted by allow rule %q (%s)", a.Name, reason))
			}
			return result{}, false
		}
	}
	if qc.tracing() {
		qc.note(fmt.Sprintf("%s: %s reply", trace, qc.set.Filter.BlockingMode))
	}
	return result{msg: s.globalReply(qc), status: status, reason: reason, listID: listID,
		blocked: true, purpose: purpose}, true
}

// globalReply answers qc with the blocking reply of filter.blockingMode
// (7.3): every block but a user rule with its own reply.
func (s *Server) globalReply(qc *qctx) *dns.Msg {
	f := &qc.set.Filter
	return s.blockReply(qc, f.BlockingMode, f.BlockingIPv4, f.BlockingIPv6)
}

// blockReply answers qc with a blocking reply of mode (ARCHITECTURE 7.3):
// null, nxdomain, nodata, refused or custom_ip with the addresses v4s and
// v6s ("" = none: NODATA + SOA for the family; settings.SelfAddress: this
// server's address for the client, selfAddr), TTL filter.blockedTtl.
func (s *Server) blockReply(qc *qctx, mode, v4s, v6s string) *dns.Msg {
	req := qc.req
	q := req.Question[0]
	ttl := qc.set.Filter.BlockedTTL
	m := newReply(req)
	// nodata adds the synthetic SOA to a reply without answer (RFC 2308).
	nodata := func() *dns.Msg {
		m.Ns = []dns.RR{syntheticSOA(q.Name, ttl)}
		return m
	}
	switch mode {
	case "nxdomain":
		m.Rcode = dns.RcodeNameError
		return nodata()
	case "nodata":
		return nodata()
	case "refused":
		m.Rcode = dns.RcodeRefused
		return m
	case "custom_ip":
		switch q.Qtype {
		case dns.TypeA:
			if v4 := s.replyAddr(qc, v4s, false); v4.Is4() {
				m.Answer = []dns.RR{&dns.A{Hdr: rrHeader(q.Name, dns.TypeA, ttl), A: v4.AsSlice()}}
				return m
			}
		case dns.TypeAAAA:
			if v6 := s.replyAddr(qc, v6s, true); v6.Is6() {
				m.Answer = []dns.RR{&dns.AAAA{Hdr: rrHeader(q.Name, dns.TypeAAAA, ttl), AAAA: v6.AsSlice()}}
				return m
			}
		}
		return nodata() // no address of the family, other types
	default: // "null"
		switch q.Qtype {
		case dns.TypeA:
			m.Answer = []dns.RR{&dns.A{Hdr: rrHeader(q.Name, dns.TypeA, ttl), A: netip.IPv4Unspecified().AsSlice()}}
		case dns.TypeAAAA:
			m.Answer = []dns.RR{&dns.AAAA{Hdr: rrHeader(q.Name, dns.TypeAAAA, ttl), AAAA: netip.IPv6Unspecified().AsSlice()}}
		default:
			return nodata()
		}
		return m
	}
}

// replyAddr returns a custom_ip address of a blocking reply: the address,
// or for settings.SelfAddress this server's address for the client
// (selfAddr); invalid when there is none. A download service's name never
// gets this server's address (3.6): the download cache (:80) and the SNI
// relay (:443) serve every download service name without asking the
// filter, so they would fetch the blocked content anyway; that family
// answers NODATA + SOA instead.
func (s *Server) replyAddr(qc *qctx, v string, v6 bool) netip.Addr {
	var ip netip.Addr
	if v == settings.SelfAddress {
		ip = s.selfAddr(qc, v6)
	} else if a, err := netip.ParseAddr(v); err == nil && a.Is6() == v6 && !a.Is4In6() {
		ip = a
	}
	if !ip.IsValid() || s.d.Services == nil || !(v == settings.SelfAddress || s.isServerAddr(qc, ip)) {
		return ip
	}
	if svc, ok := s.d.Services.MatchDNS(qc.qname); ok {
		qc.note("download service " + svc + ": no reply with this server's address, which would serve it anyway")
		return netip.Addr{}
	}
	return ip
}

// isServerAddr reports whether ip reaches this server: an address of this
// machine, a server-name answer for the client or a download cache address.
func (s *Server) isServerAddr(qc *qctx, ip netip.Addr) bool {
	if s.host.Load().isOwn(ip) {
		return true
	}
	v4s, v6s := s.serverNameAddrs(qc)
	st := s.cacheIPs.Load()
	return slices.Contains(v4s, ip) || slices.Contains(v6s, ip) || slices.Contains(st.v4, ip) || slices.Contains(st.v6, ip)
}

// selfAddr is "this server's address" of a blocking reply (3.6): the
// first address of the family that a server-name answer (step 6) would
// give the client; invalid when there is none (never IPv6 link-local).
func (s *Server) selfAddr(qc *qctx, v6 bool) netip.Addr {
	v4s, v6s := s.serverNameAddrs(qc)
	list := v4s
	if v6 {
		list = v6s
	}
	for _, ip := range list {
		if ip.Is6() == v6 && !(ip.Is6() && ip.IsLinkLocalUnicast()) {
			return ip
		}
	}
	return netip.Addr{}
}

// specialDomain answers the special domains of ARCHITECTURE 7.1 step 10
// (Mozilla canary, iCloud Private Relay) with NXDOMAIN. Their own settings
// decide, whether blocking is enabled, paused (globally or for the client's
// groups) or disabled: a browser that sees the canary unblocked during a
// pause switches to encrypted DNS and keeps it afterwards, which bypasses
// parental controls and safe search. Only a name the client's groups
// allowlist (Filter.Check with all groups → allow) is left alone.
func (s *Server) specialDomain(qc *qctx) (result, bool) {
	f := &qc.set.Filter
	var reason string
	switch {
	case f.BlockMozillaCanary && qc.qname == "use-application-dns.net" &&
		(qc.qtype == dns.TypeA || qc.qtype == dns.TypeAAAA):
		reason = "mozilla-canary"
	case f.BlockICloudPrivateRelay && (qc.qname == "mask.icloud.com" || qc.qname == "mask-h2.icloud.com"):
		reason = "icloud-private-relay"
	default:
		return result{}, false
	}
	if s.d.Filter != nil {
		if d := s.d.Filter.Check(qc.qname, qc.qtype, qc.id.GroupIDs); d.Action == filter.ActionAllow {
			if qc.tracing() {
				qc.note(fmt.Sprintf("special domain %s: allowed by %s %q", reason, d.Source, d.Name))
			}
			return result{}, false
		}
	}
	qc.note("special domain " + reason + ": NXDOMAIN")
	m := newReply(qc.req)
	m.Rcode = dns.RcodeNameError
	m.Ns = []dns.RR{syntheticSOA(qc.q.Name, f.BlockedTTL)}
	return result{msg: m, status: StatusBlockedSpecial, reason: reason, blocked: true}, true
}

// Blocking returns the current blocking state.
func (s *Server) Blocking() BlockingStatus {
	f := s.d.Settings.Get().Filter
	now := time.Now()
	zone, offset := now.In(time.Local).Zone()
	st := BlockingStatus{Enabled: f.BlockingActive(now), Permanent: !f.Enabled, TimeZone: zone, UTCOffsetMinutes: offset / 60}
	if f.Enabled && f.PausedUntil != nil && now.Before(*f.PausedUntil) {
		t := f.PausedUntil.UTC()
		st.PausedUntil = &t
	}
	return st
}

// SetBlocking enables/disables blocking; pause > 0 disables for that long.
// A timed pause is persisted and ends automatically.
func (s *Server) SetBlocking(ctx context.Context, enabled bool, pause time.Duration) (BlockingStatus, error) {
	if pause < 0 || pause > MaxPause {
		return BlockingStatus{}, apperr.Invalid("pauseSeconds", "must be between 0 and %d", int(MaxPause/time.Second))
	}
	_, err := s.d.Settings.Update(ctx, func(a *settings.All) error {
		switch {
		case enabled:
			a.Filter.Enabled, a.Filter.PausedUntil = true, nil
		case pause > 0:
			until := time.Now().Add(pause).UTC()
			a.Filter.Enabled, a.Filter.PausedUntil = true, &until
		default:
			a.Filter.Enabled, a.Filter.PausedUntil = false, nil
		}
		return nil
	})
	if err != nil {
		return BlockingStatus{}, err
	}
	return s.Blocking(), nil
}

// expirePause clears an elapsed timed pause from the settings so the
// persisted state and the UI show blocking as re-enabled.
func (s *Server) expirePause(ctx context.Context, now time.Time) {
	f := s.d.Settings.Get().Filter
	if f.PausedUntil == nil || now.Before(*f.PausedUntil) {
		return
	}
	_, err := s.d.Settings.Update(ctx, func(a *settings.All) error {
		if a.Filter.PausedUntil != nil && !now.Before(*a.Filter.PausedUntil) {
			a.Filter.PausedUntil = nil
		}
		return nil
	})
	if err != nil {
		s.log.Warn("could not clear the elapsed blocking pause", slog.Any("err", err))
		return
	}
	if f.Enabled {
		s.log.Info("blocking re-enabled after pause")
	}
}
