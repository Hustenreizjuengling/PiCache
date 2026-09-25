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
