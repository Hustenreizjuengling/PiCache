package filter

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// Query types of the tests (RFC numbers).
const (
	qtypeMX  uint16 = 15
	qtypeTXT uint16 = 16
)

// A type set admits its types, ANY and, for A or AAAA, HTTPS and SVCB; a
// negated set every type but its own (ANY always); an empty set everything.
func TestTypeSetAdmits(t *testing.T) {
	cases := []struct {
		types  []uint16
		negate bool
		q      uint16
		want   bool
	}{
		{nil, false, qtypeA, true},
		{nil, false, 65280, true},
		{[]uint16{qtypeA}, false, qtypeA, true},
		{[]uint16{qtypeA}, false, qtypeAAAA, false},
		{[]uint16{qtypeA}, false, qtypeHTTPS, true},
		{[]uint16{qtypeAAAA}, false, qtypeSVCB, true},
		{[]uint16{qtypeMX}, false, qtypeHTTPS, false},
		{[]uint16{qtypeMX}, false, qtypeANY, true},
		{[]uint16{qtypeHTTPS}, false, qtypeHTTPS, true},
		{[]uint16{qtypeHTTPS}, false, qtypeA, false},
		{[]uint16{65280, qtypeTXT}, false, 65280, true},
		{[]uint16{65280, qtypeTXT}, false, 65281, false},
		{[]uint16{qtypeA}, true, qtypeA, false},
		{[]uint16{qtypeA}, true, qtypeAAAA, true},
		{[]uint16{qtypeA}, true, qtypeANY, true},
		{[]uint16{qtypeA}, true, qtypeHTTPS, true},
		{[]uint16{65280}, true, 65280, false},
		{[]uint16{65280}, true, qtypeMX, true},
	}
	for _, c := range cases {
		ts := newTypeSet(c.types, c.negate)
		if got := ts.admits(c.q); got != c.want {
			t.Errorf("%v negate=%v admits(%d) = %v, want %v", c.types, c.negate, c.q, got, c.want)
		}
	}
	ts := newTypeSet([]uint16{qtypeAAAA, qtypeA, 65280, qtypeA}, true)
	if got := ts.key(); got != "~A|AAAA|TYPE65280" {
		t.Errorf("key %q", got)
	}
	if !slices.Equal(ts.names(), []string{"A", "AAAA", "TYPE65280"}) {
		t.Errorf("names %v", ts.names())
	}
	if empty := newTypeSet(nil, true); empty.negate || !empty.admits(qtypeMX) {
		t.Error("an empty negated set must admit everything")
	}
}

