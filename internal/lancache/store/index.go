package cachestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
)

// Index write batching and bounds.
const (
	flushEvery     = 250 * time.Millisecond // batched index writes
	flushRows      = 512                    // … or as soon as this many ops are pending
	maxPendingOps  = 8192                   // writers get errBacklog beyond this
	statsEvery     = 30 * time.Second       // dirty access statistics
	statsSoftLimit = 8192                   // flush statistics early at this many dirty objects
	maxDirtyStats  = 65536                  // hard bound of the dirty statistics map
)

var errBacklog = errors.New("cachestore: index write backlog (index database too slow or failing)")

// migrations of component "cachestore" (append-only).
var migrations = []string{
	`CREATE TABLE store_meta (
		key   TEXT    PRIMARY KEY,
		value TEXT    NOT NULL DEFAULT '',
		num   INTEGER NOT NULL DEFAULT 0
	) WITHOUT ROWID;
	CREATE TABLE store_objects (
		id           TEXT    PRIMARY KEY,
		gen          INTEGER NOT NULL,
		service      TEXT    NOT NULL,
		host         TEXT    NOT NULL,
		path         TEXT    NOT NULL,
		group_key    TEXT    NOT NULL,
		total        INTEGER NOT NULL,
		slice_size   INTEGER NOT NULL,
		headers      TEXT    NOT NULL DEFAULT '{}',
		created_at   INTEGER NOT NULL,
		last_access  INTEGER NOT NULL,
		hits         INTEGER NOT NULL DEFAULT 0,
		bytes_served INTEGER NOT NULL DEFAULT 0,
		cached_bytes INTEGER NOT NULL DEFAULT 0,
		slice_count  INTEGER NOT NULL DEFAULT 0,
		pinned       INTEGER NOT NULL DEFAULT 0,
		no_slice     INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX store_objects_lru ON store_objects (last_access) WHERE pinned = 0;
	CREATE INDEX store_objects_group ON store_objects (service, group_key);
	CREATE TABLE store_slices (
		object_id  TEXT    NOT NULL,
		idx        INTEGER NOT NULL,
		size       INTEGER NOT NULL,
		crc        INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		PRIMARY KEY (object_id, idx)
	) WITHOUT ROWID;
	CREATE TABLE store_groups (
		service      TEXT    NOT NULL,
		group_key    TEXT    NOT NULL,
		objects      INTEGER NOT NULL DEFAULT 0,
		slices       INTEGER NOT NULL DEFAULT 0,
		cached_bytes INTEGER NOT NULL DEFAULT 0,
		total_bytes  INTEGER NOT NULL DEFAULT 0,
		bytes_served INTEGER NOT NULL DEFAULT 0,
		hits         INTEGER NOT NULL DEFAULT 0,
		first_cached INTEGER NOT NULL DEFAULT 0,
		last_access  INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (service, group_key)
	) WITHOUT ROWID;
	CREATE TABLE store_pinned_groups (
		service    TEXT    NOT NULL,
		group_key  TEXT    NOT NULL,
		created_at INTEGER NOT NULL,
		PRIMARY KEY (service, group_key)
	) WITHOUT ROWID;`,
}

// openIndex opens and migrates the index DB and checks that it belongs to
// this store.
func openIndex(ctx context.Context, path, storeID string, sliceSize int64) (*db.DB, error) {
	d, err := db.Open(path, 4)
	if err != nil {
		return nil, err
	}
	if err := initIndex(ctx, d, storeID, sliceSize); err != nil {
		_ = d.Close()
		return nil, err
	}
	return d, nil
}

