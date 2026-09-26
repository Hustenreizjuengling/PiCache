package dhcpv6

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"net/netip"
	"slices"
	"testing"
)

var serverID = DUIDLL([6]byte{0x02, 0xaa, 0, 0, 0, 0x10})

func opt(code uint16, v ...byte) []byte {
	return append([]byte{byte(code >> 8), byte(code), byte(len(v) >> 8), byte(len(v))}, v...)
}

func msg(typ byte, opts ...[]byte) []byte {
	b := []byte{typ, 0xab, 0xcd, 0xef}
	for _, o := range opts {
		b = append(b, o...)
	}
	return b
}

var clientDUID = []byte{0, 3, 0, 1, 0x02, 0, 0, 0, 0, 1}

func TestDUIDLL(t *testing.T) {
	if !bytes.Equal(serverID, []byte{0, 3, 0, 1, 0x02, 0xaa, 0, 0, 0, 0x10}) {
		t.Fatalf("% x", serverID)
	}
}

// The exact REPLY: type 7, the transaction id, SERVERID, CLIENTID
// (echoed), DNS_SERVERS, DOMAIN_LIST, INFORMATION_REFRESH_TIME 3600.
func TestReply(t *testing.T) {
	req, err := Parse(msg(MsgInformationRequest, opt(optClientID, clientDUID...), opt(6, 0, 23, 0, 24)), serverID)
	if err != nil {
		t.Fatal(err)
	}
	got := Reply(req, serverID, []netip.Addr{netip.MustParseAddr("fd00::10")}, []byte{3, 'l', 'a', 'n', 0})
	want := msg(MsgReply,
		opt(optServerID, serverID...),
		opt(optClientID, clientDUID...),
		opt(optDNSServers, 0xfd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x10),
		opt(optDomainList, 3, 'l', 'a', 'n', 0),
		opt(optInfoRefreshTime, 0, 0, 0x0e, 0x10),
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("reply\n% x\nwant\n% x", got, want)
	}
	// Without a client identifier and a domain.
	req, _ = Parse(msg(MsgInformationRequest), serverID)
	got = Reply(req, serverID, []netip.Addr{netip.MustParseAddr("fd00::10")}, nil)
	var codes []uint16
	for o := got[4:]; len(o) >= 4; o = o[4+int(o[2])<<8+int(o[3]):] {
		codes = append(codes, uint16(o[0])<<8|uint16(o[1]))
	}
	if want := []uint16{optServerID, optDNSServers, optInfoRefreshTime}; !slices.Equal(codes, want) {
		t.Fatalf("options %v, want %v", codes, want)
	}
}

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    []byte
		err  error
	}{
		{"plain", msg(MsgInformationRequest), nil},
		{"our server id", msg(MsgInformationRequest, opt(optServerID, serverID...)), nil},
		{"unknown options", msg(MsgInformationRequest, opt(8, 0, 0), opt(999)), nil},
		{"solicit", msg(1, opt(optClientID, clientDUID...)), ErrIgnored},
		{"request", msg(3), ErrIgnored},
		{"reply", msg(MsgReply), ErrIgnored},
		{"other server", msg(MsgInformationRequest, opt(optServerID, 0, 3, 0, 1, 9, 9, 9, 9, 9, 9)), ErrIgnored},
		{"IA_NA", msg(MsgInformationRequest, opt(optIANA, make([]byte, 12)...)), ErrIgnored},
		{"IA_PD", msg(MsgInformationRequest, opt(optIAPD, make([]byte, 12)...)), ErrIgnored},
		{"short", []byte{MsgInformationRequest, 0, 0}, ErrMalformed},
		{"long", append(msg(MsgInformationRequest), make([]byte, MaxMessage)...), ErrMalformed},
		{"truncated header", append(msg(MsgInformationRequest), 0, 1, 0), ErrMalformed},
		{"truncated value", append(msg(MsgInformationRequest), 0, 1, 0, 10, 1, 2), ErrMalformed},
		{"empty DUID", msg(MsgInformationRequest, opt(optClientID)), ErrMalformed},
		{"long DUID", msg(MsgInformationRequest, opt(optClientID, make([]byte, 131)...)), ErrMalformed},
		{"two client ids", msg(MsgInformationRequest, opt(optClientID, clientDUID...), opt(optClientID, clientDUID...)), ErrMalformed},
	} {
		if _, err := Parse(tc.b, serverID); !errors.Is(err, tc.err) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.err)
		}
	}
	r, _ := Parse(msg(MsgInformationRequest, opt(optClientID, clientDUID...)), serverID)
	if r.TxID != [3]byte{0xab, 0xcd, 0xef} || !bytes.Equal(r.ClientID, clientDUID) {
		t.Fatalf("%+v", r)
	}
}

