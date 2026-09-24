package filter

import (
	"regexp"
	"sort"
	"strings"
)

// patternMemEstimate approximates the heap size of one compiled pattern.
const patternMemEstimate = 4 << 10

// hashSet is a sorted array of name hashes (see hashName) with a parallel
// array of source indices: 12 bytes per entry. Equal hashes (the same name
// in several sources) are adjacent and ordered by source index.
type hashSet struct {
	h   []uint64
	src []uint32
}

func (s *hashSet) Len() int { return len(s.h) }
func (s *hashSet) Less(i, j int) bool {
	return s.h[i] < s.h[j] || s.h[i] == s.h[j] && s.src[i] < s.src[j]
}
func (s *hashSet) Swap(i, j int) {
	s.h[i], s.h[j] = s.h[j], s.h[i]
	s.src[i], s.src[j] = s.src[j], s.src[i]
}

func (s *hashSet) memory() int64 { return int64(cap(s.h))*8 + int64(cap(s.src))*4 }

// lookup returns the first source with hash h that shares a group with
// groups (srcGroups[src] are the source's group IDs).
func (s *hashSet) lookup(h uint64, groups []int64, srcGroups [][]int64) (uint32, bool) {
	lo, hi := 0, len(s.h)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if s.h[m] < h {
			lo = m + 1
		} else {
			hi = m
		}
	}
	for i := lo; i < len(s.h) && s.h[i] == h; i++ {
		if src := s.src[i]; applies(srcGroups, src, groups) {
			return src, true
		}
	}
	return 0, false
}

// lookupSuffixes checks the name and each parent suffix, most specific first.
func (s *hashSet) lookupSuffixes(sf *suffixHashes, groups []int64, srcGroups [][]int64) (uint32, bool) {
	if len(s.h) == 0 {
		return 0, false
	}
	for i := sf.n - 1; i >= 0; i-- {
		if src, ok := s.lookup(sf.h[i], groups, srcGroups); ok {
			return src, true
		}
	}
	return 0, false
}

// lookupExact checks only the full name.
func (s *hashSet) lookupExact(sf *suffixHashes, groups []int64, srcGroups [][]int64) (uint32, bool) {
	if len(s.h) == 0 || sf.n == 0 {
		return 0, false
	}
	return s.lookup(sf.h[sf.n-1], groups, srcGroups)
}

// applies reports whether source src shares at least one group with the
// client's groups. Group sets are tiny (usually one element), so a nested
// loop beats anything cleverer and needs no ordering.
func applies(srcGroups [][]int64, src uint32, groups []int64) bool {
	if int(src) >= len(srcGroups) {
		return false
	}
	for _, g := range srcGroups[src] {
		for _, c := range groups {
			if g == c {
				return true
			}
		}
	}
	return false
}

// pattern is a compiled regex or wildcard rule.
type pattern struct {
	re  *regexp.Regexp
	lit string // literal every match contains ("" if unknown)
	src uint32
}

// patternSet indexes patterns by the first four bytes of their literal, so
// a lookup evaluates only the regexes whose literal occurs in the name.
type patternSet struct {
	pats   []pattern
	keys   []uint32 // sorted literal keys
	kidx   []uint32 // parallel to keys: index into pats
	always []uint32 // patterns with a literal shorter than four bytes
}

func key4(s string) uint32 {
	return uint32(s[0]) | uint32(s[1])<<8 | uint32(s[2])<<16 | uint32(s[3])<<24
}

type byKey struct{ ps *patternSet }

func (b byKey) Len() int { return len(b.ps.keys) }
func (b byKey) Less(i, j int) bool {
	k := b.ps.keys
	return k[i] < k[j] || k[i] == k[j] && b.ps.kidx[i] < b.ps.kidx[j]
}
func (b byKey) Swap(i, j int) {
	b.ps.keys[i], b.ps.keys[j] = b.ps.keys[j], b.ps.keys[i]
	b.ps.kidx[i], b.ps.kidx[j] = b.ps.kidx[j], b.ps.kidx[i]
}

func newPatternSet(pats []pattern) patternSet {
	ps := patternSet{pats: pats}
	for i, p := range pats {
		if len(p.lit) < 4 {
			ps.always = append(ps.always, uint32(i))
			continue
		}
		ps.keys = append(ps.keys, key4(p.lit))
		ps.kidx = append(ps.kidx, uint32(i))
	}
	sort.Sort(byKey{&ps})
	return ps
}

func (ps *patternSet) memory() int64 {
	return int64(len(ps.pats))*patternMemEstimate + int64(cap(ps.keys)+cap(ps.kidx)+cap(ps.always))*4
}

