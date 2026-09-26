package applog

import (
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
)

// redactedNames are the normalised (lower-case, without '_' and '-')
// attribute keys whose values never reach a sink: the names the audit log
// redacts (auth.redactedNames; a test keeps both lists equal).
var redactedNames = map[string]bool{
	"password": true, "currentpassword": true, "newpassword": true, "setuptoken": true,
	"token": true, "secret": true, "code": true, "totp": true,
	"passwordsealed": true, "passphrase": true, "apikey": true, "privatekey": true,
	"authorization": true, "cookie": true, "credentials": true, "keypem": true, "certpem": true,
}

// RedactedNames returns the normalised keys whose values are redacted.
func RedactedNames() []string {
	out := make([]string, 0, len(redactedNames))
	for k := range redactedNames {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// normalizeKey lower-cases a key and removes '_' and '-' (as the audit log
// normalises JSON member names).
func normalizeKey(k string) string {
	return strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(k))
}

// Keys masked in the ring while client addresses are anonymised, and keys
// hidden while domains are hidden.
var (
	addrKeys   = map[string]bool{"client": true, "clientIp": true, "ip": true, "peer": true, "remote": true, "source": true, "addr": true}
	domainKeys = map[string]bool{"qname": true, "domain": true, "sni": true, "host": true, "target": true, "cname": true}
)

// stderrOnly is the value of a StderrOnly attribute.
type stderrOnly string

// LogValue is the value stderr shows.
func (v stderrOnly) LogValue() slog.Value { return slog.StringValue(string(v)) }

// StderrOnly returns an attribute that reaches stderr (the journal)
// unredacted although key may be a secret's name, and is "[redacted]" in
// the ring (the web UI, its stream and the support bundle). It is only for
// a value whose log line is the documented way to find it: the first-run
// setup token.
func StderrOnly(key, value string) slog.Attr { return slog.Any(key, stderrOnly(value)) }

// isStderrOnly reports a StderrOnly value (before it is resolved).
func isStderrOnly(v slog.Value) bool {
	if v.Kind() != slog.KindLogValuer {
		return false
	}
	_, ok := v.Any().(stderrOnly)
	return ok
}

// redact replaces the value of a secret's key by "[redacted]" and reduces
// URLs (both sinks), at any group depth; a StderrOnly value is kept (the
// ring redacts it).
func redact(a slog.Attr) slog.Attr {
	if isStderrOnly(a.Value) {
		return a
	}
	if redactedNames[normalizeKey(a.Key)] {
		return slog.String(a.Key, redactedValue)
	}
	v := a.Value
	if v.Kind() == slog.KindLogValuer {
		v = v.Resolve()
		a.Value = v
	}
	switch v.Kind() {
	case slog.KindGroup:
		g := v.Group()
		out := make([]slog.Attr, len(g))
		for i, x := range g {
			out[i] = redact(x)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	case slog.KindString:
		if u, ok := reduceURL(v.String()); ok {
			return slog.String(a.Key, u)
		}
	case slog.KindAny:
		switch x := v.Any().(type) {
		case *url.URL:
			if x != nil {
				return slog.String(a.Key, reducedURL(x))
			}
		case url.URL:
			return slog.String(a.Key, reducedURL(&x))
		}
	}
	return a
}

// reduceURL reduces s when it is an absolute URL with a host:
// scheme://host[:port]/path without user information, query and fragment.
func reduceURL(s string) (string, bool) {
	if !strings.Contains(s, "://") {
		return "", false
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	if u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.RawFragment == "" {
		return "", false // nothing to remove
	}
	return reducedURL(u), true
}

func reducedURL(u *url.URL) string {
	if u.Host == "" {
		return u.Scheme + ":"
	}
	return u.Scheme + "://" + u.Host + u.EscapedPath()
}

// valueString formats an attribute value for the ring, without control
// and bidi characters.
func valueString(v slog.Value) string {
	var s string
	switch v.Kind() {
	case slog.KindString:
		s = v.String()
	case slog.KindTime:
		s = v.Time().UTC().Format(time.RFC3339Nano)
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			s = x.Error()
		case fmt.Stringer:
			s = x.String()
		default:
			s = fmt.Sprint(x)
		}
	default:
		s = v.String()
	}
	return cleanText(s)
}

// cleanText removes C0 and C1 controls, DEL and the bidi controls from s
// and makes it valid UTF-8.
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f,
			r == 0x061c, r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
}

// maskAddr masks an address, address:port or prefix to /16 (IPv4) or /48
// (IPv6), without a zone; anything else is returned unchanged.
func maskAddr(s string) string {
	mask := func(a netip.Addr, bits int) netip.Prefix {
		a = a.Unmap().WithZone("")
		keep := 16
		if a.Is6() {
			keep = 48
		}
		p, _ := a.Prefix(min(bits, keep))
		return p
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return mask(a, 128).Addr().String()
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return netip.AddrPortFrom(mask(ap.Addr(), 128).Addr(), ap.Port()).String()
	}
	if p, err := netip.ParsePrefix(s); err == nil {
		bits := p.Bits()
		if p.Addr().Is4In6() {
			bits = max(bits-96, 0)
		}
		return mask(p.Addr(), bits).String()
	}
	return s
}
