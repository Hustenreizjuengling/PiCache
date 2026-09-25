// Package dhcpv6 answers stateless DHCPv6 information requests (RFC 8415
// 18.2.6, 18.3.6) with PiCache's DNS server and domain
// (docs/ARCHITECTURE.md 18). It hands out no addresses: every other
// message type is ignored by the caller.
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
)

// Option codes.
const (
	optClientID        = 1
	optServerID        = 2
	optIANA            = 3
	optIATA            = 4
	optIAPD            = 25
	optDNSServers      = 23
	optDomainList      = 24
	optInfoRefreshTime = 32
)

// Limits.
const (
	MaxMessage  = 1500 // longer datagrams are dropped
	maxDUID     = 130  // DUID type (2) + at most 128 bytes (RFC 8415 11.1)
	headerLen   = 4    // msg-type and transaction-id
	optHdrLen   = 4    // option-code and option-len
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
