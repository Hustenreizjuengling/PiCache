package logs

import (
	"context"
	"database/sql"
	"log/slog"
	"time"
)

const (
	writeTimeout       = 30 * time.Second // bound for every writer transaction
	checkpointInterval = 10 * time.Minute // current-hour top lists are persisted this often
	diskCheckInterval  = time.Minute
)

// dbContext bounds a writer database operation. It is independent of the
// Start context so the final flush at shutdown still completes.
func dbContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), writeTimeout)
}

// writer is the single goroutine that owns batching, rollups, sessions,
// the top-list checkpoints and pruning.
type writer struct {
	s *Store

	queries   []QueryEvent
	cache     []CacheEvent
	sni       []SNIEvent
	evictions []EvictionEvent

	roll     rollups
	sessions *sessionTracker

	nextCheckpoint time.Time
	nextDiskCheck  time.Time
	nextPrune      time.Time
	errs           errLimiter
}

func newWriter(s *Store, now time.Time) *writer {
	return &writer{
		s:              s,
		roll:           newRollups(),
		sessions:       newSessionTracker(s.lookupSession),
		nextCheckpoint: now.Add(checkpointInterval),
		nextPrune:      now.Add(time.Minute),
	}
}

// run processes events until ctx is done, then drains the queues and
// flushes.
func (w *writer) run(ctx context.Context) {
	w.checkDisk(time.Now())
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			w.shutdown()
			return
		case e := <-w.s.queries:
			w.addQuery(e)
		case e := <-w.s.cache:
			w.addCache(e)
		case e := <-w.s.sni:
			w.addSNI(e)
		case e := <-w.s.evictions:
			w.addEviction(e)
		case <-t.C:
			w.tick(time.Now())
			continue
		}
		w.afterAdd()
	}
}

func (w *writer) rows() int {
	return len(w.queries) + len(w.cache) + len(w.sni) + len(w.evictions)
}

// afterAdd flushes when the batch is full.
func (w *writer) afterAdd() {
	n := w.rows()
	w.s.pending.Store(int64(n))
	if n >= batchRows {
		w.flush(time.Now())
	}
}

// shutdown drains what producers queued before the stop (bounded by the
// queue capacities), flushes and persists the current hour's top lists.
func (w *writer) shutdown() {
	for range queryQueue + cacheQueue + sniQueue + evictionQueue {
		if !w.takeQueued() {
			break
		}
	}
	w.flush(time.Now())
	w.checkpoint(time.Now())
}

// takeQueued processes one queued event without blocking; false if all
// queues are empty.
func (w *writer) takeQueued() bool {
	select {
	case e := <-w.s.queries:
		w.addQuery(e)
	case e := <-w.s.cache:
		w.addCache(e)
	case e := <-w.s.sni:
		w.addSNI(e)
	case e := <-w.s.evictions:
		w.addEviction(e)
	default:
		return false
	}
	w.afterAdd()
	return true
}

func (w *writer) addQuery(e QueryEvent) {
	cfg := w.s.cfg()
	e = cleanQuery(e, cfg.AnonymizeClientIPs, time.Now())
	w.roll.addQuery(&e)
	w.s.top.addQuery(&e)
	if !cfg.QueryLogEnabled {
		return
	}
	e.Seq = w.s.live.next()
	publish(&w.s.live, &w.s.live.queries, e)
	e.Seq = 0 // not stored
	if !w.s.paused.Load() {
		w.queries = append(w.queries, e)
	}
}

func (w *writer) addCache(e CacheEvent) {
	e = cleanCache(e, w.s.cfg().AnonymizeClientIPs, time.Now())
	w.roll.addCache(&e)
	w.s.top.addCache(&e)
	if err := w.sessions.add(&e); err != nil {
		w.errs.log(w.s.log, "cannot look up download session", err)
	}
	e.Seq = w.s.live.next()
	publish(&w.s.live, &w.s.live.cache, e)
	e.Seq = 0 // not stored
	if !w.s.paused.Load() {
		w.cache = append(w.cache, e)
	}
}

func (w *writer) addSNI(e SNIEvent) {
	e = cleanSNI(e, w.s.cfg().AnonymizeClientIPs, time.Now())
	w.roll.addSNI(&e)
	if !w.s.paused.Load() {
		w.sni = append(w.sni, e)
	}
}

func (w *writer) addEviction(e EvictionEvent) {
	e = cleanEviction(e, time.Now())
	w.roll.addEviction(&e)
	if !w.s.paused.Load() {
		w.evictions = append(w.evictions, e)
	}
}

// tick runs the periodic work: flush, top-list hour change or checkpoint,
// disk check and pruning.
func (w *writer) tick(now time.Time) {
	w.flush(now)
	if hour := hourStart(now.UnixMilli()); hour != w.s.top.currentHour() {
		w.rollover(now, hour)
	} else if !now.Before(w.nextCheckpoint) {
		w.checkpoint(now)
	}
	if !now.Before(w.nextDiskCheck) {
		w.checkDisk(now)
	}
	if !now.Before(w.nextPrune) {
		w.prune(now)
	}
}

