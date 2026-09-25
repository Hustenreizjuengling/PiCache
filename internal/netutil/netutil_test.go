package netutil

import (
	"context"
	"net"
	"net/netip"
	"slices"
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
	a := NewACL([]netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}, false, false)
	for s, want := range map[string]bool{
		"192.168.1.10": true, "10.0.0.1": true, "::1": true, "203.0.113.7": true, "8.8.8.8": false,
		"fe80::1%eth0": true, "::ffff:192.168.1.1": true,
	} {
		if got := a.Allowed(netip.MustParseAddr(s)); got != want {
			t.Errorf("Allowed(%s) = %v, want %v", s, got, want)
		}
	}
	if !NewACL(nil, true, false).Allowed(netip.MustParseAddr("8.8.8.8")) {
		t.Error("allowAll must allow everything")
	}
}

func TestCacheIPAndClientKey(t *testing.T) {
	for s, want := range map[string]bool{"192.168.1.2": true, "10.0.0.1": true, "100.64.0.1": false, "8.8.8.8": false, "fd00::1": true, "2001:4860::1": false} {
		if got := IsValidCacheIP(netip.MustParseAddr(s)); got != want {
			t.Errorf("IsValidCacheIP(%s) = %v", s, got)
		}
	}
	fakeInterfaces(t) // 2001:db8:1:2::/64 is not on-link
	a := ClientKey(netip.MustParseAddr("2001:db8:1:2:aaaa::1"))
	b := ClientKey(netip.MustParseAddr("2001:db8:1:2:bbbb::9"))
	if a != b || a.Bits() != 64 {
		t.Errorf("off-link IPv6 client key must be the /64: %v %v", a, b)
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
	acl := NewACL(nil, false, false)
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

// fakeInterfaces replaces the interface address source and clears the
// on-link cache for the duration of the test.
func fakeInterfaces(t *testing.T, cidrs ...string) {
	t.Helper()
	var addrs []net.Addr
	for _, c := range cidrs {
		ip, pn, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatal(err)
		}
		pn.IP = ip
		addrs = append(addrs, pn)
	}
	old := interfaceAddrs
	interfaceAddrs = func() ([]net.Addr, error) { return addrs, nil }
	onLinkV6Cache.reset()
	t.Cleanup(func() {
		interfaceAddrs = old
		onLinkV6Cache.reset()
	})
}

func TestIsPublicUnicastEmbeddedIPv4(t *testing.T) {
	for s, want := range map[string]bool{
		"64:ff9b::a00:1":    false, // NAT64 → 10.0.0.1
		"64:ff9b::c0a8:101": false, // NAT64 → 192.168.1.1
		"64:ff9b::7f00:1":   false, // NAT64 → 127.0.0.1
		"64:ff9b::808:808":  true,  // NAT64 → 8.8.8.8
		"::a00:1":           false, // IPv4-compatible → 10.0.0.1
		"2002:a00:1::1":     false, // 6to4 → 10.0.0.1
		"2002:808:808::1":   true,  // 6to4 → 8.8.8.8
		"fec0::1":           false, // deprecated site-local
	} {
		if got := IsPublicUnicast(netip.MustParseAddr(s)); got != want {
			t.Errorf("IsPublicUnicast(%s) = %v, want %v", s, got, want)
		}
	}
	d := &SafeDialer{OwnAddrs: func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("93.184.215.14")} }}
	ctx := WithAllowPrivate(context.Background())
	for _, s := range []string{"64:ff9b::a9fe:a9fe", "64:ff9b::7f00:1", "2002:a9fe:a9fe::1", "64:ff9b::5db8:d70e"} {
		if _, err := d.Filter(ctx, []netip.Addr{netip.MustParseAddr(s)}); err == nil {
			t.Errorf("%s embeds a link-local, loopback or own IPv4 address and must always be refused", s)
		}
	}
	if _, err := d.Filter(context.Background(), []netip.Addr{netip.MustParseAddr("64:ff9b::a00:1")}); err == nil {
		t.Error("NAT64 address of a private IPv4 must be refused without allowPrivate")
	}
}

func TestLocalSubnetsPrivateOnly(t *testing.T) {
	fakeInterfaces(t, "192.168.1.10/24", "203.0.113.5/24", "172.16.5.5/8", "10.1.2.3/7",
		"fd00:1::10/64", "2001:db8:5::10/64", "fe80::1/64", "127.0.0.1/8")
	got := LocalSubnets()
	want := []string{"192.168.1.0/24", "fd00:1::/64", "fe80::/64", "127.0.0.0/8"}
	var gs []string
	for _, p := range got {
		gs = append(gs, p.String())
	}
	if !slices.Equal(gs, want) {
		t.Errorf("LocalSubnets() = %v, want %v (public subnets are never trusted automatically)", gs, want)
	}
	a := NewACL(nil, false, false)
	if a.Allowed(netip.MustParseAddr("2001:db8:5::20")) {
		t.Error("a public on-link IPv6 subnet must not be allowed by default")
	}
	if !NewACL([]netip.Prefix{netip.MustParsePrefix("2001:db8:5::/64")}, false, false).Allowed(netip.MustParseAddr("2001:db8:5::20")) {
		t.Error("an explicitly allowed GUA prefix must be allowed")
	}
}

