package logs

import (
	"context"
	"database/sql"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// Clearing (DELETE /logs/queries, DELETE /stats) is a request to the writer
// goroutine: it owns the pending rows, the rollup deltas and the in-memory
// top lists, so nothing it holds can reappear after the delete.

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
	err     error
}

// statsTables are the statistics (docs/ARCHITECTURE.md 11): the count
// rollups and the top tables. The request, SNI, eviction and session rows,
// clients_seen and the warning history are never part of them.
var statsTables = []string{
	"logs_dns_minute", "logs_dns_hourly", "logs_cache_minute", "logs_cache_hourly",
	"logs_dns_top_hourly", "logs_dns_top_daily", "logs_cache_top_hourly", "logs_cache_top_daily",
}

// ClearQueries deletes every query row, the rows the writer has not
// written yet included, and returns how many rows were deleted. The space
// is released by the incremental vacuum of the pruning.
func (s *Store) ClearQueries(ctx context.Context) (int64, error) {
	return s.clearRequest(ctx, clearQueries)
}

// ClearStats deletes the statistics: the pending rollup deltas, the
// in-memory top lists of the current hour and every row of the count
// rollups and the hourly and daily top tables. It returns how many rows
// were deleted. The query log and the other raw data are kept.
func (s *Store) ClearStats(ctx context.Context) (int64, error) {
	return s.clearRequest(ctx, clearStats)
}

func (s *Store) clearRequest(ctx context.Context, kind clearKind) (int64, error) {
	if s.disabled != "" {
		return 0, apperr.Unavailable("logging is disabled because logs.db could not be opened; see the server log")
	}
	if !s.started.Load() || s.closed.Load() {
		return 0, apperr.Unavailable("the log database is not running")
	}
	req := clearReq{kind: kind, reply: make(chan clearResult, 1)}
	select {
	case s.control <- req:
	case <-s.stopped:
		return 0, apperr.Unavailable("the log database is closed")
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	select {
	case res := <-req.reply:
		return res.deleted, res.err
	case <-s.stopped:
		return 0, apperr.Unavailable("the log database is closed")
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// handleClear runs a clear request in the writer goroutine. The events
// queued before it are taken first (the select of run picks among ready
// channels at random), so none of them is stored or counted after the
// delete.
func (w *writer) handleClear(kind clearKind) clearResult {
	w.drainQueued()
	n, err := w.clear(kind, time.Now())
	return clearResult{n, err}
}

// clear runs a clear request in the writer goroutine.
func (w *writer) clear(kind clearKind, now time.Time) (int64, error) {
	ctx, cancel := dbContext()
	defer cancel()
	var tables []string
	switch kind {
	case clearQueries:
		clear(w.queries)
		w.queries = w.queries[:0]
		w.s.pending.Store(int64(w.rows()))
		tables = []string{"logs_queries"}
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