func initIndex(ctx context.Context, d *db.DB, storeID string, sliceSize int64) error {
	if err := d.Migrate(ctx, "cachestore", migrations); err != nil {
		return err
	}
	if _, err := d.W.ExecContext(ctx, `INSERT INTO store_meta (key, value, num)
		VALUES ('store_id', ?, 0), ('slice_size', '', ?), ('gen', '', 0)
		ON CONFLICT (key) DO NOTHING`, storeID, sliceSize); err != nil {
		return err
	}
	var id string
	var ss int64
	if err := d.W.QueryRowContext(ctx, `SELECT value FROM store_meta WHERE key = 'store_id'`).Scan(&id); err != nil {
		return err
	}
	if err := d.W.QueryRowContext(ctx, `SELECT num FROM store_meta WHERE key = 'slice_size'`).Scan(&ss); err != nil {
		return err
	}
	if id != storeID || ss != sliceSize {
		return fmt.Errorf("cachestore: index belongs to another store (%s, slice size %d)", id, ss)
	}
	// Touch every table once so that obvious corruption shows up now.
	for _, t := range []string{"store_objects", "store_slices", "store_groups", "store_pinned_groups"} {
		var n int
		if err := d.W.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM `+t+` LIMIT 1)`).Scan(&n); err != nil {
			return err
		}
	}
	return nil
}

// moveAside renames a broken index to <path>.broken-<ts> (keeping only the
// newest broken copy) and removes its WAL files.
func moveAside(path string) error {
	if old, _ := filepath.Glob(path + ".broken-*"); len(old) > 0 {
		for _, f := range old {
			_ = os.Remove(f)
		}
	}
	if err := os.Rename(path, path+".broken-"+time.Now().UTC().Format("20060102T150405Z")); err != nil {
		return err
	}
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
	return nil
}

// usageCounters mirror the sums of store_groups (updated after each commit).
type usageCounters struct {
	objects atomic.Int64
	slices  atomic.Int64
	cached  atomic.Int64
}

type usageDelta struct{ objects, slices, cached int64 }

func (u *usageCounters) add(d usageDelta) {
	u.objects.Add(d.objects)
	u.slices.Add(d.slices)
	u.cached.Add(d.cached)
}

func (u *usageCounters) load(ctx context.Context, q *sql.DB) error {
	var o, sl, cb int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(objects), 0), COALESCE(SUM(slices), 0),
		COALESCE(SUM(cached_bytes), 0) FROM store_groups`).Scan(&o, &sl, &cb); err != nil {
		return err
	}
	u.objects.Store(o)
	u.slices.Store(sl)
	u.cached.Store(cb)
	return nil
}

type opKind uint8

const (
	opPut   opKind = iota + 1 // create/update an object record (new generation if gen differs)
	opSlice                   // add or replace one slice
	opDrop                    // drop one slice (missing or corrupt file)
)

// indexOp is one batched index change. e is the entry state after the op;
// once the op is committed the overlay entry is dropped if it is still e.
type indexOp struct {
	kind       opKind
	id         string
	e          *entry
	gen        uint64
	created    int64 // opPut: created_at of a new record/generation; opSlice: slice created_at
	lastAccess int64 // opPut: last_access of a new record
	idx        int64
	size       int64
	crc        uint32
}

type statDelta struct{ hits, bytes, last int64 }

// enqueue appends an index op and kicks the flusher when a batch is full.
func (s *Store) enqueue(o indexOp) {
	s.pendMu.Lock()
	s.pending = append(s.pending, o)
	n := len(s.pending)
	s.pendMu.Unlock()
	if n >= flushRows {
		s.kickFlush()
	}
}

func (s *Store) pendingLen() int {
	s.pendMu.Lock()
	defer s.pendMu.Unlock()
	return len(s.pending)
}

func (s *Store) kickFlush() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// flush commits all pending index ops (and, with withStats, the dirty access
// statistics) in one transaction. On failure everything is kept for the next
// attempt.
func (s *Store) flush(ctx context.Context, withStats bool) error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	return s.flushLocked(ctx, withStats)
}

