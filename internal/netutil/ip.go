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
		"2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
	)

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
// The proxy only fetches from such addresses unless explicitly allowed.
func IsPublicUnicast(ip netip.Addr) bool {
	ip = Canon(ip)
	if !ip.IsValid() {
		return false
	}
	return !inAny(ip, nonPublicPrefixes)
}

// IsPrivateLAN reports whether ip is in the default allowed client ranges.
func IsPrivateLAN(ip netip.Addr) bool { return inAny(ip, PrivateLANPrefixes) }

// IsRFC1918 reports whether ip is an RFC 1918 IPv4 address.
func IsRFC1918(ip netip.Addr) bool { return inAny(ip, rfc1918) }

// IsULA reports whether ip is an IPv6 unique local address (fc00::/7).
func IsULA(ip netip.Addr) bool { return inAny(ip, ula) }

// IsValidCacheIP reports whether clients (Steam, Riot, Origin) will accept ip
// as a LanCache address in a DNS answer: RFC 1918 IPv4 or ULA IPv6.
// (Valve also accepts 127/8 and fe80::/10, which are useless in DNS answers.)
func IsValidCacheIP(ip netip.Addr) bool { return IsRFC1918(ip) || IsULA(ip) }

// ClientKey returns the rate-limit/throttle key of a client: /32 for IPv4,
// /64 for IPv6 (a host can rotate addresses within its /64).
func ClientKey(ip netip.Addr) netip.Prefix {
	ip = Canon(ip)
	if ip.Is4() {
		return netip.PrefixFrom(ip, 32)
	}
	p, _ := ip.Prefix(64)
	return p
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
// safe to trust as clients: private IPv4 subnets with at least /8 and IPv6
// subnets with at least /48 (GUA /64s of the LAN are included). Public IPv4
// subnets (e.g. a cloud VM's on-link /20) are excluded.
func LocalSubnets() []netip.Prefix {
	ifaddrs, err := net.InterfaceAddrs()
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
		ip = Canon(ip)
		ones, _ := pn.Mask.Size()
		p := netip.PrefixFrom(ip, ones).Masked()
		if ip.Is4() {
			if ones < 8 || !IsPrivateLAN(ip) {
				continue
			}
		} else if ones < 48 {
			continue
		}
		out = append(out, p)
	}
	return out
}

// PrimaryIPv4 returns the source address the kernel would use to reach the
// Internet (no packet is sent).
func PrimaryIPv4() (netip.Addr, error) {
	conn, err := net.Dial("udp4", "192.0.2.1:9") // TEST-NET-1, never actually contacted
	if err != nil {
		return netip.Addr{}, err
	}
	defer conn.Close()
	ip := AddrFromNet(conn.LocalAddr())
	if !ip.IsValid() || ip.IsLoopback() || ip.IsUnspecified() {
		return netip.Addr{}, errors.New("no primary IPv4 address")
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
