package sni

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
)

// TLS constants and ClientHello bounds.
const (
	recordHeaderLen      = 5
	recordTypeHandshake  = 0x16
	maxRecordLen         = 16384 // TLSPlaintext.length limit (RFC 8446 5.1)
	maxHelloLen          = 16384 // handshake message including its 4-byte header
	maxHelloRecords      = 64    // records a fragmented ClientHello may span
	handshakeClientHello = 1
	extServerName        = 0
	nameTypeHostName     = 0
)

var (
	errNotTLS = errors.New("not a TLS handshake")
	errNoSNI  = errors.New("ClientHello without server name")
	errHello  = errors.New("malformed ClientHello")
)

// readClientHello reads TLS handshake records from r until the first
// handshake message is complete. It never reads beyond the last record it
// needs. raw holds every byte read (replayed to the upstream); hello is the
// reassembled handshake message including its 4-byte header.
func readClientHello(r io.Reader) (raw, hello []byte, err error) {
	var hdr [recordHeaderLen]byte
	for records := 0; ; records++ {
		if records == maxHelloRecords {
			return nil, nil, fmt.Errorf("ClientHello spans more than %d records", maxHelloRecords)
		}
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, nil, err
		}
		if hdr[0] != recordTypeHandshake || hdr[1] != 3 {
			return nil, nil, errNotTLS
		}
		n := int(binary.BigEndian.Uint16(hdr[3:]))
		if n == 0 || n > maxRecordLen {
			return nil, nil, fmt.Errorf("invalid TLS record length %d", n)
		}
		start := len(raw)
		raw = append(raw, hdr[:]...)
		raw = append(raw, make([]byte, n)...)
		if _, err := io.ReadFull(r, raw[start+recordHeaderLen:]); err != nil {
			return nil, nil, err
		}
		hello = append(hello, raw[start+recordHeaderLen:]...)
		if len(hello) < 4 {
			continue
		}
		if hello[0] != handshakeClientHello {
			return nil, nil, errors.New("first handshake message is not a ClientHello")
		}
		size := 4 + (int(hello[1])<<16 | int(hello[2])<<8 | int(hello[3]))
		if size > maxHelloLen {
			return nil, nil, fmt.Errorf("ClientHello of %d bytes exceeds %d", size, maxHelloLen)
		}
		if len(hello) >= size {
			return raw, hello[:size], nil
		}
	}
}

// serverName returns the host_name of the server_name extension of a
// ClientHello handshake message (RFC 8446 4.1.2, RFC 6066 3), lower-case
// and without a trailing dot.
func serverName(hello []byte) (string, error) {
	if len(hello) < 4 || hello[0] != handshakeClientHello {
		return "", errHello
	}
	b := parser(hello[4:])
	var sessionID, suites, compression, exts parser
	if !b.skip(2+32) || // legacy_version, random
		!b.vec8(&sessionID) || len(sessionID) > 32 ||
		!b.vec16(&suites) || len(suites) < 2 || len(suites)%2 != 0 ||
		!b.vec8(&compression) || len(compression) < 1 {
		return "", errHello
	}
	if len(b) == 0 {
		return "", errNoSNI // no extensions at all
	}
	if !b.vec16(&exts) || len(b) != 0 {
		return "", errHello
	}
	for len(exts) > 0 {
		var typ int
		var data parser
		if !exts.u16(&typ) || !exts.vec16(&data) {
			return "", errHello
		}
		if typ != extServerName {
			continue
		}
		var list parser
		if !data.vec16(&list) || len(data) != 0 || len(list) == 0 {
			return "", errHello
		}
		for len(list) > 0 {
			var nameType int
			var name parser
			if !list.u8(&nameType) || !list.vec16(&name) {
				return "", errHello
			}
			if nameType == nameTypeHostName {
				return normalizeServerName(string(name))
			}
		}
		return "", errNoSNI
	}
	return "", errNoSNI
}

// normalizeServerName validates a host_name: an LDH DNS name (underscores
// tolerated), not an IP literal (RFC 6066 3), at most 253 characters.
func normalizeServerName(s string) (string, error) {
	s = strings.ToLower(strings.TrimSuffix(s, "."))
	if s == "" || len(s) > 253 {
		return "", errors.New("invalid server name length")
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return "", errors.New("server name is an IP address")
	}
	label := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '.':
			if label == 0 {
				return "", errors.New("server name has an empty label")
			}
			label = 0
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
			label++
			if label > 63 {
				return "", errors.New("server name label too long")
			}
		default:
			return "", errors.New("invalid character in server name")
		}
	}
	return s, nil
}

// parser is a minimal bounds-checked reader for TLS vectors.
type parser []byte

func (p *parser) skip(n int) bool {
	if len(*p) < n {
		return false
	}
	*p = (*p)[n:]
	return true
}

func (p *parser) u8(v *int) bool {
	if len(*p) < 1 {
		return false
	}
	*v = int((*p)[0])
	*p = (*p)[1:]
	return true
}

func (p *parser) u16(v *int) bool {
	if len(*p) < 2 {
		return false
	}
	*v = int(binary.BigEndian.Uint16(*p))
	*p = (*p)[2:]
	return true
}

func (p *parser) bytes(n int, out *parser) bool {
	if len(*p) < n {
		return false
	}
	*out = (*p)[:n:n]
	*p = (*p)[n:]
	return true
}

// vec8 reads a vector with a 1-byte length prefix.
func (p *parser) vec8(out *parser) bool {
	var n int
	return p.u8(&n) && p.bytes(n, out)
}

// vec16 reads a vector with a 2-byte length prefix.
func (p *parser) vec16(out *parser) bool {
	var n int
	return p.u16(&n) && p.bytes(n, out)
}
