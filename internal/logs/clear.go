package logs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// Clearing (DELETE /logs/queries, DELETE /stats) is a request to the writer
// goroutine: it owns the pending rows, the rollup deltas and the in-memory
// top lists, so nothing it holds can reappear after the delete.
//
// The query log can hold millions of rows: one DELETE of them all took
// longer than a writer transaction may (the clear failed and logging
// stopped meanwhile). So the writer only marks the rows (the largest id,
// kept in <logs.db>.cleared across restarts): every read leaves them out
// at once, and the writer deletes them in chunks on its ticks, between
// its flushes (clearStep).

type clearKind int

const (
	clearQueries clearKind = iota // the query log
	clearStats                    // the statistics
)

type clearReq struct {
	kind  clearKind
	reply chan clearResult
}

type clearResult struct {
	deleted int64
	mark    int64 // clearQueries: the largest id marked as cleared
	err     error
}

// clearedFile is the file next to logs.db that keeps the mark of a cleared
// query log until its rows are deleted: "<id> <ts>", the id and the time of
// the newest row the clear covered.
func clearedFile(dbPath string) string { return dbPath + ".cleared" }

// loadClearedMark returns the mark of clearedFile when it belongs to this
// logs.db: its newest cleared row is still there, with the same time (the
// writer deletes that row last). Otherwise (all rows deleted, or another
// logs.db, e.g. one that replaced a broken file) the file is removed and 0
// returned, so a fresh file never hides its own rows.
func loadClearedMark(ctx context.Context, d *db.DB) int64 {
	b, err := os.ReadFile(clearedFile(d.Path))
	if err != nil {
		return 0
	}
	var mark, ts, have int64
	f := strings.Fields(string(b))
	if len(f) == 2 {
		mark, _ = strconv.ParseInt(f[0], 10, 64)
		ts, _ = strconv.ParseInt(f[1], 10, 64)
	}
	if mark <= 0 || d.R.QueryRowContext(ctx, `SELECT ts FROM logs_queries WHERE id = ?`, mark).Scan(&have) != nil || have != ts {
		_ = os.Remove(clearedFile(d.Path))
		return 0
	}
	return mark
}

// hideCleared leaves out the query rows a clear marked and the writer has
// not deleted yet.
func (s *Store) hideCleared(w *where) {
	if m := s.clearedTo.Load(); m > 0 {
		w.add("id > ?", m)
	}
}

// statsTables are the statistics (docs/ARCHITECTURE.md 11): the count
// rollups and the top tables. The request, SNI, eviction and session rows,
// clients_seen and the warning history are never part of them.
var statsTables = []string{
	"logs_dns_minute", "logs_dns_hourly", "logs_cache_minute", "logs_cache_hourly",
	"logs_dns_top_hourly", "logs_dns_top_daily", "logs_cache_top_hourly", "logs_cache_top_daily",
}

