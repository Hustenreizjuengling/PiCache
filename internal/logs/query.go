package logs

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/listing"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

const (
	queryTimeout     = 10 * time.Second
	queryConcurrency = 2
	minSearchLen     = 3
	maxFilterLen     = 256
	maxStatusFilters = 32
	maxClientFilters = 256 // client values of one query-log filter (the addresses of a device)
	maxOffset        = 100_000
	maxGroupClients  = 1000
	groupRefsPerStmt = 200
)

// acquire bounds a read query: at most queryConcurrency run at once, each
// with a 10 s timeout (including the wait for a slot).
func (s *Store) acquire(ctx context.Context) (context.Context, func(), error) {
	if s.disabled != "" {
		return nil, nil, apperr.Unavailable("logging is disabled because logs.db could not be opened; see the server log")
	}
	if s.closed.Load() {
		return nil, nil, apperr.Unavailable("the log database is closed")
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		cancel()
		return nil, nil, queryErr(ctx, ctx.Err())
	}
	return ctx, func() { <-s.sem; cancel() }, nil
}

// queryErr turns a timeout into a user-facing error.
func queryErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return apperr.Wrap(apperr.KindUnavailable, err, "the log query took longer than %s; narrow the time range or the filters", queryTimeout)
	}
	return err
}

// where collects SQL conditions and their arguments.
type where struct {
	conds []string
	args  []any
}

func (w *where) add(cond string, args ...any) {
	w.conds = append(w.conds, cond)
	w.args = append(w.args, args...)
}

func (w *where) sql() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

// search validates a substring search term (≥ 3 characters).
func search(field, v string) (string, error) {
	v = strings.TrimSpace(v)
	if len(v) > maxFilterLen {
		return "", apperr.Invalid(field, "must be at most %d characters", maxFilterLen)
	}
	if v != "" && utf8.RuneCountInString(v) < minSearchLen {
		return "", apperr.Invalid(field, "enter at least %d characters", minSearchLen)
	}
	return strings.ToLower(v), nil
}

// exact validates an exact-match filter value.
func exact(field, v string, maxLen int) (string, error) {
	v = strings.TrimSpace(v)
	if len(v) > maxLen || !utf8.ValidString(v) {
		return "", apperr.Invalid(field, "must be at most %d characters", maxLen)
	}
	return v, nil
}

// clientFilter adds a condition that matches any of the client values: an
// IP address matches exactly, anything else (≥ 3 characters) is a
// substring of the name or address. Empty values are ignored.
func clientFilter(w *where, ipCol, nameCol string, clients ...string) error {
	if len(clients) > maxClientFilters {
		return apperr.Invalid("client", "at most %d values", maxClientFilters)
	}
	var ips, args []any
	var conds []string
	for _, c := range clients {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if ip, err := netip.ParseAddr(c); err == nil {
			ips = append(ips, netutil.Canon(ip).String())
			continue
		}
		v, err := search("client", c)
		if err != nil {
			return err
		}
		conds = append(conds, "(instr(lower("+nameCol+"), ?) > 0 OR instr("+ipCol+", ?) > 0)")
		args = append(args, v, v)
	}
	switch len(ips) {
	case 0:
	case 1:
		conds = append([]string{ipCol + " = ?"}, conds...)
	default:
		conds = append([]string{ipCol + " IN (" + strings.Repeat("?, ", len(ips)-1) + "?)"}, conds...)
	}
	switch len(conds) {
	case 0:
	case 1:
		w.add(conds[0], append(ips, args...)...)
	default:
		w.add("("+strings.Join(conds, " OR ")+")", append(ips, args...)...)
	}
	return nil
}

// timeRange adds [from, to) on col; zero bounds are open.
func timeRange(w *where, col string, from, to time.Time) error {
	if !from.IsZero() && !to.IsZero() && !from.Before(to) {
		return apperr.Invalid("from", "must be before to")
	}
	if !from.IsZero() {
		w.add(col+" >= ?", from.UnixMilli())
	}
	if !to.IsZero() {
		w.add(col+" < ?", to.UnixMilli())
	}
	return nil
}

