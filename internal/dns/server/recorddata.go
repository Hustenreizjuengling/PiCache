package dnsserver

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// Limits of the typed record values (SRV, MX, PTR, HTTPS, SVCB).
const (
	maxValueLen   = 2048 // the presentation form of a value
	maxALPN       = 8
	maxALPNLen    = 255
	maxHintAddrs  = 8
	typedDataNull = "null"
)

// SRVData is the structured value of an SRV record.
type SRVData struct {
	Priority int    `json:"priority"`
	Weight   int    `json:"weight"`
	Port     int    `json:"port"`
	Target   string `json:"target"` // a domain name or "."
}

// MXData is the structured value of an MX record.
type MXData struct {
	Preference int    `json:"preference"`
	Host       string `json:"host"` // a domain name, or "." (no mail) with preference 0
}

// PTRData is the structured value of a PTR record.
type PTRData struct {
	Target string `json:"target"`
}

// SVCBData is the structured value of an HTTPS or SVCB record: priority 0
// is the alias form (no parameters).
type SVCBData struct {
	Priority int      `json:"priority"`
	Target   string   `json:"target"` // a domain name or "."
	ALPN     []string `json:"alpn"`
	Port     *int     `json:"port,omitempty"`
	IPv4Hint []string `json:"ipv4hint"`
	IPv6Hint []string `json:"ipv6hint"`
}

// typedRecord reports whether a record type has a structured value.
func typedRecord(typ string) bool {
	switch typ {
	case "SRV", "MX", "PTR", "HTTPS", "SVCB":
		return true
	}
	return false
}

// dataPresent reports whether a raw data member was given (not absent, not
// null).
func dataPresent(raw jsontext.Value) bool {
	return len(raw) > 0 && strings.TrimSpace(string(raw)) != typedDataNull
}

// normalizeTyped validates the value of a typed record (from raw data when
// given, else from value) and returns its canonical presentation form
// (stored, so UNIQUE (name, type, value) works). Errors name value or
// data.<member>.
func normalizeTyped(typ, name, value string, raw jsontext.Value) (string, error) {
	fromData := dataPresent(raw)
	field := func(member string) string {
		if fromData {
			return "data." + member
		}
		return "value"
	}
	switch typ {
	case "SRV":
		var d SRVData
		if fromData {
			if err := decodeData(raw, &d); err != nil {
				return "", err
			}
		} else if err := parseFields(value, 4, "<priority> <weight> <port> <target>", &d.Priority, &d.Weight, &d.Port, &d.Target); err != nil {
			return "", err
		}
		for _, n := range []struct {
			member string
			v      int
		}{{"priority", d.Priority}, {"weight", d.Weight}, {"port", d.Port}} {
			if n.v < 0 || n.v > 65535 {
				return "", apperr.Invalid(field(n.member), "must be between 0 and 65535")
			}
		}
		target, ok := targetName(d.Target, true)
		if !ok {
			return "", apperr.Invalid(field("target"), "must be a domain name or .")
		}
		return fmt.Sprintf("%d %d %d %s", d.Priority, d.Weight, d.Port, target), nil
	case "MX":
		var d MXData
		if fromData {
			if err := decodeData(raw, &d); err != nil {
				return "", err
			}
		} else if err := parseFields(value, 2, "<preference> <host>", &d.Preference, &d.Host); err != nil {
			return "", err
		}
		if d.Preference < 0 || d.Preference > 65535 {
			return "", apperr.Invalid(field("preference"), "must be between 0 and 65535")
		}
		host, ok := targetName(d.Host, d.Preference == 0)
		if !ok {
			return "", apperr.Invalid(field("host"), "must be a domain name (. only with preference 0)")
		}
		return fmt.Sprintf("%d %s", d.Preference, host), nil
	case "PTR":
		if _, ok := parseReverse(name); !ok {
			return "", apperr.Invalid("name", "a PTR record needs a complete in-addr.arpa (4 labels) or ip6.arpa (32 nibbles) name")
		}
		var d PTRData
		if fromData {
			if err := decodeData(raw, &d); err != nil {
				return "", err
			}
		} else if err := parseFields(value, 1, "<target>", &d.Target); err != nil {
			return "", err
		}
		target, ok := targetName(d.Target, false)
		if !ok {
			return "", apperr.Invalid(field("target"), "must be a domain name")
		}
		return target, nil
	case "HTTPS", "SVCB":
		var d SVCBData
		if fromData {
			if err := decodeData(raw, &d); err != nil {
				return "", err
			}
		} else {
			var err error
			if d, err = parseSVCBValue(value); err != nil {
				return "", err
			}
		}
		return canonicalSVCB(d, field)
	}
	return "", apperr.Invalid("type", "unknown record type")
}

