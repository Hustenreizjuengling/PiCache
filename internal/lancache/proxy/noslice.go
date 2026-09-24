package proxy

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	cachestore "github.com/hustenreizjuengling/picache/internal/lancache/store"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

const (
	// noSliceThreshold range failures on distinct objects within
	// noSliceWindow mark a host (ARCHITECTURE 8.2 step 10 d).
	noSliceThreshold = 3
	noSliceWindow    = 24 * time.Hour
	// noSliceProbe: a marked host gets one sliced request per interval; a
	// valid 206 clears the mark.
	noSliceProbe    = 24 * time.Hour
	maxNoSliceHosts = 1024
)

var noSliceMigrations = []string{
	`CREATE TABLE proxy_noslice_hosts (
		host         TEXT    PRIMARY KEY,
		failures     INTEGER NOT NULL,
		objects      TEXT    NOT NULL DEFAULT '',
		window_start INTEGER NOT NULL,
		marked       INTEGER NOT NULL DEFAULT 0,
		since        INTEGER NOT NULL
	) WITHOUT ROWID;`,
}

// noSliceEntry is the range-failure state of one host.
type noSliceEntry struct {
	objects     []string  // distinct failed objects in the window (≤ threshold)
	windowStart time.Time // first failure of the window
	marked      bool
	since       time.Time // marked: when; else windowStart
	lastProbe   time.Time
}

// noSliceTracker keeps the no-slice state in memory (hot path) and persists
// changes asynchronously (flushed by Start).
type noSliceTracker struct {
	db  *db.DB
	log *slog.Logger

	flushMu sync.Mutex // one flush at a time: a later snapshot is never overwritten by an earlier one

	mu     sync.Mutex
	hosts  map[string]*noSliceEntry // ≤ maxNoSliceHosts
	dirty  map[string]bool
	closed bool
	signal chan struct{}
}

func newNoSliceTracker(ctx context.Context, d *db.DB, log *slog.Logger) (*noSliceTracker, error) {
	t := &noSliceTracker{db: d, log: log, hosts: make(map[string]*noSliceEntry), dirty: make(map[string]bool), signal: make(chan struct{}, 1)}
	if d == nil {
		return t, nil
	}
	if err := d.Migrate(ctx, "proxy", noSliceMigrations); err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	rows, err := d.R.QueryContext(ctx, `SELECT host, objects, window_start, marked, since FROM proxy_noslice_hosts ORDER BY since DESC LIMIT ?`, maxNoSliceHosts)
	if err != nil {
		return nil, fmt.Errorf("proxy: load no-slice hosts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var host, objects string
		var windowStart, since int64
		var marked bool
		if err := rows.Scan(&host, &objects, &windowStart, &marked, &since); err != nil {
			return nil, fmt.Errorf("proxy: load no-slice hosts: %w", err)
		}
		h, isIP, ok := netutil.NormalizeHost(host)
		if !ok || isIP || h != host {
			continue
		}
		e := &noSliceEntry{windowStart: db.Time(windowStart), marked: marked, since: db.Time(since)}
		for id := range strings.FieldsSeq(objects) {
			if cachestore.ValidObjectID(id) && len(e.objects) < noSliceThreshold && !slices.Contains(e.objects, id) {
				e.objects = append(e.objects, id)
			}
		}
		e.lastProbe = e.since
		t.hosts[host] = e
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("proxy: load no-slice hosts: %w", err)
	}
	return t, nil
}

// useSlicing reports whether requests to host use ranges: always unless it
// is marked, then once per probe interval.
func (t *noSliceTracker) useSlicing(host string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.hosts[host]
	if e == nil || !e.marked {
		return true
	}
	if now.Sub(e.lastProbe) >= noSliceProbe {
		e.lastProbe = now
		return true
	}
	return false
}

// failure counts a range failure of host for object id.
func (t *noSliceTracker) failure(host, id string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.hosts[host]
	switch {
	case e == nil:
		if len(t.hosts) >= maxNoSliceHosts && !t.evictLocked() {
			return
		}
		e = &noSliceEntry{windowStart: now, since: now}
		t.hosts[host] = e
	case e.marked:
		return // a failed probe: stays marked
	case now.Sub(e.windowStart) > noSliceWindow:
		e.objects, e.windowStart, e.since = nil, now, now
	}
	if slices.Contains(e.objects, id) {
		return
	}
	e.objects = append(e.objects, id)
	if len(e.objects) >= noSliceThreshold {
		e.marked, e.since, e.lastProbe = true, now, now
		t.log.Warn("host answers range requests without range support; fetching it without slicing",
			slog.String("host", host), slog.Int("failures", len(e.objects)))
	}
	t.markDirtyLocked(host)
}

// success clears the state of host after a valid 206.
func (t *noSliceTracker) success(host string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.hosts[host]
	if !ok {
		return
	}
	if e.marked {
		t.log.Info("host supports range requests again", slog.String("host", host))
	}
	delete(t.hosts, host)
	t.markDirtyLocked(host)
}

// evictLocked drops the oldest unmarked entry to make room.
func (t *noSliceTracker) evictLocked() bool {
	var oldest string
	for h, e := range t.hosts {
		if !e.marked && (oldest == "" || e.windowStart.Before(t.hosts[oldest].windowStart)) {
			oldest = h
		}
	}
	if oldest == "" {
		return false
	}
	delete(t.hosts, oldest)
	t.markDirtyLocked(oldest)
	return true
}

func (t *noSliceTracker) markDirtyLocked(host string) {
	if len(t.dirty) < 2*maxNoSliceHosts {
		t.dirty[host] = true
	}
	select {
	case t.signal <- struct{}{}:
	default:
	}
}

// dirtySignal fires after changes that need persisting.
func (t *noSliceTracker) dirtySignal() <-chan struct{} { return t.signal }

// decay forgets failure windows older than 24 h (marks stay until reset or
// a valid 206).
func (t *noSliceTracker) decay(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for h, e := range t.hosts {
		if !e.marked && now.Sub(e.windowStart) > noSliceWindow {
			delete(t.hosts, h)
			t.markDirtyLocked(h)
		}
	}
}

func (t *noSliceTracker) list() []NoSliceHost {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]NoSliceHost, 0, len(t.hosts))
	for h, e := range t.hosts {
		out = append(out, NoSliceHost{Host: h, Failures: len(e.objects), Marked: e.marked, Since: e.since.UTC()})
	}
	return out
}

