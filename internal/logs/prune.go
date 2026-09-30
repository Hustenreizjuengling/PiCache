package logs

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
)

const (
	pruneInterval  = 5 * time.Minute
	pruneBudget    = 2 * time.Second // work per run; unfinished work continues after the next flush
	pruneChunkRows = 5000            // rows per DELETE statement
	minuteKeep     = 48 * time.Hour  // minute rollups
	sizeTarget     = 0.95            // prune to this fraction of the cap (hysteresis)
	sizeChunkRows  = 10000           // rows of a rowid table deleted per step when logs.db is too large
	sizeStepShare  = 200             // other tables: a step deletes 1/sizeStepShare of the retention (at least an hour)
	sizeFloor      = time.Hour       // the size cap never removes data of the last full hour
	vacuumMinFree  = 16 << 20        // free space kept for reuse instead of shrinking the file
	vacuumStep     = 16 << 20        // released per incremental_vacuum statement (bounds the WAL growth)
	vacuumBudget   = time.Second     // vacuum work per run
)

// retention describes how one table is pruned.
type retention struct {
	table  string
	col    string        // time column (unix ms)
	keep   time.Duration // rows with col older than now-keep are deleted
	rowid  bool          // rowid table: delete in row chunks; else by time spans
	window int64         // span (ms) deleted per statement for WITHOUT ROWID tables
	size   bool          // shortened when logs.db exceeds its size cap
}

// retentions lists every table with its retention. The size cap shortens
// the raw events, the download sessions and the hourly and daily top
// lists; the count rollups are small, bounded by their retention and never
// pruned for size.
func (w *writer) retentions() []retention {
	c := w.s.cfg()
	queries := time.Duration(c.QueryLogRetentionHours) * time.Hour
	cache := time.Duration(c.CacheLogRetentionHours) * time.Hour
	sessions := time.Duration(c.SessionRetentionDays) * 24 * time.Hour
	stats := time.Duration(c.StatsRetentionDays) * 24 * time.Hour
	return []retention{
		{table: "logs_queries", col: "ts", keep: queries, rowid: true, size: true},
		{table: "logs_cache_requests", col: "ts", keep: cache, rowid: true, size: true},
		{table: "logs_sni", col: "ts", keep: cache, rowid: true, size: true},
		{table: "logs_evictions", col: "ts", keep: cache, rowid: true, size: true},
		{table: "logs_downloads", col: "last_seen", keep: sessions, rowid: true, size: true},
		{table: "logs_dns_minute", col: "bucket", keep: minuteKeep, rowid: true},
		{table: "logs_dns_hourly", col: "bucket", keep: stats, rowid: true},
		{table: "logs_cache_minute", col: "bucket", keep: minuteKeep, window: 6 * hourMs},
		{table: "logs_cache_hourly", col: "bucket", keep: stats, window: 7 * 24 * hourMs},
		{table: "logs_dns_top_hourly", col: "bucket", keep: stats, window: 6 * hourMs, size: true},
		{table: "logs_cache_top_hourly", col: "bucket", keep: stats, window: 6 * hourMs, size: true},
		{table: "logs_dns_top_daily", col: "bucket", keep: stats, window: 7 * dayMs, size: true},
		{table: "logs_cache_top_daily", col: "bucket", keep: stats, window: 7 * dayMs, size: true},
	}
}

// prune applies retention and the size cap within pruneBudget, then returns
// free pages to the filesystem. If the work is not finished, the next run
// starts after the next flush.
func (w *writer) prune(now time.Time) {
	deadline := time.Now().Add(pruneBudget)
	done := true
	if !w.retentionHeld() {
		w.s.events.prune(now) // the warning history keeps its own retention and is never size-trimmed
		done = w.pruneRetention(now, deadline)
	}
	if done {
		done = w.enforceSize(now, int64(w.s.cfg().MaxDBSizeMiB)<<20, deadline)
	}
	w.vacuum(vacuumMinFree, time.Now().Add(vacuumBudget))
	if done {
		w.nextPrune = now.Add(pruneInterval)
	} else {
		w.nextPrune = now.Add(tickInterval)
	}
}

const (
	// maxRetentionHold bounds how long the retention waits after the latest
	// evidence of a clock problem, measured on the monotonic clock: a clock
	// that stays wrong lets the retention apply a day late instead of never.
	maxRetentionHold = 24 * time.Hour
	// clockJumpMargin is how far the wall clock may move against the
	// monotonic clock between two prune runs (or a stored row may lie
	// after the start) before it counts as a clock problem; the corrections
	// of a running clock are far smaller.
	clockJumpMargin = time.Hour
)

// clockWatch is the evidence of a clock problem that makes the retention
// wait (retentionHeld).
type clockWatch struct {
	// read returns the wall clock (without its monotonic reading) and the
	// monotonic time since the watch started (tests replace it).
	read  func() (wall time.Time, mono time.Duration)
	wall  time.Time     // the previous reading
	mono  time.Duration // of the previous reading
	held  bool          // the retention waits
	since time.Duration // monotonic time of the latest evidence while held
}

