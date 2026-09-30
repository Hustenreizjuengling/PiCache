package logs

import (
	"context"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// A logs.db of 0.13 (logs v4) gets the ClientID column without losing its
// rows (logs v5, no table rewrite).
func TestUpgradeFrom013(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ldb, cdb, set := openTestDBs(t, dir)
	defer cdb.Close()
	defer ldb.Close()
	if err := ldb.Migrate(ctx, "logs", migrations[:4]); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := ldb.W.Exec(`INSERT INTO logs_queries (ts, client_ip, qname, qtype, status, rcode, upstream_ede_code, ecs, upstream_answer)
		VALUES (?, '10.0.0.1', 'old.example', 'A', 'forwarded', 'NOERROR', -1, '', '')`, now.Add(-time.Minute).UnixMilli()); err != nil {
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
	if err != nil || len(page.Items) != 1 || page.Items[0].QName != "old.example" || page.Items[0].DNSClientID != "" {
		t.Fatalf("stored query %+v, %v", page, err)
	}
}

// The ClientID of a query is stored, filtered exactly, published on the
// live feed and removed while client addresses are anonymised.
func TestQueryClientID(t *testing.T) {
	ctx := context.Background()
	s, set := newTestStore(t)
	live, cancel, err := s.SubscribeQueries(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	now := time.Now().Add(-time.Second)
	q := query(now, "10.0.0.1", "a.example", "forwarded")
	q.Protocol, q.DNSClientID = "dot", "Phone"
	s.w.addQuery(q)
	other := query(now, "10.0.0.2", "b.example", "forwarded")
	other.Protocol = "doh"
	s.w.addQuery(other)
	s.w.flush(now)
	if ev := <-live; ev.DNSClientID != "phone" || ev.Protocol != "dot" {
		t.Fatalf("live event %+v", ev)
	}
	page, err := s.QueryLog(ctx, QueryFilter{DNSClientID: "PHONE"})
	if err != nil || len(page.Items) != 1 || page.Items[0].DNSClientID != "phone" || page.Items[0].QName != "a.example" {
		t.Fatalf("filtered %+v, %v", page.Items, err)
	}
	for _, bad := range []string{"-x", "a.b", "x_y"} {
		_, err := s.QueryLog(ctx, QueryFilter{DNSClientID: bad})
		wantKind(t, err, apperr.KindInvalid)
	}

	updateLogs(t, set, func(l *settings.Logs) { l.AnonymizeClientIPs = true })
	q.QName = "c.example"
	s.w.addQuery(q)
	s.w.flush(now)
	page, err = s.QueryLog(ctx, QueryFilter{Domain: `"c.example"`})
	if err != nil || len(page.Items) != 1 || page.Items[0].DNSClientID != "" {
		t.Fatalf("anonymised %+v, %v", page.Items, err)
	}
	if e := cleanQuery(QueryEvent{DNSClientID: "bad id"}, false, false, now); e.DNSClientID != "" {
		t.Fatalf("invalid ClientID kept: %q", e.DNSClientID)
	}
}
