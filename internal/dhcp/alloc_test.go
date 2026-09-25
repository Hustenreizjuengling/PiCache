package dhcp

import (
	"net/netip"
	"testing"
	"time"
)

var (
	ip       = netip.MustParseAddr
	t0       = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	testSelf = ip("192.168.1.10")
	testGW   = ip("192.168.1.1")
)

// testPool is 192.168.1.0/24 with the range .8–.12: .10 (PiCache) is
// skipped, so .8, .9, .11 and .12 are handed out.
func testPool() *pool {
	return newPool(netip.MustParsePrefix("192.168.1.0/24"), ip("192.168.1.8"), ip("192.168.1.12"), testSelf, testGW)
}

func TestPool(t *testing.T) {
	p := newPool(netip.MustParsePrefix("192.168.1.0/24"), ip("192.168.1.0"), ip("192.168.1.255"), testSelf, testGW)
	if n := p.size(); n != 256-4 {
		t.Fatalf("size %d", n)
	}
	for _, a := range []string{"192.168.1.0", "192.168.1.255", "192.168.1.10", "192.168.1.1", "192.168.2.5"} {
		if p.usable(ip(a)) {
			t.Errorf("%s usable", a)
		}
	}
	if !p.dynamic(ip("192.168.1.2")) || testPool().dynamic(ip("192.168.1.13")) {
		t.Fatal("range")
	}
	if lastAddr(netip.MustParsePrefix("10.1.0.0/16")) != ip("10.1.255.255") {
		t.Fatal("broadcast")
	}
}

// Candidates in order: static entry, previous lease, requested address,
// lowest free, the lease that expired first; nothing when exhausted.
func TestCandidateOrder(t *testing.T) {
	p := testPool()
	tb := newTable()
	a, b := "02:00:00:00:00:0a", "02:00:00:00:00:0b"
	got := func(mac string, requested netip.Addr, tried map[netip.Addr]bool) (netip.Addr, bool) {
		ip, check, ok := tb.candidate(p, mac, requested, tried, t0)
		if !ok {
			return netip.Addr{}, false
		}
		return ip, check
	}
	// Lowest free, with the neighbour check.
	if a1, check := got(a, netip.Addr{}, nil); a1 != ip("192.168.1.8") || !check {
		t.Fatalf("lowest free %v %v", a1, check)
	}
	// The requested address if free and in the range.
	if a1, _ := got(a, ip("192.168.1.11"), nil); a1 != ip("192.168.1.11") {
		t.Fatalf("requested %v", a1)
	}
	if a1, _ := got(a, ip("192.168.1.50"), nil); a1 != ip("192.168.1.8") {
		t.Fatalf("requested outside the range %v", a1)
	}
	// Its previous lease (expired, kept) without a check.
	tb.putLease(&lease{mac: a, ip: ip("192.168.1.12"), expires: t0.Add(-time.Hour)})
	if a1, check := got(a, ip("192.168.1.11"), nil); a1 != ip("192.168.1.12") || check {
		t.Fatalf("previous lease %v %v", a1, check)
	}
	// Another client does not get the kept address while others are free.
	if b1, _ := got(b, ip("192.168.1.12"), nil); b1 != ip("192.168.1.8") {
		t.Fatalf("kept address given away: %v", b1)
	}
	// A static entry wins, even outside the range.
	tb.putStatic(&static{mac: a, ip: ip("192.168.1.200")})
	if a1, check := got(a, netip.Addr{}, nil); a1 != ip("192.168.1.200") || check {
		t.Fatalf("static %v", a1)
	}
	// Static addresses and quarantined ones are not given to others.
	tb.putStatic(&static{mac: a, ip: ip("192.168.1.8")})
	tb.quarantine[ip("192.168.1.9")] = t0.Add(quarantine)
	if b1, _ := got(b, netip.Addr{}, nil); b1 != ip("192.168.1.11") {
		t.Fatalf("static/quarantine skipped: %v", b1)
	}
	// Exhaustion: .11 offered to another client, .12 kept for a: the
	// kept lease is reclaimed last.
	tb.putOffer("02:00:00:00:00:0c", ip("192.168.1.11"), t0.Add(offerHold))
	if b1, check := got(b, netip.Addr{}, nil); b1 != ip("192.168.1.12") || !check {
		t.Fatalf("reclaim %v %v", b1, check)
	}
	if _, ok := got(b, netip.Addr{}, map[netip.Addr]bool{ip("192.168.1.12"): true}); ok {
		t.Fatal("exhausted pool offered an address")
	}
	// An active lease is never reclaimed.
	tb.leases[a].expires = t0.Add(time.Hour)
	if _, ok := got(b, netip.Addr{}, nil); ok {
		t.Fatal("active lease reclaimed")
	}
}

// REQUEST decisions: authoritative for the subnet.
func TestDecideRequest(t *testing.T) {
	p := testPool()
	tb := newTable()
	a, b := "02:00:00:00:00:0a", "02:00:00:00:00:0b"
	tb.putLease(&lease{mac: b, ip: ip("192.168.1.9"), expires: t0.Add(time.Hour)})
	tb.putLease(&lease{mac: "02:00:00:00:00:0d", ip: ip("192.168.1.12"), expires: t0.Add(-time.Hour)})
	tb.putStatic(&static{mac: "02:00:00:00:00:0c", ip: ip("192.168.1.11")})
	tb.quarantine[ip("192.168.1.8")] = t0.Add(time.Minute)
	for _, tc := range []struct {
		mac, want string
		v         requestVerdict
	}{
		{a, "10.0.0.5", verdictNak},      // outside the subnet
		{a, "192.168.1.10", verdictNak},  // PiCache
		{a, "192.168.1.255", verdictNak}, // broadcast
		{a, "192.168.1.9", verdictNak},   // active lease of b
		{b, "192.168.1.9", verdictAck},   // b's own
		{a, "192.168.1.11", verdictNak},  // static of c
		{a, "192.168.1.8", verdictNak},   // quarantined
		{a, "192.168.1.12", verdictNak},  // kept for d
		{a, "192.168.1.50", verdictNak},  // in the subnet, outside the range
		{b, "192.168.1.100", verdictNak}, // outside the range
		{"02:00:00:00:00:0c", "192.168.1.11", verdictAck},
	} {
		if got := tb.decideRequest(p, tc.mac, ip(tc.want), t0); got != tc.v {
			t.Errorf("%s asks %s: %v, want %v", tc.mac, tc.want, got, tc.v)
		}
	}
	// No record: acknowledged after the neighbour check.
	delete(tb.quarantine, ip("192.168.1.8"))
	if got := tb.decideRequest(p, a, ip("192.168.1.8"), t0); got != verdictCheck {
		t.Fatalf("free address: %v", got)
	}
	// A client with a free static address is moved there.
	tb.putStatic(&static{mac: a, ip: ip("192.168.1.200")})
	if got := tb.decideRequest(p, a, ip("192.168.1.8"), t0); got != verdictNak {
		t.Fatalf("static elsewhere: %v", got)
	}
	if got := tb.decideRequest(p, a, ip("192.168.1.200"), t0); got != verdictAck {
		t.Fatalf("static outside the range: %v", got)
	}
}
