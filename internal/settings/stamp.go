package settings

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// DNS stamps (sdns://…) describe an upstream in one string: its protocol,
// address, TLS name, certificate hashes and, for DNSCrypt, the provider
// key and name (docs/ARCHITECTURE.md 7.4). Only DNSCrypt, DoH, DoT and DoQ
// stamps are accepted.

// Limits and types of DNS stamps.
const (
	MaxStampLen   = 1024 // characters of the whole "sdns://…" string
	stampScheme   = "sdns://"
	stampDNSCrypt = 0x01
	stampDoH      = 0x02
	stampDoT      = 0x03
	stampDoQ      = 0x04
)

// Stamp errors (the upstream fields of the settings name them).
var (
	errStampTooLong    = errors.New("a DNS stamp is at most 1024 characters")
	errStampType       = errors.New("unsupported DNS stamp type: only DNSCrypt, DoH, DoT and DoQ stamps are supported")
	errStampShortInput = errors.New("the stamp ends early")
)

// isStamp reports whether s is a DNS stamp (the scheme case-insensitive).
func isStamp(s string) bool {
	return len(s) >= len(stampScheme) && strings.EqualFold(s[:len(stampScheme)], stampScheme)
}

// stampError wraps a parse failure as "invalid DNS stamp: <reason>".
func stampError(reason error) error { return fmt.Errorf("invalid DNS stamp: %w", reason) }

// stampReader reads the length-prefixed fields of a stamp; every length is
// checked against the remaining bytes.
type stampReader struct {
	b   []byte
	err error
}

// lp reads one length-prefixed field (LP).
func (r *stampReader) lp(what string) []byte {
	if r.err != nil {
		return nil
	}
	if len(r.b) < 1 {
		r.err = fmt.Errorf("%s: %w", what, errStampShortInput)
		return nil
	}
	n := int(r.b[0])
	if len(r.b)-1 < n {
		r.err = fmt.Errorf("%s: its length exceeds the stamp", what)
		return nil
	}
	v := r.b[1 : 1+n]
	r.b = r.b[1+n:]
	return v
}

// vlp reads a set of length-prefixed fields (VLP: every length but the
// last has the high bit set; a single 0 is the empty set). At most max
// entries are accepted.
func (r *stampReader) vlp(what string, max int) [][]byte {
	if r.err != nil {
		return nil
	}
	var out [][]byte
	for {
		if len(r.b) < 1 {
			r.err = fmt.Errorf("%s: %w", what, errStampShortInput)
			return nil
		}
		more := r.b[0]&0x80 != 0
		n := int(r.b[0] & 0x7f)
		if len(r.b)-1 < n {
			r.err = fmt.Errorf("%s: its length exceeds the stamp", what)
			return nil
		}
		if n > 0 || more {
			if len(out) == max {
				r.err = fmt.Errorf("%s: more than %d entries", what, max)
				return nil
			}
			out = append(out, r.b[1:1+n])
		}
		r.b = r.b[1+n:]
		if !more {
			return out
		}
	}
}

// maxStampHashes bounds the certificate hashes of one stamp.
const maxStampHashes = 16

