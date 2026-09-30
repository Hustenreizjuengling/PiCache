package settings

import (
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// DNSSEC modes (dns.dnssecMode, docs/ARCHITECTURE.md 7.4 and 7.6).
const (
	// DNSSECOff sends DO upstream only when the client set DO and passes
	// the upstream's AD on (dns.dnssec false).
	DNSSECOff = "off"
	// DNSSECPassthrough sends DO on every upstream query and passes the
	// upstream's AD on; nothing is validated locally (dns.dnssec true).
	DNSSECPassthrough = "passthrough"
	// DNSSECValidate sends DO on every upstream query and validates the
	// answers of the validated routes locally; AD is set only from
	// PiCache's own secure verdict.
	DNSSECValidate = "validate"
)

// reconcileDNSSEC keeps dns.dnssecMode and its alias dns.dnssec of
// versions before 0.17.0 consistent (docs/API.md "Section dns"), like
// reconcileChannel: an empty mode keeps the stored one; a changed mode
// wins (a changed dnssec in the same write that contradicts it is
// refused); a changed dnssec alone moves off to passthrough (true;
// passthrough and validate stay) or anything to off (false). Every save
// writes dnssec = (dnssecMode != "off").
func (d *DNS) reconcileDNSSEC(old DNS) error {
	d.DNSSECMode = strings.ToLower(strings.TrimSpace(d.DNSSECMode))
	if d.DNSSECMode == "" {
		d.DNSSECMode = old.DNSSECMode
	}
	if !validDNSSECMode(d.DNSSECMode) {
		return apperr.Invalid("dns.dnssecMode", "must be off, passthrough or validate")
	}
	modeChanged := d.DNSSECMode != old.DNSSECMode
	aliasChanged := d.DNSSEC != old.DNSSEC
	switch {
	case modeChanged && aliasChanged && d.DNSSEC != (d.DNSSECMode != DNSSECOff):
		return apperr.Invalid("dns.dnssecMode", "dnssec contradicts dnssecMode: send dnssecMode only")
	case !modeChanged && aliasChanged:
		switch {
		case !d.DNSSEC:
			d.DNSSECMode = DNSSECOff
		case d.DNSSECMode == DNSSECOff:
			d.DNSSECMode = DNSSECPassthrough
		}
	}
	d.DNSSEC = d.DNSSECMode != DNSSECOff
	return nil
}

func validDNSSECMode(m string) bool {
	return m == DNSSECOff || m == DNSSECPassthrough || m == DNSSECValidate
}

// storedDNSSEC completes a decoded stored document (decoded with an empty
// mode and dns.dnssec false): a document of an earlier version without
// dnssecMode gets passthrough for dns.dnssec true, else off (settings
// migration v7), and dns.dnssec follows the mode.
func (d *DNS) storedDNSSEC() {
	d.DNSSECMode = strings.ToLower(strings.TrimSpace(d.DNSSECMode))
	if d.DNSSECMode == "" {
		d.DNSSECMode = DNSSECOff
		if d.DNSSEC {
			d.DNSSECMode = DNSSECPassthrough
		}
	}
	if validDNSSECMode(d.DNSSECMode) {
		d.DNSSEC = d.DNSSECMode != DNSSECOff
	}
}

// Validating reports whether PiCache validates DNSSEC itself.
func (d *DNS) Validating() bool { return d.DNSSECMode == DNSSECValidate }
