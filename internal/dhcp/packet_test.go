package dhcp

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"net/netip"
	"slices"
	"testing"
)

var testMAC = [6]byte{0x02, 0x11, 0x22, 0x33, 0x44, 0x55}

// req is a client request under construction.
type req struct {
	op, htype, hlen byte
	xid             uint32
	flags           uint16
	ciaddr, giaddr  netip.Addr
	mac             [6]byte
	opts            [][]byte // raw options (code, len, data…)
	noEnd           bool
	sname, file     []byte
}

func newReq(typ byte) *req {
	return &req{op: opRequest, htype: hwEther, hlen: hwEtherLen, xid: 0x12345678, mac: testMAC,
		opts: [][]byte{{optMessageType, 1, typ}}}
}

func (r *req) opt(code byte, data ...byte) *req {
	r.opts = append(r.opts, append([]byte{code, byte(len(data))}, data...))
	return r
}

func (r *req) addr(code byte, a string) *req {
	return r.opt(code, netip.MustParseAddr(a).AsSlice()...)
}

func (r *req) bytes() []byte {
	b := make([]byte, minPacket)
	b[0], b[1], b[2] = r.op, r.htype, r.hlen
	binary.BigEndian.PutUint32(b[4:8], r.xid)
	binary.BigEndian.PutUint16(b[10:12], r.flags)
	put4(b[12:16], r.ciaddr)
	put4(b[24:28], r.giaddr)
	copy(b[28:34], r.mac[:])
	copy(b[44:108], r.sname)
	copy(b[108:236], r.file)
	b[236], b[237], b[238], b[239] = magic0, magic1, magic2, magic3
	for _, o := range r.opts {
		b = append(b, o...)
	}
	if !r.noEnd {
		b = append(b, optEnd)
	}
	for len(b) < minReply {
		b = append(b, optPad)
	}
	return b
}

// A request survives a round trip through the parser, options included.
func TestParseRequest(t *testing.T) {
	r := newReq(msgRequest).addr(optRequestedIP, "192.168.1.120").addr(optServerID, "192.168.1.10").
		opt(optParamRequest, optSubnetMask, optRouter, optDNS, optDomainSearch).
		opt(optHostName, []byte("Anna's iPhone")...).opt(optClientID, 1, 2, 3).opt(optMaxSize, 0x05, 0xdc)
	r.ciaddr = netip.MustParseAddr("0.0.0.0")
	r.flags = flagBcast
	m, err := parseMessage(r.bytes())
	if err != nil {
		t.Fatal(err)
	}
	if m.op != opRequest || m.xid != 0x12345678 || m.mac() != testMAC || m.flags != flagBcast || !m.giaddr.IsUnspecified() {
		t.Fatalf("header %+v", m)
	}
	if m.msgType() != msgRequest || m.addrOpt(optRequestedIP) != netip.MustParseAddr("192.168.1.120") ||
		m.addrOpt(optServerID) != netip.MustParseAddr("192.168.1.10") {
		t.Fatalf("options %v", m.opts)
	}
	if m.hostName() != "annas-iphone" || m.clientID() != "01:02:03" || m.maxSize() != 1500 {
		t.Fatalf("host %q client %q max %d", m.hostName(), m.clientID(), m.maxSize())
	}
	if !bytes.Equal(m.params(), []byte{optSubnetMask, optRouter, optDNS, optDomainSearch}) {
		t.Fatalf("params %v", m.params())
	}
}

