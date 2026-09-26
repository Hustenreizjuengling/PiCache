package netutil

import (
	"net/netip"
	"testing"
)

func TestRebindTarget(t *testing.T) {
	dns64 := netip.MustParsePrefix("2001:db8:64::/96")
	for s, want := range map[string]bool{
		// IPv4 ranges
		"0.0.0.0":         true,
		"0.1.2.3":         true,
		"10.1.2.3":        true,
		"100.64.0.1":      true,
		"100.127.255.254": true,
		"100.128.0.1":     false,
		"127.0.0.1":       true,
		"127.1.2.3":       true,
		"169.254.169.254": true,
		"172.16.0.1":      true,
		"172.31.255.1":    true,
		"172.32.0.1":      false,
		"192.168.178.1":   true,
		"8.8.8.8":         false,
		"146.112.61.104":  false,
		"198.51.100.7":    false, // documentation space is not a rebind target
		// IPv6 ranges
		"::":                   true,
		"::1":                  true,
		"fd00::1":              true,
		"fc12::1":              true,
		"fe80::1":              true,
		"fe80::1%eth0":         true,
		"64:ff9b:1::1":         true, // RFC 8215 local-use NAT64 as a whole
		"64:ff9b:1:ffff::8":    true,
		"2001:db8::1":          false,
		"2606:4700:4700::1111": false,
		// embedded IPv4
		"::ffff:192.168.1.1":   true,  // IPv4-mapped
		"::ffff:8.8.8.8":       false, // mapped public
		"::192.168.1.1":        true,  // IPv4-compatible
		"::8.8.8.8":            false,
		"2002:c0a8:101::1":     true, // 6to4 of 192.168.1.1
		"2002:808:808::1":      false,
		"64:ff9b::a00:1":       true, // NAT64 well-known prefix → 10.0.0.1
		"64:ff9b::7f00:1":      true, // → 127.0.0.1
		"64:ff9b::808:808":     false,
		"2001:db8:64::a00:1":   true, // the configured DNS64 prefix → 10.0.0.1
		"2001:db8:64::808:808": false,
		"2001:db8:65::a00:1":   false, // outside the configured prefix
	} {
		if got := RebindTarget(netip.MustParseAddr(s), dns64); got != want {
			t.Errorf("RebindTarget(%s) = %v, want %v", s, got, want)
		}
	}
	// Without a configured prefix only the well-known one embeds.
	if RebindTarget(netip.MustParseAddr("2001:db8:64::a00:1"), netip.Prefix{}) {
		t.Error("an address outside every NAT64 prefix is not judged by its last 32 bits")
	}
}

// RateKey: LAN sources always per address; public ones by the configured
// prefix lengths. The defaults reproduce ClientKey.
func TestRateKey(t *testing.T) {
	fakeInterfaces(t, "192.168.1.10/24", "203.0.113.10/24", "2001:db8:5::10/64")
	for _, tc := range []struct {
		ip     string
		v4, v6 int
		want   string
	}{
		{"8.8.8.8", 32, 64, "8.8.8.8/32"},
		{"8.8.8.8", 24, 64, "8.8.8.0/24"},
		{"8.8.8.8", 8, 64, "8.0.0.0/8"},
		{"192.168.7.9", 24, 64, "192.168.7.9/32"}, // RFC 1918
		{"10.1.2.3", 8, 64, "10.1.2.3/32"},
		{"172.20.1.1", 16, 64, "172.20.1.1/32"},
		{"100.100.1.1", 16, 64, "100.100.1.1/32"}, // CGNAT
		{"127.0.0.1", 8, 64, "127.0.0.1/32"},
		{"169.254.1.1", 16, 64, "169.254.1.1/32"},
		{"203.0.113.77", 24, 64, "203.0.113.77/32"}, // on-link public IPv4
		{"198.51.100.77", 24, 64, "198.51.100.0/24"},
		{"::ffff:8.8.4.4", 16, 64, "8.8.0.0/16"},
		{"2001:db8:9:1:aaaa::1", 32, 64, "2001:db8:9:1::/64"},
		{"2001:db8:9:1:aaaa::1", 32, 56, "2001:db8:9::/56"},
		{"2001:db8:9:1:aaaa::1", 32, 32, "2001:db8::/32"},
		{"2001:db8:5::20", 32, 48, "2001:db8:5::20/128"}, // on-link GUA
		{"fd00::a", 32, 48, "fd00::a/128"},
		{"fe80::1%eth0", 32, 48, "fe80::1/128"},
		{"::1", 32, 32, "::1/128"},
	} {
		if got := RateKey(netip.MustParseAddr(tc.ip), tc.v4, tc.v6).String(); got != tc.want {
			t.Errorf("RateKey(%s, %d, %d) = %s, want %s", tc.ip, tc.v4, tc.v6, got, tc.want)
		}
	}
	// The defaults are ClientKey, whatever the settings say for the other
	// users of ClientKey (login throttling, fill caps, connection limits).
	for _, s := range []string{"8.8.8.8", "192.168.1.5", "2001:db8:9:1::5", "fd00::1", "2001:db8:5::20", "::ffff:1.2.3.4"} {
		ip := netip.MustParseAddr(s)
		if RateKey(ip, 32, 64) != ClientKey(ip) {
			t.Errorf("RateKey(%s, 32, 64) = %s, ClientKey = %s", s, RateKey(ip, 32, 64), ClientKey(ip))
		}
	}
}