// Cursors are opaque to clients: base64url of (ts, id) of the last item.
func encodeCursor(ts, id int64) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(ts))
	binary.BigEndian.PutUint64(b[8:], uint64(id))
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func decodeCursor(c string) (ts, id int64, err error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || len(b) != 16 {
		return 0, 0, apperr.Invalid("cursor", "invalid cursor")
	}
	ts, id = int64(binary.BigEndian.Uint64(b[:8])), int64(binary.BigEndian.Uint64(b[8:]))
	if ts < 0 || id <= 0 {
		return 0, 0, apperr.Invalid("cursor", "invalid cursor")
	}
	return ts, id, nil
}

// cursorFilter continues after the cursor position (newest first).
func cursorFilter(w *where, cursor string) error {
	if cursor == "" {
		return nil
	}
	if len(cursor) > 64 {
		return apperr.Invalid("cursor", "invalid cursor")
	}
	ts, id, err := decodeCursor(cursor)
	if err != nil {
		return err
	}
	w.add("(ts < ? OR (ts = ? AND id < ?))", ts, ts, id)
	return nil
}

// cursorPage runs selectSQL (whose first two columns are id and ts) with w,
// newest first, and builds a page of at most limit items.
func cursorPage[T any](ctx context.Context, s *Store, selectSQL string, w *where, limit int,
	scan func(*sql.Rows) (T, int64, int64, error)) (listing.Page[T], error) {
	page := listing.Page[T]{Items: []T{}, Total: -1}
	ctx, release, err := s.acquire(ctx)
	if err != nil {
		return page, err
	}
	defer release()
	rows, err := s.d.R.QueryContext(ctx, selectSQL+w.sql()+` ORDER BY ts DESC, id DESC LIMIT ?`, append(w.args, limit+1)...)
	if err != nil {
		return page, queryErr(ctx, err)
	}
	defer rows.Close()
	var lastTS, lastID int64
	for rows.Next() {
		if len(page.Items) == limit {
			page.Next = encodeCursor(lastTS, lastID)
			break
		}
		item, id, ts, err := scan(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, item)
		lastTS, lastID = ts, id
	}
	return page, queryErr(ctx, rows.Err())
}

