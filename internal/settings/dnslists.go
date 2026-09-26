package settings

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// ErrPlainUpstreamName is the validation message of a plain DNS upstream
// given by a name that is not public (PublicUpstreamName).
const ErrPlainUpstreamName = "plain DNS upstreams given by name must be public DNS names; use an IP address for local servers"

// localOnlyZones are the zones whose names never reach a public resolver
// (RFC 6761, RFC 6762, RFC 7686, ICANN .internal, RFC 8375, RFC 6303).
var localOnlyZones = []string{"localhost", "test", "invalid", "onion", "local", "internal", "home.arpa", "arpa"}

// PublicUpstreamName reports whether host, the name of a plain DNS upstream
// (udp/tcp), is a public fully qualified name: at least two labels, a last
// label that is not all digits (no top-level domain is; such a string is a
// mistyped IP address) and not equal to or below the local domain,
// home.arpa, localhost, test, invalid, onion, local, internal, arpa or one
// of the extra zones (the resolv.conf search domains for conditional
// forwarders). A local resolver is configured by its IP address.
func PublicUpstreamName(host, localDomain string, extra ...string) bool {
	host = strings.ToLower(host)
	if !validHostname(host) {
		return false
	}
	if host = strings.TrimSuffix(host, "."); !strings.Contains(host, ".") || numericTLD(host) {
		return false
	}
	for _, z := range slices.Concat(localOnlyZones, []string{localDomain}, extra) {
		if z = strings.Trim(strings.ToLower(z), "."); z != "" && inZone(host, z) {
			return false
		}
	}
	return true
}

// numericTLD reports whether the last label of a host name is all digits
// (RFC 3696 2: top-level domains never are), e.g. "192.168.1781".
func numericTLD(host string) bool {
	host = strings.TrimSuffix(host, ".")
	tld := host[strings.LastIndexByte(host, '.')+1:]
	return tld != "" && strings.Trim(tld, "0123456789") == ""
}

// inZone reports whether name equals zone or is below it (both normalised).
func inZone(name, zone string) bool {
	return name == zone || (len(name) > len(zone) && name[len(name)-len(zone)-1] == '.' && strings.HasSuffix(name, zone))
}

// normalizeList trims every entry, applies norm and drops empty entries and
// duplicates (the first one stays).
func normalizeList(in []string, norm func(string) string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if s = norm(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// normalizePrefix masks a CIDR ("10.1.2.3/8" → "10.0.0.0/8"); anything
// else is returned lower-cased for Validate to report.
func normalizePrefix(s string) string {
	s = strings.ToLower(s)
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked().String()
	}
	return s
}

// normalizeAddrOrPrefix returns the canonical form of an IP address
// (unmapped) or a CIDR (masked; an IPv4-mapped prefix becomes an IPv4 one;
// a full-length prefix becomes the address). Addresses with a zone and
// anything else are returned lower-cased for Validate to report.
func normalizeAddrOrPrefix(s string) string {
	s = strings.ToLower(s)
	if p, ok := parseAddrOrPrefix(s); ok {
		if p.IsSingleIP() {
			return p.Addr().String()
		}
		return p.String()
	}
	return s
}

// parseAddrOrPrefix parses an IP address (as a full-length prefix) or a
// CIDR, canonically: unmapped, masked, never with a zone.
func parseAddrOrPrefix(s string) (netip.Prefix, bool) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil || p.Addr().Zone() != "" {
			return netip.Prefix{}, false
		}
		addr, bits := p.Addr(), p.Bits()
		if addr.Is4In6() {
			if bits < 96 {
				return netip.Prefix{}, false
			}
			addr, bits = addr.Unmap(), bits-96
		}
		return netip.PrefixFrom(addr, bits).Masked(), true
	}
	ip, err := netip.ParseAddr(s)
	if err != nil || ip.Zone() != "" {
		return netip.Prefix{}, false
	}
	ip = ip.Unmap()
	return netip.PrefixFrom(ip, ip.BitLen()), true
}

// broadEnough reports whether p is at least /8 (IPv4) or /32 (IPv6), the
// narrowest-allowed bound for the address lists of the DNS section.
func broadEnough(p netip.Prefix) bool {
	return (p.Addr().Is4() && p.Bits() >= 8) || (p.Addr().Is6() && p.Bits() >= 32)
}

// ParseBlockedClient normalises an entry of dns.blockedClients: an IP
// address (canonical, unmapped), a CIDR (masked; at least /8 for IPv4, /32
// for IPv6) or a MAC address (lower-case with colons; "-" separators are
// accepted). IPv6 zones, the all-zero MAC and group (multicast) MACs are
// refused.
func ParseBlockedClient(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > 64 {
		return "", false
	}
	if p, ok := parseAddrOrPrefix(s); ok {
		if !broadEnough(p) {
			return "", false
		}
		if p.IsSingleIP() {
			return p.Addr().String(), true
		}
		return p.String(), true
	}
	if strings.ContainsAny(s, "/%") {
		return "", false
	}
	return NormalizeMAC(s)
}

