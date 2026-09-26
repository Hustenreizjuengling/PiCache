package logs

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/listing"
)

// The warning history (docs/ARCHITECTURE.md 15.1) records the notification
// events the app emits, independent of notification channels. An
// unacknowledged entry with the same event and title is updated (count,
// last time, newest message) instead of adding one. Bounds: at most 200
// entries per area (the oldest last time is dropped first) and no entry
// whose last time is older than 90 days. The history lives in logs.db
// (table logs_events), is never size-trimmed and never cleared with the
// query log or the statistics; while logs.db is disabled it is an
// in-memory ring of 200 entries with the same rules, lost at restart.
// Writes are small direct transactions of the caller, independent of the
// batch writer.

const (
	eventRetention     = 90 * 24 * time.Hour
	maxEventsPerArea   = 200
	maxMemoryEvents    = 200
	eventWriteTimeout  = 5 * time.Second
	maxEventTitleLen   = 200
	maxEventMessageLen = 2000
	maxEventPage       = 200
	defaultEventPage   = 50
)

// EventAreas are the areas of the history: the first part of the event
// key ("health.failed" is in area health). Events of other areas are not
// recorded.
var EventAreas = []string{"health", "storage", "update", "backup", "security"}

// securityArea entries are shown to principals with admin scope only.
const securityArea = "security"

// Event is an entry of the warning history. AcknowledgedAt and
// AcknowledgedBy are set once it was acknowledged.
type Event struct {
	ID             int64      `json:"id"`
	Time           time.Time  `json:"time"`     // first occurrence
	LastTime       time.Time  `json:"lastTime"` // last occurrence
	Count          int64      `json:"count"`
	Event          string     `json:"event"`
	Severity       string     `json:"severity"` // info | warning | error
	Title          string     `json:"title"`
	Message        string     `json:"message"`
	AcknowledgedAt *time.Time `json:"acknowledgedAt,omitempty"`
	AcknowledgedBy string     `json:"acknowledgedBy,omitempty"`
}

// EventRecord is an event to record (a notification message).
type EventRecord struct {
	Event    string
	Severity string // info | warning | error (anything else is recorded as info)
	Title    string
	Message  string
	Time     time.Time // zero: now
}

// EventQuery selects entries of the history, newest last time first.
type EventQuery struct {
	Unacknowledged *bool // nil: all
	Security       bool  // include the security.* entries (admin scope)
	Cursor         string
	Limit          int // 1..200, default 50
}

// EventCounts are the unacknowledged warning and error entries: All of
// them, Security of the security area (not shown to viewers).
type EventCounts struct {
	All      int
	Security int
}

// EventPage is a cursor page of the history.
type EventPage = listing.Page[Event]

// eventStore is the warning history: logs_events, or the in-memory ring
// while logs.db is disabled (d == nil).
type eventStore struct {
	mu     sync.Mutex
	d      *db.DB
	log    *slog.Logger
	errs   errLimiter
	closed bool
	// counts are read without e.mu (the header badge polls them while a
	// write may hold the mutex).
	counts atomic.Pointer[EventCounts]

	mem    []Event // memory mode
	nextID int64
}

// openEvents opens the history in logs.db and counts its unacknowledged
// entries.
func openEvents(ctx context.Context, d *db.DB, log *slog.Logger) (*eventStore, error) {
	e := &eventStore{d: d, log: log}
	return e, e.recount(ctx)
}

// memoryEvents returns the in-memory history (logs.db disabled).
func memoryEvents(log *slog.Logger) *eventStore { return &eventStore{log: log} }

// eventArea returns the area of an event key ("" if it has none).
func eventArea(event string) string {
	area, _, ok := strings.Cut(event, ".")
	if !ok || !slices.Contains(EventAreas, area) {
		return ""
	}
	return area
}

