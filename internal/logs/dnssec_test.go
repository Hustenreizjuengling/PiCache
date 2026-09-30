package logs

import (
	"context"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// A logs.db of 0.16 (logs v5) gets the DNSSEC status column; its rows read
// as not validated (logs v6).
func TestUpgradeFrom016(t *testing.T) {
	ctx := context.Background()
	ldb, cdb, set := openTestDBs(t, t.TempDir())
	defer cdb.Close()
	defer ldb.Close()
	if err := ldb.Migrate(ctx, "logs", migrations[:5]); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := ldb.W.Exec(`INSERT INTO logs_queries (ts, client_ip, qname, qtype, status, rcode, dnssec)
		VALUES (?, '10.0.0.1', 'old.example', 'A', 'forwarded', 'NOERROR', 1)`, now.Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, ldb, set, nil)
	if err != nil {
		t.Fatal(err)
	}
	var v int
	if err := ldb.R.QueryRow(`SELECT MAX(version) FROM schema_migrations WHERE component = 'logs'`).Scan(&v); err != nil || v != 6 {
		t.Fatalf("logs v%d, %v", v, err)
	}
	page, err := s.QueryLog(ctx, QueryFilter{From: now.Add(-time.Hour), To: now})
	if err != nil || len(page.Items) != 1 || page.Items[0].DNSSECStatus != "" || !page.Items[0].DNSSEC {
		t.Fatalf("old row %+v, %v", page, err)
	}
}

// The DNSSEC status is stored, filtered (several values ORed, comma lists),
// published on the live feed and counted per status (kind dnssec) under
// the switches of the statistics.
func TestQueryDNSSECStatus(t *testing.T) {
	ctx := context.Background()
	s, set := newTestStore(t)
	live, cancel, err := s.SubscribeQueries(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	now := time.Now().Add(-time.Second)
	add := func(qname, qtype, status string) {
		q := query(now, "10.0.0.1", qname, "forwarded")
		q.QType, q.DNSSECStatus = qtype, status
		s.w.addQuery(q)
	}
	add("a.example", "A", "secure")
	add("b.example", "A", "secure")
	add("c.example", "A", "bogus")
	add("d.example", "TXT", "insecure")
	add("e.example", "A", "")
	add("f.example", "A", "weird") // not a status: stored as none
	s.w.flush(now)
	if ev := <-live; ev.DNSSECStatus != "secure" {
		t.Fatalf("live event %+v", ev)
	}
	for _, tc := range []struct {
		filter []string
		want   int
	}{{[]string{"secure"}, 2}, {[]string{"secure", "bogus"}, 3}, {[]string{"insecure,BOGUS"}, 2}, {nil, 6}} {
		page, err := s.QueryLog(ctx, QueryFilter{From: now.Add(-time.Minute), To: now.Add(time.Minute), DNSSECStatus: tc.filter})
		if err != nil || len(page.Items) != tc.want {
			t.Errorf("filter %v: %d rows, want %d (%v)", tc.filter, len(page.Items), tc.want, err)
		}
	}
	_, err = s.QueryLog(ctx, QueryFilter{From: now.Add(-time.Minute), To: now, DNSSECStatus: []string{"valid"}})
	if ae, ok := apperr.As(err); !ok || ae.Field != "dnssecStatus" || ae.Message != "must be secure, insecure, bogus or indeterminate" {
		t.Fatalf("invalid filter: %v", err)
	}
	st, err := s.DNSSEC(ctx, now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil || len(st.Statuses) != 3 || st.Statuses[0] != (DNSSECCount{"secure", 2}) || st.From.IsZero() {
		t.Fatalf("stats %+v %v", st, err)
	}
	// statsOnlyAddressQueries: the TXT query is not counted; statsEnabled
	// off: nothing is.
	updateLogs(t, set, func(l *settings.Logs) { l.StatsOnlyAddressQueries = true })
	add("g.example", "TXT", "insecure")
	updateLogs(t, set, func(l *settings.Logs) { l.StatsOnlyAddressQueries = false; l.StatsEnabled = false })
	add("h.example", "A", "secure")
	s.w.flush(now)
	st, _ = s.DNSSEC(ctx, now.Add(-time.Hour), now.Add(time.Minute))
	for _, c := range st.Statuses {
		if (c.Status == "insecure" && c.Count != 1) || (c.Status == "secure" && c.Count != 2) {
			t.Fatalf("stats after the switches %+v", st)
		}
	}
	// Empty ranges: never null.
	st, err = s.DNSSEC(ctx, now.Add(-72*time.Hour), now.Add(-48*time.Hour))
	if err != nil || st.Statuses == nil || len(st.Statuses) != 0 {
		t.Fatalf("empty range %+v %v", st, err)
	}
}

func TestDNSSECStatusFilter(t *testing.T) {
	got, err := DNSSECStatusFilter([]string{" Secure ", "secure,bogus", "", "indeterminate,insecure"})
	if err != nil || len(got) != 4 {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := DNSSECStatusFilter([]string{"secure,x"}); err == nil {
		t.Fatal("x accepted")
	}
}
