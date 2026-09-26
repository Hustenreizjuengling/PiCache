package ra

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"net/netip"
	"slices"
	"testing"
	"time"
)

var adv = Advertisement{MAC: [6]byte{0x02, 0xaa, 0, 0, 0, 0x10}, DNS: netip.MustParseAddr("fd00::10"), Domain: "lan", OtherConfig: true}

// The exact bytes: router lifetime 0, M 0, O 1, hop limit 0, timers 0,
// SLLAO, RDNSS (1800 s) and DNSSL (1800 s, padded to 8 bytes); nothing
// else.
func TestMarshal(t *testing.T) {
	want := []byte{
		134, 0, 0, 0, // type, code, checksum (kernel)
		0, 0x40, 0, 0, // hop limit, flags (O), router lifetime 0
		0, 0, 0, 0, 0, 0, 0, 0, // reachable time, retrans timer
		1, 1, 0x02, 0xaa, 0, 0, 0, 0x10, // source link-layer address
		25, 3, 0, 0, 0, 0, 0x07, 0x08, // RDNSS, lifetime 1800
		0xfd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x10,
		31, 2, 0, 0, 0, 0, 0x07, 0x08, // DNSSL, lifetime 1800
		3, 'l', 'a', 'n', 0, 0, 0, 0,
	}
	if got := adv.Marshal(Lifetime); !bytes.Equal(got, want) {
		t.Fatalf("advertisement\n% x\nwant\n% x", got, want)
	}
	// Withdrawal: the same with lifetimes 0.
	w := adv.Marshal(0)
	if !bytes.Equal(w[28:32], []byte{0, 0, 0, 0}) || !bytes.Equal(w[52:56], []byte{0, 0, 0, 0}) || len(w) != len(want) {
		t.Fatalf("withdrawal % x", w)
	}
	// Without DHCPv6 and a domain: no O flag, no DNSSL.
	a := adv
	a.OtherConfig, a.Domain = false, ""
	b := a.Marshal(Lifetime)
	if b[5] != 0 || len(b) != 16+8+24 || b[6] != 0 || b[7] != 0 {
		t.Fatalf("% x", b)
	}
	// A longer domain is padded to a multiple of 8 bytes.
	a.Domain = "home.example"
	b = a.Marshal(Lifetime)
	opt := b[48:]
	if opt[0] != 31 || int(opt[1])*8 != len(opt) || len(opt)%8 != 0 {
		t.Fatalf("DNSSL % x", opt)
	}
}