// reset clears host (apperr.Invalid for a malformed host, apperr.NotFound
// if it has no state) and persists that immediately.
func (t *noSliceTracker) reset(ctx context.Context, host string) error {
	h, isIP, ok := netutil.NormalizeHost(host)
	if !ok || isIP {
		return apperr.Invalid("host", "invalid host name")
	}
	t.mu.Lock()
	if _, ok := t.hosts[h]; !ok {
		t.mu.Unlock()
		return apperr.NotFound("no-slice host", h)
	}
	delete(t.hosts, h)
	t.markDirtyLocked(h)
	t.mu.Unlock()
	return t.flush(ctx)
}

// flush persists dirty hosts. After close it does nothing.
func (t *noSliceTracker) flush(ctx context.Context) error {
	if t.db == nil {
		return nil
	}
	t.flushMu.Lock()
	defer t.flushMu.Unlock()
	type row struct {
		host string
		e    *noSliceEntry // nil: delete
	}
	t.mu.Lock()
	if t.closed || len(t.dirty) == 0 {
		t.mu.Unlock()
		return nil
	}
	rows := make([]row, 0, len(t.dirty))
	for h := range t.dirty {
		var cp *noSliceEntry
		if e := t.hosts[h]; e != nil {
			c := *e
			c.objects = slices.Clone(e.objects)
			cp = &c
		}
		rows = append(rows, row{host: h, e: cp})
	}
	clear(t.dirty)
	t.mu.Unlock()
	err := t.db.Tx(ctx, func(tx *sql.Tx) error {
		for _, r := range rows {
			if r.e == nil {
				if _, err := tx.ExecContext(ctx, `DELETE FROM proxy_noslice_hosts WHERE host = ?`, r.host); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO proxy_noslice_hosts (host, failures, objects, window_start, marked, since)
				VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT(host) DO UPDATE SET failures = excluded.failures, objects = excluded.objects,
					window_start = excluded.window_start, marked = excluded.marked, since = excluded.since`,
				r.host, len(r.e.objects), strings.Join(r.e.objects, " "), db.Ms(r.e.windowStart), r.e.marked, db.Ms(r.e.since)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// Keep them dirty for the next flush.
		t.mu.Lock()
		for _, r := range rows {
			t.dirty[r.host] = true
		}
		t.mu.Unlock()
		t.log.Warn("persisting no-slice hosts failed", slog.Any("err", err))
		return err
	}
	return nil
}

// close stops persisting (after Start returned the DB may be closed).
func (t *noSliceTracker) close() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
}
