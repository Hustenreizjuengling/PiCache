package filter

import (
	"context"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

func TestBudgetFor(t *testing.T) {
	const mib = 1 << 20
	for _, tc := range []struct {
		limit uint64
		want  int
	}{
		{0, 4_000_000},
		{64 * mib, 500_000},
		{128 * mib, 500_000},
		{256 * mib, 1_000_000},
		{300 * mib, 1_100_000}, // 1 171 875 rounded down
		{512 * mib, 2_000_000},
		{1000 * mib, 3_900_000},
		{1024*mib - 1, 3_900_000},
		{1024 * mib, 4_000_000},
		{4096 * mib, 4_000_000},
		{1 << 62, 4_000_000},
	} {
		if got := BudgetFor(tc.limit); got != tc.want {
			t.Errorf("BudgetFor(%d MiB) = %d, want %d", tc.limit/mib, got, tc.want)
		}
	}
}

// MemTotal is rounded up to the nominal size, so 1 GB and 512 MB boards get
// the budgets of 1 GiB and 512 MiB.
func TestNominalMemory(t *testing.T) {
	const mib = 1 << 20
	for _, tc := range []struct {
		memTotal, want uint64
		budget         int
	}{
		{0, 0, 4_000_000},
		{926 * mib, 1024 * mib, 4_000_000}, // Raspberry Pi 3B
		{970 * mib, 1024 * mib, 4_000_000}, // a 1 GB VM
		{1024 * mib, 1024 * mib, 4_000_000},
		{430 * mib, 512 * mib, 2_000_000}, // Raspberry Pi Zero (2) W
		{512*mib + 1, 640 * mib, 2_500_000},
		{200 * mib, 256 * mib, 1_000_000},
		{1850 * mib, 1920 * mib, 4_000_000},
	} {
		got := NominalMemory(tc.memTotal)
		if got != tc.want || BudgetFor(got) != tc.budget {
			t.Errorf("NominalMemory(%d MiB) = %d MiB (budget %d), want %d MiB (budget %d)",
				tc.memTotal/mib, got/mib, BudgetFor(got), tc.want/mib, tc.budget)
		}
	}
}

// The batch 409 uses the engine's budget (the host's memory) and names it.
func TestBatchListsUsesHostBudget(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	if e.EntryBudget() != MaxEntryBudget {
		t.Fatalf("default budget %d", e.EntryBudget())
	}
	e.SetEntryBudget(BudgetFor(512 << 20))
	if e.EntryBudget() != 2_000_000 {
		t.Fatalf("budget %d", e.EntryBudget())
	}
	a := addLocalList(t, e, "a.txt", "||a.example^\n", ListInput{})
	if _, err := e.BatchLists(ctx, BatchDisable, []int64{a.ID}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.W.Exec(`UPDATE filter_lists SET entries = ? WHERE id = ?`, 2_000_001, a.ID); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.lists[a.ID].Entries = 2_000_001
	e.mu.Unlock()
	_, err := e.BatchLists(ctx, BatchEnable, []int64{a.ID}, false)
	ae, ok := apperr.As(err)
	if !ok || ae.Kind != apperr.KindConflict || ae.Field != "force" || !strings.Contains(ae.Message, "more than 2000000") {
		t.Fatalf("budget of a 512 MiB host: %v", err)
	}
	e.SetEntryBudget(0)
	if n, err := e.BatchLists(ctx, BatchEnable, []int64{a.ID}, false); err != nil || n != 1 {
		t.Fatalf("within the default budget: %d %v", n, err)
	}
}
