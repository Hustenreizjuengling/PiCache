package logs

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
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

// fakeClock replaces the writer's clock watch with a wall clock and a
// monotonic clock the test moves (step: both advance; jump: the wall
// clock alone).
type fakeClock struct {
	wall time.Time
	mono time.Duration
}

func (c *fakeClock) install(w *writer) {
	w.clk = clockWatch{read: func() (time.Time, time.Duration) { return c.wall, c.mono }, wall: c.wall, mono: c.mono}
}
func (c *fakeClock) step(d time.Duration) { c.wall, c.mono = c.wall.Add(d), c.mono+d }
func (c *fakeClock) jump(d time.Duration) { c.wall = c.wall.Add(d) }

// REV-1: a host clock that is merely not synchronised (a host without an
// NTP client, Docker Desktop) no longer holds the retention: rows older
// than it go at the first prune, also right after a start.
func TestRetentionNotHeldByUnsyncedClock(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Now()
	s.w.addQuery(query(now.Add(-200*time.Hour), "10.0.0.1", "old.example", "forwarded")) // retention 168 h
	s.w.addQuery(query(now.Add(-time.Hour), "10.0.0.1", "new.example", "forwarded"))
	s.w.flush(now)
	s.SetClockReader(func() (bool, bool) { return false, true })
	s.w.checkLastWritten(lastWrittenRaw(context.Background(), s.d)) // as at a start
	s.w.prune(now)
	if n := count(t, s, "logs_queries"); n != 1 || s.w.clk.held {
		t.Fatalf("an unsynchronised clock held the retention: %d rows, held %v", n, s.w.clk.held)
	}
}

// A jump of the wall clock against the monotonic clock holds the
// retention (a clock set years ahead deleted the whole query log and the
// statistics), ahead or back, until a day passed after the latest jump or
// the host reports the clock synchronised; a jump while it is synchronised
// holds nothing. Small corrections are no jump.
func TestRetentionHeldAfterClockJump(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Now()
	s.w.addQuery(query(now.Add(-time.Hour), "10.0.0.1", "a.example", "forwarded"))
	s.w.flush(now)
	var synced atomic.Bool
	s.SetClockReader(func() (bool, bool) { return synced.Load(), true })
	c := &fakeClock{wall: now.Round(0)}
	c.install(s.w)
	year := 365 * 24 * time.Hour

	c.step(5 * time.Minute)
	c.jump(-30 * time.Second) // a correction
	s.w.prune(c.wall)
	if s.w.clk.held {
		t.Fatal("a correction of 30 s held the retention")
	}
	c.step(5 * time.Minute)
	c.jump(2 * year)
	s.w.prune(c.wall)
	if n := count(t, s, "logs_queries"); n != 1 || !s.w.clk.held {
		t.Fatalf("after a jump ahead: %d rows, held %v", n, s.w.clk.held)
	}
	c.step(maxRetentionHold - time.Minute)
	s.w.prune(c.wall)
	if n := count(t, s, "logs_queries"); n != 1 {
		t.Fatalf("within the day after the jump: %d rows", n)
	}
	c.step(2 * time.Minute)
	s.w.prune(c.wall)
	if n := count(t, s, "logs_queries"); n != 0 || s.w.clk.held {
		t.Fatalf("a day after the jump: %d rows, held %v", n, s.w.clk.held)
	}

	// Back to the real time: held again, until the clock is synchronised.
	s.w.addQuery(query(now.Add(-200*time.Hour), "10.0.0.1", "b.example", "forwarded"))
	s.w.flush(now)
	c.step(5 * time.Minute)
	c.jump(-2*year - maxRetentionHold)
	s.w.prune(c.wall)
	if n := count(t, s, "logs_queries"); n != 1 || !s.w.clk.held {
		t.Fatalf("after a jump back: %d rows, held %v", n, s.w.clk.held)
	}
	synced.Store(true)
	c.step(5 * time.Minute)
	s.w.prune(c.wall)
	if n := count(t, s, "logs_queries"); n != 0 || s.w.clk.held {
		t.Fatalf("synchronised: %d rows, held %v", n, s.w.clk.held)
	}

	// A jump of a synchronised clock (NTP stepping it) holds nothing.
	s.w.addQuery(query(now.Add(-200*time.Hour), "10.0.0.1", "c.example", "forwarded"))
	s.w.flush(now)
	c.step(5 * time.Minute)
	c.jump(3 * time.Hour)
	s.w.prune(c.wall)
	if n := count(t, s, "logs_queries"); n != 0 || s.w.clk.held {
		t.Fatalf("a synchronised jump: %d rows, held %v", n, s.w.clk.held)
	}
}

// A start whose clock is more than an hour before the raw log row written
// last (the clock was ahead then, or went back since) holds the retention
// like a jump. Once this run has written rows, the next start holds
// nothing, although the rows written ahead are still stored.
func TestRetentionHeldForRowsWrittenAfterNow(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	now := time.Now()
	s.w.addQuery(query(now.Add(-200*time.Hour), "10.0.0.1", "old.example", "forwarded"))
	s.w.flush(now)
	// Events from the future are stored as now, so the row written while the
	// clock was two years ahead is inserted directly.
	ahead := now.Add(2 * 365 * 24 * time.Hour).UnixMilli()
	if _, err := s.d.W.Exec(`INSERT INTO logs_queries (ts, client_ip, qname, qtype, status) VALUES (?, '10.0.0.1', 'ahead.example', 'A', 'forwarded')`,
		ahead); err != nil {
		t.Fatal(err)
	}
	s.SetClockReader(func() (bool, bool) { return false, true })
	s.w.startClockWatch()
	s.w.checkLastWritten(lastWrittenRaw(ctx, s.d))
	s.w.prune(now)
	if n := count(t, s, "logs_queries"); n != 2 || !s.w.clk.held {
		t.Fatalf("a start before the last written row: %d rows, held %v", n, s.w.clk.held)
	}
	s.w.addQuery(query(now, "10.0.0.1", "now.example", "forwarded"))
	s.w.flush(now)
	s.w.startClockWatch() // the next start
	s.w.checkLastWritten(lastWrittenRaw(ctx, s.d))
	s.w.prune(now)
	var old int
	_ = s.d.R.QueryRow(`SELECT COUNT(*) FROM logs_queries WHERE qname = 'old.example'`).Scan(&old)
	if n := count(t, s, "logs_queries"); n != 2 || old != 0 || s.w.clk.held {
		t.Fatalf("the next start: %d rows (old %d), held %v", n, old, s.w.clk.held)
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
	if db+wal < before {
		return
	}
	if runtime.GOOS != "windows" {
		t.Fatalf("no space returned: logs.db %d MiB + WAL %d MiB, before %d MiB", db>>20, wal>>20, before>>20)
	}
	// Some Windows hosts (seen on CI runners) keep SQLite from truncating
	// logs.db at the checkpoint; PiCache runs on Linux, where the file size
	// is checked above. Here the database must at least have shrunk.
	var pages, pageSize int64
	if err := s.d.W.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := s.d.W.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if size := pages * pageSize; size >= before {
		t.Fatalf("no space returned: the database has %d MiB, before %d MiB", size>>20, before>>20)
	}
	t.Logf("this host did not truncate logs.db; the database itself shrank to %d MiB", (pages*pageSize)>>20)
}

// db2Limit is the WAL size allowed after vacuuming: the journal size limit
// plus the frames of the last transaction.
const db2Limit = db.JournalSizeLimit + 4<<20