// cleanEventText makes s valid UTF-8 without control characters (newlines
// kept when multiline) of at most n bytes.
func cleanEventText(s string, n int, multiline bool) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' && multiline {
			return r
		}
		if isControlOrBidi(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
	return clean(strings.TrimSpace(s), n)
}

func normalizeSeverity(s string) string {
	switch s {
	case "warning", "error":
		return s
	}
	return "info"
}

// RecordEvent adds a notification event to the history (merged into an
// unacknowledged entry of the same event and title). Failures are logged
// (rate-limited), never returned: the history must not stop a
// notification.
func (s *Store) RecordEvent(r EventRecord) {
	if s.events != nil {
		s.events.record(r)
	}
}

// Events returns a page of the history.
func (s *Store) Events(ctx context.Context, q EventQuery) (EventPage, error) {
	return s.events.list(ctx, q)
}

// AckEvent acknowledges an entry (idempotent: an acknowledged entry keeps
// its time and name) and returns it; apperr.NotFound for an unknown id.
func (s *Store) AckEvent(ctx context.Context, id int64, by string) (Event, error) {
	return s.events.ack(ctx, id, by)
}

// AckAllEvents acknowledges every unacknowledged entry and returns how many.
func (s *Store) AckAllEvents(ctx context.Context, by string) (int64, error) {
	return s.events.ackAll(ctx, by)
}

// EventCounts returns the unacknowledged warning and error entries (from
// memory, no query).
func (s *Store) EventCounts() EventCounts {
	if s.events == nil {
		return EventCounts{}
	}
	if c := s.events.counts.Load(); c != nil {
		return *c
	}
	return EventCounts{}
}

func (e *eventStore) record(r EventRecord) {
	area := eventArea(r.Event)
	if area == "" {
		return
	}
	now := time.Now()
	if r.Time.IsZero() || r.Time.After(now.Add(maxClockSkew)) {
		r.Time = now
	}
	ev := Event{Time: time.UnixMilli(r.Time.UnixMilli()).UTC(), Count: 1, Event: clean(r.Event, maxShortLen),
		Severity: normalizeSeverity(r.Severity), Title: cleanEventText(r.Title, maxEventTitleLen, false),
		Message: cleanEventText(r.Message, maxEventMessageLen, true)}
	ev.LastTime = ev.Time
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	if e.d == nil {
		e.recordMemory(ev)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), eventWriteTimeout)
	defer cancel()
	err := e.d.Tx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM logs_events WHERE ack_ts = 0 AND event = ? AND title = ?
			ORDER BY last_ts DESC, id DESC LIMIT 1`, ev.Event, ev.Title).Scan(&id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.ExecContext(ctx, `INSERT INTO logs_events (ts, last_ts, count, event, severity, title, message)
				VALUES (?, ?, 1, ?, ?, ?, ?)`, ev.Time.UnixMilli(), ev.LastTime.UnixMilli(), ev.Event, ev.Severity, ev.Title, ev.Message)
		case err == nil:
			_, err = tx.ExecContext(ctx, `UPDATE logs_events SET count = count + 1, last_ts = MAX(last_ts, ?), message = ?,
				severity = ? WHERE id = ?`, ev.LastTime.UnixMilli(), ev.Message, ev.Severity, id)
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM logs_events WHERE id IN (SELECT id FROM logs_events
			WHERE substr(event, 1, ?) = ? ORDER BY last_ts DESC, id DESC LIMIT -1 OFFSET ?)`,
			len(area)+1, area+".", maxEventsPerArea)
		return err
	})
	if err == nil {
		err = e.recount(ctx)
	}
	if err != nil {
		e.errs.log(e.log, "cannot record the event in the warning history", err)
	}
}

