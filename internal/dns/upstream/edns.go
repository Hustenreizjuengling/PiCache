package upstream

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"unicode/utf8"

	"github.com/miekg/dns"
)

// EDE limits: only the first maxEDEOptions options of an upstream OPT are
// examined, and the text is cut to maxEDEText bytes.
const (
	maxEDEOptions = 16
	maxEDEText    = 200
)

// EDE is the Extended DNS Error (RFC 8914) of an upstream reply.
type EDE struct {
	Code uint16
	Text string // sanitised (SanitizeEDEText): at most 200 bytes of valid UTF-8 without control or bidi characters
}

// errECSMismatch discards a reply whose client subnet option differs from
// the one sent (RFC 7871 7.3).
var errECSMismatch = errors.New("reply carries another client subnet than the query")

// parseEDE examines the first maxEDEOptions options of m's OPT (untrusted).
// blocking is the first EDE option with code 15 (Blocked), 16 (Censored) or
// 17 (Filtered); logged is that one, else the first EDE option (nil if
// none).
func parseEDE(m *dns.Msg) (blocking, logged *EDE) {
	opt := m.IsEdns0()
	if opt == nil {
		return nil, nil
	}
	var first *dns.EDNS0_EDE
	for i, o := range opt.Option {
		if i == maxEDEOptions {
			break
		}
		e, ok := o.(*dns.EDNS0_EDE)
		if !ok {
			continue
		}
		if first == nil {
			first = e
		}
		if blockingEDE(e.InfoCode) {
			b := &EDE{Code: e.InfoCode, Text: SanitizeEDEText(e.ExtraText)}
			return b, b
		}
	}
	if first == nil {
		return nil, nil
	}
	return nil, &EDE{Code: first.InfoCode, Text: SanitizeEDEText(first.ExtraText)}
}

// blockingEDE reports the EDE codes of a filtering resolver: 15 Blocked,
// 16 Censored, 17 Filtered.
func blockingEDE(code uint16) bool {
	return code == dns.ExtendedErrorCodeBlocked || code == dns.ExtendedErrorCodeCensored || code == dns.ExtendedErrorCodeFiltered
}

// SanitizeEDEText makes the text of an upstream EDE safe to store and show:
// cut to 200 bytes at a rune boundary, made valid UTF-8, and stripped of C0
// controls, DEL, C1 controls and the bidi controls U+061C, U+200E, U+200F,
// U+202A–U+202E and U+2066–U+2069.
func SanitizeEDEText(s string) string {
	if len(s) > maxEDEText {
		i := maxEDEText
		for i > 0 && !utf8.RuneStart(s[i]) {
			i--
		}
		s = s[:i]
	}
	s = strings.ToValidUTF8(s, "")
	return strings.Map(func(r rune) rune {
		if unwantedEDERune(r) {
			return -1
		}
		return r
	}, s)
}

// unwantedEDERune reports control and bidi characters.
func unwantedEDERune(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		return true
	case r == 0x061c, r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// addECS adds the client subnet option for p (SCOPE 0, the address masked
// to the source prefix) to q's OPT.
func addECS(q *dns.Msg, p netip.Prefix) {
	opt := q.IsEdns0()
	if opt == nil || !p.IsValid() {
		return
	}
	p = p.Masked()
	e := &dns.EDNS0_SUBNET{Code: dns.EDNS0SUBNET, SourceNetmask: uint8(p.Bits()), Family: 1}
	if p.Addr().Is6() {
		e.Family = 2
	}
	e.Address = net.IP(p.Addr().AsSlice())
	opt.Option = append(opt.Option, e)
}

// ecsOf returns the first client subnet option of m's OPT as a masked
// prefix; false if there is none or it is not a usable family 1 or 2
// option.
func ecsOf(m *dns.Msg) (netip.Prefix, bool) {
	opt := m.IsEdns0()
	if opt == nil {
		return netip.Prefix{}, false
	}
	for _, o := range opt.Option {
		if e, ok := o.(*dns.EDNS0_SUBNET); ok {
			return ecsPrefix(e)
		}
	}
	return netip.Prefix{}, false
}

// ecsPrefix converts a client subnet option: family 1 with a source prefix
// of at most 32, family 2 with at most 128; the address masked to it.
func ecsPrefix(e *dns.EDNS0_SUBNET) (netip.Prefix, bool) {
	var ip netip.Addr
	switch e.Family {
	case 1:
		v4 := e.Address.To4()
		if v4 == nil || e.SourceNetmask > 32 {
			return netip.Prefix{}, false
		}
		ip = netip.AddrFrom4([4]byte(v4))
	case 2:
		v6 := e.Address.To16()
		if v6 == nil || e.SourceNetmask > 128 {
			return netip.Prefix{}, false
		}
		ip = netip.AddrFrom16([16]byte(v6))
	default:
		return netip.Prefix{}, false
	}
	p, err := ip.Prefix(int(e.SourceNetmask))
	return p, err == nil
}

// checkECS verifies the client subnet of a reply to a query that sent one:
// a reply option must have the family, source prefix length and address
// that were sent. A reply without the option is accepted, and a subnet
// option in a reply to a query without one is ignored.
func checkECS(q, m *dns.Msg) error {
	sent, ok := ecsOf(q)
	if !ok {
		return nil
	}
	opt := m.IsEdns0()
	if opt == nil {
		return nil
	}
	for _, o := range opt.Option {
		e, isECS := o.(*dns.EDNS0_SUBNET)
		if !isECS {
			continue
		}
		got, ok := ecsPrefix(e)
		if !ok || got != sent {
			return errECSMismatch
		}
		return nil
	}
	return nil
}
