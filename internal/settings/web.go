package settings

import (
	"net/netip"
	"strconv"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

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
