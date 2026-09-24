package logs

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const (
	maxOpenSessions    = 8192 // sessions tracked in memory
	maxRetiredSessions = 8192 // closed sessions waiting to be written
)

var (
	gapMs = sessionGap.Milliseconds()
	// keepMs keeps a closed session in memory a little longer than the gap,
	// so late events (long requests are logged when they end) do not need a
	// database lookup.
	keepMs = (sessionGap + 5*time.Minute).Milliseconds()
)

type sessionKey struct{ client, service, group string }

// session is one download session (a logs_downloads row).
type session struct {
	id                       int64 // 0 until inserted
	key                      sessionKey
	clientName, label        string
	first, last              int64 // unix ms
	requests, sent, hit, wan int64
	dirty                    bool
}

// sessionTracker maintains download sessions (client + service + content
// group; a new session starts after a gap of more than 120 s). It is owned by
// the writer goroutine; rows are written with the batch.
type sessionTracker struct {
	open    map[sessionKey]*session
	retired []*session // closed or evicted while dirty
	dirty   bool
	// lookup finds the newest stored session of k that ended at or after
	// since (nil if none).
	lookup func(k sessionKey, since int64) (*session, error)
}

func newSessionTracker(lookup func(sessionKey, int64) (*session, error)) *sessionTracker {
	return &sessionTracker{open: map[sessionKey]*session{}, lookup: lookup}
}

// add accounts a cache request to its session.
func (t *sessionTracker) add(e *CacheEvent) error {
	if e.GroupKey == "" || e.Service == "" {
		return nil
	}
	start := e.Time.UnixMilli()
	end := start + e.DurationMs
	k := sessionKey{e.ClientIP, e.Service, e.GroupKey}
	s := t.open[k]
	if s != nil && start > s.last+gapMs {
		t.retire(k, s)
		s = nil
	}
	var err error
	if s == nil {
		if t.lookup != nil {
			s, err = t.lookup(k, start-gapMs)
		}
		if s == nil {
			s = &session{key: k, first: start, last: end}
		}
		t.insert(k, s)
	}
	s.first = min(s.first, start)
	s.last = max(s.last, end)
	s.requests++
	s.sent += e.BytesSent
	s.hit += e.BytesHit
	s.wan += e.BytesWAN
	if e.ClientName != "" {
		s.clientName = e.ClientName
	}
	if e.Label != "" {
		s.label = e.Label
	}
	s.dirty = true
	t.dirty = true
	return err
}

func (t *sessionTracker) insert(k sessionKey, s *session) {
	if len(t.open) >= maxOpenSessions {
		var oldestKey sessionKey
		var oldest *session
		for ok, os := range t.open {
			if oldest == nil || os.last < oldest.last {
				oldestKey, oldest = ok, os
			}
		}
		t.retire(oldestKey, oldest)
	}
	t.open[k] = s
}

// retire removes a session from memory; unwritten changes are kept for the
// next flush.
func (t *sessionTracker) retire(k sessionKey, s *session) {
	delete(t.open, k)
	if !s.dirty {
		return
	}
	if len(t.retired) >= maxRetiredSessions {
		t.retired = t.retired[1:] // bounded: the oldest unwritten update is lost
	}
	t.retired = append(t.retired, s)
}

// sessionID pairs a newly inserted session with its row id.
type sessionID struct {
	s  *session
	id int64
}

// write stores all changed sessions in tx and returns the ids of new rows
// (applied by committed once the transaction succeeded).
func (t *sessionTracker) write(ctx context.Context, tx *sql.Tx) ([]sessionID, error) {
	if !t.dirty {
		return nil, nil
	}
	ins, err := tx.PrepareContext(ctx, `INSERT INTO logs_downloads
		(client_ip, client_name, service, group_key, label, first_seen, last_seen, requests, bytes_sent, bytes_hit, bytes_wan)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	defer ins.Close()
	upd, err := tx.PrepareContext(ctx, `UPDATE logs_downloads SET client_name = ?, label = ?, first_seen = ?,
		last_seen = ?, requests = ?, bytes_sent = ?, bytes_hit = ?, bytes_wan = ? WHERE id = ?`)
	if err != nil {
		return nil, err
	}
	defer upd.Close()
	var created []sessionID
	one := func(s *session) error {
		if s.id != 0 {
			res, err := upd.ExecContext(ctx, s.clientName, s.label, s.first, s.last, s.requests, s.sent, s.hit, s.wan, s.id)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil || n == 1 {
				return err
			}
			// The row was pruned meanwhile: store it again.
		}
		res, err := ins.ExecContext(ctx, s.key.client, s.clientName, s.key.service, s.key.group, s.label,
			s.first, s.last, s.requests, s.sent, s.hit, s.wan)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		created = append(created, sessionID{s, id})
		return nil
	}
	for _, s := range t.retired {
		if err := one(s); err != nil {
			return nil, err
		}
	}
	for _, s := range t.open {
		if s.dirty {
			if err := one(s); err != nil {
				return nil, err
			}
		}
	}
	return created, nil
}

// committed marks everything written by the last write as clean.
func (t *sessionTracker) committed(created []sessionID) {
	for _, c := range created {
		c.s.id = c.id
	}
	for _, s := range t.open {
		s.dirty = false
	}
	clear(t.retired)
	t.retired = t.retired[:0]
	t.dirty = false
}

// expire forgets clean sessions that ended long enough ago.
func (t *sessionTracker) expire(nowMs int64) {
	for k, s := range t.open {
		if !s.dirty && s.last < nowMs-keepMs {
			delete(t.open, k)
		}
	}
}

// lookupSession reads the newest stored session of k that ended at or after
// since, so a session continues across restarts and after late events.
func (s *Store) lookupSession(k sessionKey, since int64) (*session, error) {
	ctx, cancel := dbContext()
	defer cancel()
	ss := &session{key: k}
	err := s.d.R.QueryRowContext(ctx, `SELECT id, client_name, label, first_seen, last_seen, requests, bytes_sent, bytes_hit, bytes_wan
		FROM logs_downloads WHERE service = ? AND group_key = ? AND client_ip = ? AND last_seen >= ?
		ORDER BY last_seen DESC LIMIT 1`, k.service, k.group, k.client, since).
		Scan(&ss.id, &ss.clientName, &ss.label, &ss.first, &ss.last, &ss.requests, &ss.sent, &ss.hit, &ss.wan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ss, nil
}