func (s *Store) flushLocked(ctx context.Context, withStats bool) error {
	s.pendMu.Lock()
	ops := s.pending
	s.pending = nil
	s.pendMu.Unlock()
	var st map[string]*statDelta
	if withStats {
		s.statsMu.Lock()
		if len(s.stats) > 0 {
			st = s.stats
			s.stats = make(map[string]*statDelta)
		}
		s.statsMu.Unlock()
	}
	if len(ops) == 0 && len(st) == 0 {
		return nil
	}
	var d usageDelta
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		w := s.newIndexTx(ctx, tx)
		for i := range ops {
			if err := w.apply(&ops[i]); err != nil {
				return err
			}
		}
		for id, sd := range st {
			if err := w.touch(id, sd); err != nil {
				return err
			}
		}
		if _, err := w.exec(`UPDATE store_meta SET num = max(num, ?) WHERE key = 'gen'`, int64(s.gen.Load())); err != nil {
			return err
		}
		d = w.d
		return nil
	})
	if err != nil {
		s.pendMu.Lock()
		s.pending = append(ops, s.pending...)
		s.pendMu.Unlock()
		s.mergeStats(st)
		return fmt.Errorf("cachestore: index write: %w", err)
	}
	s.usage.add(d)
	for i := range ops {
		s.heads.flushed(ops[i].id, ops[i].e)
	}
	return nil
}

// mergeStats puts statistics of a failed flush back (bounded).
func (s *Store) mergeStats(st map[string]*statDelta) {
	if len(st) == 0 {
		return
	}
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	for id, d := range st {
		cur, ok := s.stats[id]
		if !ok {
			if len(s.stats) >= maxDirtyStats {
				continue
			}
			s.stats[id] = d
			continue
		}
		cur.hits += d.hits
		cur.bytes += d.bytes
		cur.last = max(cur.last, d.last)
	}
}

// flusher commits batched index writes every 250 ms (or when kicked) and the
// access statistics every 30 s, and once more when the store closes.
func (s *Store) flusher() {
	defer close(s.flusherDone)
	tick := time.NewTicker(flushEvery)
	defer tick.Stop()
	stats := time.NewTicker(statsEvery)
	defer stats.Stop()
	for {
		withStats := false
		select {
		case <-s.stopFlush:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := s.flush(ctx, true)
			cancel()
			if err != nil {
				s.log.Error("final index flush failed", slog.Any("err", err))
			}
			return
		case <-tick.C:
			if s.pendingLen() == 0 {
				continue
			}
		case <-s.kick:
			s.statsMu.Lock()
			withStats = len(s.stats) >= statsSoftLimit
			s.statsMu.Unlock()
		case <-stats.C:
			withStats = true
		}
		if err := s.flush(context.Background(), withStats); err != nil {
			s.logRepeated("index flush failed", err)
		}
	}
}

// logRepeated logs an error when it changes or at most hourly.
func (s *Store) logRepeated(msg string, err error) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	e := err.Error()
	if e == s.lastErr && time.Since(s.lastErrAt) < time.Hour {
		return
	}
	s.lastErr, s.lastErrAt = e, time.Now()
	s.log.Error(msg, slog.Any("err", err))
}