// recordMemory is record in memory mode (e.mu held).
func (e *eventStore) recordMemory(ev Event) {
	e.pruneMemory(time.Now())
	for i := range e.mem {
		m := &e.mem[i]
		if m.AcknowledgedAt == nil && m.Event == ev.Event && m.Title == ev.Title {
			m.Count++
			if ev.LastTime.After(m.LastTime) {
				m.LastTime = ev.LastTime
			}
			m.Message, m.Severity = ev.Message, ev.Severity
			e.countMemory()
			return
		}
	}
	e.nextID++
	ev.ID = e.nextID
	e.mem = append(e.mem, ev)
	if len(e.mem) > maxMemoryEvents {
		oldest := 0
		for i, m := range e.mem {
			if m.LastTime.Before(e.mem[oldest].LastTime) || (m.LastTime.Equal(e.mem[oldest].LastTime) && m.ID < e.mem[oldest].ID) {
				oldest = i
			}
		}
		e.mem = slices.Delete(e.mem, oldest, oldest+1)
	}
	e.countMemory()
}

// pruneMemory removes entries older than eventRetention (e.mu held).
func (e *eventStore) pruneMemory(now time.Time) {
	cut := now.Add(-eventRetention)
	e.mem = slices.DeleteFunc(e.mem, func(m Event) bool { return m.LastTime.Before(cut) })
}

// countMemory recounts the unacknowledged warnings and errors (e.mu held).
func (e *eventStore) countMemory() {
	var c EventCounts
	for _, m := range e.mem {
		if m.AcknowledgedAt == nil && m.Severity != "info" {
			c.All++
			if eventArea(m.Event) == securityArea {
				c.Security++
			}
		}
	}
	e.counts.Store(&c)
}