// decodeData decodes a raw data member strictly (unknown members refused).
func decodeData(raw jsontext.Value, dst any) error {
	if err := json.Unmarshal(raw, dst, json.RejectUnknownMembers(true)); err != nil {
		msg := strings.TrimPrefix(err.Error(), "json: ")
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return apperr.Invalid("data", "invalid: %s", msg)
	}
	return nil
}

// parseFields splits a presentation value into n fields: ints for *int
// destinations, the text for *string.
func parseFields(value string, n int, form string, dst ...any) error {
	f := strings.Fields(value)
	if len(f) != n {
		return apperr.Invalid("value", "must be %q", form)
	}
	for i, d := range dst {
		switch p := d.(type) {
		case *int:
			v, err := strconv.Atoi(f[i])
			if err != nil {
				return apperr.Invalid("value", "must be %q", form)
			}
			*p = v
		case *string:
			*p = f[i]
		}
	}
	return nil
}

// targetName normalises a target name (lower-case, no trailing dot);
// root "." only when allowed.
func targetName(s string, root bool) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "." {
		return ".", root
	}
	s = strings.TrimSuffix(s, ".")
	return s, validDomain(s)
}

// parseSVCBValue parses "<priority> <target>[ key=value…]" with the keys
// alpn, port, ipv4hint and ipv6hint (each at most once).
func parseSVCBValue(value string) (SVCBData, error) {
	var d SVCBData
	f := strings.Fields(value)
	if len(f) < 2 {
		return d, apperr.Invalid("value", `must be "<priority> <target>[ alpn=…][ port=…][ ipv4hint=…][ ipv6hint=…]"`)
	}
	p, err := strconv.Atoi(f[0])
	if err != nil {
		return d, apperr.Invalid("value", "the priority must be a number")
	}
	d.Priority, d.Target = p, f[1]
	seen := map[string]bool{}
	for _, kv := range f[2:] {
		k, v, ok := strings.Cut(kv, "=")
		k = strings.ToLower(k)
		if !ok || v == "" || seen[k] {
			return d, apperr.Invalid("value", "the parameter %q is malformed or given twice", kv)
		}
		seen[k] = true
		list := strings.Split(v, ",")
		switch k {
		case "alpn":
			d.ALPN = list
		case "port":
			n, err := strconv.Atoi(v)
			if err != nil {
				return d, apperr.Invalid("value", "port must be a number")
			}
			d.Port = &n
		case "ipv4hint":
			d.IPv4Hint = list
		case "ipv6hint":
			d.IPv6Hint = list
		default:
			return d, apperr.Invalid("value", "the parameter %s is not supported (alpn, port, ipv4hint and ipv6hint are)", k)
		}
	}
	return d, nil
}

