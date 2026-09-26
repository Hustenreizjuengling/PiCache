package logs

import (
	"context"
	"testing"
	"time"
)

// A logs.db of 0.11 (logs v3) is migrated to v4 without rewriting its
// tables: the stored queries get an empty upstream answer, the warning
// history and the daily top tables exist, and the history backfills the
// days of the stored hourly top lists.
func TestUpgradeFrom011(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ldb, cdb, set := openTestDBs(t, dir)
	defer cdb.Close()
	if err := ldb.Migrate(ctx, "logs", migrations[:3]); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	yesterday := dayStart(now.UnixMilli()) - dayMs
	if _, err := ldb.W.Exec(`INSERT INTO logs_queries (ts, client_ip, qname, qtype, status, rcode, upstream_ede_code, ecs)
		VALUES (?, '10.0.0.1', 'old.example', 'A', 'forwarded', 'NOERROR', -1, '')`, now.Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := ldb.W.Exec(`INSERT INTO logs_dns_top_hourly (bucket, kind, key, count) VALUES (?, 'domain', 'old.example', 5)`,
		yesterday+3*hourMs); err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, ldb, set, nil)
	if err != nil {
		t.Fatal(err)
	}
	var v int
	if err := ldb.R.QueryRow(`SELECT MAX(version) FROM schema_migrations WHERE component = 'logs'`).Scan(&v); err != nil || v != 4 {
		t.Fatalf("logs v%d, %v", v, err)
	}
	page, err := s.QueryLog(ctx, QueryFilter{From: now.Add(-2 * time.Hour), To: now})
	if err != nil || len(page.Items) != 1 || page.Items[0].QName != "old.example" || page.Items[0].UpstreamAnswer != "" {
		t.Fatalf("stored query %+v, %v", page, err)
	}
	s.RecordEvent(EventRecord{Event: "update.available", Title: "Update available"})
	if ev, err := s.Events(ctx, EventQuery{}); err != nil || len(ev.Items) != 1 {
		t.Fatalf("history %+v, %v", ev, err)
	}
	s.w.backfillStep()
	var n int
	if err := ldb.R.QueryRow(`SELECT COUNT(*) FROM logs_dns_top_daily WHERE bucket = ?`, yesterday).Scan(&n); err != nil || n != 1 {
		t.Fatalf("backfilled daily rows %d, %v", n, err)
	}
	ldb.Close()
}
