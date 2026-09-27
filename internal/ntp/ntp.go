// Package ntp is PiCache's NTP server (docs/ARCHITECTURE.md 2 and 6.1
// "NTP"): the server side of SNTP (RFC 4330) on PICACHE_NTP_LISTEN (UDP),
// answering only while ntp.enabled is on. It imports only foundation
// packages; the ACL, the limiter settings and the clock reader are passed
// in by app.
//
// Rules: only client requests (mode 3, version 1–4, at least 48 bytes;
// extension fields and a MAC are ignored) are answered, with exactly 48
// bytes (never more than the request): mode 4, the request's version, its
// transmit timestamp as origin, receive and transmit timestamps from the
// system clock, poll copied, precision −20, root delay 0, root dispersion
// the kernel's maximum error (capped at 16 s), reference ID 0, reference
// timestamp the transmit time. While the host clock is synchronised the
// leap indicator is 0 and the stratum ntp.stratum, otherwise (or when the
// clock state cannot be read) 3 and 16. Every other mode and every
// malformed packet is dropped silently (no Kiss-o'-Death packets), and so
// is every packet while the server is switched off, from a source the DNS
// ACL refuses (counted like DNS) or over the rate limit of its key
// (netutil.RateKey: 4 packets/s, burst 8, 16 384 keys, the least recently
// seen evicted).
package ntp

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// Bounds of the server.
const (
	PacketLen      = 48
	maxPacket      = 1500
	rateQPS        = 4
	rateBurst      = 8
	rateKeys       = 16384
	precisionByte  = 0xec // −20 as a signed byte: about a microsecond
	maxDispersion  = 16 * time.Second
	unsyncStratum  = 16
	leapUnsync     = 3
	clockCacheTime = time.Second
	// ntpEpochOffset is the seconds from 1900-01-01 to 1970-01-01.
	ntpEpochOffset = 2208988800
)

// ClockState is the state of the host clock (adjtimex with modes 0).
type ClockState struct {
	Synced   bool          // STA_UNSYNC clear and the state not TIME_ERROR
	MaxError time.Duration // the kernel's maximum error
	Err      error         // the state cannot be read (EPERM: an old unit; ENOSYS)
}

// Deps are the collaborators of the server.
type Deps struct {
	// Settings returns ntp.enabled, ntp.stratum and the rate-limit prefix
	// lengths of DNS (the keys of netutil.RateKey).
	Settings func() (enabled bool, stratum, v4Bits, v6Bits int)
	// Allowed is the DNS ACL; Refused counts a refused source like DNS.
	Allowed func(netip.Addr) bool
	Refused func(netip.Addr)
	// Clock reads the state of the host clock (ReadClock).
	Clock func() ClockState
	Now   func() time.Time // nil: time.Now
	Log   *slog.Logger
}

// Stats are the counters of the server.
type Stats struct {
	Answered int64 `json:"answered"`
	Dropped  int64 `json:"dropped"` // malformed, other modes, switched off
	Refused  int64 `json:"refused"` // outside the DNS ACL
	Limited  int64 `json:"limited"` // over the rate limit
}

// Server answers NTP requests.
type Server struct {
	d   Deps
	lim *netutil.RateLimiter

	answered, dropped, refused, limited atomic.Int64
	keyBits                             atomic.Int64 // v4<<8 | v6 of the limiter's keys (reconfigured on change)

	clockMu sync.Mutex
	clockAt time.Time
	clock   ClockState
}

// New returns a server.
func New(d Deps) *Server {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	return &Server{d: d, lim: netutil.NewRateLimiterMax(rateQPS, rateBurst, rateKeys)}
}

// Stats returns the counters.
func (s *Server) Stats() Stats {
	return Stats{Answered: s.answered.Load(), Dropped: s.dropped.Load(), Refused: s.refused.Load(), Limited: s.limited.Load()}
}