// NormalizeBlockedClients returns dns.blockedClients as it is stored:
// entries normalised (ParseBlockedClient; entries that do not parse are
// kept for Validate to name), empty entries and duplicates removed.
func NormalizeBlockedClients(in []string) []string {
	return normalizeList(in, func(s string) string {
		if e, ok := ParseBlockedClient(s); ok {
			return e
		}
		return s
	})
}

// NormalizeMAC returns a unicast EUI-48 MAC address lower-case with colons
// (the forms of net.ParseMAC); false for other lengths, the all-zero
// address and group addresses.
func NormalizeMAC(s string) (string, bool) {
	hw, err := net.ParseMAC(strings.TrimSpace(s))
	if err != nil || len(hw) != 6 || hw[0]&1 != 0 {
		return "", false
	}
	if hw[0]|hw[1]|hw[2]|hw[3]|hw[4]|hw[5] == 0 {
		return "", false
	}
	return hw.String(), true
}

// DroppedDomain is a parsed entry of dns.droppedDomains: the domain and
// its subdomains, all query types (Type 0) or one.
type DroppedDomain struct {
	Domain string // lower-case, no trailing dot
	Type   uint16 // 0 = every type
}

// String returns the normalised entry: "domain" or "domain:TYPE" with the
// type's mnemonic (TYPEnnn for types without one).
func (d DroppedDomain) String() string {
	if d.Type == 0 {
		return d.Domain
	}
	return d.Domain + ":" + typeName(d.Type)
}

// typeName returns the mnemonic of t as ParseDroppedDomain accepts it back,
// else "TYPEnnn".
func typeName(t uint16) string {
	if s, ok := dns.TypeToString[t]; ok {
		if u := strings.ToUpper(s); dns.StringToType[u] == t {
			return u
		}
	}
	return "TYPE" + strconv.Itoa(int(t))
}

// ParseDroppedDomain parses "domain" or "domain:TYPE" (TYPE a mnemonic known
// to the DNS library or TYPEnnn, 1–65535). The domain must be a host name
// with at least two labels, not arpa or below it and not below localhost,
// test, invalid, onion, local or internal (those never reach the point
// where domains are dropped).
func ParseDroppedDomain(s string) (DroppedDomain, error) {
	s = strings.TrimSpace(s)
	domain, typ, hasType := strings.Cut(s, ":")
	domain = strings.TrimSuffix(strings.ToLower(domain), ".")
	var out DroppedDomain
	if !validHostname(domain) || !strings.Contains(domain, ".") {
		return out, errors.New("must be a domain name with at least two labels, optionally followed by :TYPE (e.g. example.com or example.com:AAAA)")
	}
	if inZone(domain, "arpa") {
		return out, errors.New("reverse (arpa) names cannot be dropped")
	}
	for _, z := range []string{"localhost", "test", "invalid", "onion", "local", "internal"} {
		if inZone(domain, z) {
			return out, fmt.Errorf("names below %s are answered locally and cannot be dropped", z)
		}
	}
	out.Domain = domain
	if !hasType {
		return out, nil
	}
	typ = strings.ToUpper(strings.TrimSpace(typ))
	if n, ok := strings.CutPrefix(typ, "TYPE"); ok && n != "" {
		v, err := strconv.ParseUint(n, 10, 16)
		if err != nil || v == 0 || (len(n) > 1 && n[0] == '0') {
			return out, errors.New("TYPEnnn must be between TYPE1 and TYPE65535")
		}
		out.Type = uint16(v)
		return out, nil
	}
	t, ok := dns.StringToType[typ]
	if !ok || t == 0 {
		return out, fmt.Errorf("unknown record type %q", typ)
	}
	out.Type = t
	return out, nil
}

// parseDroppedDomain is ParseDroppedDomain for the normaliser.
func parseDroppedDomain(s string) (DroppedDomain, bool) {
	d, err := ParseDroppedDomain(s)
	return d, err == nil
}

// validReverseNetwork reports whether p can be a private reverse network:
// IPv4 /8, /16 or /24, IPv6 /16–/124 in steps of 4 (reverse zones end at
// label boundaries), not IPv4-mapped, without zone.
func validReverseNetwork(p netip.Prefix) bool {
	a := p.Addr()
	switch {
	case a.Zone() != "" || a.Is4In6():
		return false
	case a.Is4():
		return p.Bits() == 8 || p.Bits() == 16 || p.Bits() == 24
	}
	return p.Bits() >= 16 && p.Bits() <= 124 && p.Bits()%4 == 0
}

