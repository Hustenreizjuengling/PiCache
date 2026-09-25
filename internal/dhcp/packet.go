package dhcp

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"slices"
)

// DHCPv4 message types (option 53, RFC 2132 9.6).
const (
	msgDiscover = 1
	msgOffer    = 2
	msgRequest  = 3
	msgDecline  = 4
	msgAck      = 5
	msgNak      = 6
	msgRelease  = 7
	msgInform   = 8
)

// BOOTP operations.
const (
	opRequest = 1
	opReply   = 2
)

// DHCPv4 option codes (RFC 2132, 3397, 4702).
const (
	optPad          = 0
	optSubnetMask   = 1
	optRouter       = 3
	optDNS          = 6
	optHostName     = 12
	optDomainName   = 15
	optBroadcast    = 28
	optRequestedIP  = 50
	optLeaseTime    = 51
	optOverload     = 52 // never honoured: sname and file are not parsed for options
	optMessageType  = 53
	optServerID     = 54
	optParamRequest = 55
	optMaxSize      = 57
	optRenewal      = 58
	optRebinding    = 59
	optClientID     = 61
	optClientFQDN   = 81
	optDomainSearch = 119
	optEnd          = 255
)

// Packet layout and sizes (RFC 2131 2). The fixed header is op, htype,
// hlen, hops (1 byte each), xid (4), secs, flags (2 each), ciaddr,
// yiaddr, siaddr, giaddr (4 each), chaddr (16), sname (64) and file (128);
// the magic cookie 99.130.83.99 starts the options.
const (
	headerLen  = 236             // fixed BOOTP header up to and including the file field
	minPacket  = headerLen + 4   // header plus the magic cookie
	maxPacket  = 1500            // larger datagrams are dropped
	minReply   = 300             // BOOTP minimum; some clients drop shorter replies
	maxReply   = 576             // default maximum reply (RFC 2131 2)
	flagBcast  = uint16(1) << 15 // BOOTP broadcast flag
	maxOptLen  = maxPacket       // bound of one (concatenated) option
	hwEther    = 1               // htype Ethernet
	hwEtherLen = 6               // hlen Ethernet
	serverPort = 67
	clientPort = 68
	magic0     = 99
	magic1     = 130
	magic2     = 83
	magic3     = 99
	optsStart  = minPacket // first option byte
	chaddrOff  = 28        // offset of chaddr
	giaddrOff  = 24        // offset of giaddr
)

var (
	errShort    = errors.New("dhcp: packet too short")
	errLong     = errors.New("dhcp: packet too long")
	errCookie   = errors.New("dhcp: bad magic cookie")
	errOptTrunc = errors.New("dhcp: truncated option")
	errNoEnd    = errors.New("dhcp: options without end")
)

// message is a parsed DHCPv4 packet. Options are the concatenated values
// per code (RFC 3396); sname and file are ignored (no option overload).
type message struct {
	op, htype, hlen, hops byte
	xid                   uint32
	secs, flags           uint16
	ciaddr, yiaddr        netip.Addr
	siaddr, giaddr        netip.Addr
	chaddr                [16]byte
	opts                  map[byte][]byte
}

// parseMessage parses a DHCPv4 packet of 240 to 1500 bytes. Every option
// is bounds-checked; a truncated option or a missing end option fails
// the whole packet. Pad options are skipped and everything after the end
// option is ignored.
func parseMessage(b []byte) (*message, error) {
	if len(b) < minPacket {
		return nil, errShort
	}
	if len(b) > maxPacket {
		return nil, errLong
	}
	if b[236] != magic0 || b[237] != magic1 || b[238] != magic2 || b[239] != magic3 {
		return nil, errCookie
	}
	m := &message{
		op: b[0], htype: b[1], hlen: b[2], hops: b[3],
		xid:    binary.BigEndian.Uint32(b[4:8]),
		secs:   binary.BigEndian.Uint16(b[8:10]),
		flags:  binary.BigEndian.Uint16(b[10:12]),
		ciaddr: addr4(b[12:16]), yiaddr: addr4(b[16:20]), siaddr: addr4(b[20:24]), giaddr: addr4(b[24:28]),
	}
	copy(m.chaddr[:], b[28:44])
	opts, err := parseOptions(b[optsStart:])
	if err != nil {
		return nil, err
	}
	m.opts = opts
	return m, nil
}

