package logs

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

const (
	pruneInterval  = 5 * time.Minute
	pruneBudget    = 2 * time.Second // work per run; unfinished work continues after the next flush
	pruneChunkRows = 5000            // rows per DELETE statement
	sizeChunkRows  = 10000           // oldest raw rows deleted per step when logs.db is too large
	minuteKeep     = 48 * time.Hour  // minute rollups
	sizeTarget     = 0.95            // prune to this fraction of the cap (hysteresis)
	vacuumMinFree  = 16 << 20        // free space kept for reuse instead of shrinking the file
)

// rawTables hold raw events, pruned oldest first when logs.db is too large.
var rawTables = []string{"logs_queries", "logs_cache_requests", "logs_sni", "logs_evictions"}

// retention describes how one table is pruned.
type retention struct {
	table  string
	col    string        // time column (unix ms)
	keep   time.Duration // rows with col older than now-keep are deleted
	rowid  bool          // rowid table: delete in row chunks; else by time spans
	window int64         // span (ms) deleted per statement for WITHOUT ROWID tables
}

func (w *writer) retentions() []retention {
	c := w.s.cfg()
	queries := time.Duration(c.QueryLogRetentionHours) * time.Hour
	cache := time.Duration(c.CacheLogRetentionHours) * time.Hour
	sessions := time.Duration(c.SessionRetentionDays) * 24 * time.Hour
	stats := time.Duration(c.StatsRetentionDays) * 24 * time.Hour
	return []retention{
		{table: "logs_queries", col: "ts", keep: queries, rowid: true},
		{table: "logs_cache_requests", col: "ts", keep: cache, rowid: true},
		{table: "logs_sni", col: "ts", keep: cache, rowid: true},
		{table: "logs_evictions", col: "ts", keep: cache, rowid: true},
		{table: "logs_downloads", col: "last_seen", keep: sessions, rowid: true},
		{table: "logs_dns_minute", col: "bucket", keep: minuteKeep, rowid: true},
		{table: "logs_dns_hourly", col: "bucket", keep: stats, rowid: true},
		{table: "logs_cache_minute", col: "bucket", keep: minuteKeep, window: 6 * hourMs},
		{table: "logs_cache_hourly", col: "bucket", keep: stats, window: 7 * 24 * hourMs},
		{table: "logs_dns_top_hourly", col: "bucket", keep: stats, window: 6 * hourMs},
		{table: "logs_cache_top_hourly", col: "bucket", keep: stats, window: 6 * hourMs},
	}
}

// prune applies retention and the size cap within pruneBudget. If the work
// is not finished, the next run starts after the next flush.
func (w *writer) prune(now time.Time) {
	deadline := time.Now().Add(pruneBudget)
	done := w.pruneRetention(now, deadline)
	if done {
		done = w.enforceSize(int64(w.s.cfg().MaxDBSizeMiB)<<20, deadline)
	}
	w.vacuum(vacuumMinFree)
	if done {
		w.nextPrune = now.Add(pruneInterval)
	} else {
		w.nextPrune = now.Add(flushInterval)
	}
}

// pruneRetention deletes expired rows; false if the budget ran out first.
func (w *writer) pruneRetention(now time.Time, deadline time.Time) bool {
	ctx, cancel := dbContext()
	defer cancel()
	for _, r := range w.retentions() {
		cutoff := now.Add(-r.keep).UnixMilli()
		for {
			if time.Now().After(deadline) {
				return false
			}
			n, err := w.deleteBefore(ctx, r, cutoff)
			if err != nil {
				w.errs.log(w.s.log, "cannot prune old log data", err)
				break
			}
			if n == 0 {
				break
			}
		}
	}
	return true
}