// canonicalSVCB validates an HTTPS/SVCB value and returns its presentation
// form: "<priority> <target>[ alpn=…][ port=…][ ipv4hint=…][ ipv6hint=…]".
func canonicalSVCB(d SVCBData, field func(string) string) (string, error) {
	if d.Priority < 0 || d.Priority > 65535 {
		return "", apperr.Invalid(field("priority"), "must be between 0 and 65535")
	}
	target, ok := targetName(d.Target, true)
	if !ok {
		return "", apperr.Invalid(field("target"), "must be a domain name or .")
	}
	params := len(d.ALPN) > 0 || d.Port != nil || len(d.IPv4Hint) > 0 || len(d.IPv6Hint) > 0
	if d.Priority == 0 && params {
		return "", apperr.Invalid(field("priority"), "priority 0 (the alias form) takes no parameters")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s", d.Priority, target)
	if len(d.ALPN) > 0 {
		if len(d.ALPN) > maxALPN {
			return "", apperr.Invalid(field("alpn"), "at most %d protocol IDs", maxALPN)
		}
		for _, id := range d.ALPN {
			if !validALPN(id) {
				return "", apperr.Invalid(field("alpn"), "protocol IDs are 1–%d characters of a-z, 0-9, /, ., _ and -", maxALPNLen)
			}
		}
		b.WriteString(" alpn=" + strings.Join(d.ALPN, ","))
	}
	if d.Port != nil {
		if *d.Port < 1 || *d.Port > 65535 {
			return "", apperr.Invalid(field("port"), "must be between 1 and 65535")
		}
		fmt.Fprintf(&b, " port=%d", *d.Port)
	}
	for _, h := range []struct {
		key  string
		list []string
		v6   bool
	}{{"ipv4hint", d.IPv4Hint, false}, {"ipv6hint", d.IPv6Hint, true}} {
		if len(h.list) == 0 {
			continue
		}
		if len(h.list) > maxHintAddrs {
			return "", apperr.Invalid(field(h.key), "at most %d addresses", maxHintAddrs)
		}
		addrs := make([]string, 0, len(h.list))
		for _, s := range h.list {
			ip, err := netip.ParseAddr(strings.TrimSpace(s))
			if err != nil || ip.Zone() != "" || ip.Is4In6() || ip.Is6() != h.v6 || ip.IsUnspecified() || ip.IsMulticast() {
				return "", apperr.Invalid(field(h.key), "must be unicast %s addresses", map[bool]string{false: "IPv4", true: "IPv6"}[h.v6])
			}
			addrs = append(addrs, ip.String())
		}
		b.WriteString(" " + h.key + "=" + strings.Join(addrs, ","))
	}
	if b.Len() > maxValueLen {
		return "", apperr.Invalid(field("target"), "the value must be at most %d bytes", maxValueLen)
	}
	return b.String(), nil
}

func validALPN(id string) bool {
	if id == "" || len(id) > maxALPNLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '/' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// recordData returns the structured form of a stored canonical value (nil
// for A, AAAA, CNAME and TXT, and for a value that does not parse).
func recordData(typ, value string) any {
	f := strings.Fields(value)
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	switch typ {
	case "SRV":
		if len(f) == 4 {
			return &SRVData{Priority: atoi(f[0]), Weight: atoi(f[1]), Port: atoi(f[2]), Target: f[3]}
		}
	case "MX":
		if len(f) == 2 {
			return &MXData{Preference: atoi(f[0]), Host: f[1]}
		}
	case "PTR":
		if len(f) == 1 {
			return &PTRData{Target: f[0]}
		}
	case "HTTPS", "SVCB":
		d, err := parseSVCBValue(value)
		if err != nil {
			return nil
		}
		for _, l := range []*[]string{&d.ALPN, &d.IPv4Hint, &d.IPv6Hint} {
			if *l == nil {
				*l = []string{}
			}
		}
		return &d
	}
	return nil
}

// typedRR builds the answer record of a stored typed value owned by owner
// (nil when it does not parse).
func typedRR(owner string, typ uint16, value string, ttl uint32) dns.RR {
	h := rrHeader(owner, typ, ttl)
	f := strings.Fields(value)
	atoi := func(s string) uint16 { n, _ := strconv.Atoi(s); return uint16(n) }
	target := func(s string) string {
		if s == "." {
			return "."
		}
		return fqdn(s)
	}
	switch typ {
	case dns.TypeSRV:
		if len(f) == 4 {
			return &dns.SRV{Hdr: h, Priority: atoi(f[0]), Weight: atoi(f[1]), Port: atoi(f[2]), Target: target(f[3])}
		}
	case dns.TypeMX:
		if len(f) == 2 {
			return &dns.MX{Hdr: h, Preference: atoi(f[0]), Mx: target(f[1])}
		}
	case dns.TypePTR:
		return &dns.PTR{Hdr: h, Ptr: fqdn(value)}
	case dns.TypeHTTPS, dns.TypeSVCB:
		d, err := parseSVCBValue(value)
		if err != nil {
			return nil
		}
		svcb := dns.SVCB{Hdr: h, Priority: uint16(d.Priority), Target: target(strings.ToLower(d.Target))}
		if len(d.ALPN) > 0 {
			svcb.Value = append(svcb.Value, &dns.SVCBAlpn{Alpn: slices.Clone(d.ALPN)})
		}
		if d.Port != nil {
			svcb.Value = append(svcb.Value, &dns.SVCBPort{Port: uint16(*d.Port)})
		}
		for _, hint := range []struct {
			list []string
			v6   bool
		}{{d.IPv4Hint, false}, {d.IPv6Hint, true}} {
			var ips []net.IP
			for _, s := range hint.list {
				if ip, err := netip.ParseAddr(s); err == nil {
					ips = append(ips, ip.AsSlice())
				}
			}
			switch {
			case len(ips) == 0:
			case hint.v6:
				svcb.Value = append(svcb.Value, &dns.SVCBIPv6Hint{Hint: ips})
			default:
				svcb.Value = append(svcb.Value, &dns.SVCBIPv4Hint{Hint: ips})
			}
		}
		if typ == dns.TypeHTTPS {
			return &dns.HTTPS{SVCB: svcb}
		}
		return &svcb
	}
	return nil
}
