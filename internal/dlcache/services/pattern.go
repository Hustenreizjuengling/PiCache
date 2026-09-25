package services

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// maxHostLen is the maximum length of a DNS name in text form.
const maxHostLen = 253

// serviceIDRE is the allowed form of service IDs (cache-domains names and
// "custom-<slug>").
var serviceIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidServiceID reports whether id has the form of a service ID
// (^[a-z0-9][a-z0-9_-]{0,31}$). It does not check that the service exists.
func ValidServiceID(id string) bool { return serviceIDRE.MatchString(id) }

// NormalizePattern trims, lower-cases and strips one trailing dot, the form
// in which host patterns are validated, stored and matched.
func NormalizePattern(p string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(p)), ".")
}

// ValidatePattern checks a host pattern: exact host or "*.suffix", valid
// A-labels, at least two labels, not "*", and the base (without "*.") must
// not be a public suffix (x/net/publicsuffix) such as "com" or "co.uk".
// Letters are compared case-insensitively; callers store NormalizePattern(p).
func ValidatePattern(p string) error {
	p = strings.ToLower(p)
	if p == "" {
		return errors.New("empty pattern")
	}
	base, _ := strings.CutPrefix(p, "*.")
	if strings.Contains(base, "*") {
		return errors.New(`a wildcard is only allowed as a leading "*."`)
	}
	if len(base) > maxHostLen {
		return errors.New("host name is too long")
	}
	if _, err := netip.ParseAddr(base); err == nil {
		return errors.New("IP addresses are not allowed")
	}
	labels := 0
	for label := range strings.SplitSeq(base, ".") {
		if err := checkLabel(label); err != nil {
			return err
		}
		labels++
	}
	if labels < 2 {
		return errors.New("at least two labels are required")
	}
	if tld := base[strings.LastIndexByte(base, '.')+1:]; allDigits(tld) {
		return errors.New("the top-level domain must not be numeric")
	}
	if ps, _ := publicsuffix.PublicSuffix(base); ps == base {
		return fmt.Errorf("%s is a public suffix", base)
	}
	return nil
}

// checkLabel validates one A-label: 1–63 letters, digits and hyphens, not
// starting or ending with a hyphen.
func checkLabel(l string) error {
	if l == "" {
		return errors.New("empty label")
	}
	if len(l) > 63 {
		return errors.New("label longer than 63 characters")
	}
	if l[0] == '-' || l[len(l)-1] == '-' {
		return errors.New("label starts or ends with a hyphen")
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return errors.New("invalid character (only a-z, 0-9, '-' and '.' are allowed)")
		}
	}
	return nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// normalizeHost prepares a host name or qname for matching. It returns ""
// for names that cannot match any pattern.
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if len(h) > maxHostLen {
		return ""
	}
	return h
}

// clip shortens untrusted text for messages.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
