package filter

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// testList is one list source of a test snapshot.
type testList struct {
	body   string
	kind   string // block (default) | allow
	groups []int64
}

func rule(id int64, action Action, typ, pattern string, groups ...int64) ruleEntry {
	r := ruleEntry{id: id, action: action, typ: typ, pattern: pattern, groups: groups}
	if typ == "regex" {
		r.re = regexp.MustCompile("(?i)" + pattern)
	}
	return r
}

// buildSnapshot compiles lists (list IDs 101, 102, …) and rules.
func buildSnapshot(t testing.TB, lists []testList, rules []ruleEntry) *snapshot {
	t.Helper()
	var ids []int64
	var results []*parsed
	s := &snapshot{rules: buildRuleMatcher(rules)}
	for i, l := range lists {
		kind := l.kind
		if kind == "" {
			kind = "block"
		}
		p, err := parseList(context.Background(), strings.NewReader(l.body), kind, "exact")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, int64(101+i))
		results = append(results, p)
		s.listNames = append(s.listNames, fmt.Sprintf("list%d", 101+i))
		s.listGroups = append(s.listGroups, l.groups)
	}
	s.lists = buildListMatcher(ids, results)
	return s
}

func TestPrecedenceMatrix(t *testing.T) {
	const q = "ads.example.com"
	type step struct {
		list *testList
		rule *ruleEntry
		want Decision // Action, Source, Kind, Important
	}
	r := func(re ruleEntry) *ruleEntry { return &re }
	l := func(body string) *testList { return &testList{body: body, groups: []int64{1}} }
	steps := []step{
		1:  {rule: r(rule(1, ActionAllow, "exact", q, 1)), want: Decision{Action: ActionAllow, Source: "rule", Kind: "exact"}},
		2:  {rule: r(rule(2, ActionAllow, "subtree", "example.com", 1)), want: Decision{Action: ActionAllow, Source: "rule", Kind: "subtree"}},
		3:  {rule: r(rule(3, ActionAllow, "regex", `^ads\.`, 1)), want: Decision{Action: ActionAllow, Source: "rule", Kind: "regex"}},
		4:  {rule: r(rule(4, ActionBlock, "exact", q, 1)), want: Decision{Action: ActionBlock, Source: "rule", Kind: "exact"}},
		5:  {rule: r(rule(5, ActionBlock, "subtree", "example.com", 1)), want: Decision{Action: ActionBlock, Source: "rule", Kind: "subtree"}},
		6:  {list: l("@@||example.com^$important"), want: Decision{Action: ActionAllow, Source: "list", Kind: "subtree", Important: true}},
		7:  {list: l("||example.com^$important"), want: Decision{Action: ActionBlock, Source: "list", Kind: "subtree", Important: true}},
		8:  {list: l("@@||example.com^"), want: Decision{Action: ActionAllow, Source: "list", Kind: "subtree"}},
		9:  {list: l("||example.com^"), want: Decision{Action: ActionBlock, Source: "list", Kind: "subtree"}},
		10: {rule: r(rule(10, ActionBlock, "regex", `ads`, 1)), want: Decision{Action: ActionBlock, Source: "rule", Kind: "regex"}},
		11: {list: l(`/ads\.example/`), want: Decision{Action: ActionBlock, Source: "list", Kind: "regex"}},
	}
	for k := 1; k < len(steps); k++ {
		var lists []testList
		var rules []ruleEntry
		for _, s := range steps[k:] {
			if s.list != nil {
				lists = append(lists, *s.list)
			} else {
				rules = append(rules, *s.rule)
			}
		}
		snap := buildSnapshot(t, lists, rules)
		got := snap.check(q, []int64{1}, false)
		w := steps[k].want
		if got.Action != w.Action || got.Source != w.Source || got.Kind != w.Kind || got.Important != w.Important {
			t.Errorf("step %d decisive: got %+v, want %+v", k, got, w)
		}
		if w.Source == "rule" && got.RuleID != int64(k) {
			t.Errorf("step %d: rule id %d", k, got.RuleID)
		}
		if w.Source == "list" && (got.ListID != 101 || got.Name != "list101") {
			t.Errorf("step %d: list %d %q", k, got.ListID, got.Name)
		}
	}
}

