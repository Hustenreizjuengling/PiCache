package logs

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
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
	if !s.w.enforceSize(now, limit, time.Now().Add(time.Minute)) {
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
	s.w.vacuum(0, time.Now().Add(time.Minute))
	if after := s.Metrics().DBSizeBytes; after >= sizeBefore {
		t.Fatalf("file size %d → %d after vacuum", sizeBefore, after)
	}
}

// fillTopHours stores n hours of DNS top rows (rows per hour) ending an hour
// before now, as checkpoints of busy hours would.
func fillTopHours(t *testing.T, s *Store, now time.Time, hours, rows int) {
	t.Helper()
	for h := range hours {
		hour := hourStart(now.Add(-time.Duration(h+1) * time.Hour).UnixMilli())
		tx, err := s.d.W.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			if _, err := tx.Exec(`INSERT INTO logs_dns_top_hourly (bucket, kind, key, count) VALUES (?, 'domain', ?, 1)`,
				hour, fmt.Sprintf("host%d.cdn.some-provider-example.com", i)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

// When the hourly top lists alone exceed the cap, the size cap must trim
// them instead of deleting the whole query log and every download session
// on every run.
func TestSizeCapTrimsTopListsInsteadOfWipingRawEvents(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	for i := range 2000 { // the last ~33 minutes
		s.w.addQuery(query(now.Add(-time.Duration(i)*time.Second), "10.0.0.1", fmt.Sprintf("d%d.example", i), "forwarded"))
	}
	for i := range 50 {
		s.w.addCache(cacheEv(now.Add(-time.Duration(i)*time.Minute), "10.0.0.1", "steam", fmt.Sprintf("steam:depot:%d", i), 1, 1, 0, 1))
	}
	s.w.flush(now)
	s.w.sessions.expire(now.Add(time.Hour).UnixMilli())
	fillTopHours(t, s, now, 24*20, 1000) // 20 days of top lists
	topBefore := count(t, s, "logs_dns_top_hourly")
	used, _, err := s.refreshSize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	capBytes := used * 3 / 4 // below what the top lists alone use
	queries, downloads := count(t, s, "logs_queries"), count(t, s, "logs_downloads")

	if !s.w.enforceSize(now, capBytes, time.Now().Add(time.Minute)) {
		t.Fatal("size enforcement did not finish")
	}
	if used, _, _ = s.refreshSize(ctx); used > capBytes {
		t.Fatalf("used %d MiB > cap %d MiB", used>>20, capBytes>>20)
	}
	if n := count(t, s, "logs_queries"); n != queries {
		t.Fatalf("queries of the last hour were deleted: %d of %d left", n, queries)
	}
	if n := count(t, s, "logs_downloads"); n == 0 {
		t.Fatalf("all %d download sessions were deleted", downloads)
	}
	top := count(t, s, "logs_dns_top_hourly")
	if top == 0 || top >= topBefore {
		t.Fatalf("top rows %d → %d", topBefore, top)
	}
	// The oldest hours went first; the newest hour is still there.
	var oldest, newest int64
	s.d.R.QueryRow(`SELECT MIN(bucket), MAX(bucket) FROM logs_dns_top_hourly`).Scan(&oldest, &newest)
	if newest != hourStart(now.Add(-time.Hour).UnixMilli()) || oldest <= hourStart(now.Add(-20*24*time.Hour).UnixMilli()) {
		t.Fatalf("top buckets %v – %v", time.UnixMilli(oldest), time.UnixMilli(newest))
	}
}

// The size cap shortens every kind of data by the same share of its
// retention, oldest entries first.
func TestSizeCapShortensDataProportionally(t *testing.T) {
	s, set := newTestStore(t)
	updateLogs(t, set, func(l *settings.Logs) { l.QueryLogRetentionHours = 10 * 24; l.StatsRetentionDays = 100 })
	ctx := context.Background()
	now := time.Now()
	answer := strings.Repeat("a", 200)
	for i := range 20000 { // 10 days of queries, one every 43 s
		q := query(now.Add(-time.Duration(i)*43*time.Second), "10.0.0.1", fmt.Sprintf("d%d.example", i), "forwarded")
		q.Answer = answer
		s.w.addQuery(q)
		if s.w.rows() >= batchRows {
			s.w.flush(now)
		}
	}
	s.w.flush(now)
	fillTopHours(t, s, now, 100*24, 50) // 100 days of top lists
	used, _, err := s.refreshSize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !s.w.enforceSize(now, used/2, time.Now().Add(time.Minute)) {
		t.Fatal("size enforcement did not finish")
	}
	var oldestQuery, oldestTop int64
	s.d.R.QueryRow(`SELECT MIN(ts) FROM logs_queries`).Scan(&oldestQuery)
	s.d.R.QueryRow(`SELECT MIN(bucket) FROM logs_dns_top_hourly`).Scan(&oldestTop)
	queryShare := float64(now.UnixMilli()-oldestQuery) / float64(10*24*hourMs)
	topShare := float64(now.UnixMilli()-oldestTop) / float64(100*24*hourMs)
	if queryShare > 0.9 || topShare > 0.9 || queryShare-topShare > 0.05 || topShare-queryShare > 0.05 {
		t.Fatalf("kept %.2f of the query retention and %.2f of the stats retention", queryShare, topShare)
	}
}

// Vacuuming after a large prune must not leave logs.db-wal at its
// high-water mark: the relocated pages are released in small steps and the
// WAL is truncated to db.JournalSizeLimit after the next checkpoint.
func TestVacuumDoesNotGrowTheWAL(t *testing.T) {
	s, set := newTestStore(t)
	now := time.Now()
	answer := strings.Repeat("a", 200)
	const total = 150000
	for i := range total {
		q := query(now.Add(-time.Duration(total-i)*2*time.Second), fmt.Sprintf("10.0.%d.%d", i%4, i%50), fmt.Sprintf("d%d.example", i%20000), "forwarded")
		q.Answer = answer
		s.w.addQuery(q)
		if s.w.rows() >= batchRows {
			s.w.flush(now)
		}
	}
	s.w.flush(now)
	if _, err := s.d.W.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	fileSize := func(p string) int64 {
		fi, err := os.Stat(p)
		if err != nil {
			return 0
		}
		return fi.Size()
	}
	before := fileSize(s.d.Path)
	updateLogs(t, set, func(l *settings.Logs) { l.QueryLogRetentionHours = 24 })
	for !s.w.pruneRetention(now, time.Now().Add(time.Minute)) {
	}
	for range 20 { // as many runs as needed, each within its budget
		s.w.vacuum(vacuumMinFree, time.Now().Add(vacuumBudget))
	}
	for i := range 20 { // normal operation afterwards
		s.w.addQuery(query(now.Add(time.Duration(i)*time.Second), "10.0.0.1", "x.example", "forwarded"))
		s.w.flush(now)
	}
	db, wal := fileSize(s.d.Path), fileSize(s.d.Path+"-wal")
	t.Logf("logs.db %d MiB → %d MiB, WAL %d MiB", before>>20, db>>20, wal>>20)
	if wal > db2Limit {
		t.Fatalf("WAL is %d MiB after vacuuming (limit %d MiB)", wal>>20, db2Limit>>20)
	}
	if db+wal >= before {
		t.Fatalf("no space returned: logs.db %d MiB + WAL %d MiB, before %d MiB", db>>20, wal>>20, before>>20)
	}
}

// db2Limit is the WAL size allowed after vacuuming: the journal size limit
// plus the frames of the last transaction.
const db2Limit = db.JournalSizeLimit + 4<<20