// $dnstype and $denyallow on list lines: accepted on exact and subtree
// lines ($denyallow on subtree blocks only), invalid when malformed,
// unsupported beyond the bounds and on patterns.
func TestParseModifiers(t *testing.T) {
	many := func(n int, prefix string) string {
		var parts []string
		for i := range n {
			parts = append(parts, fmt.Sprintf("%s%d.x.example", prefix, i))
		}
		return strings.Join(parts, "|")
	}
	types17 := "A|AAAA|MX|TXT|NS|SOA|PTR|SRV|CNAME|HTTPS|SVCB|CAA|DS|DNSKEY|NAPTR|TLSA|SSHFP"
	cases := []struct {
		line, types string
		deny        []string
		status      lineStatus
		kind        uint8
		allow, imp  bool
		allowList   bool
		noGuard     bool
	}{
		{line: "||x.example^$dnstype=AAAA", types: "AAAA", status: lineOK, kind: kindSubtree},
		{line: "||x.example^$dnstype=aaaa|A", types: "A|AAAA", status: lineOK, kind: kindSubtree},
		{line: "||x.example^$dnstype=~A|~AAAA", types: "~A|AAAA", status: lineOK, kind: kindSubtree},
		{line: "||x.example^$dnstype=TYPE65280", types: "TYPE65280", status: lineOK, kind: kindSubtree},
		{line: "|x.example^$dnstype=A", types: "A", status: lineOK, kind: kindExact},
		{line: "x.example$dnstype=A", types: "A", status: lineOK, kind: kindExact},
		{line: "@@||x.example^$dnstype=A", types: "A", status: lineOK, kind: kindSubtree, allow: true},
		{line: "||x.example^$important,dnstype=A", types: "A", status: lineOK, kind: kindSubtree, imp: true},
		{line: "||x.example^$denyallow=b.x.example|a.x.example|a.x.example", deny: []string{"a.x.example", "b.x.example"}, status: lineOK, kind: kindSubtree},
		{line: "||x.example^$dnstype=A,denyallow=a.x.example", types: "A", deny: []string{"a.x.example"}, status: lineOK, kind: kindSubtree},
		{line: "||x.example^$dnstype=~A|AAAA", status: lineInvalid},
		{line: "||x.example^$dnstype=BOGUS", status: lineInvalid},
		{line: "||x.example^$dnstype=", status: lineInvalid},
		{line: "||x.example^$dnstype=A|", status: lineInvalid},
		{line: "||x.example^$dnstype=A,dnstype=AAAA", status: lineInvalid},
		{line: "||x.example^$dnstype=" + types17, status: lineUnsupported},
		{line: "||x.example^$denyallow=bad..name", status: lineInvalid},
		{line: "||x.example^$denyallow=" + many(33, "d"), status: lineUnsupported},
		{line: "@@||x.example^$denyallow=a.x.example", status: lineUnsupported},
		{line: "|x.example^$denyallow=a.x.example", status: lineUnsupported},
		{line: "||x.example^$denyallow=a.x.example", status: lineUnsupported, allowList: true},
		{line: "||ads*.example^$dnstype=A", status: lineUnsupported},
		{line: "/ads[0-9]/$dnstype=A", status: lineUnsupported},
		{line: "||x.example^$dnstype=A,client=1.2.3.4", status: lineUnsupported},
		// The TLD guard judges a line without its denyallow set.
		{line: "||com^$denyallow=example.com", status: lineBroad},
		{line: "||com^$denyallow=example.com", deny: []string{"example.com"}, status: lineOK, kind: kindSubtree, noGuard: true},
		{line: "*$denyallow=com|net", status: lineInvalid},
		{line: "*$denyallow=com|net", status: lineInvalid, noGuard: true},
		{line: "||co.uk^$dnstype=A", status: lineBroad},
	}
	for _, c := range cases {
		lp := lineParser{tldGuard: !c.noGuard, allowList: c.allowList}
		got, st := lp.parse(c.line)
		if st != c.status {
			t.Errorf("%q: status %d, want %d", c.line, st, c.status)
			continue
		}
		if st != lineOK {
			continue
		}
		e := got[0]
		if e.kind != c.kind || e.allow != c.allow || e.important != c.imp || e.types.key() != c.types || !slices.Equal(e.deny, c.deny) {
			t.Errorf("%q: entry %+v (types %q), want kind %d types %q deny %v", c.line, e, e.types.key(), c.kind, c.types, c.deny)
		}
	}
}

// Modified list entries apply only to their types and outside their
// denyallow sets; one that does not apply is skipped: the lookup continues
// with the next source of the same name, the parent suffix and the next
// tier.
func TestModifiedEntriesMatch(t *testing.T) {
	s := buildSnapshot(t, []testList{
		{body: strings.Join([]string{
			"||x.example^$dnstype=AAAA", "||n.example^$dnstype=~A", "||d.example^$denyallow=ok.d.example",
			"@@||y.example^$dnstype=AAAA", "||z.example^$dnstype=AAAA",
			"||sub.p.example^$dnstype=AAAA", "||p.example^$dnstype=A", "|e.example^$dnstype=MX",
		}, "\n"), groups: []int64{1}},
		{body: "||y.example^\n||z.example^$dnstype=A", groups: []int64{1}},
	}, nil)
	cases := []struct {
		q      string
		qtype  uint16
		action Action
		list   int64
		kind   string
	}{
		{"x.example", qtypeA, ActionNone, 0, ""},
		{"a.x.example", qtypeAAAA, ActionBlock, 101, "subtree"},
		{"x.example", qtypeHTTPS, ActionBlock, 101, "subtree"}, // the addresses of HTTPS hints
		{"x.example", qtypeANY, ActionBlock, 101, "subtree"},
		{"x.example", qtypeMX, ActionNone, 0, ""},
		{"n.example", qtypeA, ActionNone, 0, ""},
		{"n.example", qtypeAAAA, ActionBlock, 101, "subtree"},
		{"n.example", qtypeANY, ActionBlock, 101, "subtree"},
		{"d.example", qtypeA, ActionBlock, 101, "subtree"},
		{"a.d.example", qtypeA, ActionBlock, 101, "subtree"},
		{"ok.d.example", qtypeA, ActionNone, 0, ""},
		{"deep.ok.d.example", qtypeA, ActionNone, 0, ""},
		{"y.example", qtypeA, ActionBlock, 102, "subtree"}, // the exception of list 101 is for AAAA only: next tier
		{"y.example", qtypeAAAA, ActionAllow, 101, "subtree"},
		{"z.example", qtypeA, ActionBlock, 102, "subtree"}, // the same name in two sources
		{"z.example", qtypeAAAA, ActionBlock, 101, "subtree"},
		{"www.sub.p.example", qtypeA, ActionBlock, 101, "subtree"}, // parent suffix after a row that does not apply
		{"www.sub.p.example", qtypeMX, ActionNone, 0, ""},
		{"e.example", qtypeMX, ActionBlock, 101, "exact"},
		{"a.e.example", qtypeMX, ActionNone, 0, ""},
	}
	for _, c := range cases {
		d := s.check(c.q, c.qtype, []int64{1}, false)
		if d.Action != c.action || d.ListID != c.list || (c.kind != "" && d.Kind != c.kind) {
			t.Errorf("%s type %d: %+v, want %v list %d %s", c.q, c.qtype, d, c.action, c.list, c.kind)
		}
	}
	if st := s.lists; st.modified != 9 || st.entries != 10 {
		t.Errorf("modified %d entries %d", st.modified, st.entries)
	}
	// Another group never gets them.
	if d := s.check("x.example", qtypeAAAA, []int64{2}, false); d.Action != ActionNone {
		t.Errorf("group 2: %+v", d)
	}
}