func TestPrecedenceDetails(t *testing.T) {
	g := []int64{1}
	cases := []struct {
		name  string
		lists []testList
		rules []ruleEntry
		q     string
		want  Action
		src   string
	}{
		{"allow list beats block list", []testList{{body: "||example.com^", groups: g}, {body: "example.com", kind: "allow", groups: g}}, nil, "example.com", ActionAllow, "list"},
		{"allow list is exact for plain entries", []testList{{body: "||example.com^", groups: g}, {body: "example.com", kind: "allow", groups: g}}, nil, "a.example.com", ActionBlock, "list"},
		{"@@/re/ in a list is a list allow", []testList{{body: "@@/^ads\\./\n||example.com^", groups: g}}, nil, "ads.example.com", ActionAllow, "list"},
		{"list allow beats user regex deny", []testList{{body: "@@||example.com^", groups: g}}, []ruleEntry{rule(1, ActionBlock, "regex", "example", 1)}, "example.com", ActionAllow, "list"},
		{"user subtree deny beats list important allow", []testList{{body: "@@||example.com^$important", groups: g}}, []ruleEntry{rule(1, ActionBlock, "subtree", "example.com", 1)}, "x.example.com", ActionBlock, "rule"},
		{"user exact allow beats user deny", nil, []ruleEntry{rule(1, ActionBlock, "subtree", "example.com", 1), rule(2, ActionAllow, "exact", "a.example.com", 1)}, "a.example.com", ActionAllow, "rule"},
		{"exact allow does not cover subdomains", nil, []ruleEntry{rule(1, ActionBlock, "subtree", "example.com", 1), rule(2, ActionAllow, "exact", "a.example.com", 1)}, "b.a.example.com", ActionBlock, "rule"},
		{"subtree matches apex", []testList{{body: "*.example.com", groups: g}}, nil, "example.com", ActionBlock, "list"},
		{"subtree is label-aligned", []testList{{body: "||example.com^", groups: g}}, nil, "badexample.com", ActionNone, ""},
		{"exact does not match subdomain", []testList{{body: "0.0.0.0 example.com", groups: g}}, nil, "www.example.com", ActionNone, ""},
		{"hosts entry", []testList{{body: "0.0.0.0 example.com", groups: g}}, nil, "example.com", ActionBlock, "list"},
		{"wildcard pattern", []testList{{body: "||ad*.example.com^", groups: g}}, nil, "adserver.example.com", ActionBlock, "list"},
		{"short-literal pattern", []testList{{body: "||ab*^", groups: g}}, nil, "abc.example", ActionBlock, "list"},
		{"regex is case-insensitive", nil, []ruleEntry{rule(1, ActionBlock, "regex", "^ADS\\.", 1)}, "ads.example.com", ActionBlock, "rule"},
		{"no match", []testList{{body: "||example.com^", groups: g}}, nil, "example.org", ActionNone, ""},
	}
	for _, c := range cases {
		got := buildSnapshot(t, c.lists, c.rules).check(c.q, g, false)
		if got.Action != c.want || got.Source != c.src {
			t.Errorf("%s: got %+v, want %v from %q", c.name, got, c.want, c.src)
		}
	}
}

func TestGroupScoping(t *testing.T) {
	s := buildSnapshot(t,
		[]testList{
			{body: "||kids.example^", groups: []int64{2}},
			{body: "||all.example^", groups: []int64{1, 2}},
			{body: "||nobody.example^"},
			{body: "||shared.example^", groups: []int64{1}},
		},
		[]ruleEntry{rule(1, ActionAllow, "subtree", "shared.example", 3)})
	cases := []struct {
		q      string
		groups []int64
		want   Action
	}{
		{"kids.example", []int64{1}, ActionNone},
		{"kids.example", []int64{2}, ActionBlock},
		{"kids.example", []int64{1, 2}, ActionBlock},
		{"all.example", []int64{1}, ActionBlock},
		{"all.example", []int64{2}, ActionBlock},
		{"all.example", []int64{3}, ActionNone},
		{"all.example", nil, ActionNone},
		{"nobody.example", []int64{1, 2, 3}, ActionNone},
		{"shared.example", []int64{1}, ActionBlock},
		{"shared.example", []int64{1, 3}, ActionAllow}, // the group-3 allow rule applies
		{"shared.example", []int64{3}, ActionAllow},
	}
	for _, c := range cases {
		if got := s.check(c.q, c.groups, false); got.Action != c.want {
			t.Errorf("%s for groups %v: got %v, want %v", c.q, c.groups, got.Action, c.want)
		}
	}

	// The same name in two lists with different groups: the applicable one decides.
	s = buildSnapshot(t, []testList{{body: "||x.example^", groups: []int64{2}}, {body: "||x.example^", groups: []int64{1}}}, nil)
	if got := s.check("x.example", []int64{1}, false); got.ListID != 102 {
		t.Errorf("got list %d, want 102", got.ListID)
	}
	if got := s.check("x.example", []int64{1, 2}, false); got.ListID != 101 {
		t.Errorf("got list %d, want 101 (lowest source first)", got.ListID)
	}
}

