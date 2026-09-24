package filter

import "strings"

const (
	maxDomainLen = 253 // presentation format without the trailing dot
	maxLabels    = 128 // a DNS name of 255 wire bytes has at most 127 labels

	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
)

// junkHosts are names found in the headers of hosts files that must never be
// treated as blocklist entries.
var junkHosts = map[string]struct{}{
	"localhost": {}, "localhost.localdomain": {}, "local": {}, "broadcasthost": {},
	"ip6-localhost": {}, "ip6-loopback": {}, "ip6-localnet": {}, "ip6-mcastprefix": {},
	"ip6-allnodes": {}, "ip6-allrouters": {}, "ip6-allhosts": {}, "0.0.0.0": {},
}

// normalizeName lower-cases s and strips surrounding white space and one
// trailing dot.
func normalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimSuffix(s, ".")
}

// validDomain reports whether d (lower-case, no trailing dot) is a
// syntactically valid A-label domain: labels of 1–63 characters from
// [a-z0-9_-], at most 253 characters, and a last label that neither starts
// nor ends with '-' and is not all digits. Underscores are accepted because
// real blocklists contain them (e.g. "_dmarc" style service labels).
func validDomain(d string) bool {
	if d == "" || len(d) > maxDomainLen {
		return false
	}
	labelStart := 0
	for i := 0; i <= len(d); i++ {
		if i < len(d) && d[i] != '.' {
			c := d[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
			continue
		}
		if n := i - labelStart; n == 0 || n > 63 {
			return false
		}
		labelStart = i + 1
	}
	tld := d[strings.LastIndexByte(d, '.')+1:]
	if tld[0] == '-' || tld[len(tld)-1] == '-' {
		return false
	}
	for i := 0; i < len(tld); i++ {
		if tld[i] < '0' || tld[i] > '9' {
			return true
		}
	}
	return false // all-numeric TLD: an IP address or garbage
}

// hashName returns the 64-bit FNV-1a hash of name's bytes taken from right
// to left. Hashing from the right lets suffixHashes compute the hashes of a
// name and all its parent suffixes in a single pass.
func hashName(name string) uint64 {
	h := uint64(fnvOffset64)
	for i := len(name) - 1; i >= 0; i-- {
		h ^= uint64(name[i])
		h *= fnvPrime64
	}
	return h
}

// suffixHashes holds hashName of a name and of each parent suffix: h[0] is
// the top-level label, h[n-1] the full name.
type suffixHashes struct {
	h [maxLabels]uint64
	n int
}

// compute fills s for name without allocating.
func (s *suffixHashes) compute(name string) {
	s.n = 0
	h := uint64(fnvOffset64)
	for i := len(name) - 1; i >= 0 && s.n < len(s.h); i-- {
		h ^= uint64(name[i])
		h *= fnvPrime64
		if i == 0 || name[i-1] == '.' {
			s.h[s.n] = h
			s.n++
		}
	}
}

// subtreeMatch reports whether name equals d or is a subdomain of d.
func subtreeMatch(name, d string) bool {
	return name == d || len(name) > len(d) && name[len(name)-len(d)-1] == '.' && strings.HasSuffix(name, d)
}