// recount reads the unacknowledged warning and error entries (e.mu held,
// or before the store is shared).
func (e *eventStore) recount(ctx context.Context) error {
	var c EventCounts
	err := e.d.W.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(substr(event, 1, 9) = 'security.'), 0) FROM logs_events
		WHERE ack_ts = 0 AND severity IN ('warning', 'error')`).Scan(&c.All, &c.Security)
	if err == nil {
		e.counts.Store(&c)
	}
	return err
}

// prune removes entries not seen for eventRetention (called by the
// writer's pruning).
func (e *eventStore) prune(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	if e.d == nil {
		e.pruneMemory(now)
		e.countMemory()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), eventWriteTimeout)
	defer cancel()
	res, err := e.d.W.ExecContext(ctx, `DELETE FROM logs_events WHERE last_ts < ?`, now.Add(-eventRetention).UnixMilli())
	if err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			err = e.recount(ctx)
		}
	}
	if err != nil {
		e.errs.log(e.log, "cannot prune the warning history", err)
	}
}

// close stops recording (logs.db is about to be closed).
func (e *eventStore) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = e.d != nil
}

func (e *eventStore) list(ctx context.Context, q EventQuery) (EventPage, error) {
	limit := listing.Clamp(q.Limit, defaultEventPage, maxEventPage)
	page := EventPage{Items: []Event{}, Total: -1}
	var cts, cid int64
	if q.Cursor != "" {
		var err error
		if len(q.Cursor) > 64 {
			return page, apperr.Invalid("cursor", "invalid cursor")
		}
		if cts, cid, err = decodeCursor(q.Cursor); err != nil {
			return page, err
		}
	}
	e.mu.Lock()
	memory := e.d == nil
	var items []Event
	if memory {
		e.pruneMemory(time.Now())
		items = slices.Clone(e.mem)
	}
	e.mu.Unlock()
	if memory {
		slices.SortFunc(items, func(a, b Event) int {
			return cmp.Or(b.LastTime.Compare(a.LastTime), cmp.Compare(b.ID, a.ID))
		})
		for _, m := range items {
			switch {
			case q.Unacknowledged != nil && *q.Unacknowledged != (m.AcknowledgedAt == nil):
				continue
			case !q.Security && eventArea(m.Event) == securityArea:
				continue
			case q.Cursor != "" && (m.LastTime.UnixMilli() > cts || (m.LastTime.UnixMilli() == cts && m.ID >= cid)):
				continue
			}
			if len(page.Items) == limit {
				last := page.Items[len(page.Items)-1]
				page.Next = encodeCursor(last.LastTime.UnixMilli(), last.ID)
				break
			}
			page.Items = append(page.Items, m)
		}
		return page, nil
	}
	var w where
	if q.Unacknowledged != nil {
		if *q.Unacknowledged {
			w.add("ack_ts = 0")
		} else {
			w.add("ack_ts != 0")
		}
	}
	if !q.Security {
		w.add("substr(event, 1, 9) != 'security.'")
	}
	if q.Cursor != "" {
		w.add("(last_ts < ? OR (last_ts = ? AND id < ?))", cts, cts, cid)
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	rows, err := e.d.R.QueryContext(ctx, `SELECT id, ts, last_ts, count, event, severity, title, message, ack_ts, ack_by
		FROM logs_events`+w.sql()+` ORDER BY last_ts DESC, id DESC LIMIT ?`, append(w.args, limit+1)...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(page.Items) == limit {
			last := page.Items[len(page.Items)-1]
			page.Next = encodeCursor(last.LastTime.UnixMilli(), last.ID)
			break
		}
		ev, err := scanEvent(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, ev)
	}
	return page, rows.Err()
}

type rowScanner interface{ Scan(dst ...any) error }

func scanEvent(r rowScanner) (Event, error) {
	var ev Event
	var ts, last, ack int64
	if err := r.Scan(&ev.ID, &ts, &last, &ev.Count, &ev.Event, &ev.Severity, &ev.Title, &ev.Message, &ack, &ev.AcknowledgedBy); err != nil {
		return ev, err
	}
	ev.Time, ev.LastTime = db.Time(ts), db.Time(last)
	if ack != 0 {
		t := db.Time(ack)
		ev.AcknowledgedAt = &t
	}
	return ev, nil
}

func (e *eventStore) ack(ctx context.Context, id int64, by string) (Event, error) {
	by = clean(by, maxTextLen)
	now := time.UnixMilli(time.Now().UnixMilli()).UTC()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.d == nil {
		for i := range e.mem {
			if m := &e.mem[i]; m.ID == id {
				if m.AcknowledgedAt == nil {
					m.AcknowledgedAt, m.AcknowledgedBy = &now, by
					e.countMemory()
				}
				return *m, nil
			}
		}
		return Event{}, apperr.NotFound("event", id)
	}
	ctx, cancel := context.WithTimeout(ctx, eventWriteTimeout)
	defer cancel()
	var ev Event
	err := e.d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE logs_events SET ack_ts = ?, ack_by = ? WHERE id = ? AND ack_ts = 0`,
			now.UnixMilli(), by, id); err != nil {
			return err
		}
		var err error
		ev, err = scanEvent(tx.QueryRowContext(ctx, `SELECT id, ts, last_ts, count, event, severity, title, message, ack_ts, ack_by
			FROM logs_events WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.NotFound("event", id)
		}
		return err
	})
	if err != nil {
		return Event{}, err
	}
	return ev, e.recount(ctx)
}

func (e *eventStore) ackAll(ctx context.Context, by string) (int64, error) {
	by = clean(by, maxTextLen)
	now := time.UnixMilli(time.Now().UnixMilli()).UTC()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.d == nil {
		var n int64
		for i := range e.mem {
			if m := &e.mem[i]; m.AcknowledgedAt == nil {
				m.AcknowledgedAt, m.AcknowledgedBy = &now, by
				n++
			}
		}
		e.countMemory()
		return n, nil
	}
	ctx, cancel := context.WithTimeout(ctx, eventWriteTimeout)
	defer cancel()
	res, err := e.d.W.ExecContext(ctx, `UPDATE logs_events SET ack_ts = ?, ack_by = ? WHERE ack_ts = 0`, now.UnixMilli(), by)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, e.recount(ctx)
}
