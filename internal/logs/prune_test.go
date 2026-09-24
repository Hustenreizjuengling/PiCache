package logs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRetentionPruning(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Now()
	day := 24 * time.Hour
	// Defaults: queries 168 h, cache events 48 h, sessions 90 d, stats 365 d, minutes 48 h.
	for _, age := range []time.Duration{200 * time.Hour, time.Hour} {
		s.w.addQuery(query(now.Add(-age), "10.0.0.1", "a.example", "forwarded"))
	}
	for _, age := range []time.Duration{50 * time.Hour, time.Hour} {
		s.w.addCache(cacheEv(now.Add(-age), "10.0.0.1", "steam", "g", 1, 1, 0, 1))
		s.w.addSNI(SNIEvent{Time: now.Add(-age), ClientIP: "10.0.0.1", SNI: "x.example", Service: "riot"})
		s.w.addEviction(EvictionEvent{Time: now.Add(-age), StoreID: "local", ObjectID: "o", Service: "steam", Reason: "size"})
	}
	s.w.addCache(cacheEv(now.Add(-100*day), "10.0.0.9", "steam", "old", 1, 1, 0, 1))
	s.w.addQuery(query(now.Add(-400*day), "10.0.0.1", "a.example", "forwarded"))
	s.w.flush(now)
	if _, err := s.d.W.Exec(`INSERT INTO logs_dns_top_hourly (bucket, kind, key, count) VALUES (?, 'domain', 'x', 1), (?, 'domain', 'y', 1)`,
		hourStart(now.Add(-400*day).UnixMilli()), hourStart(now.Add(-2*day).UnixMilli())); err != nil {
		t.Fatal(err)
	}

	s.w.prune(now)
	for _, tc := range []struct {
		table string
		want  int
	}{
		{"logs_queries", 1},
		{"logs_cache_requests", 1},
		{"logs_sni", 1},
		{"logs_evictions", 1},
		{"logs_downloads", 2}, // 50 h and 1 h ago (gap: two sessions); 100 d is pruned
		{"logs_dns_top_hourly", 1},
	} {
		if n := count(t, s, tc.table); n != tc.want {
			t.Errorf("%s rows = %d, want %d", tc.table, n, tc.want)
		}
	}
	// Minute rollups keep 48 h, hourly ones the stats retention.
	var oldMinutes, oldHours int
	cut := now.Add(-minuteKeep).UnixMilli()
	s.d.R.QueryRow(`SELECT COUNT(*) FROM logs_dns_minute WHERE bucket < ?`, cut).Scan(&oldMinutes)
	s.d.R.QueryRow(`SELECT COUNT(*) FROM logs_dns_hourly WHERE bucket < ?`, cut).Scan(&oldHours)
	if oldMinutes != 0 || oldHours != 1 {
		t.Fatalf("old minute rows = %d, old hourly rows = %d (want 0, 1)", oldMinutes, oldHours)
	}
	var cacheMinutes int
	s.d.R.QueryRow(`SELECT COUNT(*) FROM logs_cache_minute WHERE bucket < ?`, cut).Scan(&cacheMinutes)
	if cacheMinutes != 0 {
		t.Fatalf("old cache minute rows = %d", cacheMinutes)
	}
	if !s.w.nextPrune.After(now.Add(time.Minute)) {
		t.Fatalf("next prune = %v", s.w.nextPrune)
	}
}

func TestSizeCapPrunesOldestRawEvents(t *testing.T) {
	s, _ := newTestStore(t)
	if !s.autoVacuum {
		t.Fatal("a new logs.db must use incremental auto-vacuum")
	}
	now := time.Now()
	answer := strings.Repeat("a", 200)
	const total = 40000
	for i := range total {
		q := query(now.Add(-time.Duration(total-i)*time.Second), "10.0.0.1", fmt.Sprintf("d%d.example", i), "forwarded")
		q.Answer = answer
		s.w.addQuery(q)
		if s.w.rows() >= batchRows {
			s.w.flush(now)
		}
	}
	s.w.flush(now)
	before, _, err := s.refreshSize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	limit := before / 2
	if !s.w.enforceSize(limit, time.Now().Add(time.Minute)) {
		t.Fatal("size enforcement did not finish")
	}
	used, _, _ := s.refreshSize(context.Background())
	if used > limit {
		t.Fatalf("used %d > limit %d", used, limit)
	}
	var n int
	var minName, maxID string
	s.d.R.QueryRow(`SELECT COUNT(*) FROM logs_queries`).Scan(&n)
	s.d.R.QueryRow(`SELECT qname FROM logs_queries ORDER BY ts LIMIT 1`).Scan(&minName)
	s.d.R.QueryRow(`SELECT qname FROM logs_queries ORDER BY ts DESC LIMIT 1`).Scan(&maxID)
	if n == 0 || n >= total || minName == "d0.example" || maxID != fmt.Sprintf("d%d.example", total-1) {
		t.Fatalf("after pruning: %d rows, oldest %s, newest %s", n, minName, maxID)
	}
	// The freed pages are returned to the filesystem.
	sizeBefore := s.Metrics().DBSizeBytes
	s.w.vacuum(0)
	if after := s.Metrics().DBSizeBytes; after >= sizeBefore {
		t.Fatalf("file size %d → %d after vacuum", sizeBefore, after)
	}
}
