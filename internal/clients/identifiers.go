package clients

import (
	"net"
	"net/netip"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Identifier kinds stored in client_identifiers.kind.
const (
	kindIP   = "ip"
	kindCIDR = "cidr"
	kindMAC  = "mac"
	// kindClientID is a ClientID of encrypted DNS, stored as
	// "clientid:<ClientID>" (docs/ARCHITECTURE.md 19).
	kindClientID = "clientid"
	// kindIface is an interface: every source address a reply leaves
	// through it by (netutil.InterfaceOf), stored as "iface:<name>" with
	// the name as the kernel spells it.
	kindIface = "iface"
	// kindHost is a host name the address's own cached name must have
	// (spoofable: a device chooses its name), stored as "host:<name>"
	// lower-case without the trailing dot.
	kindHost = "host"
)

// Prefixes of the identifiers iface: and host: (case-insensitive).
const (
	IfacePrefix = "iface:"
	HostPrefix  = "host:"
	maxIfaceLen = 15
	maxHostLen  = 253
)

// identifier is a parsed, normalised client identifier.
type identifier struct {
	kind   string
	value  string       // canonical text form (stored and returned by the API)
	ip     netip.Addr   // kindIP
	prefix netip.Prefix // kindCIDR
}

// parseIdentifier normalises an IP address, a CIDR, an EUI-48 MAC address,
// clientid:<ClientID>, iface:<interface> or host:<name>. IPs are unmapped
// and lose their zone; a CIDR is masked (a full-length prefix becomes an
// IP); MACs become lower-case colon-separated; a ClientID and a host name
// become lower-case (the prefix too); an interface keeps its spelling. The
// length limit is per kind: host: + 253, iface: + 15, clientid: + 63,
// others 64.
func parseIdentifier(s string) (identifier, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return identifier{}, false
	}
	if p, ok := cutPrefixFold(s, IfacePrefix); ok {
		if !ValidIfaceName(p) {
			return identifier{}, false
		}
		return identifier{kind: kindIface, value: IfacePrefix + p}, true
	}
	if p, ok := cutPrefixFold(s, HostPrefix); ok {
		name := strings.ToLower(strings.TrimSuffix(p, "."))
		if !validHostIdentifier(name) {
			return identifier{}, false
		}
		return identifier{kind: kindHost, value: HostPrefix + name}, true
	}
	if len(s) > len(settings.ClientIDPrefix)+63 {
		return identifier{}, false
	}
	if len(s) > len(settings.ClientIDPrefix) && strings.EqualFold(s[:len(settings.ClientIDPrefix)], settings.ClientIDPrefix) {
		id, ok := settings.ParseClientIDEntry(s)
		if !ok {
			return identifier{}, false
		}
		return identifier{kind: kindClientID, value: settings.ClientIDPrefix + id}, true
	}
	if len(s) > 64 {
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

// cutPrefixFold cuts a case-insensitive prefix.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}

// ValidIfaceName reports whether s can be a Linux interface name: 1–15
// bytes, no "/", ":", white space or control characters, not "." or "..".
func ValidIfaceName(s string) bool {
	if len(s) == 0 || len(s) > maxIfaceLen || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f || c == '/' || c == ':' {
			return false
		}
	}
	return true
}

// validHostIdentifier reports a host name of host:: 1–253 characters,
// labels of 1–63 letters, digits and hyphens, not starting or ending with
// a hyphen (lower-case already).
func validHostIdentifier(s string) bool {
	if len(s) == 0 || len(s) > maxHostLen {
		return false
	}
	for label := range strings.SplitSeq(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			if c := label[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
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
