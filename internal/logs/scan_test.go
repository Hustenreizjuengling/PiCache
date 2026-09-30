package logs

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"
)

// A search whose filters match few rows reads the query log in windows of
// the ts index and ends a page after its budget with the rows found so far,
// Partial and a cursor at the scan position, instead of one read of the
// whole range that failed with 503 after the query timeout on a large log.
// Following the cursors finds every match once, newest first; the export
// moves over windows without matches instead of failing.
func TestQueryLogScansInWindows(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	const n = 30000
	if _, err := s.d.W.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO logs_queries (ts, client_ip, qname, qtype, status)
		SELECT ? - i * 100, '10.0.0.1', 'q' || i || '.example', CASE WHEN i % 7001 = 0 THEN 'AAAA' ELSE 'A' END, 'forwarded' FROM n`,
		n, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	var want []string
	for i := 1; i <= n; i++ {
		if i%7001 == 0 {
			want = append(want, "q"+strconv.Itoa(i)+".example")
		}
	}
	oldRows, oldBudget := queryScanRows, queryScanBudget
	t.Cleanup(func() { queryScanRows, queryScanBudget = oldRows, oldBudget })
	queryScanRows, queryScanBudget = 1000, 0 // one window per page

	f := QueryFilter{From: now.Add(-time.Hour), To: now.Add(time.Second), QType: "AAAA", Limit: 100}
	var got []string
	pages, partial := 0, 0
	for {
		page, err := s.QueryLog(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if page.Partial {
			partial++
		}
		for _, e := range page.Items {
			got = append(got, e.QName)
		}
		if page.Next == "" || pages > 100 {
			break
		}
		f.Cursor = page.Next
	}
	if !slices.Equal(got, want) || partial == 0 || pages < n/1000 {
		t.Fatalf("got %v in %d pages (%d partial), want %v", got, pages, partial, want)
	}
	// A full page still ends at its last row.
	page, err := s.QueryLog(ctx, QueryFilter{From: now.Add(-time.Hour), To: now.Add(time.Second), Limit: 10})
	if err != nil || len(page.Items) != 10 || page.Partial || page.Next == "" || page.Items[0].QName != "q1.example" {
		t.Fatalf("unfiltered: %d items, partial %v, next %q, %v", len(page.Items), page.Partial, page.Next, err)
	}
	// The export walks every window.
	var exported []string
	if err := s.ExportQueries(ctx, QueryFilter{From: now.Add(-time.Hour), To: now.Add(time.Second), QType: "AAAA"},
		func(chunk []QueryEvent) (bool, error) {
			for _, e := range chunk {
				exported = append(exported, e.QName)
			}
			return true, nil
		}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(exported, want) {
		t.Fatalf("exported %v, want %v", exported, want)
	}
}
