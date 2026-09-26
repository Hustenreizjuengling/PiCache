package filter

import (
	"context"
	"encoding/json/v2"
	"slices"
	"testing"
)

// filter v3 adds columns and tables only: every list and rule keeps its
// group links, the list IDs and the AUTOINCREMENT sequence stay, the new
// columns take their defaults.
func TestMigration3KeepsData(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d := openTestDB(t, dir)
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(ctx, "filter", migrations[:2]); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`
		INSERT INTO filter_lists (id, name, url, kind, plain_domains, enabled, comment, created_at, category)
			VALUES (7, 'Mine', 'https://lists.example/mine.txt', 'block', 'exact', 1, '', 1, 'other');
		INSERT INTO filter_list_groups (list_id, group_id) VALUES (7, 2), (7, 3);
		DELETE FROM filter_lists WHERE id = 1;
		INSERT INTO filter_rules (id, action, type, pattern, enabled, comment, created_at, updated_at)
			VALUES (3, 'block', 'subtree', 'ads.example', 1, 'c', 1, 1), (4, 'allow', 'regex', '^ok', 0, '', 1, 1);
		INSERT INTO filter_rule_groups (rule_id, group_id) VALUES (3, 2), (4, 1), (4, 3);`); err != nil {
		t.Fatal(err)
	}
	var seqBefore int64
	if err := d.R.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'filter_lists'`).Scan(&seqBefore); err != nil {
		t.Fatal(err)
	}
	e := newEngineAt(t, dir, d, testClient()) // migrates to v3
	var seq int64
	if err := d.R.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'filter_lists'`).Scan(&seq); err != nil || seq != seqBefore {
		t.Fatalf("sqlite_sequence %d → %d (%v)", seqBefore, seq, err)
	}
	lists, err := e.Lists(ctx)
	if err != nil || len(lists) != 1 {
		t.Fatalf("lists %+v %v", lists, err)
	}
	l := lists[0]
	if l.ID != 7 || !slices.Equal(l.GroupIDs, []int64{2, 3}) || l.Format != FormatDomains || l.NameAuto || l.IPBlocksIgnored != 0 {
		t.Fatalf("list %+v", l)
	}
	rules, err := e.Rules(ctx, RuleQuery{})
	if err != nil || len(rules) != 2 {
		t.Fatalf("rules %+v %v", rules, err)
	}
	if r := rules[0]; r.ID != 3 || !slices.Equal(r.GroupIDs, []int64{2}) || len(r.Qtypes) != 0 || r.QtypesNegate || r.Reply != "" ||
		r.ReplyIPv4 != "" || len(r.Denyallow) != 0 || r.Invert || r.Comment != "c" {
		t.Fatalf("rule %+v", r)
	}
	if r := rules[1]; r.ID != 4 || !slices.Equal(r.GroupIDs, []int64{1, 3}) || r.Enabled {
		t.Fatalf("rule %+v", r)
	}
	if !e.Check("x.ads.example", qtypeA, []int64{2}).Blocked() {
		t.Fatal("the migrated rule does not apply")
	}
	if ip, _ := e.IPRules(ctx, IPRuleQuery{}); len(ip) != 0 {
		t.Fatalf("IP rules %+v", ip)
	}
	// A new list continues the sequence.
	n, err := e.CreateList(ctx, ListInput{URL: "https://lists.example/new.txt"})
	if err != nil || n.ID != seqBefore+1 {
		t.Fatalf("new list %+v %v", n, err)
	}
}

// A body of 0.12 (no format) keeps the format of a list of answer
// addresses; a catalogue URL without a named format resets it.
func TestListFormatCompat(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	l, err := e.CreateList(ctx, ListInput{URL: "https://lists.example/addresses.txt", Format: ptr(FormatIPs), Category: CategorySecurity})
	if err != nil {
		t.Fatal(err)
	}
	var in ListInput
	if err := json.Unmarshal([]byte(`{"name":"Addresses","url":"https://lists.example/addresses.txt","kind":"block","enabled":false}`), &in); err != nil {
		t.Fatal(err)
	}
	u, err := e.UpdateList(ctx, l.ID, in)
	if err != nil || u.Format != FormatIPs || u.Category != CategorySecurity || u.Name != "Addresses" || u.NameAuto {
		t.Fatalf("0.12 body %+v %v", u, err)
	}
	u, err = e.UpdateList(ctx, l.ID, ListInput{URL: catalog[0].URL})
	if err != nil || u.Format != FormatDomains {
		t.Fatalf("catalogue URL %+v %v", u, err)
	}
	_, err = e.UpdateList(ctx, l.ID, ListInput{URL: catalog[0].URL, Format: ptr(FormatIPs)})
	wantField(t, "catalogue URL with format ips", err, "format")
}
