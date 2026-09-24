package filter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

var discard = slog.New(slog.DiscardHandler)

// loopbackOnly is a transport that reaches only loopback test servers, so a
// test can never touch the Internet (e.g. the default list).
type loopbackOnly struct{ rt http.RoundTripper }

func (l loopbackOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if ip, err := netip.ParseAddr(r.URL.Hostname()); err != nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("test transport: refusing %s", r.URL.Host)
	}
	return l.rt.RoundTrip(r)
}

func testClient() *http.Client {
	return &http.Client{Transport: loopbackOnly{http.DefaultTransport.(*http.Transport).Clone()}}
}

// openTestDB opens picache.db in dir with the client_groups table of the
// clients package (groups 1 Default, 2 Kids, 3 Guests).
func openTestDB(t testing.TB, dir string) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(dir, "picache.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`CREATE TABLE IF NOT EXISTS client_groups(id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE,
		comment TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL DEFAULT 0);
		INSERT OR IGNORE INTO client_groups (id, name) VALUES (1, 'Default'), (2, 'Kids'), (3, 'Guests');`); err != nil {
		t.Fatal(err)
	}
	return d
}

func newEngineAt(t testing.TB, dir string, d *db.DB, fetch *http.Client) *Engine {
	t.Helper()
	ctx := context.Background()
	set, err := settings.Open(ctx, d, discard)
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(ctx, d, set, fetch, filepath.Join(dir, "lists"), discard)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// newTestEngine returns an engine without the default list.
func newTestEngine(t testing.TB) *Engine {
	t.Helper()
	dir := t.TempDir()
	d := openTestDB(t, dir)
	t.Cleanup(func() { _ = d.Close() })
	e := newEngineAt(t, dir, d, testClient())
	if err := e.DeleteList(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	return e
}

// fileURL returns the file:// URL of an absolute path.
func fileURL(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}

// addLocalList writes body to <lists>/local/<name> and subscribes to it.
func addLocalList(t testing.TB, e *Engine, name, body string, in ListInput) List {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.localDir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	in.URL = fileURL(filepath.Join(e.localDir, name))
	in.Enabled = true
	l, err := e.CreateList(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func wantKind(t *testing.T, what string, err error, kind apperr.Kind) {
	t.Helper()
	if apperr.KindOf(err) != kind {
		t.Errorf("%s: err = %v (kind %d), want kind %d", what, err, apperr.KindOf(err), kind)
	}
}

func TestDefaultListAndCatalog(t *testing.T) {
	dir := t.TempDir()
	d := openTestDB(t, dir)
	defer d.Close()
	e := newEngineAt(t, dir, d, testClient())
	lists, err := e.Lists(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cat := e.Catalog()
	if len(lists) != 1 || lists[0].ID != 1 || lists[0].URL != cat[0].URL || !lists[0].Enabled ||
		!slices.Equal(lists[0].GroupIDs, []int64{1}) || lists[0].Status != statusPending || lists[0].Kind != "block" {
		t.Fatalf("default list = %+v", lists)
	}
	if !strings.Contains(cat[0].URL, "cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/multi.txt") || !cat[0].Recommended {
		t.Errorf("first catalogue entry must be the default HaGeZi list: %+v", cat[0])
	}
	keys := map[string]bool{}
	for _, c := range cat {
		if keys[c.Key] || !strings.HasPrefix(c.URL, "https://") || c.Name == "" || c.PlainDomains != "exact" {
			t.Errorf("bad catalogue entry %+v", c)
		}
		keys[c.Key] = true
		if _, err := e.checkListURL(c.URL); err != nil {
			t.Errorf("catalogue URL %s rejected: %v", c.URL, err)
		}
	}
	if len(cat) != 11 {
		t.Errorf("catalogue has %d entries", len(cat))
	}
	// a second engine on the same database does not re-create the default list
	if err := e.DeleteList(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	e2 := newEngineAt(t, dir, d, testClient())
	if l, _ := e2.Lists(context.Background()); len(l) != 0 {
		t.Errorf("default list re-created: %+v", l)
	}
}

func TestListValidation(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	inside := fileURL(filepath.Join(e.localDir, "mine.txt"))
	outside := fileURL(filepath.Join(filepath.Dir(e.localDir), "1.txt"))
	cases := []struct {
		in   ListInput
		kind apperr.Kind
	}{
		{ListInput{URL: "https://lists.example/a.txt"}, 0},
		{ListInput{URL: "http://192.168.1.10/list.txt"}, 0},
		{ListInput{URL: "https://10.0.0.5/list.txt"}, 0},
		{ListInput{URL: inside}, 0},
		{ListInput{URL: "https://lists.example/a.txt"}, apperr.KindConflict},
		{ListInput{URL: ""}, apperr.KindInvalid},
		{ListInput{URL: "lists.example/a.txt"}, apperr.KindInvalid},
		{ListInput{URL: "http://lists.example/a.txt"}, apperr.KindInvalid},
		{ListInput{URL: "http://8.8.8.8/a.txt"}, apperr.KindInvalid},
		{ListInput{URL: "http://169.254.169.254/latest"}, apperr.KindInvalid},
		{ListInput{URL: "https://169.254.169.254/latest"}, apperr.KindInvalid},
		{ListInput{URL: "https://0.0.0.0/x"}, apperr.KindInvalid},
		{ListInput{URL: "https://user:pw@lists.example/a.txt"}, apperr.KindInvalid},
		{ListInput{URL: "ftp://lists.example/a.txt"}, apperr.KindInvalid},
		{ListInput{URL: "file:///etc/passwd"}, apperr.KindInvalid},
		{ListInput{URL: outside}, apperr.KindInvalid},
		{ListInput{URL: "https://lists.example/" + strings.Repeat("a", maxURLLen)}, apperr.KindInvalid},
		{ListInput{URL: "https://lists.example/b.txt", Kind: "deny"}, apperr.KindInvalid},
		{ListInput{URL: "https://lists.example/b.txt", PlainDomains: "wild"}, apperr.KindInvalid},
		{ListInput{URL: "https://lists.example/b.txt", GroupIDs: []int64{99}}, apperr.KindInvalid},
		{ListInput{URL: "https://lists.example/b.txt", GroupIDs: []int64{-1}}, apperr.KindInvalid},
		{ListInput{URL: "https://lists.example/b.txt", Name: strings.Repeat("n", maxNameLen+1)}, apperr.KindInvalid},
		{ListInput{URL: "https://lists.example/b.txt", Name: "bad\nname"}, apperr.KindInvalid},
	}
	for _, c := range cases {
		_, err := e.CreateList(ctx, c.in)
		if c.kind == 0 && err != nil || c.kind != 0 && apperr.KindOf(err) != c.kind {
			t.Errorf("%+v: err = %v, want kind %d", c.in, err, c.kind)
		}
	}
	l, err := e.CreateList(ctx, ListInput{URL: "https://lists.example/c.txt?token=secret", GroupIDs: []int64{}})
	if err != nil {
		t.Fatal(err)
	}
	if l.Name != "lists.example" || l.Kind != "block" || l.PlainDomains != "exact" || len(l.GroupIDs) != 0 || l.GroupIDs == nil {
		t.Errorf("defaults: %+v", l)
	}
	if got := redactURL(l.URL); strings.Contains(got, "secret") {
		t.Errorf("redactURL kept the query: %s", got)
	}
	_, err = e.UpdateList(ctx, 12345, ListInput{URL: "https://lists.example/d.txt"})
	wantKind(t, "update missing", err, apperr.KindNotFound)
	wantKind(t, "delete missing", e.DeleteList(ctx, 12345), apperr.KindNotFound)
	_, err = e.RefreshList(ctx, 12345)
	wantKind(t, "refresh missing", err, apperr.KindNotFound)
}

func TestRulesCRUD(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	g1 := []int64{1}

	invalid := []RuleInput{
		{Action: "deny", Type: "exact", Pattern: "a.example"},
		{Action: "block", Type: "glob", Pattern: "a.example"},
		{Action: "block", Type: "exact", Pattern: "exa mple.com"},
		{Action: "block", Type: "exact", Pattern: ""},
		{Action: "block", Type: "subtree", Pattern: "*"},
		{Action: "block", Type: "subtree", Pattern: "||example.com"},
		{Action: "block", Type: "regex", Pattern: "(unclosed"},
		{Action: "block", Type: "regex", Pattern: `(a)\1`},
		{Action: "block", Type: "regex", Pattern: strings.Repeat("a", maxRegexLen+1)},
		{Action: "block", Type: "exact", Pattern: "ok.example", GroupIDs: []int64{42}},
		{Action: "block", Type: "exact", Pattern: "ok.example", Comment: strings.Repeat("c", maxCommentLen+1)},
	}
	for _, in := range invalid {
		_, err := e.CreateRule(ctx, in)
		wantKind(t, fmt.Sprintf("create %+v", in), err, apperr.KindInvalid)
	}

	for _, c := range []struct {
		in   RuleInput
		want string
	}{
		{RuleInput{Action: "block", Type: "subtree", Pattern: "*.Example.COM."}, "example.com"},
		{RuleInput{Action: "block", Type: "subtree", Pattern: "||ads.test^"}, "ads.test"},
		{RuleInput{Action: "allow", Type: "subtree", Pattern: "good.example.com"}, "good.example.com"},
		{RuleInput{Action: "block", Type: "regex", Pattern: `/^track[0-9]+\./`}, `^track[0-9]+\.`},
		{RuleInput{Action: "allow", Type: "exact", Pattern: "Exact.Example.com"}, "exact.example.com"},
	} {
		in, want := c.in, c.want
		in.Enabled = true
		r, err := e.CreateRule(ctx, in)
		if err != nil {
			t.Fatalf("create %+v: %v", in, err)
		}
		if r.Pattern != want || !slices.Equal(r.GroupIDs, g1) || r.CreatedAt.IsZero() {
			t.Errorf("create %+v = %+v, want pattern %q in the Default group", in, r, want)
		}
	}
	_, err := e.CreateRule(ctx, RuleInput{Action: "block", Type: "subtree", Pattern: "example.com", Enabled: true})
	wantKind(t, "duplicate", err, apperr.KindConflict)

	checks := map[string]Action{
		"example.com": ActionBlock, "a.example.com": ActionBlock, "good.example.com": ActionAllow,
		"x.good.example.com": ActionAllow, "track12.foo": ActionBlock, "exact.example.com": ActionAllow,
		"sub.exact.example.com": ActionBlock, "example.org": ActionNone,
	}
	for q, want := range checks {
		if got := e.Check(q, g1).Action; got != want {
			t.Errorf("Check(%s) = %v, want %v", q, got, want)
		}
		if got := e.CheckRules(q, g1).Action; got != want {
			t.Errorf("CheckRules(%s) = %v, want %v", q, got, want)
		}
		if got := e.Check(q, []int64{2}).Action; got != ActionNone {
			t.Errorf("Check(%s) for group 2 = %v", q, got)
		}
	}

	rules, err := e.Rules(ctx, RuleQuery{Search: "EXAMPLE"})
	if err != nil || len(rules) != 3 {
		t.Errorf("search: %d rules, %v", len(rules), err)
	}
	if rules, _ := e.Rules(ctx, RuleQuery{Action: "allow"}); len(rules) != 2 {
		t.Errorf("action filter: %d", len(rules))
	}
	if rules, _ := e.Rules(ctx, RuleQuery{Type: "regex"}); len(rules) != 1 {
		t.Errorf("type filter: %d", len(rules))
	}
	if rules, _ := e.Rules(ctx, RuleQuery{Search: "100%_"}); len(rules) != 0 {
		t.Errorf("LIKE wildcards must be escaped: %d", len(rules))
	}
	_, err = e.Rules(ctx, RuleQuery{Action: "drop"})
	wantKind(t, "query action", err, apperr.KindInvalid)

	all, _ := e.Rules(ctx, RuleQuery{})
	var blockID int64
	for _, r := range all {
		if r.Pattern == "example.com" {
			blockID = r.ID
		}
	}
	// Update: move to group 2 and disable/enable.
	r, err := e.UpdateRule(ctx, blockID, RuleInput{Action: "block", Type: "subtree", Pattern: "example.com", Enabled: true, GroupIDs: []int64{2}})
	if err != nil || !slices.Equal(r.GroupIDs, []int64{2}) {
		t.Fatalf("update: %+v %v", r, err)
	}
	if e.Check("a.example.com", g1).Blocked() || !e.Check("a.example.com", []int64{2}).Blocked() {
		t.Error("group change not applied")
	}
	if _, err := e.UpdateRule(ctx, blockID, RuleInput{Action: "block", Type: "subtree", Pattern: "example.com"}); err != nil {
		t.Fatal(err)
	}
	if e.Check("a.example.com", []int64{2}).Blocked() {
		t.Error("disabled rule applied")
	}
	if r, _ := e.rule(ctx, blockID); !slices.Equal(r.GroupIDs, []int64{2}) {
		t.Errorf("nil GroupIDs on update must keep the groups: %v", r.GroupIDs)
	}
	_, err = e.UpdateRule(ctx, blockID, RuleInput{Action: "allow", Type: "subtree", Pattern: "good.example.com"})
	wantKind(t, "update to duplicate", err, apperr.KindConflict)
	_, err = e.UpdateRule(ctx, 999, RuleInput{Action: "allow", Type: "exact", Pattern: "x.example"})
	wantKind(t, "update missing", err, apperr.KindNotFound)

	// Empty group list: the rule applies to nobody.
	r, err = e.CreateRule(ctx, RuleInput{Action: "block", Type: "exact", Pattern: "nobody.example", Enabled: true, GroupIDs: []int64{}})
	if err != nil || len(r.GroupIDs) != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	if e.Check("nobody.example", []int64{1, 2, 3}).Blocked() {
		t.Error("rule without groups applied")
	}

	if err := e.DeleteRule(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	wantKind(t, "delete twice", e.DeleteRule(ctx, r.ID), apperr.KindNotFound)
	if st := e.Stats(); st.Rules != 4 {
		t.Errorf("Stats.Rules = %d, want 4 enabled rules", st.Rules)
	}
}

func TestRuleLimits(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	if _, err := e.db.W.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO filter_rules (action, type, pattern, created_at, updated_at) SELECT 'block', 'regex', 'r' || i, 0, 0 FROM n`, maxRegexRules); err != nil {
		t.Fatal(err)
	}
	_, err := e.CreateRule(ctx, RuleInput{Action: "block", Type: "regex", Pattern: "one-more"})
	wantKind(t, "regex limit", err, apperr.KindConflict)
	if _, err := e.CreateRule(ctx, RuleInput{Action: "block", Type: "exact", Pattern: "ok.example"}); err != nil {
		t.Errorf("exact rule refused: %v", err)
	}
}

