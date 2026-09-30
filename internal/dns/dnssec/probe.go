package dnssec

import (
	"context"
	"time"

	"github.com/miekg/dns"
)

// Probe states of an upstream (upstream.UpstreamStat.dnssec).
const (
	StateCapable        = "capable"
	StateNoDNSSEC       = "no-dnssec"
	StateAnchorMismatch = "anchor-mismatch"
	StateUnknown        = "unknown"
)

// RootCheck is the outcome of a probe of an upstream: its answers to ". DNSKEY"
// and ". SOA" (DO=1, CD=0).
type RootCheck struct {
	State string // StateCapable, StateNoDNSSEC or StateAnchorMismatch
	Error string // why the state is not capable
	// TimeOK: the anchored RRSIG over the root DNSKEY RRset that verifies
	// is valid now (with the 1 h tolerance); false when none verifies.
	TimeOK bool
	// Inception and Expiration are the validity period of that RRSIG
	// (zero when none verifies), so its validity can be judged again at a
	// later time (RootValidAt).
	Inception, Expiration time.Time
	// NewRootKey: the authenticated root DNSKEY RRset holds a SEP key
	// that matches no anchor (only judged while TimeOK: an old key set
	// replayed to a probe does not count).
	NewRootKey bool
}

// RootValidAt reports whether a root RRSIG valid from inception to
// expiration (RootCheck) is valid at now with the tolerance of the time
// checks (1 h at both ends).
func RootValidAt(inception, expiration, now time.Time) bool {
	return !now.Before(inception.Add(-timeTolerance)) && !now.After(expiration.Add(timeTolerance))
}

// CheckRoot judges a probe: capable when the DNSKEY answer holds a key
// matching an anchor whose RRSIG over the set verifies and the SOA answer
// carries an RRSIG that verifies with a key of that set; no-dnssec for an
// answer without DNSKEY records or RRSIGs or with signatures that do not
// verify; anchor-mismatch when DNSKEY records and RRSIGs are there but no
// key matches an anchor. Signature dates are not judged here (TimeOK
// reports them).
func (v *Validator) CheckRoot(ctx context.Context, dnskey, soa *dns.Msg, now time.Time) RootCheck {
	sc := &sctx{ctx: ctx, v: v, b: &budget{}, now: now}
	if dnskey == nil || dnskey.Rcode != dns.RcodeSuccess {
		return RootCheck{State: StateNoDNSSEC, Error: "no DNSKEY answer"}
	}
	set := find(groupRRsets(dnskey.Answer), ".", dns.TypeDNSKEY)
	switch {
	case set == nil:
		return RootCheck{State: StateNoDNSSEC, Error: "the root DNSKEY answer holds no DNSKEY records"}
	case len(set.sigs) == 0:
		return RootCheck{State: StateNoDNSSEC, Error: "the root DNSKEY answer carries no RRSIG"}
	case len(set.rrs) > maxDNSKEYs:
		return RootCheck{State: StateNoDNSSEC, Error: "the root DNSKEY answer holds too many keys"}
	}
	var all, trusted []key
	for _, rr := range set.rrs {
		if k, ok := rr.(*dns.DNSKEY); ok {
			kk := newKey(k)
			all = append(all, kk)
			if anchored(k, v.anchors) {
				trusted = append(trusted, kk)
			}
		}
	}
	if len(trusted) == 0 {
		return RootCheck{State: StateAnchorMismatch, Error: "no root key matches the trust anchors of this version"}
	}
	vr, f := sc.verifyRRset(set, ".", trusted)
	if f != nil {
		return RootCheck{State: StateNoDNSSEC, Error: "the root DNSKEY signature does not verify"}
	}
	out := RootCheck{State: StateCapable, Inception: sigTime(vr.sig.Inception, now), Expiration: vr.expires}
	out.TimeOK = RootValidAt(out.Inception, out.Expiration, now)
	if out.TimeOK {
		out.NewRootKey = v.checkNewRootKey(all)
	}
	if soa == nil || soa.Rcode != dns.RcodeSuccess {
		out.State, out.Error = StateNoDNSSEC, "no SOA answer"
		return out
	}
	soaSet := find(groupRRsets(soa.Answer), ".", dns.TypeSOA)
	switch {
	case soaSet == nil:
		out.State, out.Error = StateNoDNSSEC, "the root SOA answer holds no SOA record"
	case len(soaSet.sigs) == 0:
		out.State, out.Error = StateNoDNSSEC, "the root SOA answer carries no RRSIG"
	default:
		if _, f := sc.verifyRRset(soaSet, ".", usableKeys(all)); f != nil {
			out.State, out.Error = StateNoDNSSEC, "the root SOA signature does not verify"
		}
	}
	return out
}
