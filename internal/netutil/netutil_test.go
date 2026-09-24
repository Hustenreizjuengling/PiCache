package netutil

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
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
		"fe80::1%eth0": true, "::ffff:192.168.1.1": true,
	} {
		if got := a.Allowed(netip.MustParseAddr(s)); got != want {
			t.Errorf("Allowed(%s) = %v, want %v", s, got, want)
		}
	}
	if !NewACL(nil, true).Allowed(netip.MustParseAddr("8.8.8.8")) {
		t.Error("allowAll must allow everything")
	}
}

func TestCacheIPAndClientKey(t *testing.T) {
	for s, want := range map[string]bool{"192.168.1.2": true, "10.0.0.1": true, "100.64.0.1": false, "8.8.8.8": false, "fd00::1": true, "2001:4860::1": false} {
		if got := IsValidCacheIP(netip.MustParseAddr(s)); got != want {
			t.Errorf("IsValidCacheIP(%s) = %v", s, got)
		}
	}
	a := ClientKey(netip.MustParseAddr("2001:db8:1:2:aaaa::1"))
	b := ClientKey(netip.MustParseAddr("2001:db8:1:2:bbbb::9"))
	if a != b || a.Bits() != 64 {
		t.Errorf("IPv6 client key must be the /64: %v %v", a, b)
	}
	if k := ClientKey(netip.MustParseAddr("::ffff:192.168.1.9")); k.String() != "192.168.1.9/32" {
		t.Errorf("IPv4 key %v", k)
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
	ok1, _ := r.Allow(ip)
	ok2, _ := r.Allow(ip)
	ok3, first := r.Allow(ip)
	if !ok1 || !ok2 || ok3 || !first {
		t.Fatal("burst of 2 expected, first drop flagged")
	}
	if _, first := r.Allow(ip); first {
		t.Fatal("second drop must not be flagged first")
	}
	if top := r.Top(5); len(top) != 1 || top[0].Dropped != 2 {
		t.Fatalf("top = %+v", top)
	}
	ex := netip.MustParseAddr("10.9.1.1")
	for range 10 {
		if ok, _ := r.Allow(ex); !ok {
			t.Fatal("exempt client limited")
		}
	}
	if ok, _ := NewRateLimiter(0, 0, nil).Allow(ip); !ok {
		t.Fatal("qps 0 must disable")
	}
}

func TestSafeDialerFilter(t *testing.T) {
	ctx := context.Background()
	d := &SafeDialer{OwnAddrs: func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("93.184.215.14")} }}
	if _, err := d.Filter(ctx, []netip.Addr{netip.MustParseAddr("192.168.1.1")}); err == nil {
		t.Fatal("private address must be refused")
	}
	if _, err := d.Filter(ctx, []netip.Addr{netip.MustParseAddr("93.184.215.14")}); err == nil {
		t.Fatal("own address must be refused")
	}
	got, err := d.Filter(ctx, []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("8.8.8.8")})
	if err != nil || len(got) != 1 || got[0].String() != "8.8.8.8" {
		t.Fatalf("got %v %v", got, err)
	}
	pctx := WithAllowPrivate(ctx)
	if _, err := d.Filter(pctx, []netip.Addr{netip.MustParseAddr("192.168.1.1")}); err != nil {
		t.Fatal("WithAllowPrivate must allow private")
	}
	if _, err := d.Filter(pctx, []netip.Addr{netip.MustParseAddr("169.254.169.254")}); err == nil {
		t.Fatal("link-local must always be refused")
	}
}

func TestLimitListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	acl := NewACL(nil, false)
	ll := LimitListener(ln, func() *ACL { return acl }, 1, 10)
	defer ll.Close()
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ll.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	c1, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	first := <-accepted
	c2, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	// Second connection from the same client exceeds perClient=1 → closed by server.
	_ = c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := c2.Read(buf); err == nil {
		t.Fatal("expected second connection to be closed")
	}
	first.Close() // releases the slot
	if UnwrapTCP(first) == nil {
		t.Fatal("UnwrapTCP must return the TCP conn")
	}
}
