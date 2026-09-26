package filter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// The title header of a list: the first "! Title:" or "# Title:" within 50
// lines, cleaned of invalid UTF-8, controls, bidi and other format
// characters, white space collapsed, cut to 100 characters.
func TestListTitle(t *testing.T) {
	rlo, bel, zwsp := string(rune(0x202e)), string(rune(0x07)), string(rune(0x200b))
	cases := []struct{ body, want string }{
		{"! Title: HaGeZi's Multi\n||a.example^\n", "HaGeZi's Multi"},
		{"# TITLE:   Steven   Black  \n", "Steven Black"},
		{"!title:x\n", "x"},
		{"! Title: " + rlo + "Evil" + bel + " List" + zwsp + "\n", "Evil List"},
		{"! Title: \xff\xfeBad UTF-8\n", "Bad UTF-8"},
		{"! Title: " + strings.Repeat("é", 150) + "\n", strings.Repeat("é", 100)},
		{"! Title: " + rlo + bel + "  \n", ""},
		{"! Description: x\n||a^\n", ""},
		{strings.Repeat("! comment\n", 50) + "! Title: too late\n", ""},
		{"\xef\xbb\xbf! Title: after a BOM\n", "after a BOM"},
		{"||a.example^ ! Title: no\n", ""},
	}
	dir := t.TempDir()
	for i, c := range cases {
		p := filepath.Join(dir, "l.txt")
		if err := os.WriteFile(p, []byte(c.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := listTitle(p); got != c.want {
			t.Errorf("case %d: title %q, want %q", i, got, c.want)
		}
	}
}

// A list created without a name takes its title from the first successful
// download, once; a new name or an empty one changes that.
func TestListNameFromTitle(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	l := addLocalList(t, e, "t.txt", "! Title: "+string(rune(0x202e))+"Family  List\n||x.example^\n", ListInput{})
	if l.Name != "t.txt" || !l.NameAuto {
		t.Fatalf("created %+v", l)
	}
	refresh := func() List {
		t.Helper()
		got, err := e.RefreshList(ctx, l.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := refresh(); got.Name != "Family List" || got.NameAuto {
		t.Fatalf("after the first download %+v", got)
	}
	if d := e.Check("x.example", qtypeA, []int64{1}); d.Name != "Family List" {
		t.Fatalf("the decision names the list %q", d.Name)
	}
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(e.localDir, "t.txt"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("! Title: Another\n||x.example^\n||y.example^\n")
	if got := refresh(); got.Name != "Family List" {
		t.Fatalf("a later download renamed the list: %+v", got)
	}
	// An empty name makes it automatic again: the next successful download
	// decides (an unchanged copy does not).
	u, err := e.UpdateList(ctx, l.ID, ListInput{URL: l.URL, Enabled: true})
	if err != nil || u.Name != "t.txt" || !u.NameAuto {
		t.Fatalf("automatic again %+v %v", u, err)
	}
	if got := refresh(); got.Name != "t.txt" || !got.NameAuto {
		t.Fatalf("an unchanged copy decided: %+v", got)
	}
	write("||z.example^\n")
	if got := refresh(); got.Name != "t.txt" || got.NameAuto {
		t.Fatalf("no title: %+v", got)
	}
	// The stored name sent back keeps the state; another name is the user's.
	u, _ = e.UpdateList(ctx, l.ID, ListInput{URL: l.URL, Enabled: true})
	if u, _ = e.UpdateList(ctx, l.ID, ListInput{Name: "t.txt", URL: l.URL, Enabled: true}); !u.NameAuto {
		t.Fatalf("the stored name cleared the flag: %+v", u)
	}
	if u, _ = e.UpdateList(ctx, l.ID, ListInput{Name: "Mine", URL: l.URL, Enabled: true}); u.NameAuto || u.Name != "Mine" {
		t.Fatalf("renamed %+v", u)
	}
	write("! Title: Nope\n||w.example^\n")
	if got := refresh(); got.Name != "Mine" {
		t.Fatalf("a named list took the title: %+v", got)
	}
	var auto bool
	if err := e.db.R.QueryRow(`SELECT name_auto FROM filter_lists WHERE id = ?`, l.ID).Scan(&auto); err != nil || auto {
		t.Fatalf("stored name_auto %v %v", auto, err)
	}
	// A list with a name never takes a title.
	n := addLocalList(t, e, "n.txt", "! Title: Other\n||n.example^\n", ListInput{Name: "Named"})
	if got, _ := e.RefreshList(ctx, n.ID); got.Name != "Named" || got.NameAuto {
		t.Fatalf("named list %+v", got)
	}
	// A rename committed while the download is applied (the database
	// already has the user's name, the memory not yet) is not overwritten
	// by the title.
	r := addLocalList(t, e, "r.txt", "! Title: Theirs\n||r.example^\n", ListInput{})
	if _, err := e.db.W.Exec(`UPDATE filter_lists SET name = 'My list', name_auto = 0 WHERE id = ?`, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RefreshList(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := e.db.R.QueryRow(`SELECT name, name_auto FROM filter_lists WHERE id = ?`, r.ID).Scan(&name, &auto); err != nil ||
		name != "My list" || auto {
		t.Fatalf("stored %q %v %v", name, auto, err)
	}
}

// Search: the bounds of q and limit, rules first, then IP rules and the
// entries of the lists with a local copy; $badfilter cancellations hidden;
// applies with a client; truncated at the limit; the semaphore; the budget.
func TestSearch(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	for _, q := range []string{"ab", strings.Repeat("a", 254), "bücher", "a\tb"} {
		_, err := e.Search(ctx, q, 10, nil, false)
		wantInvalid(t, "q "+q, err, "q")
	}
	for _, n := range []int{0, 201} {
		_, err := e.Search(ctx, "abc", n, nil, false)
		wantInvalid(t, "limit", err, "limit")
	}
	if _, err := e.CreateRule(ctx, RuleInput{Action: "block", Type: "subtree", Pattern: "tracker.example", Enabled: true, Qtypes: []string{"AAAA"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateRule(ctx, RuleInput{Action: "allow", Type: "regex", Pattern: "^Tracker", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateIPRule(ctx, IPRuleInput{Action: "block", Pattern: "198.51.100.0/24", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	l := addLocalList(t, e, "s.txt", "||ads.tracker.example^$dnstype=A\n||gone.tracker.example^\n||gone.tracker.example^$badfilter\n"+
		"||tracker*.cdn.example^\n0.0.0.0 other.example\n", ListInput{GroupIDs: []int64{2}})
	ip := addLocalList(t, e, "ip.txt", "198.51.100.7\n", ListInput{Format: ptr(FormatIPs)})
	never := addLocalList(t, e, "never.txt", "||tracker.never.example^\n", ListInput{})
	for e.Stats().Lists < 3 {
		e.runPending(ctx)
		e.compile()
	}
	if err := os.Remove(e.cachePath(never.ID)); err != nil { // no local copy: not searched
		t.Fatal(err)
	}
	res, err := e.Search(ctx, " TRACKER ", 200, []int64{2}, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Q != "tracker" || res.Truncated || res.TimedOut || res.TotalLists != 2 || res.ScannedLists != 2 {
		t.Fatalf("result %+v", res)
	}
	var got []string
	for _, it := range res.Items {
		got = append(got, it.Source+" "+it.Kind+" "+it.Entry)
	}
	want := []string{"rule subtree ||tracker.example^$dnstype=AAAA", "rule regex @@/^Tracker/",
		"list subtree ||ads.tracker.example^$dnstype=A", "list regex ||tracker*.cdn.example^"}
	if !slices.Equal(got, want) {
		t.Fatalf("items\n%q\nwant\n%q", got, want)
	}
	if it := res.Items[0]; it.Applies == nil || *it.Applies || !slices.Equal(it.Qtypes, []string{"AAAA"}) {
		t.Fatalf("a rule of group 1 applies to group 2: %+v", it)
	}
	if it := res.Items[1]; it.Applies == nil || *it.Applies || it.Enabled {
		t.Fatalf("a disabled rule applies: %+v", it)
	}
	if !res.Items[0].Enabled || !res.Items[2].Enabled {
		t.Fatalf("enabled %+v", res.Items)
	}
	if it := res.Items[2]; it.Applies == nil || !*it.Applies || it.ListID != l.ID || it.Name != "s.txt" || !slices.Equal(it.Qtypes, []string{"A"}) {
		t.Fatalf("list item %+v", it)
	}
	res, _ = e.Search(ctx, "198.51.100", 200, nil, false)
	if len(res.Items) != 2 || res.Items[0].Source != "ip-rule" || res.Items[1].ListID != ip.ID || res.Items[1].Kind != "ip" || res.Items[1].Applies != nil {
		t.Fatalf("addresses %+v", res.Items)
	}
	res, _ = e.Search(ctx, "tracker", 3, nil, false)
	if !res.Truncated || len(res.Items) != 3 {
		t.Fatalf("limit %+v", res)
	}
	// The semaphore is shared with Explain and never waited for.
	e.explain <- struct{}{}
	e.explain <- struct{}{}
	_, err = e.Search(ctx, "tracker", 10, nil, false)
	wantKind(t, "busy", err, apperr.KindUnavailable)
	<-e.explain
	<-e.explain
	old := searchBudget
	searchBudget = 0
	defer func() { searchBudget = old }()
	res, err = e.Search(ctx, "tracker", 10, nil, false)
	if err != nil || !res.TimedOut || len(res.Items) != 2 || res.ScannedLists != 0 {
		t.Fatalf("timed out %+v %v", res, err)
	}
}

// A $badfilter line after more than maxMatches hits still cancels one of
// them; list lines are shown without their inline comment, as valid UTF-8
// and cut at a rune boundary (a Latin-1 comment must not break the JSON
// answer).
func TestSearchListLines(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	var b strings.Builder
	for i := range maxMatches + 200 {
		fmt.Fprintf(&b, "||ads%d.example^\n", i)
	}
	b.WriteString("||ads5.example^$badfilter\n0.0.0.0 tracker.latin.example # Werbung f\xfcr alle\n")
	addLocalList(t, e, "many.txt", b.String(), ListInput{})
	for e.Stats().Lists < 1 {
		e.runPending(ctx)
		e.compile()
	}
	res, err := e.Search(ctx, "ads", 200, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range res.Items {
		if it.Entry == "||ads5.example^" {
			t.Fatalf("an entry cancelled by a later $badfilter is shown: %+v", res.Items)
		}
	}
	if len(res.Items) != 200 || !res.Truncated || res.ScannedLists != 1 {
		t.Fatalf("result %+v", res)
	}
	res, err = e.Search(ctx, "tracker.latin", 200, nil, false)
	if err != nil || len(res.Items) != 1 || res.Items[0].Entry != "0.0.0.0 tracker.latin.example" {
		t.Fatalf("latin-1 comment: %+v %v", res, err)
	}
	if _, err := json.Marshal(res); err != nil {
		t.Fatal(err)
	}
	m, err := e.Explain(ctx, "tracker.latin.example", qtypeA, []int64{1})
	if err != nil || len(m) != 1 || m[0].Pattern != "0.0.0.0 tracker.latin.example" {
		t.Fatalf("explain %+v %v", m, err)
	}
	// shownLine: valid UTF-8, at most maxPatternShown bytes at a rune boundary.
	for _, s := range []string{strings.Repeat("ü", 600), "/x\xfc/", "a" + strings.Repeat("€", 400)} {
		got := shownLine(s)
		if !utf8.ValidString(got) || len(got) > maxPatternShown {
			t.Errorf("shownLine(%.20q) = %.20q (%d bytes)", s, got, len(got))
		}
	}
}
