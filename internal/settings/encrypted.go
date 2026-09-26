package settings

import (
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// ClientIDs (docs/ARCHITECTURE.md 19): the device ID a DoT client sends
// as the first label of its SNI (<ClientID>.<serverName>) and a DoH client
// in the path (/dns-query/<ClientID>). A ClientID identifies a device; it
// never authenticates it.

// ClientIDPrefix introduces a ClientID in client identifiers and in
// dns.blockedClients ("clientid:<ClientID>"; the prefix is matched
// case-insensitively and stored lower-case).
const ClientIDPrefix = "clientid:"

// ErrClientID is the validation message of a ClientID.
const ErrClientID = "must be a ClientID: 1–63 letters, digits and hyphens, not starting or ending with a hyphen"

// clientIDRE is one DNS label of lower-case letters, digits and hyphens.
var clientIDRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// NormalizeClientID lower-cases a ClientID and reports whether it is
// valid: one label of 1–63 letters, digits and hyphens that neither starts
// nor ends with a hyphen. Upper-case input is never refused for its case.
func NormalizeClientID(s string) (string, bool) {
	if len(s) == 0 || len(s) > 63 {
		return "", false
	}
	s = strings.ToLower(s)
	if !clientIDRE.MatchString(s) {
		return "", false
	}
	return s, true
}

// ParseClientIDEntry parses "clientid:<ClientID>" (the prefix
// case-insensitive, surrounding white space trimmed) and returns the
// normalised ClientID; ok is false for anything else.
func ParseClientIDEntry(s string) (id string, ok bool) {
	s = strings.TrimSpace(s)
	if len(s) <= len(ClientIDPrefix) || !strings.EqualFold(s[:len(ClientIDPrefix)], ClientIDPrefix) {
		return "", false
	}
	return NormalizeClientID(s[len(ClientIDPrefix):])
}

// NormalizeServerName returns dns.encrypted.serverName as it is stored:
// trimmed, lower-case, one trailing dot removed.
func NormalizeServerName(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

// ServerNameMatch reports whether name (lower-case, no trailing dot) is
// the server name of encrypted DNS or <ClientID>.<serverName> with a
// valid ClientID; id is the ClientID of the second form ("" for the
// first). Nothing matches while no server name is set.
func ServerNameMatch(serverName, name string) (id string, ok bool) {
	if serverName == "" {
		return "", false
	}
	if name == serverName {
		return "", true
	}
	label, rest, found := strings.Cut(name, ".")
	if !found || rest != serverName {
		return "", false
	}
	return NormalizeClientID(label)
}

// specialUseZones are the zones dns.encrypted.serverName may not be (or be
// below); names below home.arpa are allowed.
var specialUseZones = []string{"localhost", "invalid", "onion", "arpa"}

// validateEncrypted checks dns.encrypted and dns.plainDns (the first
// failing rule wins): the server name is required while DoT or DoH is on
// and must be a host name of at least two labels (at most 189
// characters, no IP address, no special-use name, not the local domain
// or a parent of it); plain DNS can only be off while DoT or DoH is on;
// no upstream or fallback may be PiCache itself (SelfUpstream).
func (d *DNS) validateEncrypted() error {
	const field = "dns.encrypted.serverName"
	e := d.Encrypted
	n := e.ServerName
	if n == "" && e.Enabled() {
		return apperr.Invalid(field, "required while DoT or DoH is enabled")
	}
	if n != "" {
		if _, err := netip.ParseAddr(strings.Trim(n, "[]")); err == nil {
			return apperr.Invalid(field, "must be a host name, not an IP address")
		}
		if len(n) > MaxServerNameLen {
			return apperr.Invalid(field, "must be at most %d characters", MaxServerNameLen)
		}
		if !validHostname(n) || !strings.Contains(n, ".") {
			return apperr.Invalid(field, "must be a host name with at least two labels, e.g. dns.example.com")
		}
		if !inZone(n, "home.arpa") || n == "home.arpa" {
			for _, z := range specialUseZones {
				if inZone(n, z) {
					return apperr.Invalid(field, "must not be a special-use name (localhost, invalid, onion, arpa)")
				}
			}
		}
		if d.LocalDomain != "" && inZone(d.LocalDomain, n) {
			return apperr.Invalid(field, "must not be the local domain or a parent of it")
		}
	}
	if !d.PlainDNS && !e.Enabled() {
		return apperr.Invalid("dns.plainDns", "plain DNS can only be switched off while DoT or DoH is enabled")
	}
	for _, list := range []struct {
		field string
		ups   []string
	}{{"dns.upstreams", d.Upstreams}, {"dns.fallbackUpstreams", d.FallbackUpstreams}} {
		for i, u := range list.ups {
			if spec, err := ParseUpstream(u); err == nil && SelfUpstream(spec, d) {
				return apperr.Invalid(list.field+"["+strconv.Itoa(i)+"]", "%s", ErrSelfUpstream)
			}
		}
	}
	return nil
}

// ErrSelfUpstream is the validation message of an upstream that names
// PiCache itself.
const ErrSelfUpstream = "an upstream must not be PiCache itself"

// SelfUpstream reports whether an upstream names PiCache itself by name:
// its host (a stamp's host or provider name) equals
// dns.encrypted.serverName, a name <label>.<serverName>, or an entry of
// dns.serverNames (also with .<localDomain>). An upstream given by one of
// PiCache's own addresses is not judged (a local resolver on another port
// of this machine is legitimate).
func SelfUpstream(spec UpstreamSpec, d *DNS) bool {
	h := strings.TrimSuffix(strings.ToLower(spec.Host), ".")
	if h == "" || spec.IsIPLit {
		return false
	}
	if sn := NormalizeServerName(d.Encrypted.ServerName); sn != "" {
		if h == sn {
			return true
		}
		if _, rest, ok := strings.Cut(h, "."); ok && rest == sn {
			return true
		}
	}
	ld := strings.Trim(strings.ToLower(d.LocalDomain), ".")
	for _, n := range d.ServerNames {
		n = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(n)), ".")
		if n != "" && (h == n || (ld != "" && h == n+"."+ld)) {
			return true
		}
	}
	return false
}
