package clients

import (
	"net"
	"net/netip"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// Identifier kinds stored in client_identifiers.kind.
const (
	kindIP   = "ip"
	kindCIDR = "cidr"
	kindMAC  = "mac"
)

// identifier is a parsed, normalised client identifier.
type identifier struct {
	kind   string
	value  string       // canonical text form (stored and returned by the API)
	ip     netip.Addr   // kindIP
	prefix netip.Prefix // kindCIDR
}

// parseIdentifier normalises an IP address, a CIDR or an EUI-48 MAC address.
// IPs are unmapped and lose their zone; a CIDR is masked (a full-length
// prefix becomes an IP); MACs become lower-case colon-separated.
func parseIdentifier(s string) (identifier, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64 {
		return identifier{}, false
	}
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil || p.Addr().Zone() != "" {
			return identifier{}, false
		}
		addr := p.Addr()
		bits := p.Bits()
		if addr.Is4In6() && bits >= 96 {
			addr, bits = addr.Unmap(), bits-96
		}
		p = netip.PrefixFrom(addr, bits).Masked()
		if bits == addr.BitLen() {
			return identifier{kind: kindIP, value: addr.String(), ip: addr}, true
		}
		return identifier{kind: kindCIDR, value: p.String(), prefix: p}, true
	}
	if ip, err := netip.ParseAddr(s); err == nil {
		ip = netutil.Canon(ip)
		return identifier{kind: kindIP, value: ip.String(), ip: ip}, true
	}
	if mac, ok := normalizeMAC(s); ok {
		return identifier{kind: kindMAC, value: mac}, true
	}
	return identifier{}, false
}

// normalizeMAC accepts the forms of net.ParseMAC for 48-bit addresses and
// returns the lower-case colon-separated form.
func normalizeMAC(s string) (string, bool) {
	hw, err := net.ParseMAC(strings.TrimSpace(s))
	if err != nil || len(hw) != 6 {
		return "", false
	}
	for _, b := range hw {
		if b != 0 {
			return hw.String(), true
		}
	}
	return "", false // 00:00:00:00:00:00 is never a real client
}
