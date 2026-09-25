// Package netutil contains the network security building blocks shared by
// the DNS server, the cache proxy, the SNI pass-through and the API: client
// ACLs, IP classification, an SSRF-safe dialer, per-client rate limiting,
// connection limiting and host normalisation.
package netutil

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// PrivateLANPrefixes are the client networks allowed by default
	// (besides private directly connected subnets).
	PrivateLANPrefixes = mustPrefixes(
		"127.0.0.0/8", "::1/128", // loopback
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", // RFC 1918
		"100.64.0.0/10",  // CGNAT (RFC 6598), e.g. Tailscale
		"169.254.0.0/16", // IPv4 link-local
		"fc00::/7",       // ULA
		"fe80::/10",      // IPv6 link-local
	)

	// nonPublicPrefixes are never valid upstream destinations for the proxy
	// (SSRF guard) unless explicitly allowed.
	nonPublicPrefixes = mustPrefixes(
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"255.255.255.255/32",
		"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b:1::/48", "100::/64", "2001::/23",
		"2001:db8::/32", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
	)

	// IPv6 ranges that embed an IPv4 address; the embedded address is
	// classified by the IPv4 rules (SSRF guard).
	nat64WKP     = netip.MustParsePrefix("64:ff9b::/96") // RFC 6052 well-known prefix
	sixToFour    = netip.MustParsePrefix("2002::/16")    // RFC 3056
	v4Compatible = netip.MustParsePrefix("::/96")        // deprecated IPv4-compatible (RFC 4291 2.5.5.1)

	// alwaysForbidden are never dialed, even when private destinations are
	// allowed (link-local incl. cloud metadata 169.254.169.254).
	alwaysForbidden = mustPrefixes("169.254.0.0/16", "fe80::/10", "0.0.0.0/8", "224.0.0.0/4", "ff00::/8")

	rfc1918 = mustPrefixes("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16")
	ula     = mustPrefixes("fc00::/7")
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// Canon returns ip unmapped and without IPv6 zone. All ACL, rate-limit and
// identity keys use canonical addresses.
func Canon(ip netip.Addr) netip.Addr { return ip.Unmap().WithZone("") }