// Malformed packets fail as a whole: too short or long, a bad cookie, a
// truncated or overlong option, no end option. Pads are skipped and bytes
// after the end option ignored.
func TestParseMalformed(t *testing.T) {
	good := newReq(msgDiscover).opt(optHostName, 'p', 'c').bytes()
	if _, err := parseMessage(good); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{
		"short":   good[:minPacket-1],
		"long":    append(slices.Clone(good), make([]byte, maxPacket)...),
		"cookie":  func() []byte { b := slices.Clone(good); b[236] = 1; return b }(),
		"no end":  newReq(msgDiscover).opt(optHostName, 'x').noEndReq().bytesNoPad(),
		"overlen": append(newReq(msgDiscover).noEndReq().bytesNoPad(), optHostName, 200, 'x'),
		"no len":  append(newReq(msgDiscover).noEndReq().bytesNoPad(), optHostName),
	} {
		if _, err := parseMessage(b); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	// Truncating the options anywhere before the end option fails (a cut
	// at an option boundary still lacks the end option).
	area := newReq(msgDiscover).opt(optHostName, 'a', 'b', 'c').opt(optClientID, 1, 2).bytesNoPad()
	for n := minPacket; n < len(area)-1; n++ {
		if _, err := parseMessage(area[:n]); err == nil {
			t.Errorf("truncated at %d parsed", n)
		}
	}
	// Pads between options; garbage after the end option.
	b := newReq(msgDiscover).bytesNoPad()
	b = b[:len(b)-1] // drop the end option
	b = append(b, optPad, optPad, optHostName, 1, 'h', optPad, optEnd, 0xff, 7, 1, 2)
	m, err := parseMessage(b)
	if err != nil || m.hostName() != "h" || len(m.opts) != 2 {
		t.Fatalf("pads/end: %v %v", m, err)
	}
}

func (r *req) noEndReq() *req { r.noEnd = true; return r }

// bytesNoPad returns the packet without the padding to 300 bytes.
func (r *req) bytesNoPad() []byte {
	b := r.bytes()
	for len(b) > minPacket && b[len(b)-1] == optPad {
		b = b[:len(b)-1]
	}
	return b
}

// Option overload (52) is never honoured: options in sname and file are
// not parsed.
func TestParseOverloadIgnored(t *testing.T) {
	r := newReq(msgDiscover).opt(optOverload, 3)
	r.sname = []byte{optHostName, 4, 'e', 'v', 'i', 'l', optEnd}
	r.file = []byte{optRequestedIP, 4, 10, 0, 0, 1, optEnd}
	m, err := parseMessage(r.bytes())
	if err != nil {
		t.Fatal(err)
	}
	if m.hostName() != "" || m.addrOpt(optRequestedIP).IsValid() {
		t.Fatalf("overloaded options were parsed: %v", m.opts)
	}
}

// Repeated options are concatenated (RFC 3396).
func TestParseConcatenates(t *testing.T) {
	m, err := parseMessage(newReq(msgDiscover).opt(optHostName, 'a', 'b').opt(optHostName, 'c').bytes())
	if err != nil || m.hostName() != "abc" {
		t.Fatalf("%v %v", m, err)
	}
}

// Option 81 (client FQDN) gives the host name when option 12 is missing,
// in DNS wire format (E flag) and the old ASCII form.
func TestHostNameFromFQDN(t *testing.T) {
	wire := newReq(msgRequest).opt(optClientFQDN, 0x04, 0, 0, 6, 'l', 'a', 'p', 't', 'o', 'p', 3, 'l', 'a', 'n', 0)
	ascii := newReq(msgRequest).opt(optClientFQDN, 0x00, 0, 0, 'D', 'e', 's', 'k', '.', 'l', 'a', 'n')
	bad := newReq(msgRequest).opt(optClientFQDN, 0x04, 0, 0, 60, 'x')
	for want, r := range map[string]*req{"laptop": wire, "desk": ascii, "": bad} {
		m, err := parseMessage(r.bytes())
		if err != nil || m.hostName() != want {
			t.Errorf("want %q, got %q (%v)", want, m.hostName(), err)
		}
	}
}

// Replies: type and server identifier first, then the requested options
// in the client's order, then the rest; on-request options only when
// asked for; at most 576 bytes (or option 57); padded to 300 bytes.
func TestBuildReply(t *testing.T) {
	r := newReq(msgDiscover).opt(optParamRequest, optDNS, optRouter, optDomainSearch)
	r.flags = flagBcast
	m, _ := parseMessage(r.bytes())
	opts := []option{
		{code: optLeaseTime, data: u32(3600)}, {code: optSubnetMask, data: []byte{255, 255, 255, 0}},
		{code: optRouter, data: []byte{192, 168, 1, 1}}, {code: optDNS, data: []byte{192, 168, 1, 10}},
		{code: optDomainSearch, data: encodeDomain("lan"), onRequest: true},
	}
	b := buildReply(m, replyFields{typ: msgOffer, yiaddr: netip.MustParseAddr("192.168.1.100")}, netip.MustParseAddr("192.168.1.10"), opts)
	if len(b) != minReply {
		t.Fatalf("length %d", len(b))
	}
	got, err := parseMessage(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.op != opReply || got.xid != m.xid || got.mac() != testMAC || got.flags != flagBcast ||
		got.yiaddr != netip.MustParseAddr("192.168.1.100") {
		t.Fatalf("header %+v", got)
	}
	var codes []byte
	for i := optsStart; b[i] != optEnd; i += 2 + int(b[i+1]) {
		codes = append(codes, b[i])
	}
	want := []byte{optMessageType, optServerID, optDNS, optRouter, optDomainSearch, optLeaseTime, optSubnetMask}
	if !bytes.Equal(codes, want) {
		t.Fatalf("option order %v, want %v", codes, want)
	}
	// Without a request for it the search list is left out.
	m2, _ := parseMessage(newReq(msgDiscover).bytes())
	got, _ = parseMessage(buildReply(m2, replyFields{typ: msgOffer}, netip.Addr{}, opts))
	if _, ok := got.opts[optDomainSearch]; ok {
		t.Fatal("unrequested domain search sent")
	}
	// Options that do not fit are dropped, the end option always fits.
	long := make([]byte, 250)
	many := []option{{code: optDomainName, data: long}, {code: 200, data: long}, {code: 201, data: long}}
	b = buildReply(m2, replyFields{typ: msgOffer}, netip.MustParseAddr("192.168.1.10"), many)
	if len(b) > maxReply {
		t.Fatalf("reply of %d bytes", len(b))
	}
	if got, err := parseMessage(b); err != nil || len(got.opts[optDomainName]) != 250 || got.opts[201] != nil {
		t.Fatalf("size limit: %v %v", got, err)
	}
	// Option 57 allows more.
	m3, _ := parseMessage(newReq(msgDiscover).opt(optMaxSize, 0x05, 0xdc).bytes())
	if got, _ := parseMessage(buildReply(m3, replyFields{typ: msgOffer}, netip.Addr{}, many)); got.opts[201] == nil {
		t.Fatal("option 57 ignored")
	}
}

// The probe DISCOVER is relay-shaped: hops 1, giaddr set, chaddr the
// interface MAC.
func TestNewRequest(t *testing.T) {
	b := newRequest(0xcafe, netip.MustParseAddr("192.168.1.10"), testMAC, msgDiscover)
	m, err := parseMessage(b)
	if err != nil {
		t.Fatal(err)
	}
	if m.op != opRequest || m.hops != 1 || m.xid != 0xcafe || m.giaddr != netip.MustParseAddr("192.168.1.10") ||
		m.mac() != testMAC || m.msgType() != msgDiscover || len(b) != minReply {
		t.Fatalf("%+v", m)
	}
}

func TestEncodeDomain(t *testing.T) {
	if got := encodeDomain("home.lan"); !bytes.Equal(got, []byte{4, 'h', 'o', 'm', 'e', 3, 'l', 'a', 'n', 0}) {
		t.Fatalf("%v", got)
	}
	for _, bad := range []string{"", "a..b", ".lan", string(make([]byte, 64)) + ".lan"} {
		if encodeDomain(bad) != nil {
			t.Errorf("%q encoded", bad)
		}
	}
}

// Random mutations of valid packets never make the parser or the reply
// builder panic.
func TestParseMutations(t *testing.T) {
	seeds := [][]byte{
		newReq(msgDiscover).opt(optHostName, 'a').opt(optParamRequest, 1, 3, 6, 119).bytes(),
		newReq(msgRequest).addr(optRequestedIP, "192.168.1.5").opt(optClientFQDN, 4, 0, 0, 3, 'a', 'b', 'c', 0).bytes(),
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 20000; i++ {
		b := slices.Clone(seeds[i%len(seeds)])
		for range 1 + rng.IntN(8) {
			switch rng.IntN(3) {
			case 0:
				b[rng.IntN(len(b))] = byte(rng.IntN(256))
			case 1:
				b = b[:minPacket+rng.IntN(len(b)-minPacket+1)]
			case 2:
				b = append(b, byte(rng.IntN(256)))
			}
		}
		exercise(b)
	}
}

// exercise runs everything the server does with an untrusted packet.
func exercise(b []byte) {
	m, err := parseMessage(b)
	if err != nil {
		return
	}
	_, _, _, _ = m.msgType(), m.hostName(), m.clientID(), m.maxSize()
	_ = m.addrOpt(optRequestedIP)
	buildReply(m, replyFields{typ: msgAck}, netip.MustParseAddr("10.0.0.1"),
		[]option{{code: optHostName, data: []byte(m.hostName())}, {code: optDomainSearch, data: encodeDomain("lan"), onRequest: true}})
}

func FuzzParseMessage(f *testing.F) {
	f.Add(newReq(msgDiscover).bytes())
	f.Add(newReq(msgRequest).opt(optClientFQDN, 4, 0, 0, 3, 'a', 'b', 'c', 0).opt(optParamRequest, 1, 3, 6).bytes())
	f.Add(newReq(msgInform).opt(optOverload, 3).bytes())
	f.Fuzz(func(t *testing.T, b []byte) { exercise(b) })
}
