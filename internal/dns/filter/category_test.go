package filter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// The catalogue invariants of the parental-protection package (catalogue
// shape, categories, the switch bindings, sizes).
func TestCatalogInvariants(t *testing.T) {
	e := newTestEngine(t)
	if catalog[0].Key != "hagezi-multi" {
		t.Fatalf("the default list must stay the first entry, got %s", catalog[0].Key)
	}
	bound := map[string]bool{}
	for _, p := range categoryPresets {
		for _, k := range p.Keys {
			bound[k] = true
		}
	}
	keys, urls := map[string]bool{}, map[string]bool{}
	prevCat := 0
	for i, c := range catalog {
		where := fmt.Sprintf("entry %d (%s)", i, c.Key)
		if c.Key == "" || strings.Trim(c.Key, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" || keys[c.Key] {
			t.Errorf("%s: bad or duplicate key", where)
		}
		keys[c.Key] = true
		if urls[c.URL] || !strings.HasPrefix(c.URL, "https://") {
			t.Errorf("%s: URL %q is not a unique https URL", where, c.URL)
		}
		urls[c.URL] = true
		if u, err := e.checkListURL(c.URL); err != nil || u.String() != c.URL {
			t.Errorf("%s: URL %q is not accepted as is: %v", where, c.URL, err)
		}
		cat := slices.Index(Categories, c.Category)
		if cat < 0 || c.Category == CategoryOther {
			t.Errorf("%s: category %q", where, c.Category)
		}
		if i > 0 && cat < prevCat {
			t.Errorf("%s: the catalogue is not grouped in the order of Categories", where)
		}
		prevCat = cat
		if (c.Kind == "allow") != (c.Category == CategoryAllow) || c.Kind != "allow" && c.Kind != "block" {
			t.Errorf("%s: kind %q with category %q", where, c.Kind, c.Category)
		}
		if c.PlainDomains != "exact" && c.PlainDomains != "subtree" {
			t.Errorf("%s: plainDomains %q", where, c.PlainDomains)
		}
		if c.Entries <= 0 {
			t.Errorf("%s: no entry count", where)
		}
		if c.Category == CategoryAllow && (c.Recommended || bound[c.Key] || c.Entries > 10_000) {
			t.Errorf("%s: allowlists are small, never recommended and never bound to a switch", where)
		}
		if c.Entries > LargeEntries && (c.Recommended || bound[c.Key]) {
			t.Errorf("%s: a large list is never recommended nor bound to a switch", where)
		}
		if c.Name == "" || c.Description == "" || c.DescriptionDe == "" || c.Maintainer == "" || !strings.HasPrefix(c.Homepage, "https://") {
			t.Errorf("%s: name, both descriptions, maintainer and an https homepage are required", where)
		}
	}
	switches := map[string]bool{}
	for _, p := range categoryPresets {
		if switches[p.Switch] || len(p.Keys) == 0 || !IsProtection(p.Category) {
			t.Errorf("switch %s: duplicate, without keys or not a protection category", p.Switch)
		}
		switches[p.Switch] = true
		for _, k := range p.Keys {
			c, ok := catalogEntry(k)
			if !ok || c.Category != p.Category || c.Kind != "block" {
				t.Errorf("switch %s: key %s missing, of another category or not a blocklist", p.Switch, k)
			}
		}
	}
	for old, cur := range catalogAliases {
		if keys[old] || !keys[cur] {
			t.Errorf("alias %s → %s: the old key must be gone and the new one present", old, cur)
		}
	}
	for _, c := range []string{CategoryAdult, CategoryGambling, CategoryDating, CategoryPiracy, CategoryBypass} {
		if !IsProtection(c) {
			t.Errorf("%s is a protection category", c)
		}
	}
	if IsProtection(CategorySecurity) || IsProtection(CategoryOther) || IsProtection(CategoryAllow) {
		t.Error("security, other and allow are no protection categories")
	}
	if got := e.Catalog(); &got[0] == &catalog[0] {
		t.Error("Catalog must return a copy")
	}
}

// createOK creates a list and fails the test on an error.
func createOK(t *testing.T, e *Engine, in ListInput) List {
	t.Helper()
	l, err := e.CreateList(context.Background(), in)
	if err != nil {
		t.Fatalf("create %+v: %v", in, err)
	}
	return l
}

func wantField(t *testing.T, what string, err error, field string) {
	t.Helper()
	ae, ok := apperr.As(err)
	if !ok || ae.Kind != apperr.KindInvalid || ae.Field != field {
		t.Errorf("%s: error %v, want invalid field %q", what, err, field)
	}
}