// lookup returns the index of a pattern that matches q and applies to groups.
func (ps *patternSet) lookup(q string, groups []int64, srcGroups [][]int64) (int, bool) {
	if len(ps.pats) == 0 {
		return 0, false
	}
	for _, i := range ps.always {
		p := &ps.pats[i]
		if strings.Contains(q, p.lit) && applies(srcGroups, p.src, groups) && p.re.MatchString(q) {
			return int(i), true
		}
	}
	if len(ps.keys) == 0 {
		return 0, false
	}
	for off := 0; off+4 <= len(q); off++ {
		k := key4(q[off:])
		lo, hi := 0, len(ps.keys)
		for lo < hi {
			m := int(uint(lo+hi) >> 1)
			if ps.keys[m] < k {
				lo = m + 1
			} else {
				hi = m
			}
		}
		for j := lo; j < len(ps.keys) && ps.keys[j] == k; j++ {
			i := ps.kidx[j]
			p := &ps.pats[i]
			if strings.HasPrefix(q[off:], p.lit) && applies(srcGroups, p.src, groups) && p.re.MatchString(q) {
				return int(i), true
			}
		}
	}
	return 0, false
}

// listTier holds the entries of one precedence tier.
type listTier struct {
	exact, subtree hashSet
	pats           patternSet
}

// listMatcher is the compiled, immutable form of all enabled lists. Source
// indices refer to ids (list IDs in ascending order).
type listMatcher struct {
	tiers    [numTiers]listTier
	ids      []int64
	entries  int // domain entries (exact + subtree)
	patterns int
	dropped  int // patterns beyond the total cap
	memory   int64
}

// buildListMatcher merges the parse results of the enabled lists; ids[i] is
// the list of results[i]. Patterns are capped at maxPatterns in total.
func buildListMatcher(ids []int64, results []*parsed) *listMatcher {
	m := &listMatcher{ids: ids}
	var pats [numTiers][]pattern
	budget := maxPatterns
	for src, r := range results {
		for _, p := range r.pats {
			if budget == 0 {
				m.dropped++
				continue
			}
			budget--
			pats[p.tier] = append(pats[p.tier], pattern{re: p.re, lit: p.lit, src: uint32(src)})
		}
	}
	for t := range numTiers {
		tr := &m.tiers[t]
		tr.exact = mergeSets(results, t, kindExact)
		tr.subtree = mergeSets(results, t, kindSubtree)
		tr.pats = newPatternSet(pats[t])
		m.entries += tr.exact.Len() + tr.subtree.Len()
		m.patterns += len(pats[t])
		m.memory += tr.exact.memory() + tr.subtree.memory() + tr.pats.memory()
	}
	m.memory += int64(cap(ids)) * 8
	return m
}

func mergeSets(results []*parsed, tier, kind int) hashSet {
	n := 0
	for _, r := range results {
		n += len(r.sets[tier][kind])
	}
	if n == 0 {
		return hashSet{}
	}
	hs := hashSet{h: make([]uint64, 0, n), src: make([]uint32, 0, n)}
	nonEmpty := 0
	for src, r := range results {
		part := r.sets[tier][kind]
		if len(part) > 0 {
			nonEmpty++
		}
		hs.h = append(hs.h, part...)
		for range part {
			hs.src = append(hs.src, uint32(src))
		}
	}
	if nonEmpty > 1 { // a single source is already sorted
		sort.Sort(&hs)
	}
	return hs
}

// ruleEntry describes a compiled user rule.
type ruleEntry struct {
	id      int64
	action  Action
	typ     string // exact | subtree | regex
	pattern string
	re      *regexp.Regexp
	groups  []int64
}

// ruleMatcher is the compiled, immutable form of the enabled user rules.
type ruleMatcher struct {
	allowExact, allowSubtree, blockExact, blockSubtree hashSet
	allowRe, blockRe                                   patternSet
	rules                                              []ruleEntry // source index → rule
	groups                                             [][]int64   // source index → group IDs
	memory                                             int64
}