func inAny(ip netip.Addr, ps []netip.Prefix) bool {
	ip = Canon(ip)
	for _, p := range ps {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// IsPublicUnicast reports whether ip is a globally routable unicast address
// (not private, loopback, link-local, CGNAT, multicast, documentation, …).
// IPv6 addresses that embed an IPv4 address (NAT64 64:ff9b::/96, 6to4
// 2002::/16, IPv4-compatible ::/96) are public only if the embedded IPv4
// address is. The proxy only fetches from such addresses unless explicitly
// allowed.
func IsPublicUnicast(ip netip.Addr) bool {
	ip = Canon(ip)
	if !ip.IsValid() || inAny(ip, nonPublicPrefixes) {
		return false
	}
	if v4, ok := EmbeddedIPv4(ip); ok {
		return IsPublicUnicast(v4)
	}
	return true
}

// EmbeddedIPv4 returns the IPv4 address carried in a NAT64 well-known-prefix
// (64:ff9b::/96), 6to4 (2002::/16) or IPv4-compatible (::/96) IPv6 address.
// A NAT64 gateway or 6to4 relay delivers packets for such an address to the
// embedded IPv4 destination, so security checks must apply to it.
func EmbeddedIPv4(ip netip.Addr) (netip.Addr, bool) {
	ip = Canon(ip)
	if !ip.Is6() {
		return netip.Addr{}, false
	}
	b := ip.As16()
	switch {
	case nat64WKP.Contains(ip), v4Compatible.Contains(ip):
		return netip.AddrFrom4([4]byte(b[12:16])), true
	case sixToFour.Contains(ip):
		return netip.AddrFrom4([4]byte(b[2:6])), true
	}
	return netip.Addr{}, false
}

// IsPrivateLAN reports whether ip is in the default allowed client ranges.
func IsPrivateLAN(ip netip.Addr) bool { return inAny(ip, PrivateLANPrefixes) }

// IsRFC1918 reports whether ip is an RFC 1918 IPv4 address.
func IsRFC1918(ip netip.Addr) bool { return inAny(ip, rfc1918) }

// IsULA reports whether ip is an IPv6 unique local address (fc00::/7).
func IsULA(ip netip.Addr) bool { return inAny(ip, ula) }

// IsValidCacheIP reports whether clients (Steam, Riot, Origin) will accept ip
// as a download cache address in a DNS answer: RFC 1918 IPv4 or ULA IPv6.
// (Valve also accepts 127/8 and fe80::/10, which are useless in DNS answers.)
func IsValidCacheIP(ip netip.Addr) bool { return IsRFC1918(ip) || IsULA(ip) }

// ClientKey returns the rate-limit/throttle key of a client: /32 for IPv4;
// for IPv6 the single address (/128) of LAN hosts (loopback, link-local,
// ULA, or inside a directly connected subnet, where all hosts of the LAN
// share one /64) and the /64 of other sources (an Internet host can rotate
// addresses within its /64).
func ClientKey(ip netip.Addr) netip.Prefix {
	ip = Canon(ip)
	if ip.Is4() {
		return netip.PrefixFrom(ip, 32)
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || IsULA(ip) || onLinkV6(ip) {
		return netip.PrefixFrom(ip, 128)
	}
	p, _ := ip.Prefix(64)
	return p
}

// onLinkTTL is how long the cached directly connected IPv6 subnets are used
// before they are re-read (interfaces can be renumbered).
const onLinkTTL = time.Minute

// onLink caches the directly connected IPv6 subnets for ClientKey (hot path).
var onLink struct {
	mu      sync.Mutex // one refresh at a time
	v6      atomic.Pointer[[]netip.Prefix]
	expires atomic.Int64 // unix nanoseconds
}

// interfaceAddrs lists interface addresses (replaced in tests).
var interfaceAddrs = net.InterfaceAddrs

// onLinkV6 reports whether ip is inside a directly connected IPv6 subnet.
// The subnets are re-read at most once per onLinkTTL by one caller; other
// callers keep using the previous list meanwhile.
func onLinkV6(ip netip.Addr) bool {
	ps := onLink.v6.Load()
	if ps == nil || time.Now().UnixNano() >= onLink.expires.Load() {
		if ps == nil {
			onLink.mu.Lock()
		} else if !onLink.mu.TryLock() {
			return inAny(ip, *ps)
		}
		if cur := onLink.v6.Load(); cur != nil && time.Now().UnixNano() < onLink.expires.Load() {
			ps = cur // refreshed while we waited
		} else {
			fresh := connectedV6Subnets()
			onLink.v6.Store(&fresh)
			onLink.expires.Store(time.Now().Add(onLinkTTL).UnixNano())
			ps = &fresh
		}
		onLink.mu.Unlock()
	}
	return inAny(ip, *ps)
}

// connectedV6Subnets returns the global and unique-local IPv6 subnets
// (at least /48) of this machine's interfaces.
func connectedV6Subnets() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range interfacePrefixes() {
		if a := p.Addr(); a.Is6() && p.Bits() >= 48 && !a.IsLinkLocalUnicast() && !a.IsLoopback() {
			out = append(out, p)
		}
	}
	return out
}

// interfacePrefixes returns the masked on-link prefixes of all interface
// addresses.
func interfacePrefixes() []netip.Prefix {
	ifaddrs, err := interfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Prefix
	for _, a := range ifaddrs {
		pn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(pn.IP)
		if !ok {
			continue
		}
		ones, bits := pn.Mask.Size()
		ip = Canon(ip)
		if ip.Is4() && bits == 128 { // IPv4 with an IPv6-length mask
			ones -= 96
		}
		if ones < 0 || ones > ip.BitLen() {
			continue
		}
		out = append(out, netip.PrefixFrom(ip, ones).Masked())
	}
	return out
}

// AddrFromNet extracts the canonical client IP from a net.Addr (UDP/TCP).
func AddrFromNet(a net.Addr) netip.Addr {
	switch v := a.(type) {
	case *net.UDPAddr:
		return Canon(v.AddrPort().Addr())
	case *net.TCPAddr:
		return Canon(v.AddrPort().Addr())
	}
	if a == nil {
		return netip.Addr{}
	}
	if ap, err := netip.ParseAddrPort(a.String()); err == nil {
		return Canon(ap.Addr())
	}
	return netip.Addr{}
}

// AddrFromRemote parses http.Request.RemoteAddr ("ip:port") canonically.
func AddrFromRemote(remoteAddr string) netip.Addr {
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return Canon(ap.Addr())
	}
	if ip, err := netip.ParseAddr(remoteAddr); err == nil {
		return Canon(ip)
	}
	return netip.Addr{}
}

// LocalAddrs returns all addresses of this machine's interfaces.
func LocalAddrs() []netip.Addr {
	ifaddrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range ifaddrs {
		if pn, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(pn.IP); ok {
				out = append(out, Canon(ip))
			}
		}
	}
	return out
}