// ReverseZone returns the reverse zone of a private reverse network
// ("192.168.5.0/24" → "5.168.192.in-addr.arpa", "fd12::/16" →
// "2.1.d.f.ip6.arpa"); false for prefixes validReverseNetwork refuses.
func ReverseZone(p netip.Prefix) (string, bool) {
	if !validReverseNetwork(p) {
		return "", false
	}
	p = p.Masked()
	var labels []string
	if p.Addr().Is4() {
		b := p.Addr().As4()
		for i := p.Bits()/8 - 1; i >= 0; i-- {
			labels = append(labels, strconv.Itoa(int(b[i])))
		}
		return strings.Join(labels, ".") + ".in-addr.arpa", true
	}
	b := p.Addr().As16()
	const hex = "0123456789abcdef"
	for i := p.Bits()/4 - 1; i >= 0; i-- {
		nibble := b[i/2] >> 4
		if i%2 == 1 {
			nibble = b[i/2] & 0x0f
		}
		labels = append(labels, string(hex[nibble]))
	}
	return strings.Join(labels, ".") + ".ip6.arpa", true
}

// PrivateReverseZones returns the reverse zones of dns.privateReverseNetworks
// (invalid entries are skipped; Validate rejects them earlier).
func (d *DNS) PrivateReverseZones() []string {
	var out []string
	for _, s := range d.PrivateReverseNetworks {
		if p, err := netip.ParsePrefix(s); err == nil {
			if z, ok := ReverseZone(p); ok && !slices.Contains(out, z) {
				out = append(out, z)
			}
		}
	}
	return out
}

// ECSSubnet returns the custom ECS subnet (invalid if unset or malformed).
// Whether it is public is checked where it is used (netutil.IsPublicUnicast).
func (d *DNS) ECSSubnet() netip.Prefix {
	p, err := netip.ParsePrefix(d.ECS.CustomSubnet)
	if err != nil || !validECSSubnet(p) {
		return netip.Prefix{}
	}
	return p.Masked()
}

// validECSSubnet checks the form of the custom ECS subnet: an IPv4 /8–/24
// or IPv6 /32–/56 network, not IPv4-mapped, without zone.
func validECSSubnet(p netip.Prefix) bool {
	a := p.Addr()
	switch {
	case !p.IsValid() || a.Zone() != "" || a.Is4In6():
		return false
	case a.Is4():
		return p.Bits() >= 8 && p.Bits() <= 24
	}
	return p.Bits() >= 32 && p.Bits() <= 56
}

// validateLists checks the list members of the DNS section added in 0.9.0
// (the counts are checked by Validate).
func (d *DNS) validateLists() error {
	idx := func(field string, i int) string { return field + "[" + strconv.Itoa(i) + "]" }
	for i, s := range d.RebindAllow {
		if !validHostname(s) {
			return apperr.Invalid(idx("dns.rebindAllow", i), "must be a domain name")
		}
	}
	for i, s := range d.PrivateReverseNetworks {
		if p, err := netip.ParsePrefix(s); err != nil || !validReverseNetwork(p) {
			return apperr.Invalid(idx("dns.privateReverseNetworks", i),
				"must be an IPv4 /8, /16 or /24 or an IPv6 prefix whose length is a multiple of 4 between /16 and /124")
		}
	}
	for i, s := range d.BlockedClients {
		if _, ok := ParseBlockedClient(s); !ok {
			return apperr.Invalid(idx("dns.blockedClients", i),
				"must be an IP address, a CIDR (at least /8 for IPv4, /32 for IPv6) or a MAC address")
		}
	}
	for i, s := range d.DroppedDomains {
		if _, err := ParseDroppedDomain(s); err != nil {
			return apperr.Invalid(idx("dns.droppedDomains", i), "%v", err)
		}
	}
	for i, s := range d.BogusNXDomain {
		if p, ok := parseAddrOrPrefix(s); !ok || !broadEnough(p) {
			return apperr.Invalid(idx("dns.bogusNxdomain", i), "must be an IP address or a CIDR (at least /8 for IPv4, /32 for IPv6)")
		}
	}
	for i, s := range d.EDNSClientTrusted {
		ip, err := netip.ParseAddr(s)
		if err != nil || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() {
			return apperr.Invalid(idx("dns.ednsClientTrusted", i), "must be a single IP address")
		}
	}
	switch d.ECS.Mode {
	case ECSOff, ECSClient, ECSCustom:
	default:
		return apperr.Invalid("dns.ecs.mode", "must be off, client or custom")
	}
	if d.ECS.CustomSubnet != "" {
		if p, err := netip.ParsePrefix(d.ECS.CustomSubnet); err != nil || !validECSSubnet(p) {
			return apperr.Invalid("dns.ecs.customSubnet", "must be a public IPv4 network of /8 to /24 or a public IPv6 network of /32 to /56")
		}
	} else if d.ECS.Mode == ECSCustom {
		return apperr.Invalid("dns.ecs.customSubnet", "required in custom mode")
	}
	return nil
}