// The limiter keys public sources by the configured prefixes: a /24 shares
// one bucket, a LAN address does not.
func TestRateLimiterPrefixKeys(t *testing.T) {
	fakeInterfaces(t, "192.168.1.10/24")
	r := NewRateLimiter(1, 2, nil)
	r.Reconfigure(1, 2, nil, 24, 56)
	a, b := netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("198.51.100.2")
	r.Allow(a)
	r.Allow(a)
	if ok, _ := r.Allow(b); ok {
		t.Error("two addresses of one public /24 must share a bucket")
	}
	if top := r.Top(5); len(top) != 1 || top[0].Client != "198.51.100.0/24" {
		t.Errorf("top = %+v, want the /24", top)
	}
	lan1, lan2 := netip.MustParseAddr("192.168.1.20"), netip.MustParseAddr("192.168.1.21")
	r.Allow(lan1)
	r.Allow(lan1)
	if ok, _ := r.Allow(lan2); !ok {
		t.Error("LAN addresses keep their own bucket")
	}
}

func TestClientList(t *testing.T) {
	l := NewClientList([]string{"192.168.1.0/24", "192.168.1.5", "AA:BB:CC:DD:EE:01", "2001:db8::/32", "garbage", "10.0.0.7"})
	if l.Len() != 5 {
		t.Fatalf("entries %v", l.Entries())
	}
	for _, tc := range []struct {
		ip, mac, want string
	}{
		{"192.168.1.5", "", "192.168.1.0/24"}, // the CIDR comes first
		{"192.168.2.5", "", ""},
		{"::ffff:192.168.1.77", "", "192.168.1.0/24"},
		{"10.0.0.7", "", "10.0.0.7"},
		{"10.0.0.7", "aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:01"}, // the MAC entry comes first
		{"", "AA:BB:CC:DD:EE:01", "aa:bb:cc:dd:ee:01"},
		{"", "aa:bb:cc:dd:ee:02", ""},
		{"2001:db8:1::1", "", "2001:db8::/32"},
		{"2001:db9::1", "", ""},
	} {
		var ip netip.Addr
		if tc.ip != "" {
			ip = netip.MustParseAddr(tc.ip)
		}
		got, ok := l.Match(ip, tc.mac)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("Match(%s, %s) = %q %v, want %q", tc.ip, tc.mac, got, ok, tc.want)
		}
	}
	if e, ok := l.MatchAddr(netip.MustParseAddr("10.0.0.7")); !ok || e != "10.0.0.7" {
		t.Errorf("MatchAddr = %q %v", e, ok)
	}
	if _, ok := l.MatchMAC(""); ok {
		t.Error("an unknown MAC never matches")
	}
	var empty *ClientList
	if _, ok := empty.Match(netip.MustParseAddr("10.0.0.7"), "aa:bb:cc:dd:ee:01"); ok || empty.Len() != 0 {
		t.Error("a nil list matches nothing")
	}
}
