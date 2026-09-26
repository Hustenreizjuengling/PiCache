package filter

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Limits of the modifiers of a rule or list entry.
const (
	maxEntryTypes = 16    // query types of one rule or list entry
	maxDenyallow  = 32    // denyallow domains of one rule or list entry
	maxInverted   = 32    // inverted regular-expression rules, enabled or not
	maxModified   = 20000 // modified entries of one list and in total (about 48 bytes each plus 8 per denyallow domain)
	// maxModsPerName bounds the modified entries of one list with the same
	// tier, kind and name (distinct modifiers): the matcher scans them all
	// on every query at or below that name.
	maxModsPerName = 8
)

// Query types the type sets treat specially (RFC numbers; the filter does
// not import the DNS library).
const (
	qtypeA     uint16 = 1
	qtypeAAAA  uint16 = 28
	qtypeSVCB  uint16 = 64
	qtypeHTTPS uint16 = 65
	qtypeANY   uint16 = 255
)

// typeSet is the set of query types a rule or list entry applies to
// (ARCHITECTURE 7.2, $dnstype): empty = every type. With negate the set
// lists the types it does not apply to. Types below 64 are a bit mask, the
// others a sorted list (at most maxEntryTypes).
type typeSet struct {
	mask   uint64
	high   []uint16
	negate bool
}

// newTypeSet returns the set of types (duplicates removed).
func newTypeSet(types []uint16, negate bool) typeSet {
	t := typeSet{negate: negate && len(types) > 0}
	for _, q := range types {
		if q < 64 {
			t.mask |= 1 << q
		} else if !slices.Contains(t.high, q) {
			t.high = append(t.high, q)
		}
	}
	slices.Sort(t.high)
	return t
}

func (t *typeSet) empty() bool { return t.mask == 0 && len(t.high) == 0 }

// has reports whether q is in the set (ignoring negate).
func (t *typeSet) has(q uint16) bool {
	if q < 64 {
		return t.mask&(1<<q) != 0
	}
	_, found := slices.BinarySearch(t.high, q)
	return found
}

// admits reports whether the set admits a query of type q: an empty set
// every type; a set every type in it and ANY, and HTTPS and SVCB when it
// holds A or AAAA (their address hints hand out the addresses anyway); a
// negated set every type not in it and ANY.
func (t *typeSet) admits(q uint16) bool {
	if t.empty() || q == qtypeANY {
		return true
	}
	if t.negate {
		return !t.has(q)
	}
	if t.has(q) {
		return true
	}
	return (q == qtypeHTTPS || q == qtypeSVCB) && (t.has(qtypeA) || t.has(qtypeAAAA))
}

// types returns the members in ascending order.
func (t *typeSet) types() []uint16 {
	var out []uint16
	for q := range uint16(64) {
		if t.mask&(1<<q) != 0 {
			out = append(out, q)
		}
	}
	return append(out, t.high...)
}

// names returns the normalised type names in ascending order (never nil).
func (t *typeSet) names() []string {
	out := []string{}
	for _, q := range t.types() {
		out = append(out, settings.QTypeName(q))
	}
	return out
}

// equal reports whether two sets are the same.
func (t *typeSet) equal(o *typeSet) bool {
	return t.mask == o.mask && t.negate == o.negate && slices.Equal(t.high, o.high)
}

// key returns a canonical form of the set ("" when empty), e.g.
// "~A|AAAA": the key of $badfilter and of interning.
func (t *typeSet) key() string {
	if t.empty() {
		return ""
	}
	s := strings.Join(t.names(), "|")
	if t.negate {
		s = "~" + s
	}
	return s
}

// normalizeTypes validates the query types of a rule input (field "qtypes"):
// at most maxEntryTypes, each a mnemonic known to the DNS library or
// TYPEnnn; it returns them normalised, without duplicates and sorted by
// type number.
func normalizeTypes(in []string) ([]uint16, error) {
	if len(in) > maxEntryTypes {
		return nil, apperr.Invalid("qtypes", "at most %d query types", maxEntryTypes)
	}
	var out []uint16
	for i, s := range in {
		t, err := settings.ParseQType(s)
		if err != nil {
			return nil, apperr.Invalid(fmt.Sprintf("qtypes[%d]", i), "unknown record type")
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return out, nil
}

// typeNames returns the normalised names of types (never nil).
func typeNames(types []uint16) []string {
	out := make([]string, 0, len(types))
	for _, t := range types {
		out = append(out, settings.QTypeName(t))
	}
	return out
}

// excepted reports whether a name is equal to or below a domain of a
// denyallow set: one of its suffix hashes is in deny (hashName of each
// domain, sorted).
func excepted(deny []uint64, sf *suffixHashes) bool {
	if len(deny) == 0 {
		return false
	}
	for i := 0; i < sf.n; i++ {
		if _, found := slices.BinarySearch(deny, sf.h[i]); found {
			return true
		}
	}
	return false
}

// denyHashes returns the sorted hashes of denyallow domains.
func denyHashes(domains []string) []uint64 {
	if len(domains) == 0 {
		return nil
	}
	out := make([]uint64, 0, len(domains))
	for _, d := range domains {
		out = append(out, hashName(d))
	}
	slices.Sort(out)
	return slices.Compact(out)
}