func TestClientKeyIPv6LANHosts(t *testing.T) {
	fakeInterfaces(t, "192.168.1.10/24", "2001:db8:5::10/64")
	tests := []struct{ ip, key string }{
		{"fd00::a", "fd00::a/128"},
		{"fd00::b", "fd00::b/128"},
		{"fe80::1%eth0", "fe80::1/128"},
		{"fe80::2%wlan0", "fe80::2/128"},
		{"::1", "::1/128"},
		{"2001:db8:5::20", "2001:db8:5::20/128"},      // on-link GUA: one key per host
		{"2001:db8:9:1:aaaa::1", "2001:db8:9:1::/64"}, // off-link: the /64
		{"::ffff:192.168.1.9", "192.168.1.9/32"},
	}
	for _, tc := range tests {
		if got := ClientKey(netip.MustParseAddr(tc.ip)).String(); got != tc.key {
			t.Errorf("ClientKey(%s) = %s, want %s", tc.ip, got, tc.key)
		}
	}
	// One LAN host exhausting its bucket must not affect another.
	r := NewRateLimiter(1, 2, nil)
	a, b := netip.MustParseAddr("fd00::a"), netip.MustParseAddr("fd00::b")
	for range 5 {
		r.Allow(a)
	}
	if ok, _ := r.Allow(b); !ok {
		t.Error("another IPv6 LAN host must have its own bucket")
	}
}

func TestRateLimiterFullTableEvictsOldest(t *testing.T) {
	r := NewRateLimiter(1, 1, nil)
	r.max = 3
	ips := []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.3")}
	for _, ip := range ips {
		r.Allow(ip)
	}
	r.Allow(ips[0]) // 10.0.0.2 is now the least recently seen
	d := netip.MustParseAddr("10.0.0.4")
	if ok, _ := r.Allow(d); !ok {
		t.Fatal("first query of a new client must pass")
	}
	if ok, _ := r.Allow(d); ok {
		t.Error("a new client must be limited even when the table was full (no fail-open)")
	}
	if _, overflow := r.Dropped(); overflow != 1 {
		t.Errorf("overflow = %d, want 1", overflow)
	}
	r.mu.Lock()
	_, kept0 := r.buckets[ClientKey(ips[0])]
	_, kept1 := r.buckets[ClientKey(ips[1])]
	n := len(r.buckets)
	r.mu.Unlock()
	if n != 3 || !kept0 || kept1 {
		t.Errorf("table size %d, recent kept %v, oldest kept %v: want the least recently seen bucket evicted", n, kept0, kept1)
	}
}

func TestRateLimiterFullTableIsCheap(t *testing.T) {
	r := NewRateLimiter(50, 200, nil)
	for i := range maxBuckets {
		r.Allow(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}))
	}
	start := time.Now()
	const n = 2000
	for i := range n {
		r.Allow(netip.AddrFrom4([4]byte{172, 16, byte(i >> 8), byte(i)}))
	}
	// A full-table scan per new client took ~1 ms each (seconds in total).
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Errorf("%d new clients with a full table took %v; the full-table path must be O(1)", n, el)
	}
	if _, overflow := r.Dropped(); overflow != n {
		t.Errorf("overflow = %d, want %d", overflow, n)
	}
}

func TestRateLimiterReconfigureKeepsState(t *testing.T) {
	r := NewRateLimiter(1, 1, nil)
	ip := netip.MustParseAddr("192.168.1.2")
	r.Allow(ip)
	if ok, _ := r.Allow(ip); ok {
		t.Fatal("burst of 1 expected")
	}
	r.Reconfigure(1000, 1000, nil)
	if ok, _ := r.Allow(ip); !ok {
		t.Error("a raised limit must apply immediately to existing buckets")
	}
	if dropped, _ := r.Dropped(); dropped != 1 {
		t.Errorf("dropped = %d, want the count to survive Reconfigure", dropped)
	}
	if top := r.Top(5); len(top) != 1 || top[0].Client != "192.168.1.2/32" {
		t.Errorf("top = %+v, want drop stats kept", top)
	}
	r.mu.Lock()
	n := len(r.buckets)
	r.mu.Unlock()
	if n != 1 {
		t.Errorf("buckets = %d, want them kept", n)
	}
	r.Reconfigure(1, 1, []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")})
	for range 5 {
		if ok, _ := r.Allow(ip); !ok {
			t.Fatal("newly exempt client limited")
		}
	}
	r.Reconfigure(1, 1, nil)
	r.Allow(ip)
	if ok, _ := r.Allow(ip); ok {
		t.Error("lowered limit must apply")
	}
	r.Reconfigure(0, 0, nil)
	if ok, _ := r.Allow(ip); !ok {
		t.Error("qps 0 must disable limiting")
	}
}

func TestRateLimiterSweep(t *testing.T) {
	r := NewRateLimiter(1, 1, nil)
	for i := range 10 {
		r.Allow(netip.AddrFrom4([4]byte{10, 0, 0, byte(i)}))
	}
	r.Sweep(time.Hour)
	r.mu.Lock()
	n := len(r.buckets)
	r.mu.Unlock()
	if n != 10 {
		t.Fatalf("fresh buckets swept: %d left", n)
	}
	r.Sweep(-time.Second) // everything is idle
	r.mu.Lock()
	n, head, tail := len(r.buckets), r.newest, r.oldest
	r.mu.Unlock()
	if n != 0 || head != nil || tail != nil {
		t.Fatalf("after sweep: %d buckets, list %v/%v", n, head, tail)
	}
	if ok, _ := r.Allow(netip.MustParseAddr("10.0.0.1")); !ok {
		t.Error("a swept client starts with a full bucket")
	}
}
