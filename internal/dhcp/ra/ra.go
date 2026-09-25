// Package ra builds the IPv6 router advertisements with which PiCache
// announces itself as DNS server (RFC 4861, RFC 8106), validates router
// solicitations and schedules the advertisements (docs/ARCHITECTURE.md 18).
//
// PiCache never becomes a router: every advertisement has router
// lifetime 0, M = 0, no prefix information, no MTU and no route
// information. It carries only the source link-layer address, RDNSS
// (PiCache's stable ULA) and DNSSL (the domain), both with a lifetime of
// 1800 s, or 0 to withdraw them. The O flag is set while stateless DHCPv6
// answers information requests.
//
// Timing: 3 initial advertisements 16 s apart, then one every random 200
// to 600 s. A valid solicitation leads to a multicast advertisement after a
// random 0 to 500 ms, at most one every 3 s.
package ra

import (
	"encoding/binary"
	"net/netip"
	"time"
)

// ICMPv6 message types and layout. The M flag (0x80, managed addresses) is
// never set: PiCache hands out no IPv6 addresses.
const (
	TypeRouterSolicitation  = 133
	TypeRouterAdvertisement = 134
	hopLimit                = 255
	headerLen               = 16 // RA: type, code, checksum, hop limit, flags, router lifetime, reachable time, retrans timer
	rsHeaderLen             = 8  // RS: type, code, checksum, reserved
	optSourceLinkLayer      = 1
	optRDNSS                = 25
	optDNSSL                = 31
	flagOtherConfig         = 0x40
	maxSolicitationLen      = 1280
	maxDomainWire           = 255
)

// Lifetime is the RDNSS and DNSSL lifetime of an announcement (3 times the
// longest interval between unsolicited advertisements, RFC 8106 5.1).
const Lifetime = 1800 * time.Second

// Advertisement is the content of one router advertisement.
type Advertisement struct {
	MAC         [6]byte    // source link-layer address (the interface's)
	DNS         netip.Addr // PiCache's stable ULA (RDNSS)
	Domain      string     // DNSSL ("" = none)
	OtherConfig bool       // O flag: stateless DHCPv6 answers information requests
}

// Marshal returns the ICMPv6 message (the kernel fills in the checksum)
// announcing the DNS server and domain for lifetime (0 withdraws them).
// Router lifetime, reachable time and retransmission timer are always 0,
// the current hop limit is 0 (unspecified) and there is no prefix option.
func (a Advertisement) Marshal(lifetime time.Duration) []byte {
	secs := uint32(max(lifetime, 0) / time.Second)
	b := make([]byte, headerLen, 64)
	b[0] = TypeRouterAdvertisement
	if a.OtherConfig {
		b[5] = flagOtherConfig
	}
	// b[4] hop limit 0, b[6:8] router lifetime 0, b[8:16] timers 0.
	b = append(b, optSourceLinkLayer, 1)
	b = append(b, a.MAC[:]...)
	if a.DNS.Is6() && !a.DNS.Is4In6() {
		b = append(b, optRDNSS, 3, 0, 0)
		b = binary.BigEndian.AppendUint32(b, secs)
		ip := a.DNS.As16()
		b = append(b, ip[:]...)
	}
	if name := EncodeDomain(a.Domain); name != nil {
		l := 8 + len(name)
		units := (l + 7) / 8
		b = append(b, optDNSSL, byte(units), 0, 0)
		b = binary.BigEndian.AppendUint32(b, secs)
		b = append(b, name...)
		b = append(b, make([]byte, units*8-l)...)
	}
	return b
}

// EncodeDomain encodes a domain name in DNS wire format without
// compression (RFC 1035 3.1); nil for an empty or invalid name.
func EncodeDomain(name string) []byte {
	if name == "" || len(name) > 253 {
		return nil
	}
	out := make([]byte, 0, len(name)+2)
	start := 0
	for i := 0; i <= len(name); i++ {
		if i < len(name) && name[i] != '.' {
			continue
		}
		l := i - start
		if l == 0 || l > 63 {
			return nil
		}
		out = append(out, byte(l))
		out = append(out, name[start:i]...)
		start = i + 1
	}
	out = append(out, 0)
	if len(out) > maxDomainWire {
		return nil
	}
	return out
}