// startClockWatch starts the watch of the wall clock against the monotonic
// clock (newWriter).
func (w *writer) startClockWatch() {
	base := time.Now()
	w.clk = clockWatch{read: func() (time.Time, time.Duration) {
		t := time.Now()
		return t.Round(0), t.Sub(base)
	}}
	w.clk.wall, w.clk.mono = w.clk.read()
}

// checkLastWritten is the evidence of the start (New): the raw log row
// written last before it (lastWritten, unix ms; 0: none) lies more than
// clockJumpMargin after the current time, so the clock went back since
// then, or was ahead then. Either reading may be the wrong one, so the
// retention waits as after a jump.
func (w *writer) checkLastWritten(lastWritten int64) {
	if lastWritten > w.clk.wall.Add(clockJumpMargin).UnixMilli() {
		w.clockEvidence(fmt.Sprintf("the log was last written at %s, after the current time",
			time.UnixMilli(lastWritten).UTC().Format(time.RFC3339)))
	}
}

// clockEvidence records evidence of a clock problem: the retention waits
// for maxRetentionHold from now, unless the host reports its clock
// synchronised (the current time can then be trusted).
func (w *writer) clockEvidence(why string) {
	if w.clockSynced() {
		w.s.log.Info("the host clock changed, but it is synchronised: log retention goes on", slog.String("reason", why))
		return
	}
	w.s.log.Warn("log retention waits (at most a day, or until the host clock is synchronised), so a wrong clock "+
		"cannot delete the query log and the statistics", slog.String("reason", why))
	w.clk.held, w.clk.since = true, w.clk.mono
}

// clockSynced reports whether the host reports its clock as synchronised
// (SetClockReader; false when that is unknown).
func (w *writer) clockSynced() bool {
	fn := w.s.clock.Load()
	if fn == nil {
		return false
	}
	synced, known := (*fn)()
	return known && synced
}

// retentionHeld reports whether the retention pruning waits. The cutoff is
// the wall clock minus the retention, so a clock set far ahead would delete
// the whole query log and the statistics. It waits only on evidence of a
// clock problem: the wall clock moved more than clockJumpMargin against
// the monotonic clock since the last run (ahead or back), or the log was
// last written after the start's current time (checkLastWritten). It waits
// until maxRetentionHold passed after the latest evidence or the host
// reports its clock synchronised. An unsynchronised clock alone (a host
// without an NTP client, Docker Desktop) holds nothing: the retention is a
// privacy setting. The size cap always applies.
func (w *writer) retentionHeld() bool {
	c := &w.clk
	if c.read == nil {
		return false
	}
	wall, mono := c.read()
	skew := wall.Sub(c.wall) - (mono - c.mono)
	c.wall, c.mono = wall, mono
	if skew > clockJumpMargin || skew < -clockJumpMargin {
		dir := "ahead"
		if skew < 0 {
			dir, skew = "back", -skew
		}
		w.clockEvidence(fmt.Sprintf("the host clock jumped %s by %s", dir, skew.Round(time.Minute)))
	}
	if !c.held {
		return false
	}
	switch {
	case w.clockSynced():
		w.s.log.Info("the host clock is synchronised: log retention applies again")
	case mono-c.since >= maxRetentionHold:
		w.s.log.Info("log retention applies again: the host clock showed no further jump for a day")
	default:
		return true
	}
	c.held = false
	return false
}

