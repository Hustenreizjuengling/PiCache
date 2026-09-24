package netutil

import (
	"net/netip"
	"testing"
)

func TestIsPublicUnicast(t *testing.T) {
	for s, want := range map[string]bool{
		"8.8.8.8": true, "2620:fe::fe": true, "162.254.197.9": true,
		"10.1.2.3": false, "192.168.1.1": false, "172.20.0.1": false, "127.0.0.1": false,
		"100.64.1.1": false, "169.254.1.1": false, "::1": false, "fd00::1": false,
		"fe80::1": false, "224.0.0.1": false, "0.0.0.0": false, "::ffff:10.0.0.1": false,
		"192.0.2.10": false, "2001:db8::1": false,
	} {
		if got := IsPublicUnicast(netip.MustParseAddr(s)); got != want {
			t.Errorf("IsPublicUnicast(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestACL(t *testing.T) {
	a := NewACL([]netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}, false)
	for s, want := range map[string]bool{
		"192.168.1.10": true, "10.0.0.1": true, "::1": true, "203.0.113.7": true, "8.8.8.8": false,
	} {
		if got := a.Allowed(netip.MustParseAddr(s)); got != want {
			t.Errorf("Allowed(%s) = %v, want %v", s, got, want)
		}
	}
	if !NewACL(nil, true).Allowed(netip.MustParseAddr("8.8.8.8")) {
		t.Error("allowAll must allow everything")
	}
}

func TestNormalizeHost(t *testing.T) {
	cases := []struct {
		in, host string
		isIP, ok bool
	}{
		{"Cache1-FRA1.SteamContent.com:80", "cache1-fra1.steamcontent.com", false, true},
		{"example.com.", "example.com", false, true},
		{"192.168.1.5:80", "192.168.1.5", true, true},
		{"[fd00::1]", "fd00::1", true, true},
		{"[fd00::1]:8080", "fd00::1", true, true},
		{"bad host", "", false, false},
		{"a..b", "", false, false},
		{"", "", false, false},
	}
	for _, c := range cases {
		h, isIP, ok := NormalizeHost(c.in)
		if h != c.host || isIP != c.isIP || ok != c.ok {
			t.Errorf("NormalizeHost(%q) = %q,%v,%v", c.in, h, isIP, ok)
		}
	}
}

func TestRateLimiter(t *testing.T) {
	r := NewRateLimiter(1, 2, []netip.Prefix{netip.MustParsePrefix("10.9.0.0/16")})
	ip := netip.MustParseAddr("192.168.1.2")
	if !r.Allow(ip) || !r.Allow(ip) || r.Allow(ip) {
		t.Fatal("burst of 2 expected")
	}
	ex := netip.MustParseAddr("10.9.1.1")
	for range 10 {
		if !r.Allow(ex) {
			t.Fatal("exempt client limited")
		}
	}
	if NewRateLimiter(0, 0, nil).Allow(ip) != true {
		t.Fatal("qps 0 must disable")
	}
}

func TestSafeDialerFilter(t *testing.T) {
	d := &SafeDialer{OwnAddrs: func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("93.184.215.14")} }}
	if _, err := d.Filter([]netip.Addr{netip.MustParseAddr("192.168.1.1")}); err == nil {
		t.Fatal("private address must be refused")
	}
	if _, err := d.Filter([]netip.Addr{netip.MustParseAddr("93.184.215.14")}); err == nil {
		t.Fatal("own address must be refused")
	}
	got, err := d.Filter([]netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("8.8.8.8")})
	if err != nil || len(got) != 1 || got[0].String() != "8.8.8.8" {
		t.Fatalf("got %v %v", got, err)
	}
	d.AllowPrivate = func() bool { return true }
	if _, err := d.Filter([]netip.Addr{netip.MustParseAddr("192.168.1.1")}); err != nil {
		t.Fatal("allowPrivate must allow private")
	}
}