// buildRuleMatcher compiles rules (regex rules must already be compiled).
func buildRuleMatcher(rules []ruleEntry) *ruleMatcher {
	m := &ruleMatcher{rules: rules, groups: make([][]int64, len(rules))}
	var allowRe, blockRe []pattern
	for i, r := range rules {
		m.groups[i] = r.groups
		src := uint32(i)
		var set *hashSet
		switch {
		case r.typ == "regex" && r.action == ActionAllow:
			allowRe = append(allowRe, pattern{re: r.re, lit: regexLiteral(r.pattern), src: src})
		case r.typ == "regex":
			blockRe = append(blockRe, pattern{re: r.re, lit: regexLiteral(r.pattern), src: src})
		case r.typ == "exact" && r.action == ActionAllow:
			set = &m.allowExact
		case r.typ == "exact":
			set = &m.blockExact
		case r.action == ActionAllow:
			set = &m.allowSubtree
		default:
			set = &m.blockSubtree
		}
		if set != nil {
			set.h = append(set.h, hashName(r.pattern))
			set.src = append(set.src, src)
		}
		m.memory += int64(len(r.pattern)) + 96
	}
	for _, s := range []*hashSet{&m.allowExact, &m.allowSubtree, &m.blockExact, &m.blockSubtree} {
		sort.Sort(s)
		m.memory += s.memory()
	}
	m.allowRe, m.blockRe = newPatternSet(allowRe), newPatternSet(blockRe)
	m.memory += m.allowRe.memory() + m.blockRe.memory()
	return m
}

func (m *ruleMatcher) decision(src uint32, action Action, kind string) Decision {
	r := &m.rules[src]
	return Decision{Action: action, Source: "rule", Kind: kind, RuleID: r.id, Name: r.pattern}
}

// snapshot is the immutable state read by the DNS hot path. Editing the
// groups or name of a list swaps only listGroups/listNames (the compiled
// matcher is shared between snapshots).
type snapshot struct {
	lists      *listMatcher
	listNames  []string  // source index → list name
	listGroups [][]int64 // source index → group IDs (nil: applies to nobody)
	rules      *ruleMatcher
}

var emptySnapshot = &snapshot{lists: &listMatcher{}, rules: buildRuleMatcher(nil)}

func (s *snapshot) listDecision(src uint32, tier int, kind string) Decision {
	d := Decision{Action: ActionBlock, Source: "list", Kind: kind, Important: tier < tierAllow}
	if tier == tierImpAllow || tier == tierAllow {
		d.Action = ActionAllow
	}
	if int(src) < len(s.lists.ids) {
		d.ListID = s.lists.ids[src]
	}
	if int(src) < len(s.listNames) {
		d.Name = s.listNames[src]
	}
	return d
}

// check implements the precedence of ARCHITECTURE 7.2. With rulesOnly, only
// user rules are evaluated (steps 1–5 and 10).
func (s *snapshot) check(q string, groups []int64, rulesOnly bool) Decision {
	if len(q) == 0 || len(q) > maxDomainLen || len(groups) == 0 {
		return Decision{}
	}
	var sf suffixHashes
	sf.compute(q)
	r := s.rules
	if len(r.rules) > 0 {
		if src, ok := r.allowExact.lookupExact(&sf, groups, r.groups); ok { // 1
			return r.decision(src, ActionAllow, "exact")
		}
		if src, ok := r.allowSubtree.lookupSuffixes(&sf, groups, r.groups); ok { // 2
			return r.decision(src, ActionAllow, "subtree")
		}
		if i, ok := r.allowRe.lookup(q, groups, r.groups); ok { // 3
			return r.decision(r.allowRe.pats[i].src, ActionAllow, "regex")
		}
		if src, ok := r.blockExact.lookupExact(&sf, groups, r.groups); ok { // 4
			return r.decision(src, ActionBlock, "exact")
		}
		if src, ok := r.blockSubtree.lookupSuffixes(&sf, groups, r.groups); ok { // 5
			return r.decision(src, ActionBlock, "subtree")
		}
	}
	if !rulesOnly {
		l := s.lists
		for t := range numTiers { // 6–9
			tr := &l.tiers[t]
			if src, ok := tr.exact.lookupExact(&sf, groups, s.listGroups); ok {
				return s.listDecision(src, t, "exact")
			}
			if src, ok := tr.subtree.lookupSuffixes(&sf, groups, s.listGroups); ok {
				return s.listDecision(src, t, "subtree")
			}
			if t == tierBlock {
				break // list regex/pattern blocks come after user regex blocks
			}
			if i, ok := tr.pats.lookup(q, groups, s.listGroups); ok {
				return s.listDecision(tr.pats.pats[i].src, t, "regex")
			}
		}
	}
	if i, ok := r.blockRe.lookup(q, groups, r.groups); ok { // 10
		return r.decision(r.blockRe.pats[i].src, ActionBlock, "regex")
	}
	if !rulesOnly { // 11
		tr := &s.lists.tiers[tierBlock]
		if i, ok := tr.pats.lookup(q, groups, s.listGroups); ok {
			return s.listDecision(tr.pats.pats[i].src, tierBlock, "regex")
		}
	}
	return Decision{}
}