// ClearQueries deletes every query row, the rows the writer has not
// written yet included, and returns how many rows were deleted. The rows
// are counted on a reader first; then the writer marks them (they are
// gone for every read at once) and deletes them in the background. The
// space is released by the incremental vacuum of the pruning.
func (s *Store) ClearQueries(ctx context.Context) (int64, error) {
	if err := s.clearable(); err != nil {
		return 0, err
	}
	var n, top int64
	var err error
	prev := s.clearedTo.Load()
	if prev == 0 {
		err = s.d.R.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(id), 0) FROM logs_queries`).Scan(&n, &top)
	} else {
		// An earlier clear is still being deleted: count by the ids after
		// its mark (they are consecutive), which needs no scan.
		var low int64
		err = s.d.R.QueryRowContext(ctx, `SELECT COALESCE(MIN(id), 0), COALESCE(MAX(id), 0) FROM logs_queries WHERE id > ?`, prev).
			Scan(&low, &top)
		if top > 0 {
			n = top - low + 1
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return 0, apperr.Wrap(apperr.KindUnavailable, err, "counting the query log took too long; try again")
		}
		return 0, err
	}
	res, err := s.clearRequest(ctx, clearQueries)
	if err != nil {
		return 0, err
	}
	// Rows the writer stored after the count are cleared too.
	return n + max(0, res.mark-max(top, prev)), nil
}

// ClearStats deletes the statistics: the pending rollup deltas, the
// in-memory top lists of the current hour and every row of the count
// rollups and the hourly and daily top tables. It returns how many rows
// were deleted. The query log and the other raw data are kept.
func (s *Store) ClearStats(ctx context.Context) (int64, error) {
	if err := s.clearable(); err != nil {
		return 0, err
	}
	res, err := s.clearRequest(ctx, clearStats)
	return res.deleted, err
}

// clearable refuses a clear without a running writer on logs.db.
func (s *Store) clearable() error {
	if s.disabled != "" {
		return apperr.Unavailable("logging is disabled because logs.db could not be opened; see the server log")
	}
	if !s.started.Load() || s.closed.Load() {
		return apperr.Unavailable("the log database is not running")
	}
	return nil
}

func (s *Store) clearRequest(ctx context.Context, kind clearKind) (clearResult, error) {
	req := clearReq{kind: kind, reply: make(chan clearResult, 1)}
	select {
	case s.control <- req:
	case <-s.stopped:
		return clearResult{}, apperr.Unavailable("the log database is closed")
	case <-ctx.Done():
		return clearResult{}, ctx.Err()
	}
	select {
	case res := <-req.reply:
		return res, res.err
	case <-s.stopped:
		return clearResult{}, apperr.Unavailable("the log database is closed")
	case <-ctx.Done():
		return clearResult{}, ctx.Err()
	}
}

// handleClear runs a clear request in the writer goroutine. The events
// queued before it are taken first (the select of run picks among ready
// channels at random), so none of them is stored or counted after the
// delete.
func (w *writer) handleClear(kind clearKind) clearResult {
	w.drainQueued()
	if kind == clearQueries {
		mark, err := w.markCleared()
		return clearResult{mark: mark, err: err}
	}
	n, err := w.clear(kind, time.Now())
	return clearResult{deleted: n, err: err}
}

// markCleared drops the pending query rows and marks every stored one as
// cleared (the largest id); clearStep deletes them.
func (w *writer) markCleared() (int64, error) {
	clear(w.queries)
	w.queries = w.queries[:0]
	w.s.pending.Store(int64(w.rows()))
	ctx, cancel := dbContext()
	defer cancel()
	var mark, ts int64
	err := w.s.d.W.QueryRowContext(ctx, `SELECT id, ts FROM logs_queries ORDER BY id DESC LIMIT 1`).Scan(&mark, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return w.s.clearedTo.Load(), nil // nothing stored (beyond a mark that is being deleted)
	}
	if err != nil {
		return 0, err
	}
	if mark > w.s.clearedTo.Load() {
		w.s.clearedTo.Store(mark)
		if err := os.WriteFile(clearedFile(w.s.d.Path), fmt.Appendf(nil, "%d %d\n", mark, ts), 0o600); err != nil {
			w.s.log.Warn("cannot record the cleared query log; its rows reappear after a restart before they are deleted", slog.Any("err", err))
		}
	}
	return mark, nil
}

// clearStep deletes rows of a cleared query log in chunks until deadline
// (at least one chunk), each chunk its own short transaction; once none is
// left it forgets the mark and releases the space soon.
func (w *writer) clearStep(deadline time.Time) {
	mark := w.s.clearedTo.Load()
	if mark == 0 {
		return
	}
	ctx, cancel := dbContext()
	defer cancel()
	for first := true; first || time.Now().Before(deadline); first = false {
		res, err := w.s.d.W.ExecContext(ctx, `DELETE FROM logs_queries WHERE rowid IN
			(SELECT rowid FROM logs_queries WHERE rowid <= ? ORDER BY rowid LIMIT ?)`, mark, pruneChunkRows)
		if err != nil {
			if !errors.Is(err, context.DeadlineExceeded) {
				w.errs.log(w.s.log, "cannot delete the rows of the cleared query log", err)
			}
			return
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			w.s.clearedTo.Store(0)
			_ = os.Remove(clearedFile(w.s.d.Path))
			w.nextPrune = time.Now() // release the space soon (incremental vacuum)
			return
		}
	}
}

// clear runs a clear request of the statistics in the writer goroutine.
func (w *writer) clear(kind clearKind, now time.Time) (int64, error) {
	ctx, cancel := dbContext()
	defer cancel()
	var tables []string
	switch kind {
	case clearStats:
		w.roll.reset()
		dns, cache := emptyTopMaps()
		w.s.top.replace(w.s.top.currentHour(), dns, cache, nil)
		w.restartBackfill(now)
		tables = statsTables
	}
	var deleted int64
	err := w.s.d.Tx(ctx, func(tx *sql.Tx) error {
		for _, t := range tables {
			res, err := tx.ExecContext(ctx, `DELETE FROM `+t)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			deleted += n
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	w.nextPrune = now // release the space soon (incremental vacuum)
	return deleted, nil
}