// User rules with a type set or a denyallow set: a rule that does not apply
// is skipped (the same name in another set, the parent suffix, the list
// entries behind it).
func TestRuleModifiersMatch(t *testing.T) {
	typed := func(r ruleEntry, types []uint16, negate bool, deny ...string) ruleEntry {
		r.types = newTypeSet(types, negate)
		r.deny, r.denyall = denyHashes(deny), deny
		return r
	}
	s := buildSnapshot(t, []testList{{body: "||l.example^\n||x.example^", groups: []int64{1}}}, []ruleEntry{
		typed(rule(1, ActionBlock, "exact", "r.example", 1), []uint16{qtypeAAAA}, false),
		rule(2, ActionBlock, "subtree", "r.example", 1),
		typed(rule(3, ActionBlock, "subtree", "sub.q.example", 1), []uint16{qtypeAAAA}, false),
		rule(4, ActionBlock, "subtree", "q.example", 1),
		typed(rule(5, ActionAllow, "exact", "l.example", 1), []uint16{qtypeA}, false),
		typed(rule(6, ActionBlock, "regex", `^ads\.`, 1), []uint16{qtypeA}, true),
		typed(rule(7, ActionBlock, "subtree", "d.example", 1), nil, false, "ok.d.example"),
		typed(rule(8, ActionAllow, "subtree", "x.example", 1), []uint16{qtypeHTTPS}, false),
	})
	cases := []struct {
		q     string
		qtype uint16
		want  Action
		src   string
		id    int64
	}{
		{"r.example", qtypeA, ActionBlock, "rule", 2},
		{"r.example", qtypeAAAA, ActionBlock, "rule", 1},
		{"a.sub.q.example", qtypeA, ActionBlock, "rule", 4},
		{"a.sub.q.example", qtypeAAAA, ActionBlock, "rule", 3},
		{"l.example", qtypeA, ActionAllow, "rule", 5},
		{"l.example", qtypeAAAA, ActionBlock, "list", 0}, // an allow rule for A lifts nothing for AAAA
		{"ads.example", qtypeA, ActionNone, "", 0},
		{"ads.example", qtypeAAAA, ActionBlock, "rule", 6},
		{"a.d.example", qtypeA, ActionBlock, "rule", 7},
		{"ok.d.example", qtypeA, ActionNone, "", 0},
		{"x.ok.d.example", qtypeA, ActionNone, "", 0},
		{"x.example", qtypeHTTPS, ActionAllow, "rule", 8},
		{"x.example", qtypeA, ActionBlock, "list", 0},
	}
	for _, c := range cases {
		d := s.check(c.q, c.qtype, []int64{1}, false)
		if d.Action != c.want || d.Source != c.src || d.RuleID != c.id {
			t.Errorf("%s type %d: %+v, want %v %s %d", c.q, c.qtype, d, c.want, c.src, c.id)
		}
		if r := s.check(c.q, c.qtype, []int64{1}, true); c.src == "rule" && (r.Action != c.want || r.RuleID != c.id) {
			t.Errorf("rules only %s type %d: %+v", c.q, c.qtype, r)
		}
	}
}

