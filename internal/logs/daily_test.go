package logs

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// insertHourly stores a DNS top row of an hour (bucket unix ms).
func insertHourly(t *testing.T, s *Store, bucket int64, kind, key string, count, blocked int64) {
	t.Helper()
	if _, err := s.d.W.Exec(`INSERT INTO logs_dns_top_hourly (bucket, kind, key, label, count, blocked, duration_us, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, bucket, kind, key, "l"+fmt.Sprint(bucket), count, blocked, count*10, bucket+1); err != nil {
		t.Fatal(err)
	}
}

// sketchOf returns the encoded sketch of names.
func sketchOf(names ...string) string {
	var h hll
	for _, n := range names {
		h.add(hashName(n))
	}
	return h.encode()
}

// A day's daily rows: the top 1000 keys per kind of its hourly rows, counts
// summed, last_seen the maximum, the label of the newest row, cache rows
// likewise and the sketches merged.
func TestBuildDay(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	day := dayStart(time.Now().UnixMilli()) - 3*dayMs
	h1, h2 := day+2*hourMs, day+23*hourMs
	for i := range 1200 { // 1200 keys, the first 1000 most counted
		insertHourly(t, s, h1, "domain", fmt.Sprintf("d%04d.example", i), int64(2000-i), 0)
	}
	insertHourly(t, s, h2, "domain", "d0000.example", 5, 0)
	insertHourly(t, s, h1, "client", "10.0.0.1", 10, 3)
	insertHourly(t, s, h2, "client", "10.0.0.1", 7, 1)
	insertHourly(t, s, day+dayMs, "client", "10.0.0.1", 99, 99)  // next day: not included
	insertHourly(t, s, day-hourMs, "client", "10.0.0.1", 99, 99) // day before: not included
	if _, err := s.d.W.Exec(`INSERT INTO logs_dns_top_hourly (bucket, kind, key, label, count) VALUES (?, 'unique', '', ?, 2), (?, 'unique', '', ?, 2)`,
		h1, sketchOf("a.example", "b.example"), h2, sketchOf("b.example", "c.example")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.d.W.Exec(`INSERT INTO logs_cache_top_hourly (bucket, kind, service, key, label, requests, bytes_sent, bytes_hit, bytes_wan, last_seen)
		VALUES (?, 'content', 'steam', 'g1', 'old', 1, 100, 50, 50, ?), (?, 'content', 'steam', 'g1', 'new', 2, 300, 0, 300, ?)`,
		h1, h1, h2, h2); err != nil {
		t.Fatal(err)
	}
	if err := s.buildDay(ctx, day); err != nil {
		t.Fatal(err)
	}
	var n int
	s.d.R.QueryRow(`SELECT COUNT(*) FROM logs_dns_top_daily WHERE bucket = ? AND kind = 'domain'`, day).Scan(&n)
	if n != topKeysPerHour {
		t.Fatalf("daily domain rows %d", n)
	}
	var cnt, blocked, dur, last int64
	var label string
	if err := s.d.R.QueryRow(`SELECT count, blocked, duration_us, last_seen, label FROM logs_dns_top_daily
		WHERE bucket = ? AND kind = 'client' AND key = '10.0.0.1'`, day).Scan(&cnt, &blocked, &dur, &last, &label); err != nil {
		t.Fatal(err)
	}
	if cnt != 17 || blocked != 4 || dur != 170 || last != h2+1 || label != "l"+fmt.Sprint(h2) {
		t.Fatalf("client row %d %d %d %d %q", cnt, blocked, dur, last, label)
	}
	if err := s.d.R.QueryRow(`SELECT count FROM logs_dns_top_daily WHERE bucket = ? AND kind = 'domain' AND key = 'd0000.example'`,
		day).Scan(&cnt); err != nil || cnt != 2005 {
		t.Fatalf("domain sum %d (%v)", cnt, err)
	}
	if err := s.d.R.QueryRow(`SELECT count FROM logs_dns_top_daily WHERE bucket = ? AND kind = 'domain' AND key = 'd1100.example'`,
		day).Scan(&cnt); err == nil {
		t.Fatal("a key beyond the top 1000 was kept")
	}
	var sketch string
	if err := s.d.R.QueryRow(`SELECT label, count FROM logs_dns_top_daily WHERE bucket = ? AND kind = 'unique' AND key = ''`,
		day).Scan(&sketch, &cnt); err != nil || cnt != 3 {
		t.Fatalf("daily sketch count %d (%v)", cnt, err)
	}
	if sketch != sketchOf("a.example", "b.example", "c.example") {
		t.Fatal("daily sketch is not the union")
	}
	var req, sent int64
	if err := s.d.R.QueryRow(`SELECT label, requests, bytes_sent FROM logs_cache_top_daily WHERE bucket = ? AND kind = 'content'`,
		day).Scan(&label, &req, &sent); err != nil || label != "new" || req != 3 || sent != 400 {
		t.Fatalf("cache row %q %d %d (%v)", label, req, sent, err)
	}
	// Building again replaces the day.
	if err := s.buildDay(ctx, day); err != nil {
		t.Fatal(err)
	}
	s.d.R.QueryRow(`SELECT COUNT(*) FROM logs_dns_top_daily WHERE bucket = ?`, day).Scan(&n)
	if n != topKeysPerHour+2 {
		t.Fatalf("daily rows after a rebuild %d", n)
	}
}

// The backfill builds missing days newest first, one per tick, and skips
// days with daily rows and days without hourly rows.
func TestBackfillNewestFirst(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Now()
	today := dayStart(now.UnixMilli())
	for _, d := range []int64{1, 2, 3, 6} {
		insertHourly(t, s, today-d*dayMs+hourMs, "domain", "x.example", d, 0)
	}
	// Day 2 was built already (kept as it is).
	if _, err := s.d.W.Exec(`INSERT INTO logs_dns_top_daily (bucket, kind, key, count) VALUES (?, 'domain', 'marker', 42)`, today-2*dayMs); err != nil {
		t.Fatal(err)
	}
	built := func() []int64 {
		t.Helper()
		var out []int64
		rows, err := s.d.R.Query(`SELECT DISTINCT bucket FROM logs_dns_top_daily ORDER BY bucket DESC`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var b int64
			rows.Scan(&b)
			out = append(out, (today-b)/dayMs)
		}
		return out
	}
	s.w.restartBackfill(now)
	for i, want := range []string{"[1 2]", "[1 2 3]", "[1 2 3 6]", "[1 2 3 6]"} {
		s.w.backfillStep()
		if got := fmt.Sprint(built()); got != want {
			t.Fatalf("after step %d: days %s, want %s", i+1, got, want)
		}
	}
	if s.w.backfilling {
		t.Fatal("backfill did not finish")
	}
	var marker int64
	if err := s.d.R.QueryRow(`SELECT count FROM logs_dns_top_daily WHERE key = 'marker'`).Scan(&marker); err != nil || marker != 42 {
		t.Fatalf("an existing day was rebuilt (%v)", err)
	}
}

// The rollover into the next UTC day builds the finished day right after
// the checkpoint of its last hour.
func TestRolloverBuildsDay(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Now()
	last := dayStart(now.UnixMilli()) + dayMs - hourMs // 23:00 UTC today
	dns, cache := emptyTopMaps()
	s.top.replace(last, dns, cache, nil)
	at := time.UnixMilli(last + 30*minuteMs)
	s.w.addQuery(query(at, "10.0.0.1", "late.example", "forwarded"))
	s.w.rollover(at.Add(time.Hour), last+hourMs)
	var n int
	s.d.R.QueryRow(`SELECT COUNT(*) FROM logs_dns_top_daily WHERE bucket = ? AND key = 'late.example'`, dayStart(last)).Scan(&n)
	if n != 1 {
		t.Fatal("the finished day was not built")
	}
	if !s.w.backfilling {
		t.Fatal("the backfill is not restarted at a day change")
	}
}

// Ranges longer than 7 days read the daily rows of their complete days and
// the hourly rows of their last day; their top lists start at the UTC day.
func TestLongRangesReadDailyRows(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	today := dayStart(now.UnixMilli())
	for d := int64(1); d <= 20; d++ {
		if _, err := s.d.W.Exec(`INSERT INTO logs_dns_top_daily (bucket, kind, key, count, blocked, last_seen) VALUES
			(?, 'domain', 'daily.example', 10, 0, ?), (?, 'client', '10.0.0.1', 10, 2, ?), (?, 'purpose', 'advertising', 1, 0, ?)`,
			today-d*dayMs, today-d*dayMs, today-d*dayMs, today-d*dayMs, today-d*dayMs, today-d*dayMs); err != nil {
			t.Fatal(err)
		}
		// Hourly rows of the same days: never read for ranges > 7 days.
		insertHourly(t, s, today-d*dayMs+hourMs, "domain", "hourly.example", 1000, 0)
	}
	if _, err := s.d.W.Exec(`INSERT INTO logs_dns_top_daily (bucket, kind, key, label, count) VALUES (?, 'unique', '', ?, 2), (?, 'unique', '', ?, 2)`,
		today-dayMs, sketchOf("a", "b"), today-2*dayMs, sketchOf("b", "c")); err != nil {
		t.Fatal(err)
	}
	s.w.addQuery(query(now, "10.0.0.1", "daily.example", "forwarded")) // the in-memory hour
	from := now.Add(-10 * 24 * time.Hour)
	items, err := s.Top(ctx, TopDomains, from, now, 5)
	// The days d = 10 (the day of from) … 1 and the in-memory hour.
	if err != nil || len(items) != 1 || items[0].Key != "daily.example" || items[0].Count != 101 {
		t.Fatalf("top %+v, %v", items, err)
	}
	sum, err := s.Summary(ctx, from, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.UnixMilli(dayStart(from.UnixMilli())).UTC(); !sum.TopFrom.Equal(want) {
		t.Fatalf("topFrom %v, want %v", sum.TopFrom, want)
	}
	if sum.ActiveClients != 1 || sum.UniqueDomains != 4 { // a, b, c and daily.example of the in-memory hour
		t.Fatalf("summary %+v", sum)
	}
	st, err := s.ClientStats(ctx, from, now)
	if err != nil || len(st) != 1 || st[0].Queries != 101 || st[0].Blocked != 20 {
		t.Fatalf("client stats %+v, %v", st, err)
	}
	p, err := s.Purposes(ctx, from, now)
	if err != nil || len(p.Purposes) != 1 || p.Purposes[0].Count != 10 || !p.From.Equal(sum.TopFrom) {
		t.Fatalf("purposes %+v, %v", p, err)
	}
	// A 7-day range reads the hourly rows.
	items, err = s.Top(ctx, TopDomains, now.Add(-7*24*time.Hour), now, 5)
	if err != nil || len(items) != 2 || items[0].Key != "hourly.example" {
		t.Fatalf("7-day top %+v, %v", items, err)
	}
	if got := TopFrom(now.Add(-7*24*time.Hour), now); !got.Equal(time.UnixMilli(hourStart(now.Add(-7 * 24 * time.Hour).UnixMilli())).UTC()) {
		t.Fatalf("7-day topFrom %v", got)
	}
}

// A year of daily rows with 1000 keys per kind answers the top list, the
// client statistics and the purposes within the query timeout.
func TestYearOfDailyRowsIsFast(t *testing.T) {
	if testing.Short() {
		t.Skip("fills a year of daily top lists")
	}
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	today := dayStart(now.UnixMilli())
	start := time.Now()
	if _, err := s.d.W.Exec(`WITH RECURSIVE d(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM d WHERE n < 365),
			k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i < 999),
			kinds(kind) AS (VALUES ('domain'), ('blocked'), ('client'), ('purpose'))
		INSERT INTO logs_dns_top_daily (bucket, kind, key, label, count, blocked, duration_us, last_seen)
		SELECT ?1 - d.n * 86400000, kinds.kind, kinds.kind || '-' || k.i, '', 1000 - k.i, k.i % 7, 0, ?1 - d.n * 86400000
		FROM d, kinds, k`, today); err != nil {
		t.Fatal(err)
	}
	if _, err := s.d.W.Exec(`WITH RECURSIVE d(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM d WHERE n < 365),
			k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i < 999)
		INSERT INTO logs_cache_top_daily (bucket, kind, service, key, label, requests, bytes_sent, bytes_hit, bytes_wan, last_seen)
		SELECT ?1 - d.n * 86400000, 'client', '', 'client-' || k.i, '', 1, 1000 - k.i, 0, 0, ?1 - d.n * 86400000 FROM d, k`, today); err != nil {
		t.Fatal(err)
	}
	t.Logf("filled in %v", time.Since(start))
	from := now.Add(-365 * 24 * time.Hour)
	for _, tc := range []struct {
		name string
		run  func() (int, int64, error)
	}{
		{"top", func() (int, int64, error) {
			items, err := s.Top(ctx, TopDomains, from, now, 10)
			if len(items) == 0 {
				return 0, 0, err
			}
			return len(items), items[0].Count, err
		}},
		{"client stats", func() (int, int64, error) {
			st, err := s.ClientStats(ctx, from, now)
			if len(st) == 0 {
				return 0, 0, err
			}
			return len(st), st[0].Queries, err
		}},
		{"purposes", func() (int, int64, error) {
			p, err := s.Purposes(ctx, from, now)
			if len(p.Purposes) == 0 {
				return 0, 0, err
			}
			return len(p.Purposes), p.Purposes[0].Count, err
		}},
	} {
		t0 := time.Now()
		n, first, err := tc.run()
		d := time.Since(t0)
		t.Logf("%s: %d rows in %v", tc.name, n, d)
		if err != nil || n == 0 || first != 365*1000 || d > queryTimeout {
			t.Fatalf("%s: %d rows, first %d, %v, %v", tc.name, n, first, d, err)
		}
	}
}

// Daily rows are pruned after the statistics retention and trimmed with
// the hourly top lists when logs.db exceeds its size limit.
func TestDailyRowsPrunedAndTrimmed(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Now()
	today := dayStart(now.UnixMilli())
	for _, d := range []int64{400, 2} {
		if _, err := s.d.W.Exec(`INSERT INTO logs_dns_top_daily (bucket, kind, key, count) VALUES (?, 'domain', 'x', 1)`, today-d*dayMs); err != nil {
			t.Fatal(err)
		}
		if _, err := s.d.W.Exec(`INSERT INTO logs_cache_top_daily (bucket, kind, key, bytes_sent) VALUES (?, 'client', 'x', 1)`, today-d*dayMs); err != nil {
			t.Fatal(err)
		}
	}
	s.w.prune(now)
	if n, c := count(t, s, "logs_dns_top_daily"), count(t, s, "logs_cache_top_daily"); n != 1 || c != 1 {
		t.Fatalf("daily rows after pruning %d, %d", n, c)
	}
	sized := map[string]bool{}
	for _, r := range s.w.retentions() {
		sized[r.table] = r.size
	}
	for _, table := range []string{"logs_dns_top_hourly", "logs_cache_top_hourly", "logs_dns_top_daily", "logs_cache_top_daily"} {
		if !sized[table] {
			t.Errorf("%s is not trimmed by the size cap", table)
		}
	}
	if _, ok := sized["logs_events"]; ok {
		t.Error("the warning history must never be size-trimmed")
	}
	// A size trim removes the oldest daily day.
	c := &sizeCandidate{retention: retention{table: "logs_dns_top_daily", col: "bucket", keep: 365 * 24 * time.Hour}}
	if _, err := s.d.W.Exec(`INSERT INTO logs_dns_top_daily (bucket, kind, key, count) VALUES (?, 'domain', 'y', 1)`, today-300*dayMs); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	floor := hourStart(now.Add(-sizeFloor).UnixMilli())
	if err := s.w.loadOldest(ctx, c, floor); err != nil {
		t.Fatal(err)
	}
	if err := s.w.trimOldest(ctx, c, floor); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "logs_dns_top_daily"); n != 1 {
		t.Fatalf("daily rows after a size trim %d", n)
	}
}
