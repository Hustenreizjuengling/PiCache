// Package dhcpv6 answers stateless DHCPv6 information requests (RFC 8415
// 18.2.6, 18.3.6) with PiCache's DNS server and domain
// (docs/ARCHITECTURE.md 18). It hands out no addresses: every other
// message type is ignored by the caller. It also builds the relayed
// information request with which PiCache looks for other DHCPv6 servers
// and parses their relayed replies (RFC 8415 9, 19).
//
// A REPLY carries the server identifier (a DUID-LL from the interface's
// MAC), the client's identifier (echoed when it sent one), DNS_SERVERS
// (23), DOMAIN_LIST (24, when a domain is set) and
// INFORMATION_REFRESH_TIME (32). Every option of a request is
// bounds-checked; malformed requests are dropped.
package dhcpv6

import (
	"encoding/binary"
	"errors"
	"net/netip"
)

// Message types.
const (
	MsgReply              = 7
	MsgInformationRequest = 11
	MsgRelayForward       = 12
	MsgRelayReply         = 13
)

// Option codes.
const (
	optClientID        = 1
	optServerID        = 2
	optIANA            = 3
	optIATA            = 4
	optORO             = 6
	optElapsedTime     = 8
	optRelayMsg        = 9
	optInterfaceID     = 18
	optIAPD            = 25
	optDNSServers      = 23
	optDomainList      = 24
	optInfoRefreshTime = 32
)

// Limits.
const (
	MaxMessage = 1500 // longer datagrams are dropped
	maxDUID    = 130  // DUID type (2) + at most 128 bytes (RFC 8415 11.1)
	headerLen  = 4    // msg-type and transaction-id
	relayLen   = 34   // msg-type, hop-count, link-address, peer-address
	optHdrLen  = 4    // option-code and option-len
	// MaxDNS is the number of DNS servers kept of one reply.
	MaxDNS      = 8
	duidTypeLL  = 3
	hwEthernet  = 1
	RefreshTime = 3600 // INFORMATION_REFRESH_TIME in seconds
)

// Errors of Parse. ErrIgnored marks a well-formed message PiCache does not
// answer (another message type, or a request for another server).
var (
	ErrIgnored   = errors.New("dhcpv6: not an information request for this server")
	ErrMalformed = errors.New("dhcpv6: malformed message")
)

// Request is a parsed information request.
type Request struct {
	TxID     [3]byte
	ClientID []byte // the client's DUID (nil if absent)
}

// Parse validates an information request for the server with the DUID
// serverID: ErrMalformed for a message shorter than 4 bytes, longer than
// 1500 bytes, with a truncated option, an empty or overlong DUID or a
// repeated identifier; ErrIgnored for other message types, a server
// identifier of another server and requests carrying an IA option (RFC
// 8415 16.12).
func Parse(b []byte, serverID []byte) (Request, error) {
	var r Request
	if len(b) < headerLen || len(b) > MaxMessage {
		return r, ErrMalformed
	}
	copy(r.TxID[:], b[1:4])
	ignored := b[0] != MsgInformationRequest
	var sawClient, sawServer bool
	for o := b[headerLen:]; len(o) > 0; {
		if len(o) < optHdrLen {
			return r, ErrMalformed
		}
		code := binary.BigEndian.Uint16(o[0:2])
		l := int(binary.BigEndian.Uint16(o[2:4]))
		if optHdrLen+l > len(o) {
			return r, ErrMalformed
		}
		v := o[optHdrLen : optHdrLen+l]
		switch code {
		case optClientID:
			if sawClient || l < 3 || l > maxDUID {
				return r, ErrMalformed
			}
			sawClient = true
			r.ClientID = append([]byte(nil), v...)
		case optServerID:
			if sawServer || l < 3 || l > maxDUID {
				return r, ErrMalformed
			}
			sawServer = true
			if string(v) != string(serverID) {
				ignored = true
			}
		case optIANA, optIATA, optIAPD:
			ignored = true
		}
		o = o[optHdrLen+l:]
	}
	if ignored {
		return r, ErrIgnored
	}
	return r, nil
}

// DUIDLL returns the DUID-LL (type 3, hardware type 1) of an Ethernet
// address (RFC 8415 11.4).
func DUIDLL(mac [6]byte) []byte {
	b := make([]byte, 0, 10)
	b = binary.BigEndian.AppendUint16(b, duidTypeLL)
	b = binary.BigEndian.AppendUint16(b, hwEthernet)
	return append(b, mac[:]...)
}

// Reply builds the REPLY to req: the server identifier, the client
// identifier (if the request had one), the DNS servers, the domain list
// (domain in DNS wire format; nil for none) and the information refresh
// time.
func Reply(req Request, serverID []byte, dns []netip.Addr, domain []byte) []byte {
	b := make([]byte, 0, 128)
	b = append(b, MsgReply, req.TxID[0], req.TxID[1], req.TxID[2])
	opt := func(code uint16, v []byte) {
		b = binary.BigEndian.AppendUint16(b, code)
		b = binary.BigEndian.AppendUint16(b, uint16(len(v)))
		b = append(b, v...)
	}
	opt(optServerID, serverID)
	if req.ClientID != nil {
		opt(optClientID, req.ClientID)
	}
	var servers []byte
	for _, ip := range dns {
		if ip.Is6() && !ip.Is4In6() {
			a := ip.As16()
			servers = append(servers, a[:]...)
		}
	}
	if len(servers) > 0 {
		opt(optDNSServers, servers)
	}
	if len(domain) > 0 {
		opt(optDomainList, domain)
	}
	opt(optInfoRefreshTime, binary.BigEndian.AppendUint32(nil, RefreshTime))
	return b
}