// $badfilter cancels only the entry with the same kind, tier, domain and
// normalised modifiers.
func TestBadfilterWithModifiers(t *testing.T) {
	s := buildSnapshot(t, []testList{{body: strings.Join([]string{
		"||b.example^", "||b.example^$dnstype=A", "||b.example^$badfilter,dnstype=a",
		"||c.example^$dnstype=AAAA", "||c.example^$badfilter",
		"||e.example^$denyallow=ok.e.example", "||e.example^$denyallow=ok.e.example,badfilter",
	}, "\n"), groups: []int64{1}}}, nil)
	if s.lists.modified != 1 || s.lists.entries != 2 {
		t.Fatalf("modified %d entries %d", s.lists.modified, s.lists.entries)
	}
	for _, c := range []struct {
		q     string
		qtype uint16
		want  bool
	}{
		{"b.example", qtypeA, true}, // the plain entry stays
		{"c.example", qtypeAAAA, true},
		{"c.example", qtypeA, false},
		{"e.example", qtypeA, false},
	} {
		if got := s.check(c.q, c.qtype, []int64{1}, false).Blocked(); got != c.want {
			t.Errorf("%s type %d blocked %v", c.q, c.qtype, got)
		}
	}
}

// Modified entries are capped at 20 000 per list (unsupported) and in
// total (Stats.modifiedDropped).
func TestModifiedEntryCaps(t *testing.T) {
	gen := func(n int, prefix string) string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "||%s%05d.example^$dnstype=AAAA\n", prefix, i)
		}
		return b.String()
	}
	p, err := parseList(context.Background(), strings.NewReader(gen(maxModified+5, "a")), testFormat("block"))
	if err != nil {
		t.Fatal(err)
	}
	if p.modified != maxModified || p.unsupported != 5 || p.entries != maxModified {
		t.Fatalf("modified %d unsupported %d entries %d", p.modified, p.unsupported, p.entries)
	}
	q, err := parseList(context.Background(), strings.NewReader(gen(15000, "b")), testFormat("block"))
	if err != nil {
		t.Fatal(err)
	}
	m := buildListMatcher([]int64{1, 2}, []*parsed{p, q})
	if m.modified != maxModified || m.modDropped != 15000 || m.entries != maxModified {
		t.Fatalf("matcher modified %d dropped %d entries %d", m.modified, m.modDropped, m.entries)
	}
}

// A list keeps at most maxModsPerName modified entries with the same tier,
// kind and name (distinct modifiers): the matcher scans them all on every
// query below that name. The rest is unsupported.
func TestModifiedEntriesPerName(t *testing.T) {
	var b strings.Builder
	for i := range 20 {
		fmt.Fprintf(&b, "||googleapis.com^$dnstype=TYPE%d\n", 1000+i)
		fmt.Fprintf(&b, "|googleapis.com^$dnstype=TYPE%d\n", 1000+i)
		fmt.Fprintf(&b, "@@||googleapis.com^$dnstype=TYPE%d\n", 1000+i)
	}
	b.WriteString("||other.example^$dnstype=AAAA\n")
	p, err := parseList(context.Background(), strings.NewReader(b.String()), testFormat("block"))
	if err != nil {
		t.Fatal(err)
	}
	if p.modified != 3*maxModsPerName+1 || p.unsupported != 3*(20-maxModsPerName) {
		t.Fatalf("modified %d unsupported %d", p.modified, p.unsupported)
	}
}

// An inverted regex block rule blocks the names its expression does not
// match (also when the expression has a literal), after the other user
// regex rules, with its type set and denyallow set; allow rules and list
// exceptions before it win.
func TestInvertedRules(t *testing.T) {
	inv := func(id int64, re string, types []uint16, deny ...string) ruleEntry {
		r := rule(id, ActionBlock, "regex", re, 1)
		r.invert, r.types, r.deny = true, newTypeSet(types, false), denyHashes(deny)
		return r
	}
	s := buildSnapshot(t, []testList{{body: "@@||listok.example^\n||listblock.example^", groups: []int64{1}}}, []ruleEntry{
		inv(1, `(^|\.)allowed\.example$`, nil, "free.example"),
		rule(2, ActionAllow, "exact", "userok.example", 1),
		rule(3, ActionBlock, "regex", `^first\.`, 1),
	})
	cases := []struct {
		q    string
		want Action
		id   int64
		list int64
	}{
		{"www.allowed.example", ActionNone, 0, 0},
		{"other.example", ActionBlock, 1, 0},
		{"userok.example", ActionAllow, 2, 0},
		{"listok.example", ActionAllow, 0, 101},
		{"listblock.example", ActionBlock, 0, 101},
		{"first.other", ActionBlock, 3, 0},
		{"a.free.example", ActionNone, 0, 0},
	}
	for _, c := range cases {
		d := s.check(c.q, qtypeA, []int64{1}, false)
		if d.Action != c.want || d.RuleID != c.id || d.ListID != c.list {
			t.Errorf("%s: %+v, want %v rule %d list %d", c.q, d, c.want, c.id, c.list)
		}
	}
	if d := s.check("other.example", qtypeA, []int64{2}, false); d.Action != ActionNone {
		t.Errorf("group 2: %+v", d)
	}
	typed := buildSnapshot(t, nil, []ruleEntry{inv(1, `^keep\.`, []uint16{qtypeAAAA})})
	if typed.check("x.example", qtypeA, []int64{1}, false).Blocked() || !typed.check("x.example", qtypeAAAA, []int64{1}, false).Blocked() {
		t.Error("the type set of an inverted rule")
	}
	if !typed.check("x.example", qtypeAAAA, []int64{1}, true).Blocked() {
		t.Error("CheckRules evaluates inverted rules")
	}
}

