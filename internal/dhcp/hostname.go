package dhcp

import (
	"net"
	"strings"
)

// SanitizeHostname turns a host name a client sent (option 12 or the first
// label of option 81) into a DNS label: the part before the first dot,
// lower-case letters, digits and hyphens only (space and underscore become
// a hyphen, everything else is dropped), runs of hyphens collapsed, at
// most 63 characters, no hyphen at either end. "" when nothing remains.
func SanitizeHostname(s string) string {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 255 {
		s = s[:255]
	}
	out := make([]byte, 0, min(len(s), 63))
	for i := 0; i < len(s) && len(out) < 63; i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			c += 'a' - 'A'
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' || c == ' ' || c == '_':
			c = '-'
		default:
			continue
		}
		if c == '-' && (len(out) == 0 || out[len(out)-1] == '-') {
			continue
		}
		out = append(out, c)
	}
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	return string(out)
}

// validLabel reports whether s is a DNS label as SanitizeHostname returns
// them (1–63 of a-z, 0-9 and '-', no hyphen at either end).
func validLabel(s string) bool {
	return s != "" && SanitizeHostname(s) == s
}

// NormalizeMAC returns an Ethernet address in lower-case colon form
// ("aa:bb:cc:dd:ee:ff"). It accepts colons or hyphens as separators or
// twelve hex digits without any. ok is false for anything else and for
// group (multicast/broadcast) and all-zero addresses.
func NormalizeMAC(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) == 12 {
		var b strings.Builder
		for i := 0; i < 12; i += 2 {
			if i > 0 {
				b.WriteByte(':')
			}
			b.WriteString(s[i : i+2])
		}
		s = b.String()
	}
	if len(s) != 17 {
		return "", false
	}
	hw, err := net.ParseMAC(s)
	if err != nil || len(hw) != 6 {
		return "", false
	}
	return macString([6]byte(hw))
}

// macString formats an Ethernet address; ok is false for group and
// all-zero addresses (never a client).
func macString(a [6]byte) (string, bool) {
	if a[0]&1 != 0 || a == [6]byte{} {
		return "", false
	}
	return net.HardwareAddr(a[:]).String(), true
}
