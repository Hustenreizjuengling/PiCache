package netutil

import (
	"errors"
	"net/netip"
	"strconv"
	"strings"
)

// MaxForwardedEntries is how many X-Forwarded-For entries ForwardedClient
// inspects, counted from the right.
const MaxForwardedEntries = 16

// maxForwardedEntryLen bounds one entry ("[ipv6%zone]:port" is shorter).
const maxForwardedEntryLen = 100

// ErrForwardedFor reports an X-Forwarded-For entry that is not an address
// (the API answers 400 "malformed X-Forwarded-For header").
var ErrForwardedFor = errors.New("malformed X-Forwarded-For header")

// ForwardedClient returns the effective client of a request: the client
// address a trusted reverse proxy forwarded, else the peer
// (docs/ARCHITECTURE.md 6.1). Only X-Forwarded-For is read, and only when
// the peer is trusted; header holds its lines in order.
//
// The lines are joined with ","; entries are trimmed and empty ones
// skipped; at most the 16 right-most entries are inspected. From the
// right, entries of trusted proxies are skipped and the first other entry
// is the client. Accepted forms: ip, ip:port, [ipv6], [ipv6]:port; a zone
// is removed and the address made canonical (Canon). Any other entry
// reached before the client is found fails with ErrForwardedFor (fail
// closed: a proxy that passes a client's header through must not let the
// client pick its address). When every inspected entry is trusted, the
// left-most inspected one is the client. Without entries the peer is.
func ForwardedClient(peer netip.Addr, header []string, trusted func(netip.Addr) bool) (netip.Addr, error) {
	peer = Canon(peer)
	if !peer.IsValid() || trusted == nil || !trusted(peer) {
		return peer, nil
	}
	client, inspected := peer, 0
	for i := len(header) - 1; i >= 0 && inspected < MaxForwardedEntries; i-- {
		line := header[i]
		for inspected < MaxForwardedEntries {
			var entry string
			j := strings.LastIndexByte(line, ',')
			if j < 0 {
				entry = line
			} else {
				entry = line[j+1:]
			}
			if entry = strings.TrimSpace(entry); entry != "" {
				inspected++
				ip, ok := ParseForwardedAddr(entry)
				if !ok {
					return netip.Addr{}, ErrForwardedFor
				}
				if !trusted(ip) {
					return ip, nil
				}
				client = ip
			}
			if j < 0 {
				break
			}
			line = line[:j]
		}
	}
	return client, nil
}

// ParseForwardedAddr parses one X-Forwarded-For entry: ip, ip:port (IPv4),
// [ipv6] or [ipv6]:port; the address is returned canonical (unmapped,
// without zone).
func ParseForwardedAddr(s string) (netip.Addr, bool) {
	if s == "" || len(s) > maxForwardedEntryLen {
		return netip.Addr{}, false
	}
	host := s
	if strings.HasPrefix(s, "[") {
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return netip.Addr{}, false
		}
		host = s[1:end]
		if rest := s[end+1:]; rest != "" && !validPortSuffix(rest) {
			return netip.Addr{}, false
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.Is6() {
			return netip.Addr{}, false
		}
		return Canon(ip), true
	}
	if i := strings.IndexByte(s, ':'); i >= 0 && strings.Count(s, ":") == 1 {
		// ip:port (a bare IPv6 address has at least two colons).
		if !validPortSuffix(s[i:]) {
			return netip.Addr{}, false
		}
		host = s[:i]
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.Is4() {
			return netip.Addr{}, false
		}
		return ip, true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return Canon(ip), true
}

// validPortSuffix reports whether s is ":<port>" with a decimal port
// (1–5 digits, at most 65535).
func validPortSuffix(s string) bool {
	if len(s) < 2 || len(s) > 6 || s[0] != ':' {
		return false
	}
	for _, c := range []byte(s[1:]) {
		if c < '0' || c > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(s[1:])
	return err == nil && n <= 65535
}

// ForwardedHTTPS reports whether the right-most value of X-Forwarded-Proto
// (its lines joined with ",", split at commas, trimmed) is "https", compared
// case-insensitively. The caller reads it only from trusted proxies.
func ForwardedHTTPS(header []string) bool {
	if len(header) == 0 {
		return false
	}
	last := header[len(header)-1]
	if i := strings.LastIndexByte(last, ','); i >= 0 {
		last = last[i+1:]
	}
	return strings.EqualFold(strings.TrimSpace(last), "https")
}