// loadEntry reads the compact state of an object from the index (nil if unknown).
func (s *Store) loadEntry(ctx context.Context, id string) (*entry, error) {
	var (
		e       entry
		gen     int64
		hdr     string
		noSlice bool
	)
	err := s.db.R.QueryRowContext(ctx, `SELECT gen, service, host, path, group_key, total, headers, no_slice
		FROM store_objects WHERE id = ?`, id).Scan(&gen, &e.service, &e.host, &e.path, &e.groupKey, &e.total, &hdr, &noSlice)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.gen, e.noSlice = uint64(gen), noSlice
	if e.total <= 0 || e.total > MaxTotal {
		e.total = 1 // unusable record: every slice check fails until SetMeta replaces it
	}
	var h http.Header
	if json.Unmarshal([]byte(hdr), &h) == nil {
		if c, n, err := cleanHeader(h); err == nil {
			e.header, e.hdrSize = c, n
		}
	}
	if e.header == nil {
		e.header = http.Header{}
	}
	e.present = newBitmap(e.total, s.sliceSize)
	n := slicesFor(e.total, s.sliceSize)
	rows, err := s.db.R.QueryContext(ctx, `SELECT idx FROM store_slices WHERE object_id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var idx int64
		if err := rows.Scan(&idx); err != nil {
			return nil, err
		}
		if idx >= 0 && idx < n {
			e.present[idx/64] |= 1 << (uint(idx) % 64)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &e, nil
}

// indexTx applies changes inside one write transaction and keeps the
// aggregate table and the usage delta consistent with them.
type indexTx struct {
	ctx       context.Context
	tx        *sql.Tx
	stmts     map[string]*sql.Stmt
	sliceSize int64
	d         usageDelta
}

func (s *Store) newIndexTx(ctx context.Context, tx *sql.Tx) *indexTx {
	return &indexTx{ctx: ctx, tx: tx, stmts: map[string]*sql.Stmt{}, sliceSize: s.sliceSize}
}

func (w *indexTx) stmt(q string) (*sql.Stmt, error) {
	if st, ok := w.stmts[q]; ok {
		return st, nil
	}
	st, err := w.tx.PrepareContext(w.ctx, q)
	if err != nil {
		return nil, err
	}
	w.stmts[q] = st
	return st, nil
}

func (w *indexTx) exec(q string, args ...any) (sql.Result, error) {
	st, err := w.stmt(q)
	if err != nil {
		return nil, err
	}
	return st.ExecContext(w.ctx, args...)
}

func (w *indexTx) row(q string, args ...any) *sql.Row {
	st, err := w.stmt(q)
	if err != nil {
		return w.tx.QueryRowContext(w.ctx, q, args...) // reports the same error on Scan
	}
	return st.QueryRowContext(w.ctx, args...)
}

// objRow is the part of an object row the aggregates depend on.
type objRow struct {
	gen                                                      uint64
	service, groupKey                                        string
	total, cached, slices, hits, served, created, lastAccess int64
}

func (w *indexTx) objRow(id string) (objRow, bool, error) {
	var r objRow
	var gen int64
	err := w.row(`SELECT gen, service, group_key, total, cached_bytes, slice_count, hits, bytes_served, created_at, last_access
		FROM store_objects WHERE id = ?`, id).Scan(&gen, &r.service, &r.groupKey, &r.total, &r.cached, &r.slices,
		&r.hits, &r.served, &r.created, &r.lastAccess)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	r.gen = uint64(gen)
	return r, err == nil, err
}

type groupDelta struct{ objects, slices, cached, total, served, hits, first, last int64 }

// group applies a delta to the aggregate row of (service, key) and removes
// rows without objects.
func (w *indexTx) group(service, key string, g groupDelta) error {
	if _, err := w.exec(`INSERT INTO store_groups (service, group_key, objects, slices, cached_bytes, total_bytes,
			bytes_served, hits, first_cached, last_access) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (service, group_key) DO UPDATE SET
			objects = objects + excluded.objects,
			slices = slices + excluded.slices,
			cached_bytes = cached_bytes + excluded.cached_bytes,
			total_bytes = total_bytes + excluded.total_bytes,
			bytes_served = bytes_served + excluded.bytes_served,
			hits = hits + excluded.hits,
			first_cached = CASE WHEN excluded.first_cached > 0 AND (first_cached = 0 OR excluded.first_cached < first_cached)
				THEN excluded.first_cached ELSE first_cached END,
			last_access = max(last_access, excluded.last_access)`,
		service, key, g.objects, g.slices, g.cached, g.total, g.served, g.hits, g.first, g.last); err != nil {
		return err
	}
	if g.objects < 0 {
		if _, err := w.exec(`DELETE FROM store_groups WHERE service = ? AND group_key = ? AND objects <= 0`, service, key); err != nil {
			return err
		}
	}
	return nil
}

func (w *indexTx) apply(o *indexOp) error {
	switch o.kind {
	case opPut:
		return w.put(o)
	case opSlice:
		return w.addSlice(o)
	case opDrop:
		return w.dropSlice(o)
	}
	return fmt.Errorf("cachestore: unknown index op %d", o.kind)
}

const pinnedGroupExists = `EXISTS (SELECT 1 FROM store_pinned_groups p WHERE p.service = ? AND p.group_key = ?)`

func (w *indexTx) put(o *indexOp) error {
	e := o.e
	hdr, err := json.Marshal(e.header)
	if err != nil {
		return err
	}
	old, exists, err := w.objRow(o.id)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := w.exec(`DELETE FROM store_slices WHERE object_id = ?`, o.id); err != nil {
			return err
		}
		if _, err := w.exec(`INSERT INTO store_objects (id, gen, service, host, path, group_key, total, slice_size, headers,
				created_at, last_access, pinned, no_slice)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+pinnedGroupExists+`, ?)`,
			o.id, int64(o.gen), e.service, e.host, e.path, e.groupKey, e.total, w.sliceSize, string(hdr),
			o.created, o.lastAccess, e.service, e.groupKey, e.noSlice); err != nil {
			return err
		}
		w.d.objects++
		return w.group(e.service, e.groupKey, groupDelta{objects: 1, total: e.total, first: o.created, last: o.lastAccess})
	}
	cached, slices, created := old.cached, old.slices, old.created
	if old.gen != o.gen { // new generation: the old slices are gone
		if _, err := w.exec(`DELETE FROM store_slices WHERE object_id = ?`, o.id); err != nil {
			return err
		}
		w.d.slices -= old.slices
		w.d.cached -= old.cached
		cached, slices, created = 0, 0, o.created
	}
	if _, err := w.exec(`UPDATE store_objects SET gen = ?, host = ?, group_key = ?, total = ?, headers = ?, no_slice = ?,
			cached_bytes = ?, slice_count = ?, created_at = ?, pinned = pinned OR `+pinnedGroupExists+`
		WHERE id = ?`, int64(o.gen), e.host, e.groupKey, e.total, string(hdr), e.noSlice, cached, slices, created,
		e.service, e.groupKey, o.id); err != nil {
		return err
	}
	if old.groupKey == e.groupKey && old.service == e.service {
		return w.group(e.service, e.groupKey, groupDelta{slices: slices - old.slices, cached: cached - old.cached,
			total: e.total - old.total, first: created})
	}
	if err := w.group(old.service, old.groupKey, groupDelta{objects: -1, slices: -old.slices, cached: -old.cached,
		total: -old.total, served: -old.served, hits: -old.hits}); err != nil {
		return err
	}
	return w.group(e.service, e.groupKey, groupDelta{objects: 1, slices: slices, cached: cached, total: e.total,
		served: old.served, hits: old.hits, first: created, last: old.lastAccess})
}