// parseOptions parses the option area. Repeated options are concatenated
// (RFC 3396), bounded by the packet size.
func parseOptions(b []byte) (map[byte][]byte, error) {
	opts := map[byte][]byte{}
	for i := 0; i < len(b); {
		code := b[i]
		switch code {
		case optPad:
			i++
			continue
		case optEnd:
			return opts, nil
		}
		if i+1 >= len(b) {
			return nil, errOptTrunc
		}
		l := int(b[i+1])
		if i+2+l > len(b) {
			return nil, errOptTrunc
		}
		if len(opts[code])+l > maxOptLen {
			return nil, errOptTrunc
		}
		opts[code] = append(opts[code], b[i+2:i+2+l]...)
		i += 2 + l
	}
	return nil, errNoEnd
}

func addr4(b []byte) netip.Addr {
	return netip.AddrFrom4([4]byte{b[0], b[1], b[2], b[3]})
}

// mac returns the client hardware address of an Ethernet request.
func (m *message) mac() [6]byte {
	var a [6]byte
	copy(a[:], m.chaddr[:6])
	return a
}

// msgType returns option 53 (0 when absent or malformed).
func (m *message) msgType() byte {
	if v := m.opts[optMessageType]; len(v) == 1 {
		return v[0]
	}
	return 0
}

// addrOpt returns a 4-byte address option (invalid when absent or malformed).
func (m *message) addrOpt(code byte) netip.Addr {
	if v := m.opts[code]; len(v) == 4 {
		return addr4(v)
	}
	return netip.Addr{}
}

// maxSize returns the largest reply the client accepts (option 57 within
// 576..1500, else 576).
func (m *message) maxSize() int {
	if v := m.opts[optMaxSize]; len(v) == 2 {
		if n := int(binary.BigEndian.Uint16(v)); n > maxReply {
			return min(n, maxPacket)
		}
	}
	return maxReply
}

// params returns the parameter request list (option 55).
func (m *message) params() []byte { return m.opts[optParamRequest] }

// hostName returns the client's host name: option 12, else the first label
// of the name in option 81 (RFC 4702), sanitised to a DNS label ("" if
// none remains).
func (m *message) hostName() string {
	if v := m.opts[optHostName]; len(v) > 0 {
		if h := SanitizeHostname(string(v)); h != "" {
			return h
		}
	}
	return SanitizeHostname(fqdnFirstLabel(m.opts[optClientFQDN]))
}

// fqdnFirstLabel returns the first label of a Client FQDN option (flags,
// two RCODE bytes, then the name: DNS wire format with the E flag, else
// ASCII).
func fqdnFirstLabel(v []byte) string {
	if len(v) < 4 {
		return ""
	}
	name := v[3:]
	if v[0]&0x04 == 0 { // ASCII (deprecated form)
		for i, c := range name {
			if c == '.' {
				return string(name[:i])
			}
		}
		return string(name)
	}
	l := int(name[0])
	if l == 0 || l > 63 || 1+l > len(name) {
		return ""
	}
	return string(name[1 : 1+l])
}

// clientID returns option 61 as colon-separated hex (at most 64 bytes
// shown; "" if absent).
func (m *message) clientID() string {
	v := m.opts[optClientID]
	if len(v) == 0 {
		return ""
	}
	if len(v) > 64 {
		v = v[:64]
	}
	const hexd = "0123456789abcdef"
	out := make([]byte, 0, 3*len(v))
	for i, c := range v {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hexd[c>>4], hexd[c&0xf])
	}
	return string(out)
}

