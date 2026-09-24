package dnsserver

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// normalizeName lower-cases s and strips surrounding space and one trailing dot.
func normalizeName(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

// fqdn returns name with a trailing dot (name is normalised).
func fqdn(name string) string { return name + "." }

// validDomain reports whether s (normalised) is a syntactically valid
// domain name: 1–63 character labels of [a-z0-9_-] not starting or ending
// with '-', at most 253 characters in total.
func validDomain(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for label := range strings.SplitSeq(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// inZone reports whether name equals zone or is below it.
func inZone(name, zone string) bool {
	return name == zone || (len(name) > len(zone) && name[len(name)-len(zone)-1] == '.' && strings.HasSuffix(name, zone))
}

// parent returns the name without its first label ("" for a single label).
func parent(name string) string {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return ""
}

// reverseName returns the in-addr.arpa / ip6.arpa name of ip (normalised).
func reverseName(ip netip.Addr) string {
	ip = ip.Unmap()
	var b strings.Builder
	if ip.Is4() {
		a := ip.As4()
		for i := 3; i >= 0; i-- {
			b.WriteString(strconv.Itoa(int(a[i])))
			b.WriteByte('.')
		}
		b.WriteString("in-addr.arpa")
		return b.String()
	}
	a := ip.As16()
	const hex = "0123456789abcdef"
	for i := 15; i >= 0; i-- {
		b.WriteByte(hex[a[i]&0x0f])
		b.WriteByte('.')
		b.WriteByte(hex[a[i]>>4])
		b.WriteByte('.')
	}
	b.WriteString("ip6.arpa")
	return b.String()
}

// parseReverse parses a complete reverse name back into an address.
func parseReverse(name string) (netip.Addr, bool) {
	switch {
	case strings.HasSuffix(name, ".in-addr.arpa"):
		labels := strings.Split(strings.TrimSuffix(name, ".in-addr.arpa"), ".")
		if len(labels) != 4 {
			return netip.Addr{}, false
		}
		var a [4]byte
		for i, l := range labels {
			n, err := strconv.ParseUint(l, 10, 8)
			if err != nil || (len(l) > 1 && l[0] == '0') {
				return netip.Addr{}, false
			}
			a[3-i] = byte(n)
		}
		return netip.AddrFrom4(a), true
	case strings.HasSuffix(name, ".ip6.arpa"):
		labels := strings.Split(strings.TrimSuffix(name, ".ip6.arpa"), ".")
		if len(labels) != 32 {
			return netip.Addr{}, false
		}
		var a [16]byte
		for i, l := range labels {
			if len(l) != 1 {
				return netip.Addr{}, false
			}
			n, err := strconv.ParseUint(l, 16, 8)
			if err != nil {
				return netip.Addr{}, false
			}
			pos := 15 - i/2
			if i%2 == 0 {
				a[pos] |= byte(n)
			} else {
				a[pos] |= byte(n) << 4
			}
		}
		return netip.AddrFrom16(a), true
	}
	return netip.Addr{}, false
}

// privateReverseZones are the locally served reverse zones of RFC 6303 §4
// (plus the whole ULA range fc00::/7) and RFC 7793 (100.64.0.0/10).
var privateReverseZones = func() map[string]bool {
	m := map[string]bool{}
	for _, z := range []string{
		"10.in-addr.arpa", "168.192.in-addr.arpa", // RFC 1918
		"0.in-addr.arpa", "127.in-addr.arpa", "254.169.in-addr.arpa", // this network, loopback, link-local
		"2.0.192.in-addr.arpa", "100.51.198.in-addr.arpa", "113.0.203.in-addr.arpa", // documentation
		"255.255.255.255.in-addr.arpa",               // broadcast
		strings.Repeat("0.", 32) + "ip6.arpa",        // ::
		"1." + strings.Repeat("0.", 31) + "ip6.arpa", // ::1
		"c.f.ip6.arpa", "d.f.ip6.arpa", // ULA fc00::/7
		"8.e.f.ip6.arpa", "9.e.f.ip6.arpa", "a.e.f.ip6.arpa", "b.e.f.ip6.arpa", // link-local fe80::/10
		"8.b.d.0.1.0.0.2.ip6.arpa", // documentation 2001:db8::/32
	} {
		m[z] = true
	}
	for i := 16; i <= 31; i++ {
		m[fmt.Sprintf("%d.172.in-addr.arpa", i)] = true
	}
	for i := 64; i <= 127; i++ {
		m[fmt.Sprintf("%d.100.in-addr.arpa", i)] = true
	}
	return m
}()

// privateReverseZone returns the locally served reverse zone containing name.
func privateReverseZone(name string) (string, bool) {
	if !strings.HasSuffix(name, ".arpa") {
		return "", false
	}
	for n := name; n != ""; n = parent(n) {
		if privateReverseZones[n] {
			return n, true
		}
	}
	return "", false
}

// specialZones are special-use domains that never reach the default
// upstreams (RFC 6761, RFC 7686, RFC 6762, ICANN .internal).
var specialZones = []string{"test", "invalid", "onion", "internal", "local"}

// reservedRecordName reports names that the pipeline answers before local
// records, so records for them would never be served.
func reservedRecordName(name string) bool {
	base := strings.TrimPrefix(name, "*.")
	return inZone(base, "localhost") || inZone(base, "resolver.arpa")
}
