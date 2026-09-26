package filter

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// Rule and IP rule batches: all or nothing (an unknown id changes
// nothing), the count of changed rows, one matcher rebuild.
func TestBatchRules(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	var ids []int64
	for i := range 3 {
		r, err := e.CreateRule(ctx, RuleInput{Action: "block", Type: "exact", Pattern: fmt.Sprintf("r%d.example", i), Enabled: i != 2})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	_, err := e.BatchRules(ctx, BatchDisable, append(ids, 99))
	wantKind(t, "unknown id", err, apperr.KindNotFound)
	if err == nil || !strings.Contains(err.Error(), "rule 99 not found") {
		t.Fatalf("message %v", err)
	}
	if !e.Check("r0.example", qtypeA, []int64{1}).Blocked() {
		t.Fatal("a refused batch changed a rule")
	}
	if n, err := e.BatchRules(ctx, BatchDisable, ids); err != nil || n != 2 {
		t.Fatalf("disable %d %v", n, err)
	}
	if e.Check("r0.example", qtypeA, []int64{1}).Blocked() {
		t.Fatal("the matcher was not rebuilt")
	}
	if n, _ := e.BatchRules(ctx, BatchEnable, ids); n != 3 {
		t.Fatalf("enable %d", n)
	}
	if n, err := e.BatchRules(ctx, BatchDelete, ids[:2]); err != nil || n != 2 {
		t.Fatalf("delete %d %v", n, err)
	}
	if rules, _ := e.Rules(ctx, RuleQuery{}); len(rules) != 1 || e.Check("r0.example", qtypeA, []int64{1}).Blocked() {
		t.Fatalf("after delete %+v", rules)
	}
	ip, err := e.CreateIPRule(ctx, IPRuleInput{Action: "block", Pattern: "203.0.113.0/24", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := e.BatchIPRules(ctx, BatchDisable, []int64{ip.ID}); err != nil || n != 1 || e.Stats().IPRules != 0 {
		t.Fatalf("IP rules %d %v", n, err)
	}
	_, err = e.BatchIPRules(ctx, BatchDelete, []int64{ip.ID, ip.ID + 1})
	wantKind(t, "unknown IP rule", err, apperr.KindNotFound)
}

// List batches: all or nothing, cached copies removed after a delete, the
// entry budget refuses enabling beyond it unless forced.
func TestBatchLists(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	a := addLocalList(t, e, "a.txt", "||a.example^\n", ListInput{})
	b := addLocalList(t, e, "b.txt", "||b.example^\n", ListInput{})
	for !e.Check("a.example", qtypeA, []int64{1}).Blocked() || !e.Check("b.example", qtypeA, []int64{1}).Blocked() {
		e.runPending(ctx)
		e.compile()
	}
	_, err := e.BatchLists(ctx, BatchDisable, []int64{a.ID, 999}, false)
	wantKind(t, "unknown list", err, apperr.KindNotFound)
	if n, err := e.BatchLists(ctx, BatchDisable, []int64{a.ID, b.ID}, false); err != nil || n != 2 {
		t.Fatalf("disable %d %v", n, err)
	}
	e.compile()
	if e.Check("a.example", qtypeA, []int64{1}).Blocked() {
		t.Fatal("a disabled list still blocks")
	}
	// The budget: a list whose stored count would exceed it.
	if _, err := e.db.W.Exec(`UPDATE filter_lists SET entries = ? WHERE id = ?`, EntryBudget, a.ID); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.lists[a.ID].Entries = EntryBudget
	e.mu.Unlock()
	_, err = e.BatchLists(ctx, BatchEnable, []int64{a.ID, b.ID}, false)
	ae, ok := apperr.As(err)
	if !ok || ae.Kind != apperr.KindConflict || ae.Field != "force" || !strings.Contains(ae.Message, "more than 4000000") {
		t.Fatalf("budget: %v", err)
	}
	if l, _ := e.list(a.ID); l.Enabled {
		t.Fatal("a refused batch enabled a list")
	}
	if n, err := e.BatchLists(ctx, BatchEnable, []int64{a.ID, b.ID}, true); err != nil || n != 2 {
		t.Fatalf("forced %d %v", n, err)
	}
	for !e.Check("b.example", qtypeA, []int64{1}).Blocked() {
		e.runPending(ctx)
		e.compile()
	}
	if n, err := e.BatchLists(ctx, BatchDelete, []int64{a.ID, b.ID}, false); err != nil || n != 2 {
		t.Fatalf("delete %d %v", n, err)
	}
	e.compile()
	if _, err := os.Stat(e.cachePath(a.ID)); !os.IsNotExist(err) {
		t.Fatalf("cached copy kept: %v", err)
	}
	if lists, _ := e.Lists(ctx); len(lists) != 0 || e.Check("b.example", qtypeA, []int64{1}).Blocked() {
		t.Fatalf("after delete %+v", lists)
	}
}

// The estimate of a list being enabled: its stored count, else the
// catalogue entry's count for the same URL, else 0.
func TestListEstimate(t *testing.T) {
	c := catalog[0]
	if got := listEstimate(&listRT{List: List{URL: c.URL}}); got != c.Entries {
		t.Errorf("catalogue estimate %d, want %d", got, c.Entries)
	}
	if got := listEstimate(&listRT{List: List{URL: c.URL, Entries: 7}}); got != 7 {
		t.Errorf("stored estimate %d", got)
	}
	if got := listEstimate(&listRT{List: List{URL: "https://own.example/l.txt"}}); got != 0 {
		t.Errorf("unknown estimate %d", got)
	}
}