func TestValidSolicitation(t *testing.T) {
	ll := netip.MustParseAddr("fe80::1")
	rs := []byte{133, 0, 0, 0, 0, 0, 0, 0}
	withSLLA := append(append([]byte{}, rs...), 1, 1, 2, 0, 0, 0, 0, 1)
	for _, tc := range []struct {
		name string
		b    []byte
		hops int
		src  netip.Addr
		ok   bool
	}{
		{"plain", rs, 255, ll, true},
		{"with SLLA", withSLLA, 255, ll, true},
		{"unspecified source", rs, 255, netip.IPv6Unspecified(), true},
		{"unspecified with SLLA", withSLLA, 255, netip.IPv6Unspecified(), false},
		{"hop limit", rs, 64, ll, false},
		{"unknown hop limit", rs, -1, ll, false},
		{"global source", rs, 255, netip.MustParseAddr("2001:db8::1"), false},
		{"code", []byte{133, 1, 0, 0, 0, 0, 0, 0}, 255, ll, false},
		{"type", []byte{134, 0, 0, 0, 0, 0, 0, 0}, 255, ll, false},
		{"short", rs[:7], 255, ll, false},
		{"zero-length option", append(append([]byte{}, rs...), 1, 0), 255, ll, false},
		{"overlong option", append(append([]byte{}, rs...), 1, 2, 0, 0, 0, 0, 0, 0), 255, ll, false},
		{"odd tail", append(append([]byte{}, rs...), 1), 255, ll, false},
	} {
		if got := ValidSolicitation(tc.b, tc.hops, tc.src); got != tc.ok {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
	rng := rand.New(rand.NewPCG(3, 4))
	for range 5000 { // never panics
		b := make([]byte, rng.IntN(64))
		for i := range b {
			b[i] = byte(rng.IntN(256))
		}
		if len(b) > 0 {
			b[0] = 133
		}
		ValidSolicitation(b, 255, ll)
	}
}

// Timing: 3 advertisements 16 s apart, then every 200–600 s; a
// solicitation is answered after 0–500 ms, at most one every 3 s.
func TestSchedule(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	var s Schedule
	var draws []int64
	s.Rand = func(n int64) int64 { draws = append(draws, n); return n - 1 }
	if s.Solicit(now) {
		t.Fatal("solicited while stopped")
	}
	s.Start(now)
	var sent []time.Duration
	at := now
	for len(sent) < 5 {
		at = s.Next()
		if !s.Due(at) || s.Due(at.Add(-time.Millisecond)) {
			t.Fatalf("due at %v", at)
		}
		s.Sent(at)
		sent = append(sent, at.Sub(now))
	}
	want := []time.Duration{0, 16 * time.Second, 32 * time.Second, 32*time.Second + MaxInterval, 32*time.Second + 2*MaxInterval}
	for i := range want {
		if sent[i] != want[i] {
			t.Fatalf("send times %v, want %v", sent, want)
		}
	}
	// A solicitation 10 s after the last advertisement: answered after the
	// random delay (here the maximum, 500 ms).
	rs := at.Add(10 * time.Second)
	if !s.Solicit(rs) || s.Next() != rs.Add(MaxResponseDelay) {
		t.Fatalf("solicited answer at %v", s.Next())
	}
	if s.Solicit(rs.Add(100 * time.Millisecond)) {
		t.Fatal("second solicitation while one is pending")
	}
	s.Sent(s.Next())
	last := rs.Add(MaxResponseDelay)
	// Another one right after: not sooner than 3 s after the last.
	if !s.Solicit(last.Add(time.Second)) || s.Next() != last.Add(MinGap) {
		t.Fatalf("rate limited answer at %v, want %v", s.Next(), last.Add(MinGap))
	}
	s.Sent(s.Next())
	// The answer did not move the unsolicited timer.
	if s.Next() != now.Add(32*time.Second+3*MaxInterval) {
		t.Fatalf("unsolicited moved to %v", s.Next())
	}
	// A solicitation just before the unsolicited one: covered by it.
	if s.Solicit(s.Next().Add(-100 * time.Millisecond)) {
		t.Fatal("solicitation not merged into the due advertisement")
	}
	s.Stop()
	if s.Running() || !s.Next().IsZero() || s.Due(now.Add(time.Hour*24)) {
		t.Fatal("stopped schedule still due")
	}
	for _, n := range draws {
		if n != int64(MaxResponseDelay)+1 && n != int64(MaxInterval-MinInterval)+1 {
			t.Fatalf("random draw over %d", n)
		}
	}
}

// With the random minimum the intervals are 200 s and no delay.
func TestScheduleMinimum(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	s := Schedule{Rand: func(int64) int64 { return 0 }}
	s.Start(now)
	for range 3 {
		s.Sent(s.Next())
	}
	if s.Next() != now.Add(32*time.Second+MinInterval) {
		t.Fatalf("next %v", s.Next().Sub(now))
	}
	rs := now.Add(40 * time.Second)
	if !s.Solicit(rs) || s.Next() != rs {
		t.Fatalf("immediate answer %v", s.Next())
	}
}

func TestEncodeDomain(t *testing.T) {
	if got := EncodeDomain("a.bc"); !bytes.Equal(got, []byte{1, 'a', 2, 'b', 'c', 0}) {
		t.Fatalf("% x", got)
	}
	for _, bad := range []string{"", ".", "a..b", "a."} {
		if EncodeDomain(bad) != nil {
			t.Errorf("%q", bad)
		}
	}
}

// advMsg is a router advertisement with the given options after the header.
func advMsg(flags byte, lifetime uint16, opts ...[]byte) []byte {
	b := make([]byte, headerLen)
	b[0], b[5] = TypeRouterAdvertisement, flags
	binary.BigEndian.PutUint16(b[6:8], lifetime)
	for _, o := range opts {
		b = append(b, o...)
	}
	return b
}

func rdnss(life uint32, addrs ...string) []byte {
	b := []byte{optRDNSS, byte(1 + 2*len(addrs)), 0, 0}
	b = binary.BigEndian.AppendUint32(b, life)
	for _, a := range addrs {
		x := netip.MustParseAddr(a).As16()
		b = append(b, x[:]...)
	}
	return b
}

// Other routers' advertisements: hop limit 255, a link-local source, code
// 0, at least 16 bytes, options with a length inside the message; RDNSS of
// a wrong length is skipped, at most 8 addresses are kept.
func TestParseAdvertisement(t *testing.T) {
	ll := netip.MustParseAddr("fe80::1")
	mac := []byte{optSourceLinkLayer, 1, 0x3c, 0xa6, 0x2f, 0, 0, 1}
	b := advMsg(0xc0, 1800, mac, rdnss(600, "fd00::1", "fe80::1"))
	r, err := ParseAdvertisement(b, 255, ll)
	if err != nil || !r.Managed || !r.Other || r.RouterLifetime != 1800*time.Second || !r.HasMAC || r.SourceMAC != [6]byte(mac[2:]) ||
		len(r.DNS) != 2 || r.DNS[0].Lifetime != 600*time.Second || r.DNS[1].Addr != ll {
		t.Fatalf("%+v %v", r, err)
	}
	var many []string
	for i := range 10 {
		many = append(many, netip.AddrFrom16([16]byte{0xfd, 15: byte(i + 1)}).String())
	}
	if r, _ := ParseAdvertisement(advMsg(0, 0, rdnss(600, many...)), 255, ll); len(r.DNS) != MaxDNS {
		t.Fatalf("bounded %d", len(r.DNS))
	}
	if r, err := ParseAdvertisement(advMsg(0, 0, []byte{optRDNSS, 2, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0}), 255, ll); err != nil || len(r.DNS) != 0 {
		t.Fatalf("short RDNSS: %+v %v", r, err)
	}
	for name, tc := range map[string]struct {
		b    []byte
		hops int
		src  netip.Addr
	}{
		"hop limit":   {b, 64, ll},
		"global src":  {b, 255, netip.MustParseAddr("2001:db8::1")},
		"ipv4 src":    {b, 255, netip.MustParseAddr("192.168.1.1")},
		"short":       {b[:15], 255, ll},
		"code":        {func() []byte { c := slices.Clone(b); c[1] = 1; return c }(), 255, ll},
		"type":        {func() []byte { c := slices.Clone(b); c[0] = 133; return c }(), 255, ll},
		"zero length": {advMsg(0, 0, []byte{optRDNSS, 0, 0, 0, 0, 0, 0, 0}), 255, ll},
		"overlong":    {advMsg(0, 0, []byte{optRDNSS, 3, 0, 0}), 255, ll},
		"one byte":    {advMsg(0, 0, []byte{1}), 255, ll},
		"too long":    {append(slices.Clone(b), make([]byte, 1500)...), 255, ll},
	} {
		if _, err := ParseAdvertisement(tc.b, tc.hops, tc.src); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	// PiCache's own advertisement parses (it carries RDNSS and DNSSL).
	own := Advertisement{MAC: [6]byte{2, 0, 0, 0, 0, 1}, DNS: netip.MustParseAddr("fd00::10"), Domain: "lan", OtherConfig: true}
	if r, err := ParseAdvertisement(own.Marshal(Lifetime), 255, ll); err != nil || !r.Other || r.RouterLifetime != 0 || len(r.DNS) != 1 {
		t.Fatalf("own %+v %v", r, err)
	}
}

// The solicitation of the search: type 133 code 0 with the source
// link-layer address; a valid one by ValidSolicitation's rules.
func TestSolicitation(t *testing.T) {
	mac := [6]byte{2, 0, 0, 0, 0, 1}
	b := Solicitation(mac)
	if !ValidSolicitation(b, 255, netip.MustParseAddr("fe80::1")) {
		t.Fatalf("% x", b)
	}
	if got, ok := SolicitationSource(b); !ok || got != mac {
		t.Fatalf("source %v %v", got, ok)
	}
	if _, ok := SolicitationSource([]byte{133, 0, 0, 0, 0, 0, 0, 0}); ok {
		t.Fatal("source without the option")
	}
}

func FuzzParseAdvertisement(f *testing.F) {
	f.Add(advMsg(0xc0, 1800, []byte{optSourceLinkLayer, 1, 1, 2, 3, 4, 5, 6}, rdnss(600, "fd00::1")))
	f.Add(advMsg(0, 0, []byte{optRDNSS, 0}))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := ParseAdvertisement(b, 255, netip.MustParseAddr("fe80::1"))
		if err == nil && len(r.DNS) > MaxDNS {
			t.Fatalf("%d DNS servers", len(r.DNS))
		}
		SolicitationSource(b)
	})
}
