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
// groups (srcGroups[src] are the source's group IDs) and that accept, when
// set, admits (the type set and denyallow of a user rule). A hit that does
// not apply is skipped: the search continues with the next source.
func (s *hashSet) lookup(h uint64, groups []int64, srcGroups [][]int64, accept func(uint32) bool) (uint32, bool) {
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
		if src := s.src[i]; applies(srcGroups, src, groups) && (accept == nil || accept(src)) {
			return src, true
		}
	}
	return 0, false
}

// lookupSuffixes checks the name and each parent suffix, most specific first.
func (s *hashSet) lookupSuffixes(sf *suffixHashes, groups []int64, srcGroups [][]int64, accept func(uint32) bool) (uint32, bool) {
	if len(s.h) == 0 {
		return 0, false
	}
	for i := sf.n - 1; i >= 0; i-- {
		if src, ok := s.lookup(sf.h[i], groups, srcGroups, accept); ok {
			return src, true
		}
	}
	return 0, false
}

// lookupExact checks only the full name.
func (s *hashSet) lookupExact(sf *suffixHashes, groups []int64, srcGroups [][]int64, accept func(uint32) bool) (uint32, bool) {
	if len(s.h) == 0 || sf.n == 0 {
		return 0, false
	}
	return s.lookup(sf.h[sf.n-1], groups, srcGroups, accept)
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

// lookup returns the index of a pattern that matches q, applies to groups
// and that accept, when set, admits (checked before the expression runs; a
// pattern that does not apply is skipped).
func (ps *patternSet) lookup(q string, groups []int64, srcGroups [][]int64, accept func(uint32) bool) (int, bool) {
	if len(ps.pats) == 0 {
		return 0, false
	}
	for _, i := range ps.always {
		p := &ps.pats[i]
		if strings.Contains(q, p.lit) && applies(srcGroups, p.src, groups) && (accept == nil || accept(p.src)) && p.re.MatchString(q) {
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
			if strings.HasPrefix(q[off:], p.lit) && applies(srcGroups, p.src, groups) && (accept == nil || accept(p.src)) && p.re.MatchString(q) {
				return int(i), true
			}
		}
	}
	return 0, false
}

// modRow is a list entry with $dnstype or $denyallow (a modified entry):
// it applies only while its type set admits the query type and its
// denyallow set does not except the name. It never goes into the hash sets.
type modRow struct {
	hash  uint64
	src   uint32
	kind  uint8    // kindExact | kindSubtree
	types *typeSet // nil: every type (shared by the rows with the same set)
	deny  []uint64 // denyallow hashes, sorted
}

// modTable is the modified-entries table of one tier: sorted by name hash,
// then source index.
type modTable []modRow

func sortMods(t modTable) {
	sort.Slice(t, func(i, j int) bool { return t[i].hash < t[j].hash || t[i].hash == t[j].hash && t[i].src < t[j].src })
}

func (t modTable) memory() int64 {
	n := int64(cap(t)) * 48
	for i := range t {
		n += int64(len(t[i].deny)) * 8
	}
	return n
}

// lookup probes the name and its parent suffixes (most specific first;
// exact rows only for the full name) and returns the first row that shares
// a group with groups and applies to qtype and the name. A row that does
// not apply means "no match, continue".
func (t modTable) lookup(sf *suffixHashes, qtype uint16, groups []int64, srcGroups [][]int64) (int, bool) {
	if len(t) == 0 {
		return 0, false
	}
	for i := sf.n - 1; i >= 0; i-- {
		h := sf.h[i]
		lo, hi := 0, len(t)
		for lo < hi {
			m := int(uint(lo+hi) >> 1)
			if t[m].hash < h {
				lo = m + 1
			} else {
				hi = m
			}
		}
		for j := lo; j < len(t) && t[j].hash == h; j++ {
			r := &t[j]
			if (r.kind == kindExact && i != sf.n-1) || !applies(srcGroups, r.src, groups) ||
				(r.types != nil && !r.types.admits(qtype)) || excepted(r.deny, sf) {
				continue
			}
			return j, true
		}
	}
	return 0, false
}

// listTier holds the entries of one precedence tier.
type listTier struct {
	exact, subtree hashSet
	mods           modTable
	pats           patternSet
}

// listMatcher is the compiled, immutable form of all enabled lists. Source
// indices refer to ids (list IDs in ascending order).
type listMatcher struct {
	tiers      [numTiers]listTier
	ip         [numIPTiers]ipTier // address entries of lists of format ips: allow, block
	ids        []int64
	entries    int // domain, modified and address entries (patterns are counted in patterns)
	patterns   int
	dropped    int // patterns beyond the total caps
	modified   int // of entries: modified entries
	modDropped int // modified entries beyond the total cap
	ipEntries  int // of entries: address entries
	memory     int64
}

// buildListMatcher merges the parse results of the enabled lists; ids[i] is
// the list of results[i]. Patterns are capped at maxPatterns and
// maxPatternCost in total, modified entries at maxModified.
func buildListMatcher(ids []int64, results []*parsed) *listMatcher {
	m := &listMatcher{ids: ids}
	var pats [numTiers][]pattern
	var mods [numTiers]modTable
	budget, costBudget, modBudget := maxPatterns, int64(maxPatternCost), maxModified
	for src, r := range results {
		for _, p := range r.pats {
			if budget == 0 || int64(p.cost) > costBudget {
				m.dropped++
				continue
			}
			budget--
			costBudget -= int64(p.cost)
			pats[p.tier] = append(pats[p.tier], pattern{re: p.re, lit: p.lit, src: uint32(src)})
		}
		for t := range numTiers {
			for _, row := range r.mods[t] {
				if modBudget == 0 {
					m.modDropped++
					continue
				}
				modBudget--
				row.src = uint32(src)
				mods[t] = append(mods[t], row)
			}
		}
	}
	for t := range numTiers {
		tr := &m.tiers[t]
		tr.exact = mergeSets(results, t, kindExact)
		tr.subtree = mergeSets(results, t, kindSubtree)
		tr.mods = mods[t]
		sortMods(tr.mods)
		tr.pats = newPatternSet(pats[t])
		m.entries += tr.exact.Len() + tr.subtree.Len() + len(tr.mods)
		m.modified += len(tr.mods)
		m.patterns += len(pats[t])
		m.memory += tr.exact.memory() + tr.subtree.memory() + tr.mods.memory() + tr.pats.memory()
	}
	for t := range numIPTiers {
		m.ip[t] = mergeIPTier(results, t)
		n := m.ip[t].v4.Len() + m.ip[t].v6.Len()
		m.entries += n
		m.ipEntries += n
		m.memory += m.ip[t].v4.memory() + m.ip[t].v6.memory()
	}
	m.memory += int64(cap(ids)) * 8
	return m
}

func mergeSets(results []*parsed, tier, kind int) hashSet {
	parts := make([][]uint64, len(results))
	for i, r := range results {
		parts[i] = r.sets[tier][kind]
	}
	return mergeHashes(parts)
}

// mergeHashes merges sorted hash lists (parts[src]) into one set.
func mergeHashes(parts [][]uint64) hashSet {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	if n == 0 {
		return hashSet{}
	}
	hs := hashSet{h: make([]uint64, 0, n), src: make([]uint32, 0, n)}
	nonEmpty := 0
	for src, part := range parts {
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
	types   typeSet
	deny    []uint64 // denyallow hashes, sorted
	denyall []string // the denyallow domains (Explain, Search)
	invert  bool
	// reply replaces the blocking mode for the answers the rule decides
	// ("" = the global mode); replyV4/replyV6 are the custom_ip addresses.
	reply, replyV4, replyV6 string
}

// appliesTo reports whether the rule applies to a query of type qtype for
// the name of sf: its type set admits the type and no denyallow domain
// excepts the name (groups are checked by the caller).
func (r *ruleEntry) appliesTo(qtype uint16, sf *suffixHashes) bool {
	return r.types.admits(qtype) && !excepted(r.deny, sf)
}

// modified reports whether the rule has a type set or a denyallow set.
func (r *ruleEntry) modified() bool { return !r.types.empty() || len(r.deny) > 0 }

// ruleMatcher is the compiled, immutable form of the enabled user rules.
type ruleMatcher struct {
	allowExact, allowSubtree, blockExact, blockSubtree hashSet
	allowRe, blockRe                                   patternSet
	inverted                                           []uint32    // source indices of the inverted block rules (evaluated after blockRe)
	rules                                              []ruleEntry // source index → rule
	groups                                             [][]int64   // source index → group IDs
	modified                                           bool        // a rule has a type set or a denyallow set
	memory                                             int64
}

// buildRuleMatcher compiles rules (regex rules must already be compiled).
// Inverted rules are kept in their own set without literal index: a
// missing literal is exactly when they match.
func buildRuleMatcher(rules []ruleEntry) *ruleMatcher {
	m := &ruleMatcher{rules: rules, groups: make([][]int64, len(rules))}
	var allowRe, blockRe []pattern
	for i, r := range rules {
		m.groups[i] = r.groups
		m.modified = m.modified || r.modified()
		src := uint32(i)
		var set *hashSet
		switch {
		case r.typ == "regex" && r.action == ActionAllow:
			allowRe = append(allowRe, pattern{re: r.re, lit: regexLiteral(r.pattern), src: src})
		case r.typ == "regex" && r.invert:
			m.inverted = append(m.inverted, src)
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
		m.memory += int64(len(r.pattern)) + 96 + int64(len(r.deny))*8 + int64(len(r.types.high))*2
		if r.re != nil && r.invert {
			m.memory += patternMemEstimate
		}
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
	return Decision{Action: action, Source: "rule", Kind: kind, RuleID: r.id, Name: r.pattern,
		Reply: r.reply, ReplyIPv4: r.replyV4, ReplyIPv6: r.replyV6}
}

// checkInverted evaluates the inverted rules (step 10, after the other
// user regex block rules): the first that applies to the client, the type
// and the name and whose expression does not match q. At most maxInverted
// expressions run.
func (m *ruleMatcher) checkInverted(q string, qtype uint16, sf *suffixHashes, groups []int64) (Decision, bool) {
	for _, src := range m.inverted {
		r := &m.rules[src]
		if applies(m.groups, src, groups) && r.appliesTo(qtype, sf) && !r.re.MatchString(q) {
			return m.decision(src, ActionBlock, "regex"), true
		}
	}
	return Decision{}, false
}

// snapshot is the immutable state read by the DNS hot path. Editing the
// groups, name or category of a list swaps only listGroups/listNames/
// listCats/protGroups (the compiled matcher is shared between snapshots).
type snapshot struct {
	lists      *listMatcher
	listNames  []string  // source index → list name
	listCats   []string  // source index → list category
	listGroups [][]int64 // source index → group IDs (nil: applies to nobody)
	// protGroups is listGroups restricted to the protection lists (nil
	// for every other list), the table of checkProtection; hasProt: at
	// least one entry is set.
	protGroups [][]int64
	hasProt    bool
	rules      *ruleMatcher
	ipRules    *ipRuleMatcher
	// hasIP: an address entry of a list or an enabled IP rule exists
	// (CheckIP returns at once otherwise).
	hasIP bool
}

var emptySnapshot = &snapshot{lists: &listMatcher{}, rules: buildRuleMatcher(nil), ipRules: buildIPRuleMatcher(nil)}

func (s *snapshot) listDecision(src uint32, tier int, kind string) Decision {
	d := Decision{Action: ActionBlock, Source: "list", Kind: kind, Important: tier < tierAllow}
	if tier == tierImpAllow || tier == tierAllow {
		d.Action = ActionAllow
	}
	s.nameList(&d, src)
	return d
}

// nameList sets the list ID, name and category of a list decision.
func (s *snapshot) nameList(d *Decision, src uint32) {
	if int(src) < len(s.lists.ids) {
		d.ListID = s.lists.ids[src]
	}
	if int(src) < len(s.listNames) {
		d.Name = s.listNames[src]
	}
	if int(src) < len(s.listCats) {
		d.Category = s.listCats[src]
	}
}

// checkTier applies one list tier (exact, subtree, modified entries, then
// patterns unless skipPats) for the groups table srcGroups.
func (s *snapshot) checkTier(t int, q string, qtype uint16, sf *suffixHashes, groups []int64, srcGroups [][]int64, skipPats bool) (Decision, bool) {
	tr := &s.lists.tiers[t]
	if src, ok := tr.exact.lookupExact(sf, groups, srcGroups, nil); ok {
		return s.listDecision(src, t, "exact"), true
	}
	if src, ok := tr.subtree.lookupSuffixes(sf, groups, srcGroups, nil); ok {
		return s.listDecision(src, t, "subtree"), true
	}
	if i, ok := tr.mods.lookup(sf, qtype, groups, srcGroups); ok {
		kind := "subtree"
		if tr.mods[i].kind == kindExact {
			kind = "exact"
		}
		return s.listDecision(tr.mods[i].src, t, kind), true
	}
	if skipPats {
		return Decision{}, false
	}
	if i, ok := tr.pats.lookup(q, groups, srcGroups, nil); ok {
		return s.listDecision(tr.pats.pats[i].src, t, "regex"), true
	}
	return Decision{}, false
}

// checkProtection applies the list precedence (7.2 steps 6–9 and 11) over
// the protection lists only (protGroups); there are no user rules, so the
// list regex/pattern blocks directly follow the domain blocks.
func (s *snapshot) checkProtection(q string, qtype uint16, groups []int64) Decision {
	if !s.hasProt || len(q) == 0 || len(q) > maxDomainLen || len(groups) == 0 {
		return Decision{}
	}
	var sf suffixHashes
	sf.compute(q)
	for t := range numTiers {
		if d, ok := s.checkTier(t, q, qtype, &sf, groups, s.protGroups, false); ok {
			return d
		}
	}
	return Decision{}
}

// check implements the precedence of ARCHITECTURE 7.2 for a query of type
// qtype: a rule or entry that does not apply to the type or the name is
// skipped as if absent. With rulesOnly, only user rules are evaluated
// (steps 1–5 and 10).
func (s *snapshot) check(q string, qtype uint16, groups []int64, rulesOnly bool) Decision {
	if len(q) == 0 || len(q) > maxDomainLen || len(groups) == 0 {
		return Decision{}
	}
	var sf suffixHashes
	sf.compute(q)
	r := s.rules
	var accept func(uint32) bool
	if r.modified {
		accept = func(src uint32) bool { return r.rules[src].appliesTo(qtype, &sf) }
	}
	if len(r.rules) > 0 {
		if src, ok := r.allowExact.lookupExact(&sf, groups, r.groups, accept); ok { // 1
			return r.decision(src, ActionAllow, "exact")
		}
		if src, ok := r.allowSubtree.lookupSuffixes(&sf, groups, r.groups, accept); ok { // 2
			return r.decision(src, ActionAllow, "subtree")
		}
		if i, ok := r.allowRe.lookup(q, groups, r.groups, accept); ok { // 3
			return r.decision(r.allowRe.pats[i].src, ActionAllow, "regex")
		}
		if src, ok := r.blockExact.lookupExact(&sf, groups, r.groups, accept); ok { // 4
			return r.decision(src, ActionBlock, "exact")
		}
		if src, ok := r.blockSubtree.lookupSuffixes(&sf, groups, r.groups, accept); ok { // 5
			return r.decision(src, ActionBlock, "subtree")
		}
	}
	if !rulesOnly {
		for t := range numTiers { // 6–9; list regex/pattern blocks come after user regex blocks
			if d, ok := s.checkTier(t, q, qtype, &sf, groups, s.listGroups, t == tierBlock); ok {
				return d
			}
		}
	}
	if i, ok := r.blockRe.lookup(q, groups, r.groups, accept); ok { // 10
		return r.decision(r.blockRe.pats[i].src, ActionBlock, "regex")
	}
	if d, ok := r.checkInverted(q, qtype, &sf, groups); ok { // 10: inverted
		return d
	}
	if !rulesOnly { // 11
		tr := &s.lists.tiers[tierBlock]
		if i, ok := tr.pats.lookup(q, groups, s.listGroups, nil); ok {
			return s.listDecision(tr.pats.pats[i].src, tierBlock, "regex")
		}
	}
	return Decision{}
}