func (w *indexTx) addSlice(o *indexOp) error {
	old, exists, err := w.objRow(o.id)
	if err != nil || !exists || old.gen != o.gen {
		return err // stale op: the object was replaced or removed
	}
	var size int64
	err = w.row(`SELECT size FROM store_slices WHERE object_id = ? AND idx = ?`, o.id, o.idx).Scan(&size)
	switch {
	case err == nil:
		if _, err := w.exec(`UPDATE store_slices SET size = ?, crc = ?, created_at = ? WHERE object_id = ? AND idx = ?`,
			o.size, int64(o.crc), o.created, o.id, o.idx); err != nil {
			return err
		}
		if d := o.size - size; d != 0 {
			if _, err := w.exec(`UPDATE store_objects SET cached_bytes = cached_bytes + ? WHERE id = ?`, d, o.id); err != nil {
				return err
			}
			w.d.cached += d
			return w.group(old.service, old.groupKey, groupDelta{cached: d})
		}
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	if _, err := w.exec(`INSERT INTO store_slices (object_id, idx, size, crc, created_at) VALUES (?, ?, ?, ?, ?)`,
		o.id, o.idx, o.size, int64(o.crc), o.created); err != nil {
		return err
	}
	if _, err := w.exec(`UPDATE store_objects SET cached_bytes = cached_bytes + ?, slice_count = slice_count + 1 WHERE id = ?`,
		o.size, o.id); err != nil {
		return err
	}
	w.d.slices++
	w.d.cached += o.size
	return w.group(old.service, old.groupKey, groupDelta{slices: 1, cached: o.size})
}

func (w *indexTx) dropSlice(o *indexOp) error {
	old, exists, err := w.objRow(o.id)
	if err != nil || !exists || old.gen != o.gen {
		return err
	}
	var size int64
	err = w.row(`DELETE FROM store_slices WHERE object_id = ? AND idx = ? RETURNING size`, o.id, o.idx).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := w.exec(`UPDATE store_objects SET cached_bytes = cached_bytes - ?, slice_count = slice_count - 1 WHERE id = ?`,
		size, o.id); err != nil {
		return err
	}
	w.d.slices--
	w.d.cached -= size
	return w.group(old.service, old.groupKey, groupDelta{slices: -1, cached: -size})
}

// touch applies accumulated access statistics.
func (w *indexTx) touch(id string, d *statDelta) error {
	var service, key string
	err := w.row(`UPDATE store_objects SET hits = hits + ?, bytes_served = bytes_served + ?, last_access = max(last_access, ?)
		WHERE id = ? RETURNING service, group_key`, d.hits, d.bytes, d.last, id).Scan(&service, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return w.group(service, key, groupDelta{hits: d.hits, served: d.bytes, last: d.last})
}

// objectCols selects an Object (see scanObject); the pinned flag includes
// the group pin.
const objectCols = `o.id, o.gen, o.service, o.host, o.path, o.group_key, o.total, o.slice_size, o.headers, o.created_at,
	o.last_access, o.hits, o.bytes_served, o.cached_bytes, o.slice_count,
	o.pinned <> 0 OR EXISTS (SELECT 1 FROM store_pinned_groups p WHERE p.service = o.service AND p.group_key = o.group_key),
	o.no_slice`

type scanner interface{ Scan(dest ...any) error }

// scanObject scans objectCols.
func scanObject(sc scanner, o *Object) (gen uint64, err error) {
	var (
		g                   int64
		hdr                 string
		created, lastAccess int64
	)
	if err := sc.Scan(&o.ID, &g, &o.Service, &o.Host, &o.Path, &o.GroupKey, &o.Total, &o.SliceSize, &hdr, &created,
		&lastAccess, &o.Hits, &o.BytesServed, &o.CachedBytes, &o.SliceCount, &o.Pinned, &o.NoSlice); err != nil {
		return 0, err
	}
	o.CreatedAt, o.LastAccess = db.Time(created), db.Time(lastAccess)
	if o.SliceSize > 0 {
		o.SlicesTotal = slicesFor(o.Total, o.SliceSize)
	}
	var h http.Header
	if json.Unmarshal([]byte(hdr), &h) == nil {
		o.ContentType = h.Get("Content-Type")
	}
	return uint64(g), nil
}

// readObject reads one object row (pinned includes the group pin).
func (w *indexTx) readObject(id string) (Object, uint64, bool, error) {
	var o Object
	gen, err := scanObject(w.row(`SELECT `+objectCols+` FROM store_objects o WHERE o.id = ?`, id), &o)
	if errors.Is(err, sql.ErrNoRows) {
		return o, 0, false, nil
	}
	return o, gen, err == nil, err
}

// deleteObject removes the rows of o and returns the indexes of the slices
// the index knew about.
func (w *indexTx) deleteObject(o *Object) ([]int64, error) {
	st, err := w.stmt(`SELECT idx FROM store_slices WHERE object_id = ?`)
	if err != nil {
		return nil, err
	}
	rows, err := st.QueryContext(w.ctx, o.ID)
	if err != nil {
		return nil, err
	}
	var idxs []int64
	for rows.Next() {
		var i int64
		if err := rows.Scan(&i); err != nil {
			rows.Close()
			return nil, err
		}
		idxs = append(idxs, i)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := w.exec(`DELETE FROM store_slices WHERE object_id = ?`, o.ID); err != nil {
		return nil, err
	}
	if _, err := w.exec(`DELETE FROM store_objects WHERE id = ?`, o.ID); err != nil {
		return nil, err
	}
	w.d.objects--
	w.d.slices -= o.SliceCount
	w.d.cached -= o.CachedBytes
	return idxs, w.group(o.Service, o.GroupKey, groupDelta{objects: -1, slices: -o.SliceCount, cached: -o.CachedBytes,
		total: -o.Total, served: -o.BytesServed, hits: -o.Hits})
}