// lastWrittenRaw returns the time (unix ms) of the raw log row written last
// (the query log and the cache requests: the highest id, not the latest
// time); 0 when there is none or it cannot be read.
func lastWrittenRaw(ctx context.Context, d *db.DB) int64 {
	var last int64
	for _, table := range []string{"logs_queries", "logs_cache_requests"} {
		var ts int64
		if err := d.R.QueryRowContext(ctx, `SELECT ts FROM `+table+` ORDER BY id DESC LIMIT 1`).Scan(&ts); err == nil {
			last = max(last, ts)
		}
	}
	return last
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

// sizeCandidate is a table the size cap may shorten.
type sizeCandidate struct {
	retention
	oldest int64 // unix ms of the oldest row
	empty  bool  // no row older than the floor is left
}

// share is how far the oldest row reaches into the retention (1 = a full
// retention period of data).
func (c *sizeCandidate) share(now int64) float64 {
	keep := max(c.keep.Milliseconds(), hourMs)
	return float64(now-c.oldest) / float64(keep)
}

// enforceSize keeps the used pages of logs.db below capBytes. When the cap
// is exceeded it shortens the raw events, the download sessions and the
// hourly top lists together, oldest entries first: each step trims the
// table whose oldest row reaches furthest into its retention (age divided
// by retention), so every kind of data loses the same share of its history.
// The long-lived top lists therefore cannot fill the cap and wipe the query
// log and the download history. Rows of the last full hour are never
// removed for size. False if the budget ran out first.
func (w *writer) enforceSize(now time.Time, capBytes int64, deadline time.Time) bool {
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
	nowMs := now.UnixMilli()
	floor := hourStart(now.Add(-sizeFloor).UnixMilli())
	var cands []*sizeCandidate
	for _, r := range w.retentions() {
		if !r.size {
			continue
		}
		c := &sizeCandidate{retention: r}
		if err := w.loadOldest(ctx, c, floor); err != nil {
			w.errs.log(w.s.log, "cannot prune the log database to its size limit", err)
			return true
		}
		cands = append(cands, c)
	}
	for used > target {
		if time.Now().After(deadline) {
			return false
		}
		var c *sizeCandidate
		for _, x := range cands {
			if !x.empty && (c == nil || x.share(nowMs) > c.share(nowMs)) {
				c = x
			}
		}
		if c == nil {
			w.errs.log(w.s.log, "the log database exceeds its size limit although only the last hour of log data is left",
				fmt.Errorf("logs.db uses %d MiB of %d MiB", used>>20, capBytes>>20))
			return true
		}
		if err := w.trimOldest(ctx, c, floor); err != nil {
			w.errs.log(w.s.log, "cannot prune the log database to its size limit", err)
			return true
		}
		if err := w.loadOldest(ctx, c, floor); err != nil {
			w.errs.log(w.s.log, "cannot prune the log database to its size limit", err)
			return true
		}
		if used, _, err = w.s.refreshSize(ctx); err != nil {
			w.errs.log(w.s.log, "cannot read the log database size", err)
			return true
		}
	}
	// Every table now holds at most this share of its retention.
	var kept float64
	for _, c := range cands {
		if !c.empty {
			kept = max(kept, min(c.share(nowMs), 1))
		}
	}
	w.s.log.Info("pruned the oldest log data to keep logs.db below its size limit",
		slog.Int64("limitMiB", capBytes>>20), slog.Int("retentionKeptPercent", int(kept*100)))
	return true
}

// loadOldest reads the oldest row of c; c.empty is set when no row older
// than floor is left.
func (w *writer) loadOldest(ctx context.Context, c *sizeCandidate, floor int64) error {
	var ts *int64
	if err := w.s.d.W.QueryRowContext(ctx, `SELECT MIN(`+c.col+`) FROM `+c.table).Scan(&ts); err != nil {
		return err
	}
	c.empty = ts == nil || *ts >= floor
	if ts != nil {
		c.oldest = *ts
	}
	return nil
}

// trimOldest deletes the oldest rows of c, never rows at or after floor: the
// rows of the oldest 1/sizeStepShare of its retention (at least one hour;
// for rowid tables at most sizeChunkRows rows).
func (w *writer) trimOldest(ctx context.Context, c *sizeCandidate, floor int64) error {
	span := max(floorTo(c.keep.Milliseconds()/sizeStepShare, hourMs), hourMs)
	cut := min(floor, hourStart(c.oldest)+span)
	if c.rowid {
		_, err := w.s.d.W.ExecContext(ctx, `DELETE FROM `+c.table+` WHERE rowid IN
			(SELECT rowid FROM `+c.table+` WHERE `+c.col+` < ? ORDER BY `+c.col+` LIMIT ?)`, cut, sizeChunkRows)
		return err
	}
	_, err := w.s.d.W.ExecContext(ctx, `DELETE FROM `+c.table+` WHERE `+c.col+` < ?`, cut)
	return err
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
// auto-vacuum; small free lists are kept for reuse). It releases at most
// vacuumStep per statement until the deadline: every relocated page is
// written to logs.db-wal first, and small steps let the automatic
// checkpoints and journal_size_limit (db.Open) keep the WAL small, so the
// space actually returns to the filesystem while PiCache runs.
func (w *writer) vacuum(minFree int64, deadline time.Time) {
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
	if free <= 0 || free < minFree || free < total/4 {
		return
	}
	var pageSize int64
	if err := w.s.d.W.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil || pageSize <= 0 {
		return
	}
	step := strconv.FormatInt(max(vacuumStep/pageSize, 1), 10)
	for free > 0 {
		if _, err := w.s.d.W.ExecContext(ctx, `PRAGMA incremental_vacuum(`+step+`)`); err != nil {
			w.errs.log(w.s.log, "cannot release free space of the log database", err)
			break
		}
		prev := free
		if used, total, err = w.s.refreshSize(ctx); err != nil {
			return
		}
		if free = total - used; free >= prev || time.Now().After(deadline) {
			break
		}
	}
	// Copy the relocated pages back and truncate logs.db now (a passive
	// checkpoint never waits for readers; the next one finishes the job).
	_, _ = w.s.d.W.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`)
	_, _, _ = w.s.refreshSize(ctx)
}
