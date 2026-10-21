package battlenet

import (
	"maps"
	"math/bits"
	"slices"
	"strings"
)

// Choosing the files of an install: the Battle.net agent's tag query
// (TACT's tag table, as reverse-engineered by CASInstaller). A query has
// sections (speech, text) whose results are united. In a section every
// tag type narrows the entries: to those with one of the type's tags the
// query names, or, when it names none, to those with all of them (the
// files every variant needs). Tags of a type ≥ 0x4000 exclude what they
// mark unless named.

// Bitset is a set of manifest entries.
type Bitset []uint64

func newBitset(n int, all bool) Bitset {
	b := make(Bitset, (n+63)/64)
	if all {
		for i := range b {
			b[i] = ^uint64(0)
		}
		if r := n % 64; r != 0 {
			b[len(b)-1] = 1<<r - 1
		}
	}
	return b
}

// Has reports whether entry i is in the set.
func (b Bitset) Has(i int) bool { return b[i>>6]&(1<<(i&63)) != 0 }

func (b Bitset) set(i int) { b[i>>6] |= 1 << (i & 63) }

// Count is the number of entries in the set.
func (b Bitset) Count() int {
	n := 0
	for _, w := range b {
		n += bits.OnesCount64(w)
	}
	return n
}

// maskBits converts a tag's mask (most significant bit first) to a set.
func maskBits(t *Tag, n int) Bitset {
	b := newBitset(n, false)
	for i := range n {
		if t.Has(i) {
			b.set(i)
		}
	}
	return b
}

// Query is a tag query: sections of tag names.
type Query [][]string

func (q Query) String() string {
	s := make([]string, len(q))
	for i, sec := range q {
		s[i] = strings.Join(sec, " ")
	}
	return strings.Join(s, ":")
}

// Select applies the agent's algorithm.
func Select(tags []Tag, n int, q Query) Bitset {
	types := map[uint16][]int{}
	for i := range tags {
		types[tags[i].Type] = append(types[tags[i].Type], i)
	}
	masks := make([]Bitset, len(tags))
	mask := func(i int) Bitset {
		if masks[i] == nil {
			masks[i] = maskBits(&tags[i], n)
		}
		return masks[i]
	}
	out := newBitset(n, false)
	for _, sec := range q {
		res := newBitset(n, true)
		for _, typ := range slices.Sorted(maps.Keys(types)) {
			idx := types[typ]
			if typ >= 0x4000 {
				for _, i := range idx {
					if !slices.Contains(sec, tags[i].Name) {
						m := mask(i)
						for w := range res {
							res[w] &^= m[w]
						}
					}
				}
				continue
			}
			var matched []int
			for _, i := range idx {
				if slices.Contains(sec, tags[i].Name) {
					matched = append(matched, i)
				}
			}
			if len(matched) > 0 {
				or := newBitset(n, false)
				for _, i := range matched {
					m := mask(i)
					for w := range or {
						or[w] |= m[w]
					}
				}
				for w := range res {
					res[w] &= or[w]
				}
			} else {
				for _, i := range idx {
					m := mask(i)
					for w := range res {
						res[w] &= m[w]
					}
				}
			}
		}
		for w := range out {
			out[w] |= res[w]
		}
	}
	return out
}

// Superset is the safe fallback (BattleNetPrefill's): per tag type the
// entries with one of the named tags, across types all of them; a type
// with no named tag does not narrow.
func Superset(tags []Tag, n int, q Query) Bitset {
	named := map[string]bool{}
	for _, sec := range q {
		for _, t := range sec {
			named[t] = true
		}
	}
	byType := map[uint16]Bitset{}
	for i := range tags {
		if !named[tags[i].Name] {
			continue
		}
		or := byType[tags[i].Type]
		if or == nil {
			or = newBitset(n, false)
			byType[tags[i].Type] = or
		}
		m := maskBits(&tags[i], n)
		for w := range or {
			or[w] |= m[w]
		}
	}
	res := newBitset(n, true)
	for _, or := range byType {
		for w := range res {
			res[w] &= or[w]
		}
	}
	return res
}

// Choose selects with the agent's algorithm, unless that chooses nothing
// or far less than the superset (an unexpected tag layout): then the
// superset.
func Choose(tags []Tag, n int, q Query) (Bitset, bool) {
	a := Select(tags, n, q)
	s := Superset(tags, n, q)
	if ca := a.Count(); ca == 0 && n > 0 || ca*10 < s.Count() {
		return s, true
	}
	return a, false
}
