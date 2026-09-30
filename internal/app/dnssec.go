package app

import (
	"context"
	"fmt"

	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// --- health check "dnssec" (DNSSEC mode validate only) ---

// dnssecHealthInput is what the check "dnssec" judges.
type dnssecHealthInput struct {
	anchorMismatch bool   // an upstream of a validated route is anchor-mismatch
	timeActive     bool   // RRSIG validity periods are checked
	timeReason     string // why not (upstream.TimeReason*)
	// noDNSSEC names the no-dnssec upstreams of the default set, the
	// fallbacks, the clock-guard set (while the clock guard is active) and
	// the validating forwarders ("forwarder <domain>: <upstream>"); group
	// sets are left out (a family preset without DNSSEC data is a
	// deliberate choice).
	noDNSSEC     []string
	newRootKey   bool
	bogus, total int64 // validated answers of the last hour and the bogus ones among them
}

// dnssecCheck evaluates the check "dnssec", present only in the DNSSEC
// mode validate.
func (a *App) dnssecCheck(ctx context.Context, set *settings.All) (status, msg, hint string, show bool) {
	if !set.DNS.Validating() {
		return "", "", "", false
	}
	status, msg, hint = dnssecHealth(a.dnssecHealthInput(ctx))
	return status, msg, hint, true
}

// dnssecHealthInput collects the input of the check "dnssec".
func (a *App) dnssecHealthInput(ctx context.Context) dnssecHealthInput {
	h := a.up.DNSSECHealth()
	active, reason := a.up.TimeChecks()
	in := dnssecHealthInput{anchorMismatch: h.AnchorMismatch, timeActive: active, timeReason: reason,
		noDNSSEC: h.NoDNSSEC, newRootKey: a.up.NewRootKey()}
	if fwds, err := a.dns.Forwarders(ctx); err == nil {
		for _, f := range fwds {
			if !f.Enabled || !f.Validate {
				continue
			}
			for _, st := range a.up.ForwarderStats(f.Upstreams) {
				if st.DNSSEC == upstream.ProbeNoDNSSEC {
					in.noDNSSEC = append(in.noDNSSEC, "forwarder "+f.Domain+": "+st.Name)
				}
			}
		}
	}
	in.bogus, in.total = a.dns.DNSSECLastHour()
	return in
}

// timeReasonText explains why the DNSSEC time checks are suspended.
var timeReasonText = map[string]string{
	upstream.TimeReasonUnsynced:       "the host clock is not synchronised",
	upstream.TimeReasonClockGuard:     "the system clock is before the build date",
	upstream.TimeReasonRootSignatures: "the host clock disagrees with the root zone's signatures",
}

// dnssecHealth evaluates the check "dnssec"; the first match wins.
func dnssecHealth(in dnssecHealthInput) (status, msg, hint string) {
	switch {
	case in.anchorMismatch:
		return "fail", "the DNSSEC trust anchors of this version do not match the root zone: update PiCache",
			"answers from it are passed on without validation until then"
	case !in.timeActive:
		reason, ok := timeReasonText[in.timeReason]
		if !ok {
			reason = in.timeReason
		}
		return "warn", "DNSSEC time checks are suspended: " + reason, "enable time synchronisation on the host (timedatectl set-ntp true)"
	case len(in.noDNSSEC) > 0:
		msg := in.noDNSSEC[0] + " does not return DNSSEC data: its answers are not validated"
		if n := len(in.noDNSSEC) - 1; n > 0 {
			msg += fmt.Sprintf(" (and %d more)", n)
		}
		return "warn", msg, "choose upstreams that support DNSSEC, or set the DNSSEC mode to passthrough"
	case in.newRootKey:
		return "warn", "a new root key is published: update PiCache before it is used", ""
	case in.bogus >= 20 && in.bogus*100 > in.total:
		return "warn", fmt.Sprintf("%d of %d validated answers in the last hour failed DNSSEC validation", in.bogus, in.total),
			"the query log shows them (DNSSEC status bogus)"
	}
	return "ok", "", ""
}
