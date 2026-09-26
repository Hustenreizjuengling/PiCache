package logs

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// safesearch is an allowed status: counted in the local column (series
// class allowed, summary), accepted by the status filters and counted as
// an allowed domain.
func TestSafeSearchStatus(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	for _, st := range []string{"safesearch", "safesearch", "forwarded", "blocked-list"} {
		s.w.addQuery(query(now, "192.168.1.7", st+".example", st))
	}
	s.w.flush(now)
	for _, tc := range []struct {
		filter []string
		want   int
	}{{[]string{"safesearch"}, 2}, {[]string{"allowed"}, 3}, {[]string{"blocked"}, 1}} {
		page, err := s.QueryLog(ctx, QueryFilter{From: now.Add(-time.Minute), To: now.Add(time.Minute), Status: tc.filter})
		if err != nil || len(page.Items) != tc.want {
			t.Errorf("filter %v: %d rows, want %d (%v)", tc.filter, len(page.Items), tc.want, err)
		}
	}
	sum, err := s.Summary(ctx, now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil || sum.DNSQueries != 4 || sum.DNSBlocked != 1 || sum.DNSForwarded != 1 {
		t.Fatalf("summary %+v %v", sum, err)
	}
	ser, err := s.DNSSeries(ctx, now.Add(-10*time.Minute), now.Add(time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	var allowed float64
	for _, v := range ser.Values["allowed"] {
		allowed += v
	}
	if allowed != 3 {
		t.Errorf("series allowed = %v, want 3", allowed)
	}
	top, err := s.Top(ctx, TopDomains, now.Add(-time.Hour), now.Add(time.Hour), 10)
	if err != nil || len(top) != 2 || top[0].Key != "safesearch.example" || top[0].Count != 2 {
		t.Errorf("top domains %v %v", top, err)
	}
	if m, err := QueryMatcher(nil, []string{"safesearch"}); err != nil || !m(QueryEvent{Status: "safesearch"}) {
		t.Errorf("live filter: %v", err)
	}
}

// The purpose is counted in the hourly top table (also with the query log
// disabled), merged with the in-memory hour, never stored with the query
// or sent to the live feed, and pruned with the table.
func TestPurposes(t *testing.T) {
	s, set := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	sub, cancel, err := s.SubscribeQueries(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	add := func(n int, status, purpose string) {
		for range n {
			q := query(now, "10.0.0.1", purpose+".example", status)
			q.Purpose = purpose
			s.w.addQuery(q)
		}
	}
	add(3, "blocked-list", "adult")
	add(1, "blocked-list", "security")
	add(2, "safesearch", "safesearch")
	add(1, "blocked-rule", "rule")
	add(4, "forwarded", "")
	s.w.flush(now)

	want := "[{adult 3} {safesearch 2} {rule 1} {security 1}]"
	check := func(stage string) {
		t.Helper()
		st, err := s.Purposes(ctx, now.Add(-30*time.Minute), now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(st.Purposes); got != want || !st.From.Equal(TopFrom(now.Add(-30*time.Minute), now.Add(time.Minute))) {
			t.Errorf("%s: purposes %s from %v, want %s", stage, got, st.From, want)
		}
	}
	check("in memory")
	s.w.checkpoint(now)
	check("after checkpoint")
	var stored int
	if err := s.d.R.QueryRow(`SELECT COUNT(*) FROM logs_dns_top_hourly WHERE kind = 'purpose'`).Scan(&stored); err != nil || stored != 4 {
		t.Fatalf("stored purpose rows %d %v", stored, err)
	}
	// The query log and the live feed never carry the purpose.
	page, err := s.QueryLog(ctx, QueryFilter{From: now.Add(-time.Minute), To: now.Add(time.Minute)})
	if err != nil || len(page.Items) != 11 {
		t.Fatalf("query log %d %v", len(page.Items), err)
	}
	for _, it := range page.Items {
		if it.Purpose != "" {
			t.Fatalf("stored purpose %+v", it)
		}
		b, _ := json.Marshal(it)
		if strings.Contains(string(b), "urpose") {
			t.Fatalf("purpose in JSON: %s", b)
		}
	}
	select {
	case ev := <-sub:
		if ev.Purpose != "" {
			t.Errorf("live event carries the purpose %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no live event")
	}
	// Counted while the query log is disabled.
	updateLogs(t, set, func(l *settings.Logs) { l.QueryLogEnabled = false })
	add(5, "blocked-service", "service")
	want = "[{service 5} {adult 3} {safesearch 2} {rule 1} {security 1}]"
	check("query log disabled")

	// An empty range: an empty list, never null.
	st, err := s.Purposes(ctx, now.Add(-72*time.Hour), now.Add(-71*time.Hour))
	if err != nil || st.Purposes == nil || len(st.Purposes) != 0 {
		t.Fatalf("empty range %+v %v", st, err)
	}
	if b, _ := json.Marshal(st); !strings.Contains(string(b), `"purposes":[]`) {
		t.Errorf("JSON %s", b)
	}
	// The kind has a key cap per hour like the other kinds.
	for i := range 70 {
		q := query(now, "10.0.0.1", "x.example", "blocked-list")
		q.Purpose = fmt.Sprint("p", i)
		s.w.addQuery(q)
	}
	s.top.mu.Lock()
	n := len(s.top.dns[dnsTopPurpose])
	s.top.mu.Unlock()
	if n != 64 {
		t.Errorf("purpose keys in memory %d, want the cap 64", n)
	}
	// Pruned with the table.
	if _, err := s.d.W.Exec(`INSERT INTO logs_dns_top_hourly (bucket, kind, key, count) VALUES (?, 'purpose', 'adult', 1)`,
		hourStart(now.Add(-400*24*time.Hour).UnixMilli())); err != nil {
		t.Fatal(err)
	}
	s.w.prune(now)
	var old int
	if err := s.d.R.QueryRow(`SELECT COUNT(*) FROM logs_dns_top_hourly WHERE kind = 'purpose' AND bucket < ?`,
		now.Add(-365*24*time.Hour).UnixMilli()).Scan(&old); err != nil || old != 0 {
		t.Fatalf("old purpose rows %d %v", old, err)
	}
}

// Rows of a kind this version does not know are ignored (a newer
// version's rows after a downgrade, and 0.9 ignores the purpose rows).
func TestUnknownTopKindIgnored(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	hour := hourStart(time.Now().UnixMilli())
	if _, err := s.d.W.Exec(`INSERT INTO logs_dns_top_hourly (bucket, kind, key, count) VALUES (?, 'future', 'x', 7)`, hour); err != nil {
		t.Fatal(err)
	}
	if err := s.loadTop(ctx, hour); err != nil {
		t.Fatalf("unknown kind: %v", err)
	}
}