// option is one option of a reply.
type option struct {
	code      byte
	data      []byte
	onRequest bool // sent only when the parameter request list names it
}

// replyFields are the header fields of a reply.
type replyFields struct {
	typ    byte
	ciaddr netip.Addr // echoed for INFORM and renewals
	yiaddr netip.Addr // the address given (zero for INFORM and NAK)
}

// buildReply encodes a BOOTREPLY to req. The message type (53) and the
// server identifier (54) come first, then the options the client asked for
// in its parameter request list in that order, then the remaining ones in
// the order given. Options that do not fit into the client's maximum
// message size (option 57, else 576 bytes) are left out; the reply is
// padded to 300 bytes.
func buildReply(req *message, f replyFields, serverID netip.Addr, opts []option) []byte {
	limit := req.maxSize()
	b := make([]byte, minPacket, limit)
	b[0] = opReply
	b[1], b[2] = req.htype, req.hlen
	binary.BigEndian.PutUint32(b[4:8], req.xid)
	binary.BigEndian.PutUint16(b[10:12], req.flags&flagBcast)
	put4(b[12:16], f.ciaddr)
	put4(b[16:20], f.yiaddr)
	copy(b[28:44], req.chaddr[:])
	b[236], b[237], b[238], b[239] = magic0, magic1, magic2, magic3

	add := func(o option) {
		// Room for this option and the end option.
		if len(o.data) > 255 || len(b)+2+len(o.data)+1 > limit {
			return
		}
		b = append(b, o.code, byte(len(o.data)))
		b = append(b, o.data...)
	}
	add(option{code: optMessageType, data: []byte{f.typ}})
	if serverID.IsValid() {
		add(option{code: optServerID, data: serverID.AsSlice()})
	}
	done := make([]bool, 256)
	done[optMessageType], done[optServerID] = true, true
	for _, code := range req.params() {
		if done[code] {
			continue
		}
		if i := slices.IndexFunc(opts, func(o option) bool { return o.code == code }); i >= 0 {
			add(opts[i])
			done[code] = true
		}
	}
	for _, o := range opts {
		if !done[o.code] && !o.onRequest {
			add(o)
			done[o.code] = true
		}
	}
	b = append(b, optEnd)
	for len(b) < minReply {
		b = append(b, optPad)
	}
	return b
}

func put4(dst []byte, a netip.Addr) {
	if a.Is4() {
		v := a.As4()
		copy(dst, v[:])
	}
}

func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// encodeDomain encodes a domain name in DNS wire format (RFC 1035 3.1, no
// compression); nil for an empty or invalid name.
func encodeDomain(name string) []byte {
	if name == "" || len(name) > 253 {
		return nil
	}
	var out []byte
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
	return append(out, 0)
}

// newRequest builds a BOOTREQUEST (the relay-style probe DISCOVER): hops
// 1, giaddr = relay, chaddr = mac, options message type and a parameter
// request list, padded to 300 bytes.
func newRequest(xid uint32, relay netip.Addr, mac [6]byte, typ byte) []byte {
	b := make([]byte, minPacket, minReply)
	b[0], b[1], b[2], b[3] = opRequest, hwEther, hwEtherLen, 1
	binary.BigEndian.PutUint32(b[4:8], xid)
	put4(b[giaddrOff:giaddrOff+4], relay)
	copy(b[chaddrOff:chaddrOff+6], mac[:])
	b[236], b[237], b[238], b[239] = magic0, magic1, magic2, magic3
	b = append(b, optMessageType, 1, typ)
	b = append(b, optParamRequest, 4, optSubnetMask, optRouter, optDNS, optServerID)
	b = append(b, optEnd)
	for len(b) < minReply {
		b = append(b, optPad)
	}
	return b
}