// appendOpt appends one option.
func appendOpt(b []byte, code uint16, v []byte) []byte {
	b = binary.BigEndian.AppendUint16(b, code)
	b = binary.BigEndian.AppendUint16(b, uint16(len(v)))
	return append(b, v...)
}

// RelayForward builds PiCache's search for other DHCPv6 servers: a
// Relay-Forward (type 12, hop count 0, link-address link, peer-address
// peer) carrying RELAY_MSG with an Information-Request (transaction ID
// txid, CLIENTID clientDUID, ELAPSED_TIME 0, ORO DNS_SERVERS and
// DOMAIN_LIST) and INTERFACE_ID ifaceID. Only an Information-Request: no
// Solicit, no IA and no rapid commit, so it never creates a binding.
func RelayForward(txid [3]byte, clientDUID []byte, link, peer netip.Addr, ifaceID []byte) []byte {
	inner := []byte{MsgInformationRequest, txid[0], txid[1], txid[2]}
	inner = appendOpt(inner, optClientID, clientDUID)
	inner = appendOpt(inner, optElapsedTime, []byte{0, 0})
	inner = appendOpt(inner, optORO, []byte{0, optDNSServers, 0, optDomainList})
	b := make([]byte, 0, relayLen+len(inner)+optHdrLen*2+len(ifaceID))
	b = append(b, MsgRelayForward, 0)
	l, p := link.As16(), peer.As16()
	b = append(b, l[:]...)
	b = append(b, p[:]...)
	b = appendOpt(b, optRelayMsg, inner)
	return appendOpt(b, optInterfaceID, ifaceID)
}

// RelayReply is a server's answer to the search.
type RelayReply struct {
	Link, Peer netip.Addr
	TxID       [3]byte      // of the Reply inside
	ServerID   []byte       // its SERVERID (DUID)
	DNS        []netip.Addr // DNS_SERVERS, at most MaxDNS
}

// ParseRelayReply parses a Relay-Reply (type 13) of 34 to 1500 bytes that
// carries exactly one RELAY_MSG with a Reply (type 7, not another relay
// message) with exactly one SERVERID of 3 to 130 bytes. Every option is
// bounds-checked; ErrMalformed otherwise, ErrIgnored for other message
// types.
func ParseRelayReply(b []byte) (RelayReply, error) {
	var r RelayReply
	if len(b) < relayLen || len(b) > MaxMessage {
		return r, ErrMalformed
	}
	if b[0] != MsgRelayReply {
		return r, ErrIgnored
	}
	r.Link = netip.AddrFrom16([16]byte(b[2:18]))
	r.Peer = netip.AddrFrom16([16]byte(b[18:34]))
	var inner []byte
	seen := false
	if err := eachOpt(b[relayLen:], func(code uint16, v []byte) error {
		if code == optRelayMsg {
			if seen {
				return ErrMalformed
			}
			seen, inner = true, v
		}
		return nil
	}); err != nil || !seen {
		return RelayReply{}, ErrMalformed
	}
	if len(inner) < headerLen || inner[0] != MsgReply {
		return RelayReply{}, ErrMalformed
	}
	copy(r.TxID[:], inner[1:4])
	if err := eachOpt(inner[headerLen:], func(code uint16, v []byte) error {
		switch code {
		case optServerID:
			if r.ServerID != nil || len(v) < 3 || len(v) > maxDUID {
				return ErrMalformed
			}
			r.ServerID = append([]byte(nil), v...)
		case optDNSServers:
			if len(v)%16 != 0 {
				return ErrMalformed
			}
			for ; len(v) >= 16 && len(r.DNS) < MaxDNS; v = v[16:] {
				r.DNS = append(r.DNS, netip.AddrFrom16([16]byte(v[:16])))
			}
		}
		return nil
	}); err != nil || r.ServerID == nil {
		return RelayReply{}, ErrMalformed
	}
	return r, nil
}

// eachOpt calls fn for every option of b (bounds-checked).
func eachOpt(b []byte, fn func(code uint16, v []byte) error) error {
	for len(b) > 0 {
		if len(b) < optHdrLen {
			return ErrMalformed
		}
		code := binary.BigEndian.Uint16(b[0:2])
		l := int(binary.BigEndian.Uint16(b[2:4]))
		if optHdrLen+l > len(b) {
			return ErrMalformed
		}
		if err := fn(code, b[optHdrLen:optHdrLen+l]); err != nil {
			return err
		}
		b = b[optHdrLen+l:]
	}
	return nil
}