// Random bytes never make Parse or Reply panic.
func TestParseRandom(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	for range 20000 {
		b := make([]byte, rng.IntN(80))
		for i := range b {
			b[i] = byte(rng.IntN(256))
		}
		if len(b) > 0 && rng.IntN(2) == 0 {
			b[0] = MsgInformationRequest
		}
		if r, err := Parse(b, serverID); err == nil {
			Reply(r, serverID, []netip.Addr{netip.MustParseAddr("fd00::1")}, nil)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add(msg(MsgInformationRequest, opt(optClientID, clientDUID...)))
	f.Add(msg(MsgInformationRequest, opt(optServerID, serverID...), opt(6, 0, 23)))
	f.Fuzz(func(t *testing.T, b []byte) {
		if r, err := Parse(b, serverID); err == nil {
			Reply(r, serverID, []netip.Addr{netip.MustParseAddr("fd00::1")}, []byte{1, 'a', 0})
		}
	})
}

// The search: a Relay-Forward (hop count 0, link and peer address) with an
// Information-Request (CLIENTID, ELAPSED_TIME 0, ORO 23 and 24) and the
// interface id; nothing that could create a binding.
func TestRelayForward(t *testing.T) {
	link, peer := netip.MustParseAddr("fd00::10"), netip.MustParseAddr("fe80::10")
	b := RelayForward([3]byte{1, 2, 3}, serverID, link, peer, []byte("eth0"))
	if b[0] != MsgRelayForward || b[1] != 0 || netip.AddrFrom16([16]byte(b[2:18])) != link || netip.AddrFrom16([16]byte(b[18:34])) != peer {
		t.Fatalf("header % x", b[:34])
	}
	var inner, iface []byte
	if err := eachOpt(b[relayLen:], func(code uint16, v []byte) error {
		switch code {
		case optRelayMsg:
			inner = v
		case optInterfaceID:
			iface = v
		default:
			t.Errorf("option %d", code)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if string(iface) != "eth0" || inner[0] != MsgInformationRequest || !bytes.Equal(inner[1:4], []byte{1, 2, 3}) {
		t.Fatalf("inner % x iface %q", inner, iface)
	}
	want := map[uint16][]byte{optClientID: serverID, optElapsedTime: {0, 0}, optORO: {0, 23, 0, 24}}
	if err := eachOpt(inner[headerLen:], func(code uint16, v []byte) error {
		if w, ok := want[code]; !ok || !bytes.Equal(v, w) {
			t.Errorf("inner option %d = % x", code, v)
		}
		delete(want, code)
		return nil
	}); err != nil || len(want) != 0 {
		t.Fatalf("inner options: %v, missing %v", err, want)
	}
	// The information request inside is one PiCache itself would answer.
	if _, err := Parse(inner, nil); err != nil {
		t.Fatalf("inner request: %v", err)
	}
}

// relayReply wraps opts in a Reply inside a Relay-Reply.
func relayReply(peer netip.Addr, inner []byte, extra ...[]byte) []byte {
	b := []byte{MsgRelayReply, 0}
	l, p := netip.MustParseAddr("fd00::10").As16(), peer.As16()
	b = append(b, l[:]...)
	b = append(b, p[:]...)
	b = append(b, opt(optRelayMsg, inner...)...)
	for _, e := range extra {
		b = append(b, e...)
	}
	return b
}

// A Relay-Reply with exactly one level of RELAY_MSG holding a Reply with
// one SERVERID parses; everything else is refused.
func TestParseRelayReply(t *testing.T) {
	peer := netip.MustParseAddr("fe80::10")
	dns := netip.MustParseAddr("fd00::53").As16()
	reply := msg(MsgReply, opt(optServerID, clientDUID...), opt(optDNSServers, dns[:]...))
	r, err := ParseRelayReply(relayReply(peer, reply, opt(optInterfaceID, 'e')))
	if err != nil || r.Peer != peer || r.TxID != [3]byte{0xab, 0xcd, 0xef} || !bytes.Equal(r.ServerID, clientDUID) ||
		!slices.Equal(r.DNS, []netip.Addr{netip.MustParseAddr("fd00::53")}) {
		t.Fatalf("%+v %v", r, err)
	}
	many := make([]byte, 0, 16*12)
	for range 12 {
		many = append(many, dns[:]...)
	}
	if r, err := ParseRelayReply(relayReply(peer, msg(MsgReply, opt(optServerID, clientDUID...), opt(optDNSServers, many...)))); err != nil ||
		len(r.DNS) != MaxDNS {
		t.Fatalf("bounded DNS: %d %v", len(r.DNS), err)
	}
	nested := relayReply(peer, reply)
	for name, b := range map[string][]byte{
		"short":         relayReply(peer, reply)[:30],
		"nested relay":  relayReply(peer, nested),
		"no relay msg":  relayReply(peer, reply)[:34],
		"two relay msg": relayReply(peer, reply, opt(optRelayMsg, reply...)),
		"advertise":     relayReply(peer, msg(2, opt(optServerID, clientDUID...))),
		"no server id":  relayReply(peer, msg(MsgReply, opt(optDNSServers, dns[:]...))),
		"two server id": relayReply(peer, msg(MsgReply, opt(optServerID, clientDUID...), opt(optServerID, clientDUID...))),
		"odd dns":       relayReply(peer, msg(MsgReply, opt(optServerID, clientDUID...), opt(optDNSServers, 1, 2, 3))),
		"truncated":     relayReply(peer, reply)[:len(relayReply(peer, reply))-1],
		"long":          append(relayReply(peer, reply), make([]byte, MaxMessage)...),
	} {
		if _, err := ParseRelayReply(b); err == nil {
			t.Errorf("%s parsed", name)
		}
	}
	if _, err := ParseRelayReply(RelayForward([3]byte{}, serverID, peer, peer, nil)); !errors.Is(err, ErrIgnored) {
		t.Fatalf("relay forward: %v", err)
	}
}

func FuzzParseRelayReply(f *testing.F) {
	peer := netip.MustParseAddr("fe80::10")
	dns := netip.MustParseAddr("fd00::53").As16()
	f.Add(relayReply(peer, msg(MsgReply, opt(optServerID, clientDUID...), opt(optDNSServers, dns[:]...))))
	f.Add(relayReply(peer, relayReply(peer, msg(MsgReply))))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := ParseRelayReply(b)
		if err != nil {
			return
		}
		if len(r.DNS) > MaxDNS || len(r.ServerID) < 3 || len(r.ServerID) > maxDUID {
			t.Fatalf("out of bounds: %+v", r)
		}
	})
}
