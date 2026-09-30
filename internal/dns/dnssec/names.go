package dnssec

import (
	"bytes"
	"strings"

	"github.com/miekg/dns"
)

// canon returns the canonical form of a name in presentation format (miekg
// decodes a wire name into exactly one presentation form, up to the case of
// ASCII letters): ASCII letters lower-cased, fully qualified.
func canon(name string) string {
	for i := 0; i < len(name); i++ {
		if 'A' <= name[i] && name[i] <= 'Z' {
			b := []byte(name)
			for j := i; j < len(b); j++ {
				if 'A' <= b[j] && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			name = string(b)
			break
		}
	}
	return dns.Fqdn(name)
}

// equalName reports whether two names are equal (ASCII case-insensitive).
func equalName(a, b string) bool { return canon(a) == canon(b) }

// parentName returns the parent of name ("" for the root).
func parentName(name string) string {
	if name == "." || name == "" {
		return ""
	}
	off, end := dns.NextLabel(name, 0)
	if end || off >= len(name) {
		return "."
	}
	return name[off:]
}

// below reports whether child is equal to or below zone on label boundaries.
func below(zone, child string) bool { return dns.IsSubDomain(zone, child) }

// strictlyBelow reports whether child is below zone and not equal to it.
func strictlyBelow(zone, child string) bool { return below(zone, child) && !equalName(zone, child) }

// display returns a name for reasons and logs: lower-case without the
// trailing dot, "." for the root.
func display(name string) string {
	name = canon(name)
	if name == "." {
		return "."
	}
	return strings.TrimSuffix(name, ".")
}

// wireLabels returns the labels of name in wire form (escapes decoded,
// ASCII letters lower-cased), the root having none; false for a name that
// does not pack.
func wireLabels(name string) ([][]byte, bool) {
	var buf [256]byte
	n, err := dns.PackDomainName(dns.Fqdn(name), buf[:], 0, nil, false)
	if err != nil {
		return nil, false
	}
	var out [][]byte
	for off := 0; off < n; {
		l := int(buf[off])
		if l == 0 {
			break
		}
		if off+1+l > n {
			return nil, false
		}
		out = append(out, lowerLabel(buf[off+1:off+1+l]))
		off += 1 + l
	}
	return out, true
}

// lowerLabel lower-cases the ASCII letters of a label (every other byte is
// kept; RFC 4034 6.1 compares labels as unsigned octets).
func lowerLabel(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

// compareNames orders two names canonically (RFC 4034 6.1): label by label
// from the right, each label as unsigned octets with ASCII letters
// lower-cased; a name sorts before its descendants. Names that do not pack
// sort as the root.
func compareNames(a, b string) int {
	la, _ := wireLabels(a)
	lb, _ := wireLabels(b)
	i, j := len(la)-1, len(lb)-1
	for i >= 0 && j >= 0 {
		if c := bytes.Compare(la[i], lb[j]); c != 0 {
			return c
		}
		i--
		j--
	}
	switch {
	case i < 0 && j < 0:
		return 0
	case i < 0:
		return -1
	}
	return 1
}

// commonAncestor returns the longest common ancestor of two names (at
// least the root).
func commonAncestor(a, b string) string {
	n := dns.CompareDomainName(a, b)
	return lastLabels(a, n)
}

// wildcardAt returns the wildcard name at the closest encloser ce: "*." for
// the root (never "*.."), else "*." + ce.
func wildcardAt(ce string) string {
	if ce = canon(ce); ce == "." {
		return "*."
	}
	return "*." + ce
}

// sigLabels returns the labels field of an RRSIG over an RRset owned by
// name that is no wildcard expansion: the labels of name without a leading
// "*" (RFC 4034 3.1.3), so an RRset owned by the wildcard name itself is
// not taken for an expansion (RFC 4035 5.3.2).
func sigLabels(name string) int {
	n := dns.CountLabel(name)
	if name == "*." || strings.HasPrefix(name, "*.") {
		n--
	}
	return n
}

// lastLabels returns the name made of the last n labels of name.
func lastLabels(name string, n int) string {
	idx := dns.Split(dns.Fqdn(name))
	if n <= 0 || len(idx) == 0 {
		return "."
	}
	if n >= len(idx) {
		return canon(name)
	}
	return canon(dns.Fqdn(name)[idx[len(idx)-n]:])
}

// ancestorsBelow returns the names from the child of anc down to target
// (top-down); empty when target is anc. anc must be an ancestor of target
// or equal to it.
func ancestorsBelow(anc, target string) []string {
	var out []string
	for n := canon(target); n != "" && !equalName(n, anc); n = parentName(n) {
		out = append(out, n)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