func TestListCategoryAndCatalogKey(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	gambling, _ := catalogEntry("hagezi-gambling-medium")
	allowEntry, _ := catalogEntry("hagezi-allow-referral")

	// A catalogue URL takes the entry's category, kind and key.
	l := createOK(t, e, ListInput{URL: gambling.URL})
	if l.Category != CategoryGambling || l.CatalogKey != gambling.Key || l.Kind != "block" {
		t.Fatalf("catalogue list %+v", l)
	}
	a := createOK(t, e, ListInput{URL: allowEntry.URL})
	if a.Kind != "allow" || a.Category != CategoryAllow || a.CatalogKey != allowEntry.Key {
		t.Fatalf("catalogue allowlist %+v", a)
	}
	// Own lists: other, allow for allowlists, or the chosen category.
	own := createOK(t, e, ListInput{URL: "https://lists.example/own.txt"})
	ownAllow := createOK(t, e, ListInput{URL: "https://lists.example/allow.txt", Kind: "allow"})
	adult := createOK(t, e, ListInput{URL: "https://lists.example/adult.txt", Category: " Adult "})
	if own.Category != CategoryOther || own.CatalogKey != "" || ownAllow.Category != CategoryAllow || adult.Category != CategoryAdult {
		t.Fatalf("own lists %+v / %+v / %+v", own, ownAllow, adult)
	}

	for _, tc := range []struct {
		in    ListInput
		field string
	}{
		{ListInput{URL: "https://lists.example/c1.txt", Category: "sports"}, "category"},
		{ListInput{URL: "https://lists.example/c2.txt", Category: CategoryAllow}, "category"},
		{ListInput{URL: "https://lists.example/c3.txt", Kind: "allow", Category: CategoryAdult}, "category"},
		{ListInput{URL: gambling.URL + "?x", Kind: "allow"}, "category"}, // not a catalogue URL: an own allowlist with a category
		{ListInput{URL: gambling.URL, Kind: "allow"}, "kind"},
		{ListInput{URL: allowEntry.URL, Kind: "block"}, "kind"},
	} {
		in := tc.in
		if tc.field == "category" && in.Category == "" {
			in.Category = CategoryGambling
		}
		_, err := e.CreateList(ctx, in)
		wantField(t, fmt.Sprintf("%+v", in), err, tc.field)
	}
	_, err := e.CreateList(ctx, ListInput{URL: gambling.URL, Kind: "allow"})
	if ae, _ := apperr.As(err); ae == nil || !strings.Contains(ae.Message, "blocklist") {
		t.Errorf("kind mismatch message: %v", err)
	}
	_, err = e.CreateList(ctx, ListInput{URL: allowEntry.URL + "#", Kind: "block"})
	if ae, _ := apperr.As(err); ae == nil || !strings.Contains(ae.Message, "allowlist") {
		t.Errorf("kind mismatch message: %v", err)
	}

	// Update: an absent category keeps the stored one (old clients), the
	// catalogue key is kept while the URL stays.
	u, err := e.UpdateList(ctx, l.ID, ListInput{Name: "Renamed", URL: l.URL, Enabled: true})
	if err != nil || u.Category != CategoryGambling || u.CatalogKey != gambling.Key {
		t.Fatalf("update keeps: %+v %v", u, err)
	}
	u, err = e.UpdateList(ctx, own.ID, ListInput{URL: own.URL, Category: CategorySecurity})
	if err != nil || u.Category != CategorySecurity {
		t.Fatalf("update category: %+v %v", u, err)
	}
	// A list that becomes an allowlist gets allow; back to a blocklist
	// without a category: other.
	u, err = e.UpdateList(ctx, own.ID, ListInput{URL: own.URL, Kind: "allow"})
	if err != nil || u.Category != CategoryAllow {
		t.Fatalf("kind allow: %+v %v", u, err)
	}
	u, err = e.UpdateList(ctx, own.ID, ListInput{URL: own.URL, Kind: "block"})
	if err != nil || u.Category != CategoryOther {
		t.Fatalf("kind block: %+v %v", u, err)
	}
	_, err = e.UpdateList(ctx, own.ID, ListInput{URL: own.URL, Kind: "block", Category: CategoryAllow})
	wantField(t, "update allow for a blocklist", err, "category")
	// A URL change sets the catalogue key from the new URL.
	piracy, _ := catalogEntry("hagezi-anti-piracy")
	u, err = e.UpdateList(ctx, own.ID, ListInput{URL: piracy.URL})
	if err != nil || u.CatalogKey != piracy.Key || u.Category != CategoryOther {
		t.Fatalf("URL change to a catalogue URL: %+v %v", u, err)
	}
	u, err = e.UpdateList(ctx, own.ID, ListInput{URL: "https://lists.example/elsewhere.txt"})
	if err != nil || u.CatalogKey != "" {
		t.Fatalf("URL change to an own URL: %+v %v", u, err)
	}

	// Stored and loaded again.
	stored, err := loadLists(ctx, e.db.R)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stored {
		if s.ID == l.ID && (s.Category != CategoryGambling || s.CatalogKey != gambling.Key) {
			t.Errorf("stored %+v", s.List)
		}
	}
}