func TestListGroupsAndLifecycle(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	l := addLocalList(t, e, "block.txt", "||blocked.example^\n0.0.0.0 host.example.org\n", ListInput{Name: "Mine"})
	if l.Status != statusPending || !slices.Equal(l.GroupIDs, []int64{1}) {
		t.Fatalf("created: %+v", l)
	}
	l, err := e.RefreshList(ctx, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if l.Status != statusOK || l.Entries != 2 || l.SizeBytes == 0 || l.LastSuccess.IsZero() || l.LastUpdated.IsZero() {
		t.Fatalf("after refresh: %+v", l)
	}
	d := e.Check("a.blocked.example", []int64{1})
	if !d.Blocked() || d.Name != "Mine" || d.ListID != l.ID || d.Source != "list" || d.Kind != "subtree" {
		t.Fatalf("decision %+v", d)
	}

	// Editing groups and name swaps only the group table, not the matcher.
	before := e.snap.Load()
	l, err = e.UpdateList(ctx, l.ID, ListInput{Name: "Renamed", URL: l.URL, Enabled: true, GroupIDs: []int64{2}})
	if err != nil {
		t.Fatal(err)
	}
	after := e.snap.Load()
	if after.lists != before.lists {
		t.Error("group edit rebuilt the matcher")
	}
	if e.Check("a.blocked.example", []int64{1}).Blocked() {
		t.Error("old group still applies")
	}
	if d := e.Check("a.blocked.example", []int64{2}); !d.Blocked() || d.Name != "Renamed" {
		t.Errorf("new group: %+v", d)
	}

	// Deleting group 2 cascades in the database; ReloadGroups picks it up.
	if _, err := e.db.W.Exec(`DELETE FROM client_groups WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	if err := e.ReloadGroups(ctx); err != nil {
		t.Fatal(err)
	}
	if e.Check("a.blocked.example", []int64{2}).Blocked() {
		t.Error("membership of the deleted group still applies")
	}
	if ls, _ := e.Lists(ctx); len(ls[0].GroupIDs) != 0 {
		t.Errorf("groups after cascade: %v", ls[0].GroupIDs)
	}

	// Disabling stops the list immediately and frees its parse result.
	l, err = e.UpdateList(ctx, l.ID, ListInput{URL: l.URL, Enabled: false, GroupIDs: []int64{1}})
	if err != nil {
		t.Fatal(err)
	}
	if e.Check("a.blocked.example", []int64{1}).Blocked() {
		t.Error("disabled list applies")
	}
	e.compile()
	if st := e.Stats(); st.Lists != 0 || st.Entries != 0 {
		t.Errorf("stats after disable: %+v", st)
	}
	_, err = e.RefreshList(ctx, l.ID)
	wantKind(t, "refresh disabled", err, apperr.KindConflict)

	// Re-enabling re-parses the cached copy without downloading.
	if _, err := e.UpdateList(ctx, l.ID, ListInput{URL: l.URL, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if id, download, ok := e.nextJob(); !ok || id != l.ID {
		t.Fatalf("nextJob = %d %v %v", id, download, ok)
	}
	if _, err := e.refresh(ctx, l.ID, false); err != nil {
		t.Fatal(err)
	}
	e.compile()
	if !e.Check("host.example.org", []int64{1}).Blocked() {
		t.Error("re-enabled list not loaded")
	}

	// Switching plainDomains/kind re-parses: an allow list now allows.
	if _, err := e.UpdateList(ctx, l.ID, ListInput{URL: l.URL, Enabled: true, Kind: "allow"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.refresh(ctx, l.ID, false); err != nil {
		t.Fatal(err)
	}
	e.compile()
	if d := e.Check("host.example.org", []int64{1}); d.Action != ActionAllow {
		t.Errorf("allow list: %+v", d)
	}

	if err := e.DeleteList(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	if e.Check("host.example.org", []int64{1}).Action != ActionNone {
		t.Error("deleted list applies")
	}
	if e.hasCache(l.ID) {
		t.Error("cached copy not removed")
	}
}

func TestExplain(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	l := addLocalList(t, e, "a.txt", strings.Join([]string{
		"||example.com^",
		"@@||good.example.com^",
		"||x.good.example.com^$important",
		"||cancelled.example.com^",
		"||cancelled.example.com^$badfilter",
		"/^x\\.good\\./",
		"0.0.0.0 x.good.example.com other.example.net",
	}, "\n"), ListInput{Name: "A"})
	if _, err := e.RefreshList(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateRule(ctx, RuleInput{Action: "allow", Type: "exact", Pattern: "x.good.example.com", Enabled: true, GroupIDs: []int64{2}}); err != nil {
		t.Fatal(err)
	}

	ms, err := e.Explain(ctx, "X.Good.Example.com.", []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		src, action, kind string
		imp, applies, dec bool
	}
	var got []row
	for _, m := range ms {
		got = append(got, row{m.Source, m.Action, m.Kind, m.Important, m.Applies, m.Decisive})
	}
	want := []row{
		{"rule", "allow", "exact", false, false, false},  // group 2 only
		{"list", "block", "subtree", true, true, true},   // $important beats @@
		{"list", "allow", "subtree", false, true, false}, // @@||good.example.com^
		{"list", "block", "exact", false, true, false},   // hosts line
		{"list", "block", "subtree", false, true, false}, // ||example.com^
		{"list", "block", "regex", false, true, false},   // /re/
	}
	if !slices.Equal(got, want) {
		t.Errorf("explain rows:\n got  %v\n want %v", got, want)
	}
	if d := e.Check("x.good.example.com", []int64{1}); !d.Blocked() || !d.Important {
		t.Errorf("Check disagrees with Explain: %+v", d)
	}
	for _, m := range ms {
		if m.Source == "list" && (m.Name != "A" || m.ListID != l.ID || m.Pattern == "") {
			t.Errorf("list match fields: %+v", m)
		}
	}
	// With group 2 the user allow rule is decisive.
	ms, _ = e.Explain(ctx, "x.good.example.com", []int64{1, 2})
	if !ms[0].Decisive || ms[0].Source != "rule" {
		t.Errorf("group 2: %+v", ms[0])
	}
	// $badfilter-cancelled entries are not reported.
	if ms, _ := e.Explain(ctx, "cancelled.example.com", []int64{1}); len(ms) != 1 || ms[0].Pattern != "||example.com^" {
		t.Errorf("badfilter: %+v", ms)
	}
	_, err = e.Explain(ctx, "not a domain", []int64{1})
	wantKind(t, "explain invalid", err, apperr.KindInvalid)
}

func TestStartLoadsCachedLists(t *testing.T) {
	dir := t.TempDir()
	d := openTestDB(t, dir)
	defer d.Close()
	e := newEngineAt(t, dir, d, testClient())
	if err := e.DeleteList(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	l := addLocalList(t, e, "l.txt", "||cached.example^\n", ListInput{})
	if _, err := e.RefreshList(context.Background(), l.ID); err != nil {
		t.Fatal(err)
	}
	// Remove the source: the restarted engine must use its cached copy.
	if err := os.Remove(filepath.Join(e.localDir, "l.txt")); err != nil {
		t.Fatal(err)
	}

	e2 := newEngineAt(t, dir, d, testClient())
	if e2.Check("cached.example", []int64{1}).Blocked() {
		t.Fatal("blocked before Start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e2.Start(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for !e2.Check("cached.example", []int64{1}).Blocked() {
		if time.Now().After(deadline) {
			t.Fatal("cached list not loaded after Start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := e2.Stats(); st.Lists != 1 || st.Entries != 1 || st.CompiledAt.IsZero() || st.MemoryBytes <= 0 {
		t.Errorf("stats %+v", st)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return")
	}
	if _, err := e2.RefreshList(context.Background(), l.ID); apperr.KindOf(err) != apperr.KindUnavailable {
		t.Errorf("RefreshList after stop: %v", err)
	}
}

func TestStats(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }
	a := addLocalList(t, e, "a.txt", "||a.example^\n", ListInput{})
	b := addLocalList(t, e, "b.txt", "||b.example^\n", ListInput{})
	if _, err := e.RefreshList(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(e.localDir, "b.txt")); err != nil {
		t.Fatal(err)
	}
	l, err := e.RefreshList(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if l.Status != statusFailedEmpty || !strings.Contains(l.LastError, "does not exist") {
		t.Errorf("missing local file: %+v", l)
	}
	st := e.Stats()
	if st.FailedLists != 1 || st.StaleLists != 0 || st.Lists != 1 {
		t.Errorf("stats %+v", st)
	}
	now = now.Add(73 * time.Hour) // > 3 × 24 h since the last success
	if st := e.Stats(); st.StaleLists != 2 {
		t.Errorf("stale lists = %d, want 2", st.StaleLists)
	}
}

func TestDue(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	cases := []struct {
		name     string
		rt       listRT
		now      time.Time
		interval time.Duration
		want     bool
	}{
		{"never checked", listRT{jitter: 1}, t0, 0, true},
		{"manual", listRT{List: List{LastChecked: t0}, jitter: 1}, t0.Add(100 * day), 0, false},
		{"not yet", listRT{List: List{LastChecked: t0, Status: statusOK}, jitter: 1}, t0.Add(23 * time.Hour), day, false},
		{"due", listRT{List: List{LastChecked: t0, Status: statusOK}, jitter: 1}, t0.Add(day), day, true},
		{"jitter -10%", listRT{List: List{LastChecked: t0, Status: statusOK}, jitter: 0.9}, t0.Add(22 * time.Hour), day, true},
		{"jitter +10%", listRT{List: List{LastChecked: t0, Status: statusOK}, jitter: 1.1}, t0.Add(26 * time.Hour), day, false},
		{"failed retries hourly", listRT{List: List{LastChecked: t0, Status: statusFailedCached}, jitter: 1}, t0.Add(time.Hour), day, true},
	}
	for _, c := range cases {
		if got := due(&c.rt, c.now, c.interval); got != c.want {
			t.Errorf("%s: due = %v", c.name, got)
		}
	}
	for range 1000 {
		if j := newJitter(); j < 0.9 || j > 1.1 {
			t.Fatalf("jitter %v out of range", j)
		}
	}
}

func TestContextCancelledRefresh(t *testing.T) {
	e := newTestEngine(t)
	l := addLocalList(t, e, "c.txt", "||c.example^\n", ListInput{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.refresh(ctx, l.ID, true); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestLocalListConfinement(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	outside := filepath.Join(filepath.Dir(e.localDir), "secret.txt")
	if err := os.WriteFile(outside, []byte("||leak.example^\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(e.localDir, "dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	l, err := e.CreateList(ctx, ListInput{URL: fileURL(filepath.Join(e.localDir, "dir")), Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if l, _ = e.RefreshList(ctx, l.ID); l.Status != statusFailedEmpty || !strings.Contains(l.LastError, "not a regular file") {
		t.Errorf("directory: %+v", l)
	}
	link := filepath.Join(e.localDir, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	l, err = e.CreateList(ctx, ListInput{URL: fileURL(link), Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if l, _ = e.RefreshList(ctx, l.ID); l.Status != statusFailedEmpty || e.Check("leak.example", []int64{1}).Blocked() {
		t.Errorf("symlink escaped the local directory: %+v", l)
	}
}