// expandStatuses validates status filters; the class names of Series
// (allowed, blocked, other, …) stand for their statuses.
func expandStatuses(in []string) ([]string, error) {
	if len(in) > maxStatusFilters {
		return nil, apperr.Invalid("status", "at most %d values", maxStatusFilters)
	}
	var out []string
	for _, v := range in {
		v = strings.ToLower(strings.TrimSpace(v))
		switch {
		case v == "":
			continue
		case statusClasses[v] != nil:
			out = append(out, statusClasses[v]...)
		case slices.Contains(knownStatuses, v):
			out = append(out, v)
		default:
			return nil, apperr.Invalid("status", "unknown status %q", clean(v, 40))
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// QueryMatcher builds a live-feed filter from the query-log parameters
// clients (any of: an IP address, or ≥ 3 characters of the name or
// address; at most 256) and statuses (statuses or class names). It returns
// nil when nothing is filtered.
func QueryMatcher(clients []string, statuses []string) (func(QueryEvent) bool, error) {
	st, err := expandStatuses(statuses)
	if err != nil {
		return nil, err
	}
	if len(clients) > maxClientFilters {
		return nil, apperr.Invalid("client", "at most %d values", maxClientFilters)
	}
	var ips, subs []string
	for _, c := range clients {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if a, err := netip.ParseAddr(c); err == nil {
			ips = append(ips, netutil.Canon(a).String())
		} else if sub, err := search("client", c); err != nil {
			return nil, err
		} else {
			subs = append(subs, sub)
		}
	}
	if len(st) == 0 && len(ips)+len(subs) == 0 {
		return nil, nil
	}
	return func(e QueryEvent) bool {
		if len(st) > 0 && !slices.Contains(st, e.Status) {
			return false
		}
		if len(ips)+len(subs) == 0 || slices.Contains(ips, e.ClientIP) {
			return true
		}
		name := strings.ToLower(e.ClientName)
		return slices.ContainsFunc(subs, func(sub string) bool {
			return strings.Contains(name, sub) || strings.Contains(e.ClientIP, sub)
		})
	}, nil
}

// Limits of the rcode filter.
const (
	maxRCodeFilters = 16
	maxRCodeLen     = 16
)

// queryWhere builds the conditions of a query-log filter (without the
// cursor): the range (default the last hour), clients, domain, statuses,
// query type, upstream, rcodes and the AD flag.
func queryWhere(f *QueryFilter) (where, error) {
	var w where
	to := f.To
	if to.IsZero() {
		to = time.Now()
	}
	from := f.From
	if from.IsZero() {
		from = to.Add(-time.Hour)
	}
	if err := timeRange(&w, "ts", from, to); err != nil {
		return w, err
	}
	if err := clientFilter(&w, "client_ip", "client_name", f.Clients...); err != nil {
		return w, err
	}
	if d := strings.TrimSpace(f.Domain); len(d) >= 2 && d[0] == '"' && d[len(d)-1] == '"' {
		name := cleanName(d[1 : len(d)-1])
		if name == "" {
			return w, apperr.Invalid("domain", "empty exact domain")
		}
		w.add("qname = ?", name)
	} else {
		v, err := search("domain", strings.TrimSuffix(d, "."))
		if err != nil {
			return w, err
		}
		if v != "" {
			w.add("instr(qname, ?) > 0", v)
		}
	}
	st, err := expandStatuses(f.Status)
	if err != nil {
		return w, err
	}
	if len(st) > 0 {
		w.add("status IN ("+strings.Repeat("?, ", len(st)-1)+"?)", anySlice(st)...)
	}
	qtype, err := exact("qtype", f.QType, maxShortLen)
	if err != nil {
		return w, err
	}
	if qtype != "" {
		w.add("qtype = ?", strings.ToUpper(qtype))
	}
	upstream, err := exact("upstream", f.Upstream, maxTextLen)
	if err != nil {
		return w, err
	}
	if upstream != "" {
		w.add("upstream = ?", upstream)
	}
	rcodes, err := rcodeFilter(f.RCode)
	if err != nil {
		return w, err
	}
	if len(rcodes) > 0 {
		w.add("rcode IN ("+strings.Repeat("?, ", len(rcodes)-1)+"?)", anySlice(rcodes)...)
	}
	if f.DNSSEC != nil {
		w.add("dnssec = ?", *f.DNSSEC)
	}
	return w, nil
}

// rcodeFilter validates the rcode values of a filter: at most 16, each 1–16
// characters of A-Z and 0-9 after upper-casing (NOERROR, NXDOMAIN, RCODE23).
// Repeated values count once; the work stays linear in the input (the
// 17th distinct value ends it).
func rcodeFilter(in []string) ([]string, error) {
	var out []string
	for _, v := range in {
		v = strings.ToUpper(strings.TrimSpace(v))
		if v == "" {
			continue
		}
		if len(v) > maxRCodeLen || strings.Trim(v, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
			return nil, apperr.Invalid("rcode", "must be a response code such as NOERROR or NXDOMAIN")
		}
		if !slices.Contains(out, v) {
			if len(out) == maxRCodeFilters {
				return nil, apperr.Invalid("rcode", "at most %d values", maxRCodeFilters)
			}
			out = append(out, v)
		}
	}
	return out, nil
}

// queryColumns are the columns scanQuery reads (id and ts first).
const queryColumns = `id, ts, client_ip, client_name, qname, qtype, status, rcode, reason, list_id,
		rule_id, service, upstream, duration_us, answer, dnssec, protocol, upstream_ede_code, upstream_ede_text, ecs, upstream_answer`

// scanQuery reads a row of queryColumns.
func scanQuery(r *sql.Rows) (QueryEvent, int64, int64, error) {
	var e QueryEvent
	var ts int64
	var edeCode int
	var edeText string
	err := r.Scan(&e.ID, &ts, &e.ClientIP, &e.ClientName, &e.QName, &e.QType, &e.Status, &e.RCode, &e.Reason,
		&e.ListID, &e.RuleID, &e.Service, &e.Upstream, &e.DurationUs, &e.Answer, &e.DNSSEC, &e.Protocol,
		&edeCode, &edeText, &e.ECS, &e.UpstreamAnswer)
	if edeCode >= 0 {
		e.UpstreamEDE = &UpstreamEDE{Code: edeCode, Text: edeText}
	}
	e.Time = db.Time(ts)
	return e, e.ID, ts, err
}

// QueryLog returns a page of query events.
func (s *Store) QueryLog(ctx context.Context, f QueryFilter) (QueryPage, error) {
	w, err := queryWhere(&f)
	if err != nil {
		return QueryPage{}, err
	}
	if err := cursorFilter(&w, f.Cursor); err != nil {
		return QueryPage{}, err
	}
	return cursorPage(ctx, s, `SELECT `+queryColumns+` FROM logs_queries`, &w, listing.Clamp(f.Limit, 100, 1000), scanQuery)
}

// ExportChunk is the size of the chunks ExportQueries reads.
const ExportChunk = 5000

// ExportQueries reads the query log of f (Cursor and Limit are ignored)
// newest first in keyset chunks of at most ExportChunk rows: every chunk is
// its own bounded read (a query slot with the normal timeout), so no read
// transaction spans chunks. fn receives each chunk and returns false to
// stop; ExportQueries returns when the rows are exhausted, fn stops or an
// error occurs.
func (s *Store) ExportQueries(ctx context.Context, f QueryFilter, fn func([]QueryEvent) (bool, error)) error {
	base, err := queryWhere(&f)
	if err != nil {
		return err
	}
	var lastTS, lastID int64
	for first := true; ; first = false {
		w := where{conds: slices.Clone(base.conds), args: slices.Clone(base.args)}
		if !first {
			w.add("(ts, id) < (?, ?)", lastTS, lastID)
		}
		chunk, err := s.exportChunk(ctx, &w)
		if err != nil {
			return err
		}
		if len(chunk) == 0 {
			return nil
		}
		last := chunk[len(chunk)-1]
		lastTS, lastID = last.Time.UnixMilli(), last.ID
		more, err := fn(chunk)
		if err != nil || !more || len(chunk) < ExportChunk {
			return err
		}
	}
}

// exportChunk reads one chunk of ExportQueries.
func (s *Store) exportChunk(ctx context.Context, w *where) ([]QueryEvent, error) {
	ctx, release, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := s.d.R.QueryContext(ctx, `SELECT `+queryColumns+` FROM logs_queries`+w.sql()+
		` ORDER BY ts DESC, id DESC LIMIT ?`, append(w.args, ExportChunk)...)
	if err != nil {
		return nil, queryErr(ctx, err)
	}
	defer rows.Close()
	out := make([]QueryEvent, 0, 256)
	for rows.Next() {
		e, _, _, err := scanQuery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, queryErr(ctx, rows.Err())
}

func anySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// eventBase applies the filters shared by all raw event lists.
func eventBase(w *where, f *EventFilter) (service, status, term string, err error) {
	if err = timeRange(w, "ts", f.From, f.To); err != nil {
		return
	}
	if service, err = exact("service", f.Service, maxIDLen); err != nil {
		return
	}
	if status, err = exact("status", f.Status, maxShortLen); err != nil {
		return
	}
	if term, err = search("search", f.Search); err != nil {
		return
	}
	err = cursorFilter(w, f.Cursor)
	return
}

// CacheRequests returns raw cache events.
func (s *Store) CacheRequests(ctx context.Context, f EventFilter) (listing.Page[CacheEvent], error) {
	var w where
	service, status, term, err := eventBase(&w, &f)
	if err != nil {
		return listing.Page[CacheEvent]{}, err
	}
	if err := clientFilter(&w, "client_ip", "client_name", f.Client); err != nil {
		return listing.Page[CacheEvent]{}, err
	}
	if service != "" {
		w.add("service = ?", service)
	}
	if status != "" {
		w.add("cache_status = ?", strings.ToUpper(status))
	}
	if term != "" {
		w.add("(instr(host, ?) > 0 OR instr(lower(path), ?) > 0)", term, term)
	}
	return cursorPage(ctx, s, `SELECT id, ts, client_ip, client_name, service, host, path, method, status, cache_status,
		byte_range, bytes_sent, bytes_hit, bytes_wan, bytes_stored, duration_ms, group_key, label, user_agent
		FROM logs_cache_requests`, &w, listing.Clamp(f.Limit, 100, 1000),
		func(r *sql.Rows) (CacheEvent, int64, int64, error) {
			var e CacheEvent
			var ts int64
			err := r.Scan(&e.ID, &ts, &e.ClientIP, &e.ClientName, &e.Service, &e.Host, &e.Path, &e.Method, &e.Status,
				&e.CacheStatus, &e.Range, &e.BytesSent, &e.BytesHit, &e.BytesWAN, &e.BytesStored, &e.DurationMs,
				&e.GroupKey, &e.Label, &e.UserAgent)
			e.Time = db.Time(ts)
			return e, e.ID, ts, err
		})
}

// SNIEvents returns pass-through events. Status is not used.
func (s *Store) SNIEvents(ctx context.Context, f EventFilter) (listing.Page[SNIEvent], error) {
	var w where
	f.Status = ""
	service, _, term, err := eventBase(&w, &f)
	if err != nil {
		return listing.Page[SNIEvent]{}, err
	}
	if err := clientFilter(&w, "client_ip", "client_name", f.Client); err != nil {
		return listing.Page[SNIEvent]{}, err
	}
	if service != "" {
		w.add("service = ?", service)
	}
	if term != "" {
		w.add("instr(sni, ?) > 0", term)
	}
	return cursorPage(ctx, s, `SELECT id, ts, client_ip, client_name, sni, service, bytes_up, bytes_down, duration_ms
		FROM logs_sni`, &w, listing.Clamp(f.Limit, 100, 1000),
		func(r *sql.Rows) (SNIEvent, int64, int64, error) {
			var e SNIEvent
			var ts int64
			err := r.Scan(&e.ID, &ts, &e.ClientIP, &e.ClientName, &e.SNI, &e.Service, &e.BytesUp, &e.BytesDown, &e.DurationMs)
			e.Time = db.Time(ts)
			return e, e.ID, ts, err
		})
}

// Evictions returns eviction events. Status filters the reason; Search
// matches the content group or object id; Client is not used.
func (s *Store) Evictions(ctx context.Context, f EventFilter) (listing.Page[EvictionEvent], error) {
	var w where
	service, reason, term, err := eventBase(&w, &f)
	if err != nil {
		return listing.Page[EvictionEvent]{}, err
	}
	if service != "" {
		w.add("service = ?", service)
	}
	if reason != "" {
		w.add("reason = ?", strings.ToLower(reason))
	}
	if term != "" {
		w.add("(instr(lower(group_key), ?) > 0 OR instr(object_id, ?) > 0)", term, term)
	}
	return cursorPage(ctx, s, `SELECT id, ts, store_id, object_id, service, group_key, bytes, reason FROM logs_evictions`,
		&w, listing.Clamp(f.Limit, 100, 1000), func(r *sql.Rows) (EvictionEvent, int64, int64, error) {
			var e EvictionEvent
			var ts int64
			err := r.Scan(&e.ID, &ts, &e.StoreID, &e.ObjectID, &e.Service, &e.GroupKey, &e.Bytes, &e.Reason)
			e.Time = db.Time(ts)
			return e, e.ID, ts, err
		})
}

// Downloads returns download sessions.
func (s *Store) Downloads(ctx context.Context, f DownloadFilter) (listing.Page[Download], error) {
	page := listing.Page[Download]{Items: []Download{}}
	if f.Offset < 0 || f.Offset > maxOffset {
		return page, apperr.Invalid("offset", "must be between 0 and %d", maxOffset)
	}
	limit := listing.Clamp(f.Limit, 50, 500)
	now := time.Now()
	var w where
	if !f.From.IsZero() && !f.To.IsZero() && !f.From.Before(f.To) {
		return page, apperr.Invalid("from", "must be before to")
	}
	if !f.From.IsZero() {
		w.add("last_seen >= ?", f.From.UnixMilli())
	}
	if !f.To.IsZero() {
		w.add("first_seen < ?", f.To.UnixMilli())
	}
	if err := clientFilter(&w, "client_ip", "client_name", f.Client); err != nil {
		return page, err
	}
	service, err := exact("service", f.Service, maxIDLen)
	if err != nil {
		return page, err
	}
	if service != "" {
		w.add("service = ?", service)
	}
	group, err := exact("group", f.GroupKey, maxTextLen)
	if err != nil {
		return page, err
	}
	if group != "" {
		w.add("group_key = ?", group)
	}
	term, err := search("search", f.Search)
	if err != nil {
		return page, err
	}
	if term != "" {
		w.add("(instr(lower(label), ?) > 0 OR instr(lower(group_key), ?) > 0)", term, term)
	}
	activeSince := now.Add(-sessionActive).UnixMilli()
	if f.ActiveOnly {
		w.add("last_seen >= ?", activeSince)
	}

	ctx, release, err := s.acquire(ctx)
	if err != nil {
		return page, err
	}
	defer release()
	if err := s.d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM logs_downloads`+w.sql(), w.args...).Scan(&page.Total); err != nil {
		return page, queryErr(ctx, err)
	}
	rows, err := s.d.R.QueryContext(ctx, `SELECT id, client_ip, client_name, service, group_key, label, first_seen,
		last_seen, requests, bytes_sent, bytes_hit, bytes_wan FROM logs_downloads`+w.sql()+`
		ORDER BY last_seen DESC, id DESC LIMIT ? OFFSET ?`, append(w.args, limit, f.Offset)...)
	if err != nil {
		return page, queryErr(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var d Download
		var first, last int64
		if err := rows.Scan(&d.ID, &d.ClientIP, &d.ClientName, &d.Service, &d.GroupKey, &d.Label, &first, &last,
			&d.Requests, &d.BytesSent, &d.BytesHit, &d.BytesWAN); err != nil {
			return page, err
		}
		d.FirstSeen, d.LastSeen = db.Time(first), db.Time(last)
		d.Active = last >= activeSince
		page.Items = append(page.Items, d)
	}
	return page, queryErr(ctx, rows.Err())
}

// GroupClients lists the clients that downloaded a content group (within
// the session retention).
func (s *Store) GroupClients(ctx context.Context, service, groupKey string) ([]GroupClient, error) {
	service, err := exact("service", service, maxIDLen)
	if err != nil {
		return nil, err
	}
	groupKey, err = exact("key", groupKey, maxTextLen)
	if err != nil {
		return nil, err
	}
	if service == "" || groupKey == "" {
		return nil, apperr.Invalid("key", "service and group key are required")
	}
	ctx, release, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	// client_name is taken from the row with the newest last_seen (SQLite
	// bare-column rule for a single MAX()).
	rows, err := s.d.R.QueryContext(ctx, `SELECT client_ip, client_name, MAX(last_seen) AS last, COUNT(*), SUM(bytes_sent)
		FROM logs_downloads WHERE service = ? AND group_key = ? GROUP BY client_ip ORDER BY last DESC LIMIT ?`,
		service, groupKey, maxGroupClients)
	if err != nil {
		return nil, queryErr(ctx, err)
	}
	defer rows.Close()
	out := []GroupClient{}
	for rows.Next() {
		var g GroupClient
		var last int64
		if err := rows.Scan(&g.ClientIP, &g.ClientName, &last, &g.Sessions, &g.BytesSent); err != nil {
			return nil, err
		}
		g.LastSeen = db.Time(last)
		out = append(out, g)
	}
	return out, queryErr(ctx, rows.Err())
}

// GroupClientCounts counts distinct clients per content group (one query for a page of groups).
func (s *Store) GroupClientCounts(ctx context.Context, refs []GroupRef) (map[GroupRef]int, error) {
	out := make(map[GroupRef]int, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	if len(refs) > 10*groupRefsPerStmt {
		return nil, apperr.Invalid("groups", "at most %d groups per request", 10*groupRefsPerStmt)
	}
	ctx, release, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	for chunk := range slices.Chunk(refs, groupRefsPerStmt) {
		args := make([]any, 0, 2*len(chunk))
		for _, r := range chunk {
			args = append(args, r.Service, r.GroupKey)
		}
		rows, err := s.d.R.QueryContext(ctx, `SELECT service, group_key, COUNT(DISTINCT client_ip) FROM logs_downloads
			WHERE (service, group_key) IN (VALUES `+strings.Repeat("(?, ?), ", len(chunk)-1)+`(?, ?))
			GROUP BY service, group_key`, args...)
		if err != nil {
			return nil, queryErr(ctx, err)
		}
		for rows.Next() {
			var r GroupRef
			var n int
			if err := rows.Scan(&r.Service, &r.GroupKey, &n); err != nil {
				rows.Close()
				return nil, err
			}
			out[r] = n
		}
		if err := rows.Close(); err != nil {
			return nil, queryErr(ctx, err)
		}
	}
	return out, nil
}