// Filter migration 2 gives existing lists a category and catalogue key,
// once and only for rows without a category.
func TestCategoryBackfill(t *testing.T) {
	dir := t.TempDir()
	d := openTestDB(t, dir)
	defer d.Close()
	ctx := context.Background()
	if err := d.Migrate(ctx, "filter", migrations[:1]); err != nil {
		t.Fatal(err)
	}
	bypass, _ := catalogEntry("hagezi-doh-vpn-bypass")
	nsfw, _ := catalogEntry("oisd-nsfw")
	listsDir := filepath.Join(dir, "lists")
	if err := os.MkdirAll(listsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for i, l := range []struct{ url, kind, copy string }{
		{bypass.URL, "block", ""},                       // → doh-vpn-bypass, a protection list
		{"https://lists.example/mine.txt", "block", ""}, // → other
		{"https://lists.example/ok.txt", "allow", ""},   // → allow
		{nsfw.URL, "allow", ""},                         // legacy allowlist at a catalogue URL → allow, key kept
		// An own list that blocks mostly whole TLDs (it did before the TLD
		// guard) → abused-tlds, so the guard does not drop its entries.
		{"https://lists.example/spam-tlds.txt", "block", "||zip^\n||mov^\n*.xyz^\n||example.com^\n"},
		// Mostly ordinary entries → other (the TLD entry is ignored).
		{"https://lists.example/mixed.txt", "block", "||zip^\n||a.example^\n||b.example^\n"},
	} {
		res, err := d.W.Exec(`INSERT INTO filter_lists (name, url, kind, plain_domains, enabled, comment, created_at)
			VALUES (?, ?, ?, 'exact', 1, '', 0)`, fmt.Sprint("l", i), l.url, l.kind)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if l.copy != "" {
			if err := os.WriteFile(filepath.Join(listsDir, fmt.Sprint(id, ".txt")), []byte(l.copy), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	e := newEngineAt(t, dir, d, testClient())
	want := map[string][2]string{
		bypass.URL:                            {CategoryBypass, bypass.Key},
		"https://lists.example/mine.txt":      {CategoryOther, ""},
		"https://lists.example/ok.txt":        {CategoryAllow, ""},
		nsfw.URL:                              {CategoryAllow, nsfw.Key},
		"https://lists.example/spam-tlds.txt": {CategoryAbusedTLDs, ""},
		"https://lists.example/mixed.txt":     {CategoryOther, ""},
	}
	lists, err := e.Lists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lists {
		if w, ok := want[l.URL]; ok && (l.Category != w[0] || l.CatalogKey != w[1]) {
			t.Errorf("%s: category %q key %q, want %v", l.URL, l.Category, l.CatalogKey, w)
		}
	}
	// Idempotent: a stored category is never touched again.
	if _, err := d.W.Exec(`UPDATE filter_lists SET category = 'security' WHERE url = ?`, bypass.URL); err != nil {
		t.Fatal(err)
	}
	if err := backfillCategories(ctx, d, filepath.Join(dir, "lists")); err != nil {
		t.Fatal(err)
	}
	var cat string
	if err := d.R.QueryRow(`SELECT category FROM filter_lists WHERE url = ?`, bypass.URL).Scan(&cat); err != nil || cat != CategorySecurity {
		t.Fatalf("backfill changed a stored category: %q %v", cat, err)
	}
}

// presetState returns the state of switch sw for group g.
func presetState(e *Engine, g int64, sw string) PresetState { return e.Presets(g)[sw] }

func TestSetPresets(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	gambling, _ := catalogEntry("hagezi-gambling-medium")

	if st := presetState(e, 2, "gambling"); st.On || st.State != PresetOff {
		t.Fatalf("initial %+v", st)
	}
	// On: a missing list is created with only this group (never Default).
	change, err := e.SetPresets(ctx, 2, map[string]bool{"gambling": true})
	changed := change.Lists
	if err != nil || len(changed) != 1 || !slices.Equal(change.Created, []int64{changed[0].ID}) {
		t.Fatalf("switch on: %+v %v", change, err)
	}
	l := changed[0]
	if l.URL != gambling.URL || l.CatalogKey != gambling.Key || l.Category != CategoryGambling || !l.Enabled ||
		!slices.Equal(l.GroupIDs, []int64{2}) || l.Name != gambling.Name || l.Kind != "block" || l.Status != statusPending {
		t.Fatalf("created %+v", l)
	}
	if st := presetState(e, 2, "gambling"); !st.On || st.State != PresetPending {
		t.Fatalf("after on %+v", st)
	}
	if st := presetState(e, 3, "gambling"); st.On {
		t.Fatalf("another group %+v", st)
	}
	// On again: nothing changes.
	if change, err := e.SetPresets(ctx, 2, map[string]bool{"gambling": true}); err != nil || len(change.Lists) != 0 || len(change.Created) != 0 {
		t.Fatalf("idempotent: %+v %v", change, err)
	}
	// States follow the list status.
	e.mu.Lock()
	e.lists[l.ID].Status = statusFailedEmpty
	e.mu.Unlock()
	if st := presetState(e, 2, "gambling"); st.State != PresetFailed {
		t.Errorf("failed-empty: %+v", st)
	}
	e.mu.Lock()
	e.lists[l.ID].Status = statusFailedCached
	e.mu.Unlock()
	if st := presetState(e, 2, "gambling"); st.State != PresetActive {
		t.Errorf("failed-cached: %+v", st)
	}

	// Another group joins; switching off removes only that group.
	if _, err := e.SetPresets(ctx, 3, map[string]bool{"gambling": true}); err != nil {
		t.Fatal(err)
	}
	change, err = e.SetPresets(ctx, 2, map[string]bool{"gambling": false})
	if changed = change.Lists; err != nil || len(changed) != 1 || !slices.Equal(changed[0].GroupIDs, []int64{3}) {
		t.Fatalf("switch off: %+v %v", changed, err)
	}
	if !presetState(e, 3, "gambling").On || presetState(e, 2, "gambling").On {
		t.Error("off must remove only the group")
	}
	// Off for the last group: the list stays, without groups.
	if _, err := e.SetPresets(ctx, 3, map[string]bool{"gambling": false}); err != nil {
		t.Fatal(err)
	}
	lists, _ := e.Lists(ctx)
	if len(lists) != 1 || len(lists[0].GroupIDs) != 0 {
		t.Fatalf("the list stays without groups: %+v", lists)
	}
	// A disabled bound list is enabled again (for all its groups).
	if _, err := e.UpdateList(ctx, l.ID, ListInput{URL: l.URL, Enabled: false, GroupIDs: []int64{1}}); err != nil {
		t.Fatal(err)
	}
	change, err = e.SetPresets(ctx, 2, map[string]bool{"gambling": true})
	if changed = change.Lists; err != nil || len(changed) != 1 || !changed[0].Enabled || !slices.Equal(changed[0].GroupIDs, []int64{1, 2}) {
		t.Fatalf("enable a disabled list: %+v %v", changed, err)
	}
	if lists, _ := e.Lists(ctx); len(lists) != 1 {
		t.Fatalf("no second list: %+v", lists)
	}
	// Revert (the API undoes a failed update with the previous states).
	if _, err := e.SetPresets(ctx, 2, map[string]bool{"gambling": false}); err != nil || presetState(e, 2, "gambling").On {
		t.Fatalf("revert: %v", err)
	}

	// Unknown switch and unknown group.
	_, err = e.SetPresets(ctx, 2, map[string]bool{"sports": true})
	wantField(t, "unknown switch", err, "categories.sports")
	if _, err := e.SetPresets(ctx, 99, map[string]bool{"adult": true}); apperr.KindOf(err) != apperr.KindNotFound {
		t.Errorf("unknown group: %v", err)
	}

	// A list at a bound URL that is an allowlist (created before the kind
	// rule): 409 naming the list, nothing changed.
	piracy, _ := catalogEntry("hagezi-anti-piracy")
	if _, err := e.db.W.Exec(`INSERT INTO filter_lists (name, url, kind, plain_domains, category, catalog_key, enabled, comment, created_at)
		VALUES ('Legacy', ?, 'allow', 'exact', 'allow', '', 1, '', 0)`, piracy.URL); err != nil {
		t.Fatal(err)
	}
	_, err = e.SetPresets(ctx, 2, map[string]bool{"piracy": true, "adult": true})
	if apperr.KindOf(err) != apperr.KindConflict || !strings.Contains(err.Error(), "Legacy") {
		t.Fatalf("allowlist at the URL: %v", err)
	}
	if presetState(e, 2, "adult").On {
		t.Error("a failed SetPresets must change nothing")
	}
}

// Switching on needs headroom below the list limit, checked in the
// transaction.
func TestSetPresetsListLimit(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	for i := range maxLists {
		createOK(t, e, ListInput{URL: fmt.Sprintf("https://lists.example/%d.txt", i)})
	}
	_, err := e.SetPresets(ctx, 2, map[string]bool{"dating": true})
	if apperr.KindOf(err) != apperr.KindConflict || !strings.Contains(err.Error(), "at most 100 lists") {
		t.Fatalf("list limit: %v", err)
	}
	if st := presetState(e, 2, "dating"); st.On {
		t.Error("switch on despite the limit")
	}
}

// A switch is on only while its bound list is a protection list of the
// switch's category; switching on makes the bound list that again.
func TestSetPresetsBindsProtection(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	change, err := e.SetPresets(ctx, 2, map[string]bool{"bypass": true})
	if err != nil || len(change.Lists) != 1 {
		t.Fatalf("switch on: %+v %v", change, err)
	}
	l := change.Lists[0]
	// Recategorised to security (the pre-0.10 behaviour): no longer a
	// protection list, so the switch is off.
	if _, err := e.UpdateList(ctx, l.ID, ListInput{Name: l.Name, URL: l.URL, Enabled: true, Category: CategorySecurity}); err != nil {
		t.Fatal(err)
	}
	if st := presetState(e, 2, "bypass"); st.On || st.State != PresetOff {
		t.Fatalf("security list: %+v", st)
	}
	// Switching on for another group gives the list its category back
	// (reported as a changed list, and revertible).
	change, err = e.SetPresets(ctx, 3, map[string]bool{"bypass": true})
	if err != nil || len(change.Lists) != 1 || change.Lists[0].Category != CategoryBypass || len(change.Created) != 0 ||
		!slices.Equal(change.Lists[0].GroupIDs, []int64{2, 3}) {
		t.Fatalf("switch on again: %+v %v", change, err)
	}
	if !presetState(e, 2, "bypass").On || !presetState(e, 3, "bypass").On {
		t.Error("both groups are protected again")
	}
	if ls, _ := e.Lists(ctx); len(ls) != 1 || ls[0].Category != CategoryBypass {
		t.Errorf("the list is a protection list again: %+v", ls)
	}
	// A list at the catalogue URL without a catalogue key (added before it
	// had one) gets the key when a switch picks it.
	piracy, _ := catalogEntry("hagezi-anti-piracy")
	own := createOK(t, e, ListInput{URL: piracy.URL, Category: CategoryOther, GroupIDs: []int64{1}})
	if _, err := e.db.W.Exec(`UPDATE filter_lists SET catalog_key = '' WHERE id = ?`, own.ID); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.lists[own.ID].CatalogKey = ""
	e.mu.Unlock()
	change, err = e.SetPresets(ctx, 2, map[string]bool{"piracy": true})
	if err != nil || len(change.Lists) != 1 || change.Lists[0].CatalogKey != piracy.Key || change.Lists[0].Category != CategoryPiracy {
		t.Fatalf("list found by URL: %+v %v", change, err)
	}
	if !presetState(e, 2, "piracy").On {
		t.Error("the switch must be on")
	}
	stored, err := loadLists(ctx, e.db.R)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stored {
		if s.ID == own.ID && (s.CatalogKey != piracy.Key || s.Category != CategoryPiracy) {
			t.Errorf("stored %+v", s.List)
		}
	}
	// A bound list turned into an allowlist: off, and switching on is a 409.
	if _, err := e.UpdateList(ctx, own.ID, ListInput{Name: own.Name, URL: own.URL, Kind: "allow", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if presetState(e, 2, "piracy").On {
		t.Error("an allowlist never counts for a switch")
	}
	if _, err := e.SetPresets(ctx, 3, map[string]bool{"piracy": true}); apperr.KindOf(err) != apperr.KindConflict {
		t.Errorf("allowlist: %v", err)
	}
}

// RevertPresets undoes exactly what SetPresets did: created lists are
// deleted, a list that was enabled again is disabled, a recategorised list
// gets its category back, group memberships are restored.
func TestRevertPresets(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	gambling, _ := catalogEntry("hagezi-gambling-medium")
	g := createOK(t, e, ListInput{URL: gambling.URL, Category: CategorySecurity, Enabled: false, GroupIDs: []int64{3}})
	change, err := e.SetPresets(ctx, 2, map[string]bool{"gambling": true, "piracy": true})
	if err != nil || len(change.Lists) != 2 || len(change.Created) != 1 {
		t.Fatalf("switch on: %+v %v", change, err)
	}
	created := change.Created[0]
	deleted, restored, err := e.RevertPresets(ctx, change)
	if err != nil || !slices.Equal(deleted, []int64{created}) || len(restored) != 1 {
		t.Fatalf("revert: %v %+v %v", deleted, restored, err)
	}
	if r := restored[0]; r.ID != g.ID || r.Enabled || r.Category != CategorySecurity || !slices.Equal(r.GroupIDs, []int64{3}) {
		t.Errorf("restored %+v", r)
	}
	lists, _ := e.Lists(ctx)
	if len(lists) != 1 || lists[0].Enabled || !slices.Equal(lists[0].GroupIDs, []int64{3}) {
		t.Errorf("memory after the revert: %+v", lists)
	}
	stored, err := loadLists(ctx, e.db.R)
	if err != nil || len(stored) != 1 || stored[0].Enabled || stored[0].Category != CategorySecurity || !slices.Equal(stored[0].GroupIDs, []int64{3}) {
		t.Fatalf("database after the revert: %+v %v", stored, err)
	}
	// Switching off is reverted too: the group is assigned again.
	if _, err := e.SetPresets(ctx, 3, map[string]bool{"gambling": true}); err != nil {
		t.Fatal(err)
	}
	change, err = e.SetPresets(ctx, 3, map[string]bool{"gambling": false})
	if err != nil || len(change.Lists) != 1 {
		t.Fatalf("switch off: %+v %v", change, err)
	}
	if _, restored, err := e.RevertPresets(ctx, change); err != nil || len(restored) != 1 || !presetState(e, 3, "gambling").On {
		t.Fatalf("revert off: %+v %v", restored, err)
	}
	// Nothing to undo.
	if deleted, restored, err := e.RevertPresets(ctx, PresetChange{}); err != nil || len(deleted) != 0 || len(restored) != 0 {
		t.Errorf("empty revert: %v %v %v", deleted, restored, err)
	}
}

// SetPresets updates memory from the rows of its transaction and repairs a
// bound list that memory lost, even when the database needs no change; list
// writes are serialised, so a concurrent delete never leaves a list behind
// in memory.
func TestSetPresetsMemory(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	change, err := e.SetPresets(ctx, 2, map[string]bool{"adult": true})
	if err != nil || len(change.Created) != 1 {
		t.Fatalf("switch on: %+v %v", change, err)
	}
	id := change.Created[0]
	e.mu.Lock()
	delete(e.lists, id) // memory behind the database (e.g. an earlier failure)
	e.mu.Unlock()
	change, err = e.SetPresets(ctx, 2, map[string]bool{"adult": true})
	if err != nil || len(change.Lists) != 0 {
		t.Fatalf("nothing to change in the database: %+v %v", change, err)
	}
	if st := presetState(e, 2, "adult"); !st.On {
		t.Fatalf("the bound list must be back in memory: %+v", st)
	}
	e.mu.Lock()
	rt := e.lists[id]
	e.mu.Unlock()
	if rt == nil || !rt.wantDownload {
		t.Fatalf("the repaired list is downloaded: %+v", rt)
	}

	for i := range 20 {
		var wg sync.WaitGroup
		g := int64(2 + i%2)
		wg.Go(func() { _ = e.DeleteList(ctx, id) })
		wg.Go(func() { _, _ = e.SetPresets(ctx, g, map[string]bool{"adult": true}) })
		wg.Wait()
		stored, err := loadLists(ctx, e.db.R)
		if err != nil {
			t.Fatal(err)
		}
		e.mu.Lock()
		var inMemory []int64
		for k := range e.lists {
			inMemory = append(inMemory, k)
		}
		e.mu.Unlock()
		slices.Sort(inMemory)
		var inDB []int64
		for _, s := range stored {
			inDB = append(inDB, s.ID)
			id = s.ID
		}
		if !slices.Equal(inMemory, inDB) {
			t.Fatalf("round %d: memory %v, database %v", i, inMemory, inDB)
		}
	}
}

// Loading a cached copy at start shows its counts: a release may parse the
// same copy differently (the TLD guard).
func TestLoadCachedRefreshesCounts(t *testing.T) {
	dir := t.TempDir()
	d := openTestDB(t, dir)
	t.Cleanup(func() { _ = d.Close() })
	ctx := context.Background()
	e := newEngineAt(t, dir, d, testClient())
	l := addLocalList(t, e, "own.txt", "||zip^\n||a.example^\n||b.example^\n", ListInput{Name: "Own"})
	if _, err := e.RefreshList(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	// Counts of an older release without the guard.
	if _, err := d.W.Exec(`UPDATE filter_lists SET entries = 3, invalid = 0 WHERE id = ?`, l.ID); err != nil {
		t.Fatal(err)
	}
	e2 := newEngineAt(t, dir, d, testClient())
	e2.loadCached(ctx)
	lists, err := e2.Lists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(lists, func(x List) bool { return x.ID == l.ID })
	if i < 0 || lists[i].Entries != 2 || lists[i].Invalid != 1 || lists[i].TLDBlocksIgnored != 1 {
		t.Fatalf("counts %+v", lists)
	}
	var entries, invalid int
	if err := d.R.QueryRow(`SELECT entries, invalid FROM filter_lists WHERE id = ?`, l.ID).Scan(&entries, &invalid); err != nil || entries != 2 || invalid != 1 {
		t.Fatalf("stored counts %d %d %v", entries, invalid, err)
	}
}

// protectionEnv has an adult protection list for Kids (2), a general list
// for Kids, an allowlist of category allow for Kids and a user allow rule.
func protectionEnv(t *testing.T) (*Engine, List) {
	t.Helper()
	e := newTestEngine(t)
	ctx := context.Background()
	adult := addLocalList(t, e, "adult.txt", "||adult.example^\n@@||ok.adult.example^\n/porn[0-9]+\\.example/\n",
		ListInput{Name: "Adult", Category: CategoryAdult, GroupIDs: []int64{2}})
	general := addLocalList(t, e, "ads.txt", "||ads.example^\n", ListInput{Name: "Ads", GroupIDs: []int64{2}})
	allow := addLocalList(t, e, "allow.txt", "adult.example\nx.adult.example\n", ListInput{Name: "Fixes", Kind: "allow", GroupIDs: []int64{2}})
	for _, l := range []List{adult, general, allow} {
		if _, err := e.RefreshList(ctx, l.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.CreateRule(ctx, RuleInput{Action: "allow", Type: "exact", Pattern: "y.adult.example", Enabled: true, GroupIDs: []int64{2}}); err != nil {
		t.Fatal(err)
	}
	e.compile()
	return e, adult
}

func TestCheckProtection(t *testing.T) {
	e, adult := protectionEnv(t)
	kids := []int64{2}
	for _, tc := range []struct {
		name   string
		groups []int64
		action Action
		kind   string
	}{
		{"www.adult.example", kids, ActionBlock, "subtree"},
		{"x.adult.example", kids, ActionBlock, "subtree"},  // an allowlist does not lift it
		{"y.adult.example", kids, ActionBlock, "subtree"},  // nor does a user rule (the DNS server lifts at 7a)
		{"ok.adult.example", kids, ActionAllow, "subtree"}, // the list's own @@ entries apply
		{"porn42.example", kids, ActionBlock, "regex"},
		{"ads.example", kids, ActionNone, ""}, // other lists are not evaluated
		{"www.adult.example", []int64{1}, ActionNone, ""},
		{"www.adult.example", nil, ActionNone, ""},
	} {
		d := e.CheckProtection(tc.name, qtypeA, tc.groups)
		if d.Action != tc.action || d.Kind != tc.kind {
			t.Errorf("%s %v: %+v, want %s %s", tc.name, tc.groups, d, tc.action, tc.kind)
		}
		if d.Action == ActionBlock && (d.ListID != adult.ID || d.Name != "Adult" || d.Category != CategoryAdult || d.Source != "list") {
			t.Errorf("%s: decision %+v", tc.name, d)
		}
	}
	// Check still sees the protection list (CNAME inspection keeps
	// checking targets); its decision carries the category; rules carry none.
	if d := e.Check("www.adult.example", qtypeA, kids); !d.Blocked() || d.Category != CategoryAdult {
		t.Errorf("Check %+v", d)
	}
	if d := e.Check("ads.example", qtypeA, kids); d.Category != CategoryOther {
		t.Errorf("an own list without category %+v", d)
	}
	if d := e.Check("y.adult.example", qtypeA, kids); d.Source != "rule" || d.Category != "" {
		t.Errorf("rule decision %+v", d)
	}
	if n := testing.AllocsPerRun(200, func() { e.CheckProtection("a.b.www.adult.example", qtypeA, kids) }); n != 0 {
		t.Errorf("CheckProtection allocates %v times", n)
	}
	// A category change swaps only the tables: security is no protection
	// category (the pre-0.10 behaviour of the bypass list).
	ctx := context.Background()
	if _, err := e.UpdateList(ctx, adult.ID, ListInput{Name: "Adult", URL: adult.URL, Enabled: true, Category: CategorySecurity}); err != nil {
		t.Fatal(err)
	}
	if d := e.CheckProtection("www.adult.example", qtypeA, kids); d.Action != ActionNone {
		t.Errorf("security list is no protection list: %+v", d)
	}
	if e.snap.Load().hasProt {
		t.Error("no protection list is enabled")
	}
	if n := testing.AllocsPerRun(200, func() { e.CheckProtection("www.adult.example", qtypeA, kids) }); n != 0 {
		t.Errorf("CheckProtection allocates %v times without protection lists", n)
	}
}

// The TLD guard: subtree, wildcard and pattern blocks of a single label or
// an ICANN public suffix are invalid, except in abused-tlds lists; exact
// blocks, allow entries, private suffixes and patterns that cover only part
// of a TLD are unaffected.
func TestTLDGuard(t *testing.T) {
	body := strings.Join([]string{
		"||com^", "||co.uk^$important", "*.de", "||example.com^", "0.0.0.0 com.example", "|org^", "@@||net^",
		"||github.io^", "co.jp",
		// Patterns that cover a whole TLD (refused) …
		"*.com^", "||*.com^", ".com^", `/\.com$/`, "*.co.uk^", `/\.(xyz|top)$/`, "||app*^", "/./",
		// … and patterns that do not (kept).
		"||ads*.example.com^", `/^ad[0-9]*\./`, "||*.example.co.uk^", "@@||*.org^",
	}, "\n")
	parse := func(f listFormat) *parsed {
		t.Helper()
		p, err := parseList(context.Background(), strings.NewReader(body), f)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	guarded := parse(formatOf("block", "subtree", CategoryGeneral, FormatDomains))
	// Invalid: ||com^, ||co.uk^$important, *.de, co.jp (plain in subtree
	// mode) and the eight whole-TLD patterns.
	if guarded.invalid != 12 || guarded.broad != 12 || guarded.entries != 9 {
		t.Errorf("guarded: %d entries, %d invalid, %d TLD blocks", guarded.entries, guarded.invalid, guarded.broad)
	}
	exempt := parse(formatOf("block", "subtree", CategoryAbusedTLDs, FormatDomains))
	if exempt.invalid != 0 || exempt.broad != 0 || exempt.entries != 21 {
		t.Errorf("abused-tlds: %d entries, %d invalid", exempt.entries, exempt.invalid)
	}
	for _, tc := range []struct {
		d    string
		want bool
	}{
		{"com", true}, {"co.uk", true}, {"de", true}, {"xn--p1ai", true}, {"k12.ma.us", true},
		{"example.com", false}, {"github.io", false}, {"blogspot.com", false}, {"example.co.uk", false},
	} {
		if got := broadDomain(tc.d); got != tc.want {
			t.Errorf("broadDomain(%q) = %v", tc.d, got)
		}
	}
	// Pattern forms, one line at a time.
	for _, tc := range []struct {
		line  string
		broad bool
	}{
		{"*.com^", true}, {"||*.com^", true}, {"|*.com^", true}, {"://*.com^", true}, {".com^", true}, {"com^", true},
		{"*.co.uk^", true}, {"||*.co.uk^", true}, {"*.xn--p1ai|", true}, {"||*.*.com^", true}, {"||com*^", true},
		{`/\.com$/`, true}, {`/(?:^|\.)xyz$/`, true}, {`/\.(xyz|top)$/`, true}, {`/\.C[O]M$/`, true},
		{"/./", true}, {"/^.+$/", true}, {"/com/", true}, {"||app*^", true}, {"*.com^$important", true},
		{"||ads*.example.com^", false}, {"||ads.*.com^", false}, {"||doubleclick*^", false}, {"||*.example.co.uk^", false},
		{`/^ad[0-9]*\./`, false}, {`/^[a-z]{12}\.(xyz|top)$/`, false}, {`/ads?[0-9]*\.example\.com$/`, false},
		{"@@||*.com^", false}, {`@@/\.com$/`, false}, {"||*.com^$badfilter", false}, {"||tracking*.com^", false},
	} {
		lp := newLineParser(formatOf("block", "exact", CategoryAdult, FormatDomains))
		if _, st := lp.parse(tc.line); (st == lineBroad) != tc.broad || st != lineBroad && st != lineOK {
			t.Errorf("%q: status %d, want broad %v", tc.line, st, tc.broad)
		}
		lp = newLineParser(formatOf("block", "exact", CategoryAbusedTLDs, FormatDomains))
		if _, st := lp.parse(tc.line); st != lineOK {
			t.Errorf("%q in an abused-tlds list: status %d", tc.line, st)
		}
	}
}

// The guard follows a category change into or out of abused-tlds (the
// list is parsed again).
func TestTLDGuardFollowsCategory(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	l := addLocalList(t, e, "tlds.txt", "||zip^\n||example.com^\n", ListInput{Name: "TLDs", Category: CategoryAbusedTLDs})
	if _, err := e.RefreshList(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	e.compile()
	if !e.Check("files.zip", qtypeA, []int64{1}).Blocked() {
		t.Fatal("abused-tlds list must block the TLD")
	}
	if _, err := e.UpdateList(ctx, l.ID, ListInput{Name: "TLDs", URL: l.URL, Enabled: true, Category: CategorySecurity}); err != nil {
		t.Fatal(err)
	}
	if id, download, ok := e.nextJob(); !ok || id != l.ID || download {
		t.Fatalf("a category change out of abused-tlds must re-parse: %d %v %v", id, download, ok)
	}
	if _, err := e.refresh(ctx, l.ID, false); err != nil {
		t.Fatal(err)
	}
	e.compile()
	if e.Check("files.zip", qtypeA, []int64{1}).Blocked() || !e.Check("example.com", qtypeA, []int64{1}).Blocked() {
		t.Error("the guarded list must drop the TLD block only")
	}
	if ls, _ := e.Lists(ctx); ls[0].Invalid != 1 || ls[0].TLDBlocksIgnored != 1 {
		t.Errorf("invalid count %d, TLD blocks ignored %d", ls[0].Invalid, ls[0].TLDBlocksIgnored)
	}
	// An own list whose TLD entries are ignored shows up in the stats (health).
	if st := e.Stats(); st.TLDGuardLists != 1 {
		t.Errorf("stats: %d own lists with ignored TLD blocks", st.TLDGuardLists)
	}
}

// The parser is the entry point of untrusted list content: it must never
// panic, and the guard must hold for every line.
func FuzzParseLine(f *testing.F) {
	for _, s := range []string{"||com^", "*.co.uk", "0.0.0.0 a.example", "@@||x^$important", "/re+/", "||a*b^", "co.jp",
		"||xn--p1ai^$badfilter", "|x.example|", "example.com # c", "##.ad", "[Adblock]", "||a.b^$third-party",
		"*.com^", "||*.co.uk^", `/\.xyz$/`, "/./", "||app*^",
		"||x.example^$dnstype=A|AAAA", "||x.example^$dnstype=~A|~TYPE65280", "@@||x.example^$dnstype=AAAA,important",
		"||x.example^$denyallow=a.x.example|b.x.example", "||com^$denyallow=example.com", "*$denyallow=com|net",
		"||x.example^$dnstype=A,badfilter", "|x.example^$denyallow=y.example", "/re/$dnstype=A"} {
		f.Add(s, true, true)
	}
	f.Fuzz(func(t *testing.T, line string, subtree, guard bool) {
		lp := lineParser{plainSubtree: subtree, tldGuard: guard}
		entries, st := lp.parse(line)
		if st != lineOK && len(entries) != 0 {
			t.Fatalf("%q: status %d with entries", line, st)
		}
		for _, en := range entries {
			if en.kind != kindPattern && !validDomain(en.domain) {
				t.Fatalf("%q: invalid domain %q", line, en.domain)
			}
			if en.modified() {
				if en.kind == kindPattern || len(en.types.types()) > maxEntryTypes || len(en.deny) > maxDenyallow ||
					len(en.deny) > 0 && (en.allow || en.kind != kindSubtree) {
					t.Fatalf("%q: modifiers out of bounds: %+v", line, en)
				}
				for _, d := range en.deny {
					if !validDomain(d) {
						t.Fatalf("%q: invalid denyallow domain %q", line, d)
					}
				}
			}
			if !guard || en.allow || en.badfilter {
				continue
			}
			if en.kind == kindSubtree && broadDomain(en.domain) {
				t.Fatalf("%q: the guard let a TLD block through", line)
			}
			if en.kind != kindPattern {
				continue
			}
			if re, err := en.compile(); err == nil {
				for _, sfx := range probeSuffixes {
					if name := probeLabel + "." + sfx; re.MatchString(name) {
						t.Fatalf("%q: the guard let a pattern through that blocks %s", line, name)
					}
				}
			}
		}
	})
}

// The migration adds the columns without touching existing rows.
func TestMigration2Columns(t *testing.T) {
	d := openTestDB(t, t.TempDir())
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(context.Background(), "filter", migrations[:1]); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(context.Background(), "filter", migrations); err != nil {
		t.Fatal(err)
	}
	var cat, key string
	if err := d.R.QueryRow(`SELECT category, catalog_key FROM filter_lists WHERE id = 1`).Scan(&cat, &key); err != nil || cat != "" || key != "" {
		t.Fatalf("new columns: %q %q %v", cat, key, err)
	}
}