func TestCheckRulesIgnoresLists(t *testing.T) {
	s := buildSnapshot(t,
		[]testList{{body: "||steamcontent.com^", groups: []int64{1}}},
		[]ruleEntry{rule(1, ActionBlock, "subtree", "steampowered.com", 2), rule(2, ActionBlock, "regex", "^cdn\\.", 2)})
	if got := s.check("lancache.steamcontent.com", []int64{1}, true); got.Action != ActionNone {
		t.Errorf("CheckRules must ignore lists: %+v", got)
	}
	if got := s.check("store.steampowered.com", []int64{2}, true); !got.Blocked() || got.RuleID != 1 {
		t.Errorf("CheckRules subtree: %+v", got)
	}
	if got := s.check("cdn.example", []int64{2}, true); !got.Blocked() || got.RuleID != 2 {
		t.Errorf("CheckRules regex: %+v", got)
	}
	if got := s.check("store.steampowered.com", []int64{1}, true); got.Action != ActionNone {
		t.Errorf("rule of another group applied: %+v", got)
	}
}

func TestCheckAllocationFree(t *testing.T) {
	s := buildSnapshot(t,
		[]testList{{body: "||ads.example.com^\n0.0.0.0 t.example.org\n||ad*.pattern.net^\n/^re[0-9]+\\./", groups: []int64{1}}},
		[]ruleEntry{rule(1, ActionAllow, "exact", "ok.example.com", 1), rule(2, ActionBlock, "regex", "^evil", 1)})
	for _, q := range []string{"ads.example.com", "www.example.org", "adx.pattern.net", "re12.foo", "ok.example.com", "a.b.c.d.e.f.nothing.test"} {
		if n := testing.AllocsPerRun(100, func() { s.check(q, []int64{1}, false) }); n != 0 {
			t.Errorf("check(%q) allocates %.1f times", q, n)
		}
	}
}

// syntheticList generates n "||dNNNNNNN.example-NN.com^" lines without
// holding them in memory.
type syntheticList struct {
	n, i int
	buf  []byte
}

func (s *syntheticList) Read(p []byte) (int, error) {
	for len(s.buf) < len(p) && s.i < s.n {
		s.buf = fmt.Appendf(s.buf, "||d%07d.example-%02d.com^\n", s.i, s.i%97)
		s.i++
	}
	if len(s.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.buf)
	s.buf = s.buf[:copy(s.buf, s.buf[n:])]
	return n, nil
}

func buildSynthetic(t testing.TB, n int) (*parsed, *snapshot) {
	t.Helper()
	p, err := parseList(context.Background(), &syntheticList{n: n}, "block", "exact")
	if err != nil {
		t.Fatal(err)
	}
	m := buildListMatcher([]int64{1}, []*parsed{p})
	return p, &snapshot{lists: m, rules: buildRuleMatcher(nil), listNames: []string{"big"}, listGroups: [][]int64{{1}}}
}

func TestMatcherMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a 1M-entry matcher")
	}
	const n = 1_000_000
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	p, s := buildSynthetic(t, n)
	runtime.GC()
	runtime.ReadMemStats(&after)
	used := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("1M entries: heap %.1f MiB (parse result %.1f MiB, matcher %.1f MiB)",
		float64(used)/(1<<20), float64(p.memory())/(1<<20), float64(s.lists.memory)/(1<<20))
	if used > 40<<20 {
		t.Errorf("1M entries use %d MiB, want < 40 MiB", used>>20)
	}
	if s.lists.entries != n || p.entries != n {
		t.Errorf("entries = %d/%d", s.lists.entries, p.entries)
	}
	for _, q := range []string{"d0000000.example-00.com", "x.d0999999.example-" + fmt.Sprintf("%02d", 999999%97) + ".com"} {
		if !s.check(q, []int64{1}, false).Blocked() {
			t.Errorf("%s not blocked", q)
		}
	}
	if s.check("d1000000.example-00.com", []int64{1}, false).Blocked() {
		t.Error("unexpected block")
	}
	runtime.KeepAlive(p)
}

func BenchmarkCheck(b *testing.B) {
	_, s := buildSynthetic(b, 1_000_000)
	s.rules = buildRuleMatcher([]ruleEntry{
		rule(1, ActionAllow, "subtree", "allowed.example", 1),
		rule(2, ActionBlock, "regex", `^telemetry[0-9]*\.`, 1),
	})
	queries := []string{
		"d0123456.example-" + fmt.Sprintf("%02d", 123456%97) + ".com", // hit (exact name of a subtree entry)
		"cdn.d0000042.example-42.com",                                 // hit via parent suffix
		"www.google.com",                                              // miss
		"a.b.c.allowed.example",                                       // user allow
		"telemetry3.vendor.test",                                      // user regex block
		"img.static.some-long-cdn-name.example.net",                   // miss, many labels
	}
	groups := []int64{1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		s.check(queries[i%len(queries)], groups, false)
	}
}
