package ra

import (
	"bytes"
	"math/rand/v2"
	"net/netip"
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
