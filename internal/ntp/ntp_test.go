package ntp

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type fakeEnv struct {
	enabled bool
	stratum int
	clock   ClockState
	refused atomic.Int64
	now     time.Time
}

func newServer(e *fakeEnv) *Server {
	return New(Deps{
		Settings: func() (bool, int, int, int) { return e.enabled, e.stratum, 32, 64 },
		Allowed:  func(ip netip.Addr) bool { return ip.IsPrivate() || ip.IsLoopback() },
		Refused:  func(netip.Addr) { e.refused.Add(1) },
		Clock:    func() ClockState { return e.clock },
		Now:      func() time.Time { return e.now },
	})
}

// request builds a client request (mode 3) of the given version and size.
func request(version byte, size int) []byte {
	b := make([]byte, max(size, 3))[:size]
	if size < 3 {
		return b
	}
	b[0] = version<<3 | 3
	b[2] = 6 // poll
	for i := 40; i < min(48, size); i++ {
		b[i] = byte(i) // the transmit timestamp
	}
	return b
}

func ntpTime(b []byte) time.Time {
	secs := int64(binary.BigEndian.Uint32(b[0:4])) - ntpEpochOffset
	frac := int64(binary.BigEndian.Uint32(b[4:8]))
	return time.Unix(secs, frac*int64(time.Second)>>32)
}

func TestAnswer(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 500_000_000, time.UTC)
	e := &fakeEnv{enabled: true, stratum: 3, now: now, clock: ClockState{Synced: true, MaxError: 250 * time.Millisecond}}
	s := newServer(e)
	src := netip.MustParseAddr("192.168.1.20")
	for _, v := range []byte{1, 2, 3, 4} {
		// Longer requests (extension fields, a MAC) get 48 bytes too.
		out := s.Handle(request(v, 48+68), src, now.Add(-time.Millisecond))
		if len(out) != PacketLen {
			t.Fatalf("v%d: %d bytes", v, len(out))
		}
		if out[0] != 0<<6|v<<3|4 || out[1] != 3 || out[2] != 6 || int8(out[3]) != -20 {
			t.Fatalf("v%d header % x", v, out[:4])
		}
		if binary.BigEndian.Uint32(out[4:8]) != 0 || binary.BigEndian.Uint32(out[12:16]) != 0 {
			t.Fatalf("root delay / reference ID % x", out[4:16])
		}
		if d := binary.BigEndian.Uint32(out[8:12]); d != 1<<14 { // 0.25 s in 16.16
			t.Fatalf("root dispersion %x", d)
		}
		for i := 24; i < 32; i++ {
			if out[i] != byte(i+16) {
				t.Fatalf("origin must be the request's transmit timestamp: % x", out[24:32])
			}
		}
		if got := ntpTime(out[40:48]); got.Sub(now).Abs() > time.Microsecond {
			t.Fatalf("transmit %v, want %v", got, now)
		}
		if got := ntpTime(out[32:40]); got.Sub(now.Add(-time.Millisecond)).Abs() > time.Microsecond {
			t.Fatalf("receive %v", got)
		}
		if ntpTime(out[16:24]) != ntpTime(out[40:48]) {
			t.Fatal("reference timestamp must be the transmit time")
		}
	}
	if st := s.Stats(); st.Answered != 4 {
		t.Fatalf("stats %+v", st)
	}
}

func TestDropped(t *testing.T) {
	now := time.Now()
	e := &fakeEnv{enabled: true, stratum: 3, now: now, clock: ClockState{Synced: true}}
	s := newServer(e)
	src := netip.MustParseAddr("192.168.1.20")
	for name, pkt := range map[string][]byte{
		"short":     request(4, 47),
		"empty":     {},
		"version 0": request(0, 48),
		"version 5": request(5, 48),
	} {
		if s.Handle(pkt, src, now) != nil {
			t.Errorf("%s answered", name)
		}
	}
	for _, mode := range []byte{0, 1, 2, 4, 5, 6, 7} {
		p := request(4, 48)
		p[0] = 4<<3 | mode
		if s.Handle(p, src, now) != nil {
			t.Errorf("mode %d answered", mode)
		}
	}
	// Outside the DNS ACL: dropped and counted like DNS.
	if s.Handle(request(4, 48), netip.MustParseAddr("8.8.8.8"), now) != nil || e.refused.Load() != 1 {
		t.Fatal("a source outside the ACL was answered")
	}
	// Switched off: bound but silent.
	e.enabled = false
	if s.Handle(request(4, 48), src, now) != nil {
		t.Fatal("answered while off")
	}
}