// deleteBefore deletes one chunk of rows of r older than cutoff and reports
// how many rows it deleted.
func (w *writer) deleteBefore(ctx context.Context, r retention, cutoff int64) (int64, error) {
	if r.rowid {
		res, err := w.s.d.W.ExecContext(ctx, `DELETE FROM `+r.table+` WHERE rowid IN
			(SELECT rowid FROM `+r.table+` WHERE `+r.col+` < ? ORDER BY `+r.col+` LIMIT ?)`, cutoff, pruneChunkRows)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	var oldest *int64
	if err := w.s.d.W.QueryRowContext(ctx, `SELECT MIN(`+r.col+`) FROM `+r.table).Scan(&oldest); err != nil {
		return 0, err
	}
	if oldest == nil || *oldest >= cutoff {
		return 0, nil
	}
	res, err := w.s.d.W.ExecContext(ctx, `DELETE FROM `+r.table+` WHERE `+r.col+` < ?`, min(cutoff, *oldest+r.window))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return max(n, 1), err // progress was made even if the span held no rows
}

// enforceSize keeps the used pages of logs.db below capBytes by deleting the
// oldest raw events (then the oldest download sessions). False if the budget
// ran out first.
func (w *writer) enforceSize(capBytes int64, deadline time.Time) bool {
	ctx, cancel := dbContext()
	defer cancel()
	used, _, err := w.s.refreshSize(ctx)
	if err != nil {
		w.errs.log(w.s.log, "cannot read the log database size", err)
		return true
	}
	if used <= capBytes {
		return true
	}
	target := int64(float64(capBytes) * sizeTarget)
	for used > target {
		if time.Now().After(deadline) {
			return false
		}
		table, col := w.oldestRaw(ctx)
		if table == "" {
			w.errs.log(w.s.log, "the log database exceeds its size limit although no raw events are left",
				fmt.Errorf("logs.db uses %d MiB of %d MiB", used>>20, capBytes>>20))
			return true
		}
		if _, err := w.s.d.W.ExecContext(ctx, `DELETE FROM `+table+` WHERE rowid IN
			(SELECT rowid FROM `+table+` ORDER BY `+col+` LIMIT ?)`, sizeChunkRows); err != nil {
			w.errs.log(w.s.log, "cannot prune the log database to its size limit", err)
			return true
		}
		if used, _, err = w.s.refreshSize(ctx); err != nil {
			w.errs.log(w.s.log, "cannot read the log database size", err)
			return true
		}
	}
	w.s.log.Info("pruned the oldest log events to keep logs.db below its size limit",
		slog.Int64("limitMiB", capBytes>>20))
	return true
}

// oldestRaw returns the raw table holding the oldest event, or the download
// sessions once no raw events are left ("" if everything is empty).
func (w *writer) oldestRaw(ctx context.Context) (table, col string) {
	var best *int64
	for _, t := range rawTables {
		var ts *int64
		if err := w.s.d.W.QueryRowContext(ctx, `SELECT MIN(ts) FROM `+t).Scan(&ts); err != nil || ts == nil {
			continue
		}
		if best == nil || *ts < *best {
			best, table = ts, t
		}
	}
	if table != "" {
		return table, "ts"
	}
	var n int
	if err := w.s.d.W.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM logs_downloads LIMIT 1)`).Scan(&n); err == nil && n > 0 {
		return "logs_downloads", "last_seen"
	}
	return "", ""
}

// refreshSize reads the used and total size of logs.db and records the
// total for Metrics.
func (s *Store) refreshSize(ctx context.Context) (used, total int64, err error) {
	var pages, free, pageSize int64
	for _, p := range []struct {
		pragma string
		dst    *int64
	}{{"page_count", &pages}, {"freelist_count", &free}, {"page_size", &pageSize}} {
		if err := s.d.W.QueryRowContext(ctx, `PRAGMA `+p.pragma).Scan(p.dst); err != nil {
			return 0, 0, err
		}
	}
	total = pages * pageSize
	s.dbSize.Store(total)
	return (pages - free) * pageSize, total, nil
}

// vacuum returns large amounts of free pages to the filesystem (incremental
// auto-vacuum; small free lists are kept for reuse).
func (w *writer) vacuum(minFree int64) {
	if !w.s.autoVacuum {
		return
	}
	ctx, cancel := dbContext()
	defer cancel()
	used, total, err := w.s.refreshSize(ctx)
	if err != nil {
		return
	}
	free := total - used
	if free < minFree || free < total/4 {
		return
	}
	var pageSize int64
	if err := w.s.d.W.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil || pageSize <= 0 {
		return
	}
	pages := min(free/pageSize, (256<<20)/pageSize) // at most 256 MiB per run
	if _, err := w.s.d.W.ExecContext(ctx, `PRAGMA incremental_vacuum(`+strconv.FormatInt(pages, 10)+`)`); err != nil {
		w.errs.log(w.s.log, "cannot release free space of the log database", err)
		return
	}
	_, _, _ = w.s.refreshSize(ctx)
}
