package logs

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"
)

// Daily top tables (docs/ARCHITECTURE.md 11): logs_dns_top_daily and
// logs_cache_top_daily hold one row per UTC day, kind and key (bucket =
// start of the day), the top 1000 keys per kind of the day's hourly rows
// (ordered like the hourly checkpoint), counts and bytes summed, last_seen
// the maximum, the unique sketches merged. Ranges longer than 7 days read
// them for their complete days and the hourly rows plus the in-memory hour
// for the day of their end, so a year of history needs no scan of 8760
// hourly lists. The writer builds a day after the hour rollover into the
// next UTC day and backfills missing days newest first, one per
// maintenance tick.

const (
	dayMs = 24 * hourMs
	// dailyAfter: ranges longer than this read the daily top tables.
	dailyAfter = 7 * 24 * time.Hour
	// backfillChecks bounds the days one maintenance tick checks for
	// daily rows while backfilling (each check is two index lookups).
	backfillChecks = 512
)

func dayStart(ms int64) int64 { return floorTo(ms, dayMs) }

// TopFrom returns the effective start of the top lists, client statistics,
// Summary.ActiveClients and Summary.UniqueDomains of the range [from, to):
// from aligned down to the full hour (the hourly top tables), for ranges
// longer than 7 days to the start of its UTC day (the daily top tables).
func TopFrom(from, to time.Time) time.Time {
	if to.Sub(from) > dailyAfter {
		return time.UnixMilli(dayStart(from.UnixMilli())).UTC()
	}
	return time.UnixMilli(hourStart(from.UnixMilli())).UTC()
}

// topSpan is what a top-table query reads for a range: the daily rows of
// [dayLo, dayHi), the hourly rows of [hourLo, hourHi) and, if mem, the
// in-memory hour. start is the effective start of the range (TopFrom).
type topSpan struct {
	start          int64
	dayLo, dayHi   int64
	hourLo, hourHi int64
	mem            bool
}

// spanFor returns the span of [from, to) given the in-memory hour.
func spanFor(from, to time.Time, memHour int64) topSpan {
	if to.Sub(from) <= dailyAfter {
		lo, hi, mem := topWindow(from, to, memHour)
		return topSpan{start: lo, dayLo: lo, dayHi: lo, hourLo: lo, hourHi: hi, mem: mem}
	}
	lo, last := dayStart(from.UnixMilli()), dayStart(to.UnixMilli())
	hLo, hHi, mem := topWindow(time.UnixMilli(last), to, memHour)
	return topSpan{start: lo, dayLo: lo, dayHi: last, hourLo: hLo, hourHi: hHi, mem: mem}
}

// spanCTE generates the buckets of a span as two tables, days(b) and
// hours(b), for the parameters ?1..?4 (dayLo, dayHi, hourLo, hourHi). A
// query joins them as the outer loop (CROSS JOIN) with a top table, so
// every bucket is one index lookup of (bucket, kind) instead of a scan of
// every kind of the range.
const spanCTE = `WITH RECURSIVE
	days(b) AS (SELECT ?1 WHERE ?1 < ?2 UNION ALL SELECT b + 86400000 FROM days WHERE b + 86400000 < ?2),
	hours(b) AS (SELECT ?3 WHERE ?3 < ?4 UNION ALL SELECT b + 3600000 FROM hours WHERE b + 3600000 < ?4)
`

// args returns the parameters ?1..?4 of spanCTE.
func (sp topSpan) args() []any { return []any{sp.dayLo, sp.dayHi, sp.hourLo, sp.hourHi} }

// restartBackfill starts the backfill of missing daily rows again from the
// day before now (at start, at every day change and after the statistics
// were cleared).
func (w *writer) restartBackfill(now time.Time) {
	w.backfill = dayStart(now.UnixMilli()) - dayMs
	w.backfilling, w.backfillFloor = true, nil
}

// backfillStep builds at most one missing day, newest first, down to the
// day of the oldest hourly top row. Days that have daily rows or no hourly
// rows are skipped.
func (w *writer) backfillStep() {
	if !w.backfilling {
		return
	}
	ctx, cancel := dbContext()
	defer cancel()
	if w.backfillFloor == nil {
		floor, ok, err := w.s.oldestTopDay(ctx)
		if err != nil {
			w.errs.log(w.s.log, "cannot build the daily top lists", err)
			return
		}
		if !ok {
			w.backfilling = false
			return
		}
		w.backfillFloor = &floor
	}
	for range backfillChecks {
		if w.backfill < *w.backfillFloor {
			w.backfilling = false
			return
		}
		day := w.backfill
		need, err := w.s.dayNeedsBuild(ctx, day)
		if err != nil {
			w.errs.log(w.s.log, "cannot build the daily top lists", err)
			return
		}
		w.backfill -= dayMs
		if !need {
			continue
		}
		if err := w.s.buildDay(ctx, day); err != nil {
			w.errs.log(w.s.log, "cannot build the daily top lists", err)
		}
		return
	}
}

