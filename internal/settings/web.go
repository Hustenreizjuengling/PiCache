package settings

import (
	"net/netip"
	"slices"
	"strconv"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// Languages are the ids of the UI languages, in the order of the language
// pickers: the same list as LOCALES in web/src/i18n/locales.ts (a test
// compares them). web.language is "" (the browser's language) or one of
// them.
var Languages = []string{"en", "de"}

// KnownLanguage reports whether web.language may be l.
func KnownLanguage(l string) bool { return l == "" || slices.Contains(Languages, l) }

// forgetUnknownLanguage reads a stored web.language this version does not
// know (a later version's language, kept by a downgrade) as "" (the
// browser's language) instead of leaving the whole document invalid; it
// returns the value it dropped. Writes still refuse such a value.
func (w *Web) forgetUnknownLanguage() (string, bool) {
	if KnownLanguage(w.Language) {
		return "", false
	}
	old := w.Language
	w.Language = ""
	return old, true
}

// validateAccess checks the web access members: web.allowedNetworks (at
// most 64 addresses or CIDRs of at least /8 IPv4, /32 IPv6),
// web.trustedProxies (at most 16 of at least /24 IPv4, /64 IPv6; so
// 0.0.0.0/0 and ::/0 are impossible) and web.tlsMinVersion. Zones are
// refused; normalize has made the entries canonical.
func (w *Web) validateAccess() error {
	for _, l := range []struct {
		field      string
		list       []string
		max        int
		min4, min6 int
	}{
		{"web.allowedNetworks", w.AllowedNetworks, MaxWebAllowedNetworks, 8, 32},
		{"web.trustedProxies", w.TrustedProxies, MaxTrustedProxies, 24, 64},
	} {
		if len(l.list) > l.max {
			return apperr.Invalid(l.field, "at most %d entries", l.max)
		}
		for i, s := range l.list {
			field := l.field + "[" + strconv.Itoa(i) + "]"
			p, ok := parseAddrOrPrefix(s)
			if !ok {
				return apperr.Invalid(field, "must be an IP address or CIDR")
			}
			if (p.Addr().Is4() && p.Bits() < l.min4) || (p.Addr().Is6() && p.Bits() < l.min6) {
				return apperr.Invalid(field, "network is too broad: use at least /%d (IPv4) or /%d (IPv6)", l.min4, l.min6)
			}
		}
	}
	switch w.TLSMinVersion {
	case TLSVersion12, TLSVersion13:
	default:
		return apperr.Invalid("web.tlsMinVersion", "must be 1.2 or 1.3")
	}
	return nil
}

// WebPrefixes parses the entries of web.allowedNetworks or
// web.trustedProxies (addresses as full-length prefixes; entries that do
// not parse are skipped: Validate refuses them earlier).
func WebPrefixes(in []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		if p, ok := parseAddrOrPrefix(s); ok {
			out = append(out, p)
		}
	}
	return out
}