// At most 32 inverted rules, enabled or not, counted within the regex
// rules (409).
func TestInvertedCap(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	yes := true
	for i := range maxInverted {
		if _, err := e.CreateRule(ctx, RuleInput{Action: "block", Type: "regex", Pattern: fmt.Sprintf("^keep%d\\.", i), Enabled: i%2 == 0, Invert: &yes}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := e.CreateRule(ctx, RuleInput{Action: "block", Type: "regex", Pattern: `^more\.`, Invert: &yes})
	wantKind(t, "33rd inverted rule", err, apperr.KindConflict)
	if err == nil || !strings.Contains(err.Error(), "at most 32 inverted regular expressions are supported") {
		t.Fatalf("message: %v", err)
	}
	r, err := e.CreateRule(ctx, RuleInput{Action: "block", Type: "regex", Pattern: `^plain\.`, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.UpdateRule(ctx, r.ID, RuleInput{Action: "block", Type: "regex", Pattern: `^plain\.`, Invert: &yes})
	wantKind(t, "update to inverted", err, apperr.KindConflict)
	if st := e.Stats(); st.Rules != 17 {
		t.Errorf("enabled rules %d", st.Rules)
	}
}

// The hot path stays allocation-free with type sets, denyallow sets,
// modified list entries and inverted rules.
func TestCheckAllocationFreeWithModifiers(t *testing.T) {
	r1 := rule(1, ActionBlock, "subtree", "x.example", 1)
	r1.types, r1.deny = newTypeSet([]uint16{qtypeAAAA}, false), denyHashes([]string{"ok.x.example"})
	r2 := rule(2, ActionBlock, "regex", `^keep\.`, 1)
	r2.invert = true
	s := buildSnapshot(t, []testList{{body: "||m.example^$dnstype=AAAA\n||d.example^$denyallow=ok.d.example", groups: []int64{1}}},
		[]ruleEntry{r1, r2, rule(3, ActionAllow, "exact", "keep.example", 1)})
	for _, q := range []string{"a.x.example", "ok.x.example", "m.example", "a.ok.d.example", "keep.example", "other.test"} {
		for _, qt := range []uint16{qtypeA, qtypeAAAA} {
			if n := testing.AllocsPerRun(100, func() { s.check(q, qt, []int64{1}, false) }); n != 0 {
				t.Errorf("check(%q, %d) allocates %.1f times", q, qt, n)
			}
		}
	}
}

// Check with 32 inverted rules (each query that matches nothing runs all 32
// expressions).
func BenchmarkCheckInverted(b *testing.B) {
	_, s := buildSynthetic(b, 100_000)
	var rules []ruleEntry
	for i := range maxInverted {
		r := rule(int64(i+1), ActionBlock, "regex", fmt.Sprintf(`(^|\.)keep%02d\.example$`, i), 1)
		r.invert = true
		rules = append(rules, r, rule(int64(100+i), ActionAllow, "exact", fmt.Sprintf("x%02d.example", i), 2))
	}
	s.rules = buildRuleMatcher(rules)
	queries := []string{"www.keep07.example", "cdn.d0000042.example-42.com", "img.static.some-long-cdn-name.example.net"}
	groups := []int64{2} // the inverted rules do not apply: every query checks all 32 groups
	b.Run("not applying", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; b.Loop(); i++ {
			s.check(queries[i%len(queries)], qtypeA, groups, false)
		}
	})
	b.Run("applying", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; b.Loop(); i++ {
			s.check("www.keep31.example", qtypeA, []int64{1}, false) // 31 expressions do not match
		}
	})
}