// ClockState returns the state of the host clock (read at most once a
// second).
func (s *Server) ClockState() ClockState {
	s.clockMu.Lock()
	defer s.clockMu.Unlock()
	now := s.d.Now()
	if s.clockAt.IsZero() || now.Sub(s.clockAt) >= clockCacheTime || now.Before(s.clockAt) {
		s.clock, s.clockAt = s.d.Clock(), now
	}
	return s.clock
}

// Serve answers on the packet connections until ctx ends (it closes
// them then). It blocks until every reader returned.
func (s *Server) Serve(ctx context.Context, conns []net.PacketConn) error {
	var wg sync.WaitGroup
	for _, pc := range conns {
		wg.Go(func() { s.read(ctx, pc) })
	}
	<-ctx.Done()
	for _, pc := range conns {
		_ = pc.Close()
	}
	wg.Wait()
	return nil
}

func (s *Server) read(ctx context.Context, pc net.PacketConn) {
	buf := make([]byte, maxPacket)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		recv := s.d.Now()
		src := netutil.AddrFromNet(from)
		reply := s.Handle(buf[:n], src, recv)
		if reply != nil {
			_, _ = pc.WriteTo(reply, from)
		}
	}
}

// Handle returns the reply to one datagram from src received at recv, or
// nil to drop it.
func (s *Server) Handle(pkt []byte, src netip.Addr, recv time.Time) []byte {
	enabled, stratum, v4Bits, v6Bits := s.d.Settings()
	if !enabled {
		s.dropped.Add(1)
		return nil
	}
	src = netutil.Canon(src)
	if !src.IsValid() || s.d.Allowed == nil || !s.d.Allowed(src) {
		s.refused.Add(1)
		if s.d.Refused != nil && src.IsValid() {
			s.d.Refused(src)
		}
		return nil
	}
	if len(pkt) < PacketLen {
		s.dropped.Add(1)
		return nil
	}
	version, mode := (pkt[0]>>3)&0x7, pkt[0]&0x7
	if mode != 3 || version < 1 || version > 4 {
		s.dropped.Add(1)
		return nil
	}
	if bits := int64(v4Bits)<<8 | int64(v6Bits); s.keyBits.Swap(bits) != bits {
		s.lim.Reconfigure(rateQPS, rateBurst, nil, v4Bits, v6Bits)
	}
	if ok, _ := s.lim.Allow(src); !ok {
		s.limited.Add(1)
		return nil
	}
	clock := s.ClockState()
	leap, st := byte(0), byte(stratum)
	if !clock.Synced || clock.Err != nil {
		leap, st = leapUnsync, unsyncStratum
	}
	out := make([]byte, PacketLen)
	out[0] = leap<<6 | version<<3 | 4
	out[1] = st
	out[2] = pkt[2] // poll
	out[3] = precisionByte
	// Root delay 0 (bytes 4–7); root dispersion in NTP short format.
	disp := min(max(clock.MaxError, 0), maxDispersion)
	if clock.Err != nil || !clock.Synced {
		disp = maxDispersion
	}
	binary.BigEndian.PutUint32(out[8:12], uint32(disp*(1<<16)/time.Second))
	// Reference ID 0 (bytes 12–15).
	copy(out[24:32], pkt[40:48]) // origin: the request's transmit timestamp
	putTime(out[32:40], recv)
	tx := s.d.Now()
	putTime(out[16:24], tx) // reference timestamp: the transmit time
	putTime(out[40:48], tx)
	s.answered.Add(1)
	return out
}

// putTime writes t as an NTP timestamp (seconds since 1900 and a 32-bit
// fraction; era 0 wraps in 2036 as NTP does).
func putTime(b []byte, t time.Time) {
	secs := uint64(t.Unix()) + ntpEpochOffset
	frac := uint64(t.Nanosecond()) << 32 / uint64(time.Second)
	binary.BigEndian.PutUint32(b[0:4], uint32(secs))
	binary.BigEndian.PutUint32(b[4:8], uint32(frac))
}