// LocalSubnets returns the prefixes of directly connected networks that are
// safe to trust as clients: private subnets (entirely inside the
// PrivateLANPrefixes ranges) of at least /8 (IPv4) or /48 (IPv6). Public
// subnets of either family are never trusted automatically: a cloud VM's
// on-link /20 or a VPS's shared SLAAC /64 contains other tenants. A LAN's
// public (GUA) IPv6 prefix must be added to dns.allowedNetworks explicitly.
func LocalSubnets() []netip.Prefix {
	return privateSubnets(interfacePrefixes())
}

// privateSubnets filters on-link prefixes for LocalSubnets.
func privateSubnets(ps []netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	for _, p := range ps {
		minBits := 8
		if p.Addr().Is6() {
			minBits = 48
		}
		if p.Bits() >= minBits && prefixWithin(p, PrivateLANPrefixes) {
			out = append(out, p)
		}
	}
	return out
}

// prefixWithin reports whether all of p lies inside one of ps.
func prefixWithin(p netip.Prefix, ps []netip.Prefix) bool {
	for _, q := range ps {
		if q.Bits() <= p.Bits() && q.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

// PrimaryIPv4 returns the source address the kernel would use to reach the
// Internet (no packet is sent).
func PrimaryIPv4() (netip.Addr, error) {
	return primaryAddr("udp4", "192.0.2.1:9", "IPv4") // TEST-NET-1, never actually contacted
}

// PrimaryIPv6 returns the IPv6 source address the kernel would use to reach
// the Internet (no packet is sent). Link-local addresses are not returned.
func PrimaryIPv6() (netip.Addr, error) {
	return primaryAddr("udp6", "[2001:db8::1]:9", "IPv6") // documentation prefix, never actually contacted
}

func primaryAddr(network, target, family string) (netip.Addr, error) {
	conn, err := net.Dial(network, target)
	if err != nil {
		return netip.Addr{}, err
	}
	defer conn.Close()
	ip := AddrFromNet(conn.LocalAddr())
	if !ip.IsValid() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
		return netip.Addr{}, errors.New("no primary " + family + " address")
	}
	return ip, nil
}

// NormalizeHost lower-cases a Host header value and strips the port, IPv6
// brackets and a trailing dot. It returns ok=false for syntactically invalid
// hosts. isIP reports whether the host is an IP literal.
func NormalizeHost(h string) (host string, isIP bool, ok bool) {
	h = strings.TrimSpace(h)
	if h == "" || len(h) > 260 {
		return "", false, false
	}
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if ip, err := netip.ParseAddr(h); err == nil {
		return Canon(ip).String(), true, true
	}
	if len(h) == 0 || len(h) > 253 {
		return "", false, false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_') {
			return "", false, false
		}
	}
	if strings.Contains(h, "..") || strings.HasPrefix(h, ".") {
		return "", false, false
	}
	return h, false, true
}

// Resolver resolves a hostname to addresses (implemented by upstream.Resolver.LookupIP).
type Resolver func(ctx context.Context, host string) ([]netip.Addr, error)