// oldestTopDay returns the day of the oldest hourly top row (false when
// there is none).
func (s *Store) oldestTopDay(ctx context.Context) (int64, bool, error) {
	var dns, cache sql.NullInt64
	if err := s.d.W.QueryRowContext(ctx, `SELECT (SELECT MIN(bucket) FROM logs_dns_top_hourly),
		(SELECT MIN(bucket) FROM logs_cache_top_hourly)`).Scan(&dns, &cache); err != nil {
		return 0, false, err
	}
	switch {
	case dns.Valid && cache.Valid:
		return dayStart(min(dns.Int64, cache.Int64)), true, nil
	case dns.Valid:
		return dayStart(dns.Int64), true, nil
	case cache.Valid:
		return dayStart(cache.Int64), true, nil
	}
	return 0, false, nil
}

// dayNeedsBuild reports whether day has hourly top rows but no daily ones.
func (s *Store) dayNeedsBuild(ctx context.Context, day int64) (bool, error) {
	var daily, hourly bool
	err := s.d.W.QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM logs_dns_top_daily WHERE bucket = ?1) OR EXISTS (SELECT 1 FROM logs_cache_top_daily WHERE bucket = ?1),
		EXISTS (SELECT 1 FROM logs_dns_top_hourly WHERE bucket >= ?1 AND bucket < ?2)
			OR EXISTS (SELECT 1 FROM logs_cache_top_hourly WHERE bucket >= ?1 AND bucket < ?2)`, day, day+dayMs).Scan(&daily, &hourly)
	return !daily && hourly, err
}

// buildDay replaces the daily top rows of day (the start of a UTC day) with
// the aggregate of its hourly rows.
func (s *Store) buildDay(ctx context.Context, day int64) error {
	sketch, err := s.daySketch(ctx, day)
	if err != nil {
		return err
	}
	return s.d.Tx(ctx, func(tx *sql.Tx) error {
		for _, table := range []string{"logs_dns_top_daily", "logs_cache_top_daily"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE bucket = ?`, day); err != nil {
				return err
			}
		}
		// The label is the one of the newest row (bare column of MAX()).
		if _, err := tx.ExecContext(ctx, `INSERT INTO logs_dns_top_daily
				(bucket, kind, key, label, count, blocked, duration_us, last_seen)
			SELECT ?1, kind, key, label, c, b, d, t FROM (
				SELECT kind, key, label, SUM(count) AS c, SUM(blocked) AS b, SUM(duration_us) AS d, MAX(last_seen) AS t,
					ROW_NUMBER() OVER (PARTITION BY kind ORDER BY SUM(count) DESC, key) AS n
				FROM logs_dns_top_hourly WHERE bucket >= ?1 AND bucket < ?2 AND kind != ?3
				GROUP BY kind, key
			) WHERE n <= ?4`, day, day+dayMs, uniqueKind, topKeysPerHour); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO logs_cache_top_daily
				(bucket, kind, service, key, label, requests, bytes_sent, bytes_hit, bytes_wan, last_seen)
			SELECT ?1, kind, service, key, label, r, b, h, w, t FROM (
				SELECT kind, service, key, label, SUM(requests) AS r, SUM(bytes_sent) AS b, SUM(bytes_hit) AS h,
					SUM(bytes_wan) AS w, MAX(last_seen) AS t,
					ROW_NUMBER() OVER (PARTITION BY kind ORDER BY SUM(bytes_sent) DESC, SUM(requests) DESC, service, key) AS n
				FROM logs_cache_top_hourly WHERE bucket >= ?1 AND bucket < ?2
				GROUP BY kind, service, key
			) WHERE n <= ?3`, day, day+dayMs, topKeysPerHour); err != nil {
			return err
		}
		if sketch.empty() {
			return nil
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO logs_dns_top_daily (bucket, kind, key, label, count) VALUES (?, ?, '', ?, ?)`,
			day, uniqueKind, sketch.encode(), sketch.estimate())
		return err
	})
}

// daySketch merges the hourly unique sketches of day.
func (s *Store) daySketch(ctx context.Context, day int64) (*hll, error) {
	rows, err := s.d.W.QueryContext(ctx, spanCTE+`SELECT t.label FROM hours CROSS JOIN logs_dns_top_hourly t
		WHERE t.bucket = hours.b AND t.kind = ?5 AND t.key = ''`, day, day, day, day+dayMs, uniqueKind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out hll
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, err
		}
		if h, ok := decodeHLL(label); ok {
			out.merge(h)
		}
	}
	return &out, rows.Err()
}

// dayChanged builds the day that the hour rollover finished (prev: the
// previous top hour) and restarts the backfill.
func (w *writer) dayChanged(now time.Time, prev, hour int64) {
	if prev == 0 || dayStart(prev) >= dayStart(hour) {
		return
	}
	ctx, cancel := dbContext()
	defer cancel()
	if err := w.s.buildDay(ctx, dayStart(prev)); err != nil && !errors.Is(err, context.Canceled) {
		w.errs.log(w.s.log, "cannot build the daily top lists", err)
	} else if err == nil {
		w.s.log.Debug("built the daily top lists", slog.Time("day", time.UnixMilli(dayStart(prev)).UTC()))
	}
	w.restartBackfill(now)
}