// flush writes the batch, rollup deltas and changed sessions in one
// transaction. A failed batch is dropped and counted (sessions are retried
// because they are written as absolute values).
func (w *writer) flush(now time.Time) {
	if w.rows() == 0 && w.roll.empty() && !w.sessions.dirty {
		return
	}
	ctx, cancel := dbContext()
	defer cancel()
	var created []sessionID
	err := w.s.d.Tx(ctx, func(tx *sql.Tx) error {
		if err := insertQueries(ctx, tx, w.queries); err != nil {
			return err
		}
		if err := insertCache(ctx, tx, w.cache); err != nil {
			return err
		}
		if err := insertSNI(ctx, tx, w.sni); err != nil {
			return err
		}
		if err := insertEvictions(ctx, tx, w.evictions); err != nil {
			return err
		}
		if err := writeRollups(ctx, tx, &w.roll); err != nil {
			return err
		}
		var err error
		created, err = w.sessions.write(ctx, tx)
		return err
	})
	if err != nil {
		w.s.dropped.Add(uint64(w.rows()))
		w.errs.log(w.s.log, "cannot write log events; the batch was dropped", err)
	} else {
		w.sessions.committed(created)
		w.s.lastFlush.Store(now.UnixMilli())
	}
	w.sessions.expire(now.UnixMilli())
	clear(w.queries)
	clear(w.cache)
	clear(w.sni)
	clear(w.evictions)
	w.queries, w.cache, w.sni, w.evictions = w.queries[:0], w.cache[:0], w.sni[:0], w.evictions[:0]
	w.roll.reset()
	w.s.pending.Store(0)
}

// checkpoint persists the current hour's top lists.
func (w *writer) checkpoint(now time.Time) {
	w.nextCheckpoint = now.Add(checkpointInterval)
	ctx, cancel := dbContext()
	defer cancel()
	if err := w.s.writeTop(ctx); err != nil {
		w.errs.log(w.s.log, "cannot store the hourly top lists", err)
	}
}

// rollover persists the finished hour's top lists and switches to hour.
// Queries rely on this order: the finished hour is committed before the
// in-memory counters move on, so it is always visible in one of both.
func (w *writer) rollover(now time.Time, hour int64) {
	w.checkpoint(now)
	ctx, cancel := dbContext()
	defer cancel()
	if err := w.s.loadTop(ctx, hour); err != nil {
		w.errs.log(w.s.log, "cannot load the hourly top lists", err)
		dns, cache := emptyTopMaps()
		w.s.top.replace(hour, dns, cache)
	}
}

// checkDisk pauses raw inserts while the data disk has less than 1 GiB free.
func (w *writer) checkDisk(now time.Time) {
	w.nextDiskCheck = now.Add(diskCheckInterval)
	free, ok := w.s.diskFree(w.s.dataDir())
	paused := ok && free < minFreeDiskBytes
	if paused == w.s.paused.Swap(paused) {
		return
	}
	if paused {
		w.s.log.Warn("the data disk has less than 1 GiB free; raw log events are not stored until space is freed (statistics continue)",
			slog.Uint64("freeMiB", free>>20))
	} else {
		w.s.log.Info("the data disk has enough free space again; storing raw log events")
	}
}

func insertQueries(ctx context.Context, tx *sql.Tx, events []QueryEvent) error {
	if len(events) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO logs_queries
		(ts, client_ip, client_name, qname, qtype, status, rcode, reason, list_id, rule_id, service, upstream,
		 duration_us, answer, dnssec, protocol)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i := range events {
		e := &events[i]
		if _, err := stmt.ExecContext(ctx, e.Time.UnixMilli(), e.ClientIP, e.ClientName, e.QName, e.QType, e.Status,
			e.RCode, e.Reason, e.ListID, e.RuleID, e.Service, e.Upstream, e.DurationUs, e.Answer, e.DNSSEC, e.Protocol); err != nil {
			return err
		}
	}
	return nil
}

func insertCache(ctx context.Context, tx *sql.Tx, events []CacheEvent) error {
	if len(events) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO logs_cache_requests
		(ts, client_ip, client_name, service, host, path, method, status, cache_status, byte_range, bytes_sent,
		 bytes_hit, bytes_wan, bytes_stored, duration_ms, group_key, label, user_agent)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i := range events {
		e := &events[i]
		if _, err := stmt.ExecContext(ctx, e.Time.UnixMilli(), e.ClientIP, e.ClientName, e.Service, e.Host, e.Path,
			e.Method, e.Status, e.CacheStatus, e.Range, e.BytesSent, e.BytesHit, e.BytesWAN, e.BytesStored,
			e.DurationMs, e.GroupKey, e.Label, e.UserAgent); err != nil {
			return err
		}
	}
	return nil
}

func insertSNI(ctx context.Context, tx *sql.Tx, events []SNIEvent) error {
	if len(events) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO logs_sni
		(ts, client_ip, client_name, sni, service, bytes_up, bytes_down, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i := range events {
		e := &events[i]
		if _, err := stmt.ExecContext(ctx, e.Time.UnixMilli(), e.ClientIP, e.ClientName, e.SNI, e.Service,
			e.BytesUp, e.BytesDown, e.DurationMs); err != nil {
			return err
		}
	}
	return nil
}

func insertEvictions(ctx context.Context, tx *sql.Tx, events []EvictionEvent) error {
	if len(events) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO logs_evictions
		(ts, store_id, object_id, service, group_key, bytes, reason) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i := range events {
		e := &events[i]
		if _, err := stmt.ExecContext(ctx, e.Time.UnixMilli(), e.StoreID, e.ObjectID, e.Service, e.GroupKey,
			e.Bytes, e.Reason); err != nil {
			return err
		}
	}
	return nil
}

// errLimiter logs a repeated error only when its text changes or once an
// hour (per message).
type errLimiter struct {
	last map[string]loggedErr
}

type loggedErr struct {
	text string
	at   time.Time
}

func (l *errLimiter) log(log *slog.Logger, msg string, err error) {
	now := time.Now()
	text := err.Error()
	if prev, ok := l.last[msg]; ok && prev.text == text && now.Sub(prev.at) < time.Hour {
		return
	}
	if l.last == nil {
		l.last = map[string]loggedErr{} // keyed by the fixed messages of this package
	}
	l.last[msg] = loggedErr{text, now}
	log.Error(msg, slog.Any("err", err))
}
