package dnsserver

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/parental"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// maxPause bounds a timed blocking pause.
const maxPause = 7 * 24 * time.Hour

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

// blocked builds the blocking reply for decision d (ARCHITECTURE 7.3).
func (s *Server) blocked(qc *qctx, d filter.Decision, status string) result {
	if qc.tracing() {
		qc.note(fmt.Sprintf("blocked by %s %q (%s): %s reply", d.Source, d.Name, d.Kind, qc.set.Filter.BlockingMode))
	}
	return result{
		msg:     blockReply(qc.req, &qc.set.Filter),
		status:  status,
		reason:  d.Name,
		listID:  d.ListID,
		ruleID:  d.RuleID,
		blocked: true,
	}
}

// parentalBlock applies the parental controls of the client's groups
// (step 7a). It runs whether or not blocking is active (pausing the lists
// and rules does not lift a bedtime) and before the download cache answer
// (a blocked Steam or a bedtime also stops cached downloads). A user allow
// rule that applies to the client lifts the block.
func (s *Server) parentalBlock(qc *qctx) (result, bool) {
	if s.d.Parental == nil {
		return result{}, false
	}
	d := s.d.Parental.Check(qc.qname, qc.id.GroupIDs, qc.start)
	if !d.Blocked {
		qc.note("parental: no restriction")
		return result{}, false
	}
	reason := d.Reason()
	if s.d.Filter != nil {
		if a := s.d.Filter.CheckRules(qc.qname, qc.id.GroupIDs); a.Action == filter.ActionAllow {
			if qc.tracing() {
				qc.note(fmt.Sprintf("parental block lifted by allow rule %q (%s)", a.Name, reason))
			}
			return result{}, false
		}
	}
	status := StatusBlockedSchedule
	if d.Kind == parental.KindService {
		status = StatusBlockedService
	}
	if qc.tracing() {
		until := ""
		if !d.Until.IsZero() {
			until = " until " + d.Until.UTC().Format(time.RFC3339)
		}
		qc.note(fmt.Sprintf("parental: blocked by %s%s: %s reply", reason, until, qc.set.Filter.BlockingMode))
	}
	return result{msg: blockReply(qc.req, &qc.set.Filter), status: status, reason: reason, blocked: true}, true
}

// blockReply answers req according to the blocking mode.
func blockReply(req *dns.Msg, f *settings.Filter) *dns.Msg {
	q := req.Question[0]
	ttl := f.BlockedTTL
	m := newReply(req)
	nodata := func(withSOA bool) *dns.Msg {
		if withSOA {
			m.Ns = []dns.RR{syntheticSOA(q.Name, ttl)}
		}
		return m
	}
	switch f.BlockingMode {
	case "nxdomain":
		m.Rcode = dns.RcodeNameError
		return nodata(true)
	case "nodata":
		return nodata(true)
	case "refused":
		m.Rcode = dns.RcodeRefused
		return m
	case "custom_ip":
		v4, _ := netip.ParseAddr(f.BlockingIPv4)
		v6, _ := netip.ParseAddr(f.BlockingIPv6)
		switch {
		case q.Qtype == dns.TypeA && v4.Is4():
			m.Answer = []dns.RR{&dns.A{Hdr: rrHeader(q.Name, dns.TypeA, ttl), A: v4.AsSlice()}}
		case q.Qtype == dns.TypeAAAA && v6.Is6():
			m.Answer = []dns.RR{&dns.AAAA{Hdr: rrHeader(q.Name, dns.TypeAAAA, ttl), AAAA: v6.AsSlice()}}
		}
		return m
	default: // "null"
		switch q.Qtype {
		case dns.TypeA:
			m.Answer = []dns.RR{&dns.A{Hdr: rrHeader(q.Name, dns.TypeA, ttl), A: netip.IPv4Unspecified().AsSlice()}}
		case dns.TypeAAAA:
			m.Answer = []dns.RR{&dns.AAAA{Hdr: rrHeader(q.Name, dns.TypeAAAA, ttl), AAAA: netip.IPv6Unspecified().AsSlice()}}
		}
		return m
	}
}

// specialDomain answers the special domains of ARCHITECTURE 7.1 step 10
// (Mozilla canary, iCloud Private Relay) with NXDOMAIN.
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
	st := BlockingStatus{Enabled: f.BlockingActive(now), Permanent: !f.Enabled}
	if f.Enabled && f.PausedUntil != nil && now.Before(*f.PausedUntil) {
		t := f.PausedUntil.UTC()
		st.PausedUntil = &t
	}
	return st
}

// SetBlocking enables/disables blocking; pause > 0 disables for that long.
// A timed pause is persisted and ends automatically.
func (s *Server) SetBlocking(ctx context.Context, enabled bool, pause time.Duration) (BlockingStatus, error) {
	if pause < 0 || pause > maxPause {
		return BlockingStatus{}, apperr.Invalid("pauseSeconds", "must be between 0 and %d", int(maxPause/time.Second))
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