// ValidSolicitation reports whether b is a router solicitation PiCache
// answers (RFC 4861 6.1.1): received with hop limit 255, ICMPv6 type 133
// code 0, at least 8 bytes, from a link-local or the unspecified address,
// every option with a non-zero length inside the message, and no source
// link-layer address option when the source is unspecified.
func ValidSolicitation(b []byte, hops int, src netip.Addr) bool {
	if hops != hopLimit || len(b) < rsHeaderLen || len(b) > maxSolicitationLen {
		return false
	}
	if b[0] != TypeRouterSolicitation || b[1] != 0 {
		return false
	}
	if !src.IsLinkLocalUnicast() && !src.IsUnspecified() {
		return false
	}
	for o := b[rsHeaderLen:]; len(o) > 0; {
		if len(o) < 2 {
			return false
		}
		l := int(o[1]) * 8
		if l == 0 || l > len(o) {
			return false
		}
		if o[0] == optSourceLinkLayer && src.IsUnspecified() {
			return false
		}
		o = o[l:]
	}
	return true
}

// Timing of the advertisements (RFC 4861 6.2.1, 6.2.4, 6.2.6).
const (
	InitialCount     = 3
	InitialInterval  = 16 * time.Second
	MinInterval      = 200 * time.Second
	MaxInterval      = 600 * time.Second
	MaxResponseDelay = 500 * time.Millisecond
	MinGap           = 3 * time.Second // between two multicast advertisements answering solicitations
)

// Schedule decides when advertisements are due. It is not safe for
// concurrent use. Rand returns a uniform value in [0, n) (math/rand/v2's
// Int64N; replaced in tests).
type Schedule struct {
	Rand func(n int64) int64

	count   int       // unsolicited advertisements sent since Start
	next    time.Time // next unsolicited advertisement (zero: stopped)
	pending time.Time // advertisement answering a solicitation (zero: none)
	last    time.Time // last advertisement sent
}

// Start (re)starts the unsolicited advertisements: the first is due now,
// followed by the initial burst.
func (s *Schedule) Start(now time.Time) {
	s.count, s.next, s.pending = 0, now, time.Time{}
}

// Stop ends all advertisements.
func (s *Schedule) Stop() { s.next, s.pending = time.Time{}, time.Time{} }

// Running reports whether advertisements are scheduled.
func (s *Schedule) Running() bool { return !s.next.IsZero() }

// Solicit schedules an advertisement answering a solicitation received at
// now: after a random delay of up to 500 ms, but not sooner than 3 s after
// the previous one. It reports false (nothing new is scheduled) while
// stopped, while one is pending already or when the next unsolicited
// advertisement comes first anyway.
func (s *Schedule) Solicit(now time.Time) bool {
	if s.next.IsZero() || !s.pending.IsZero() {
		return false
	}
	at := now.Add(time.Duration(s.Rand(int64(MaxResponseDelay) + 1)))
	if !s.last.IsZero() && at.Before(s.last.Add(MinGap)) {
		at = s.last.Add(MinGap)
	}
	if !s.next.After(at) {
		return false
	}
	s.pending = at
	return true
}

// Next returns when the next advertisement is due (zero: none).
func (s *Schedule) Next() time.Time {
	switch {
	case s.pending.IsZero():
		return s.next
	case s.next.IsZero() || s.pending.Before(s.next):
		return s.pending
	}
	return s.next
}

// Due reports whether an advertisement is due at now.
func (s *Schedule) Due(now time.Time) bool {
	at := s.Next()
	return !at.IsZero() && !now.Before(at)
}

// Sent records an advertisement sent at now: a due solicitation is
// answered by it, and a due unsolicited one moves the timer on (16 s
// during the initial burst, then a random 200 to 600 s).
func (s *Schedule) Sent(now time.Time) {
	s.last = now
	if !s.pending.IsZero() && !now.Before(s.pending) {
		s.pending = time.Time{}
	}
	if s.next.IsZero() || now.Before(s.next) {
		return
	}
	s.count++
	if s.count < InitialCount {
		s.next = now.Add(InitialInterval)
		return
	}
	s.next = now.Add(MinInterval + time.Duration(s.Rand(int64(MaxInterval-MinInterval)+1)))
}