func TestRateLimit(t *testing.T) {
	now := time.Now()
	e := &fakeEnv{enabled: true, stratum: 3, now: now, clock: ClockState{Synced: true}}
	s := newServer(e)
	src := netip.MustParseAddr("10.0.0.7")
	answered := 0
	for range 20 {
		if s.Handle(request(4, 48), src, now) != nil {
			answered++
		}
	}
	if answered != rateBurst || s.Stats().Limited != 20-rateBurst {
		t.Fatalf("answered %d of 20 (burst %d), stats %+v", answered, rateBurst, s.Stats())
	}
	// Another key is not affected.
	if s.Handle(request(4, 48), netip.MustParseAddr("10.0.0.8"), now) == nil {
		t.Fatal("another source was limited")
	}
}

// Unsynchronised or unreadable clock: LI 3, stratum 16.
func TestClockStates(t *testing.T) {
	now := time.Now()
	for name, c := range map[string]ClockState{
		"unsynchronised": {Synced: false, MaxError: time.Second},
		"EPERM":          {Err: syscall.EPERM},
		"ENOSYS":         {Err: errors.New("function not implemented")},
	} {
		e := &fakeEnv{enabled: true, stratum: 2, now: now, clock: c}
		out := newServer(e).Handle(request(3, 48), netip.MustParseAddr("192.168.1.2"), now)
		if out == nil || out[0]>>6 != leapUnsync || out[1] != unsyncStratum || binary.BigEndian.Uint32(out[8:12]) != 16<<16 {
			t.Errorf("%s: % x", name, out[:12])
		}
	}
	// A huge maximum error is capped at 16 s.
	e := &fakeEnv{enabled: true, stratum: 15, now: now, clock: ClockState{Synced: true, MaxError: time.Hour}}
	out := newServer(e).Handle(request(4, 48), netip.MustParseAddr("192.168.1.2"), now)
	if out[1] != 15 || out[0]>>6 != 0 || binary.BigEndian.Uint32(out[8:12]) != 16<<16 {
		t.Fatalf("% x", out[:12])
	}
}

// Over UDP: the reply is never larger than the request.
func TestServeUDP(t *testing.T) {
	e := &fakeEnv{enabled: true, stratum: 3, now: time.Now(), clock: ClockState{Synced: true}}
	s := New(Deps{Settings: func() (bool, int, int, int) { return true, 3, 32, 64 }, Allowed: func(netip.Addr) bool { return true },
		Clock: func() ClockState { return e.clock }})
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Serve(ctx, []net.PacketConn{pc}); close(done) }()
	c, err := net.Dial("udp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write(request(4, 48))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil || n != PacketLen || buf[0]&7 != 4 {
		t.Fatalf("%d bytes, %v", n, err)
	}
	cancel()
	<-done
}

func TestReadClockDoesNotPanic(t *testing.T) {
	_ = ReadClock()
}

func FuzzHandle(f *testing.F) {
	f.Add(request(4, 48))
	f.Add(request(3, 120))
	f.Add([]byte{0x1b})
	f.Fuzz(func(t *testing.T, pkt []byte) {
		e := &fakeEnv{enabled: true, stratum: 3, now: time.Now(), clock: ClockState{Synced: true}}
		out := newServer(e).Handle(pkt, netip.MustParseAddr("192.168.1.2"), time.Now())
		if out != nil && (len(out) != PacketLen || len(pkt) < PacketLen || out[0]&7 != 4) {
			t.Fatalf("reply of %d bytes to %d bytes", len(out), len(pkt))
		}
	})
}
