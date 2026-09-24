// Package netutil contains the network security building blocks shared by
// the DNS server, the cache proxy, the SNI pass-through and the API: client
// ACLs, IP classification, an SSRF-safe dialer, per-client rate limiting and
// host normalisation.
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
	// (besides directly connected subnets).
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

	// cacheIPPrefixes are the ranges clients accept as a LanCache address
	// (Valve: RFC 1918, 127/8, fc00::/7, fe80::/10).
	cacheIPPrefixes = mustPrefixes("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8", "fc00::/7", "fe80::/10")
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

func inAny(ip netip.Addr, ps []netip.Prefix) bool {
	ip = ip.Unmap()
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
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	return !inAny(ip, nonPublicPrefixes)
}

// IsPrivateLAN reports whether ip is in the default allowed client ranges.
func IsPrivateLAN(ip netip.Addr) bool { return inAny(ip, PrivateLANPrefixes) }

// IsValidCacheIP reports whether clients (Steam, Riot, …) will accept ip as a
// LanCache address (they only downgrade to HTTP for private addresses).
func IsValidCacheIP(ip netip.Addr) bool { return inAny(ip, cacheIPPrefixes) }

// AddrFromNet extracts the client IP from a net.Addr (UDP/TCP), unmapped.
func AddrFromNet(a net.Addr) netip.Addr {
	switch v := a.(type) {
	case *net.UDPAddr:
		return v.AddrPort().Addr().Unmap()
	case *net.TCPAddr:
		return v.AddrPort().Addr().Unmap()
	}
	if ap, err := netip.ParseAddrPort(a.String()); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.Addr{}
}

// AddrFromRemote parses http.Request.RemoteAddr ("ip:port").
func AddrFromRemote(remoteAddr string) netip.Addr {
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return ap.Addr().Unmap()
	}
	if ip, err := netip.ParseAddr(remoteAddr); err == nil {
		return ip.Unmap()
	}
	return netip.Addr{}
}

// LocalAddrs returns all unicast addresses of this machine's interfaces.
func LocalAddrs() []netip.Addr {
	ifaddrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range ifaddrs {
		if pn, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(pn.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out
}

// LocalSubnets returns the prefixes of directly connected networks.
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
		ones, _ := pn.Mask.Size()
		out = append(out, netip.PrefixFrom(ip.Unmap(), ones).Masked())
	}
	return out
}

// PrimaryIPv4 returns the source address the kernel would use to reach the
// Internet (no packet is sent). Used to auto-detect the cache IP.
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
		return ip.Unmap().String(), true, true
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