// parseStamp parses a DNS stamp into spec (Raw is set by the caller). The
// base64url text has no padding; every length is checked and no bytes may
// follow the last field (the optional bootstrap addresses of DoH, DoT and
// DoQ stamps are read and ignored: dns.bootstrap is used).
func parseStamp(s string, spec *UpstreamSpec) error {
	if len(s) > MaxStampLen {
		return errStampTooLong
	}
	enc := s[len(stampScheme):]
	if strings.ContainsAny(enc, "=") {
		return stampError(errors.New("base64url without padding expected"))
	}
	bin, err := base64.RawURLEncoding.Strict().DecodeString(enc)
	if err != nil {
		return stampError(errors.New("not base64url"))
	}
	if len(bin) < 1 {
		return stampError(errStampShortInput)
	}
	typ := bin[0]
	switch typ {
	case stampDNSCrypt, stampDoH, stampDoT, stampDoQ:
	default:
		return errStampType
	}
	if len(bin) < 9 {
		return stampError(fmt.Errorf("properties: %w", errStampShortInput))
	}
	r := &stampReader{b: bin[9:]} // the 8 bytes of properties are ignored
	spec.Stamp = true
	addr := r.lp("address")
	if typ == stampDNSCrypt {
		pk := r.lp("provider key")
		name := r.lp("provider name")
		if r.err != nil {
			return stampError(r.err)
		}
		if len(r.b) != 0 {
			return stampError(errors.New("unexpected bytes after the last field"))
		}
		ip, port, err := stampAddr(string(addr))
		if err != nil {
			return stampError(err)
		}
		if !ip.IsValid() {
			return stampError(errors.New("a DNSCrypt stamp needs an IP address"))
		}
		if len(pk) != 32 {
			return stampError(errors.New("the provider key must be 32 bytes"))
		}
		provider := strings.ToLower(strings.TrimSuffix(string(name), "."))
		if !validHostname(provider) {
			return stampError(errors.New("invalid provider name"))
		}
		if port == 0 {
			port = 443
		}
		spec.Proto = "dnscrypt"
		spec.Host, spec.ProviderName = provider, provider
		copy(spec.ProviderKey[:], pk)
		spec.Port = int(port)
		spec.DialAddr = netip.AddrPortFrom(ip, port)
		return nil
	}
	hashes := r.vlp("certificate hashes", maxStampHashes)
	hostPort := r.lp("host name")
	var path []byte
	if typ == stampDoH {
		path = r.lp("path")
	}
	if r.err == nil && len(r.b) > 0 {
		_ = r.vlp("bootstrap addresses", maxStampHashes) // optional; ignored
	}
	if r.err != nil {
		return stampError(r.err)
	}
	if len(r.b) != 0 {
		return stampError(errors.New("unexpected bytes after the last field"))
	}
	for _, h := range hashes {
		if len(h) != 32 {
			return stampError(errors.New("a certificate hash must be 32 bytes (SHA-256)"))
		}
		spec.Pins = append(spec.Pins, [32]byte(h))
	}
	ip, addrPort, err := stampAddr(string(addr))
	if err != nil {
		return stampError(err)
	}
	host, hostPortNum, err := stampHost(string(hostPort))
	if err != nil {
		return stampError(err)
	}
	def := map[byte]uint16{stampDoH: 443, stampDoT: 853, stampDoQ: 853}[typ]
	port := addrPort
	if port == 0 {
		port = hostPortNum
	}
	if port == 0 {
		port = def
	}
	spec.Host, spec.Port = host, int(port)
	if hip, err := netip.ParseAddr(host); err == nil {
		spec.IsIPLit = true
		if !ip.IsValid() {
			ip = hip
		}
	}
	if ip.IsValid() {
		spec.DialAddr = netip.AddrPortFrom(ip, port)
	}
	switch typ {
	case stampDoH:
		p := string(path)
		if p == "" {
			p = "/dns-query"
		}
		if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "#? \t\r\n") || !printableASCII(p) {
			return stampError(errors.New("invalid path"))
		}
		spec.Proto = "https"
		h := host
		if strings.Contains(h, ":") {
			h = "[" + h + "]"
		}
		if port != 443 {
			h += ":" + strconv.Itoa(int(port))
		}
		spec.URL = "https://" + h + p
	case stampDoT:
		spec.Proto = "tls"
	case stampDoQ:
		spec.Proto = "quic"
	}
	return nil
}

// stampAddr parses the address field of a stamp: "", ":port", "IPv4",
// "IPv4:port", "[IPv6]" or "[IPv6]:port" (no zone). port 0 = none.
func stampAddr(s string) (netip.Addr, uint16, error) {
	if s == "" {
		return netip.Addr{}, 0, nil
	}
	if strings.HasPrefix(s, ":") {
		p, err := stampPort(s[1:])
		return netip.Addr{}, p, err
	}
	host, port := s, ""
	if strings.HasPrefix(s, "[") {
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return netip.Addr{}, 0, errors.New("invalid address")
		}
		host, port = s[1:end], strings.TrimPrefix(s[end+1:], ":")
		if s[end+1:] != "" && !strings.HasPrefix(s[end+1:], ":") {
			return netip.Addr{}, 0, errors.New("invalid address")
		}
	} else if h, p, err := net.SplitHostPort(s); err == nil {
		host, port = h, p
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" || strings.HasPrefix(s, "[") != ip.Is6() {
		return netip.Addr{}, 0, errors.New("invalid address") // IPv6 only in brackets
	}
	var p uint16
	if port != "" || strings.HasSuffix(s, ":") {
		if p, err = stampPort(port); err != nil {
			return netip.Addr{}, 0, err
		}
	}
	return ip.Unmap(), p, nil
}

// stampHost parses the host name field: "name", "name:port", an IP
// literal ("[IPv6]" with brackets). The name is lower-cased.
func stampHost(s string) (string, uint16, error) {
	if s == "" {
		return "", 0, errors.New("the host name is missing")
	}
	host, port := s, ""
	if strings.HasPrefix(s, "[") {
		end := strings.IndexByte(s, ']')
		if end < 0 || (s[end+1:] != "" && !strings.HasPrefix(s[end+1:], ":")) {
			return "", 0, errors.New("invalid host name")
		}
		host, port = s[1:end], strings.TrimPrefix(s[end+1:], ":")
		if ip, err := netip.ParseAddr(host); err != nil || !ip.Is6() || ip.Zone() != "" {
			return "", 0, errors.New("invalid host name")
		}
	} else if i := strings.LastIndexByte(s, ':'); i >= 0 {
		host, port = s[:i], s[i+1:]
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	var p uint16
	if port != "" || strings.HasSuffix(s, ":") {
		var err error
		if p, err = stampPort(port); err != nil {
			return "", 0, err
		}
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return "", 0, errors.New("invalid host name")
		}
		return ip.Unmap().String(), p, nil
	}
	if !validHostname(host) {
		return "", 0, errors.New("invalid host name")
	}
	return host, p, nil
}

func stampPort(s string) (uint16, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 || s[0] == '+' {
		return 0, errors.New("invalid port")
	}
	return uint16(n), nil
}

// printableASCII reports whether s holds only printable ASCII.
func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
