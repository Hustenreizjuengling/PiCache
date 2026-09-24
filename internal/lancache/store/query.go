package cachestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/listing"
)

const (
	queryTimeout    = 10 * time.Second
	maxSearchLen    = 256
	maxSearchKeys   = 1000
	maxOffset       = 1_000_000
	maxPinnedGroups = 10_000
)

// Sort allowlists (API sort name → column).
var (
	groupSorts = map[string]string{
		"bytes":       "g.cached_bytes",
		"lastAccess":  "g.last_access",
		"firstCached": "g.first_cached",
		"served":      "g.bytes_served",
		"name":        "g.group_key",
	}
	objectSorts = map[string]string{
		"lastAccess": "o.last_access",
		"size":       "o.total",
		"created":    "o.created_at",
		"path":       "o.path",
	}
)

// expires returns lastAccess + retention (zero for pinned content or when
// there is no retention).
func expires(lastAccess time.Time, retention time.Duration, pinned bool) time.Time {
	if pinned || retention <= 0 || lastAccess.IsZero() {
		return time.Time{}
	}
	return lastAccess.Add(retention)
}

// pageBounds validates and clamps limit/offset.
func pageBounds(limit, offset int) (int, int, error) {
	if offset < 0 || offset > maxOffset {
		return 0, 0, apperr.Invalid("offset", "must be between 0 and %d", maxOffset)
	}
	return listing.Clamp(limit, 50, 500), offset, nil
}

func orderBy(sorts map[string]string, sort, def string, desc bool, tiebreak ...string) (string, error) {
	if sort == "" {
		sort = def
	}
	col, ok := sorts[sort]
	if !ok {
		return "", apperr.Invalid("sort", "unknown sort %q", sort)
	}
	dir := " ASC"
	if desc {
		dir = " DESC"
	}
	var b strings.Builder
	b.WriteString(" ORDER BY " + col + dir)
	for _, t := range tiebreak {
		b.WriteString(", " + t + dir)
	}
	return b.String(), nil
}

func validSearch(q string) error {
	if len(q) > maxSearchLen || !validText(q) {
		return apperr.Invalid("search", "must be at most %d printable characters", maxSearchLen)
	}
	return nil
}

// Services aggregates per service.
func (s *Store) Services(ctx context.Context) ([]ServiceUsage, error) {
	if !s.enter() {
		return nil, ErrClosed
	}
	defer s.leave()
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	rows, err := s.db.R.QueryContext(ctx, `SELECT service, SUM(objects), COUNT(*), SUM(cached_bytes), SUM(bytes_served),
		MAX(last_access) FROM store_groups GROUP BY service ORDER BY SUM(cached_bytes) DESC, service`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ServiceUsage{}
	for rows.Next() {
		var u ServiceUsage
		var last int64
		if err := rows.Scan(&u.Service, &u.Objects, &u.Groups, &u.CachedBytes, &u.BytesServed, &last); err != nil {
			return nil, err
		}
		u.LastAccess = db.Time(last)
		out = append(out, u)
	}
	return out, rows.Err()
}

// Groups lists content groups.
func (s *Store) Groups(ctx context.Context, q GroupQuery) (listing.Page[GroupUsage], error) {
	page := listing.Page[GroupUsage]{Items: []GroupUsage{}}
	if !s.enter() {
		return page, ErrClosed
	}
	defer s.leave()
	limit, offset, err := pageBounds(q.Limit, q.Offset)
	if err != nil {
		return page, err
	}
	order, err := orderBy(groupSorts, q.Sort, "bytes", q.Desc, "g.service", "g.group_key")
	if err != nil {
		return page, err
	}
	var where []string
	var args []any
	if q.Service != "" {
		where, args = append(where, "g.service = ?"), append(args, q.Service)
	}
	if q.GroupKey != "" {
		where, args = append(where, "g.group_key = ?"), append(args, q.GroupKey)
	}
	if q.Search != "" {
		if err := validSearch(q.Search); err != nil {
			return page, err
		}
		keys := q.SearchKeys
		if len(keys) > maxSearchKeys {
			keys = keys[:maxSearchKeys]
		}
		if keys == nil {
			keys = []string{}
		}
		kj, err := json.Marshal(keys)
		if err != nil {
			return page, apperr.Invalid("search", "invalid search keys")
		}
		where = append(where, "(instr(lower(g.group_key), lower(?)) > 0 OR g.group_key IN (SELECT value FROM json_each(?)))")
		args = append(args, q.Search, string(kj))
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	if err := s.db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM store_groups g`+cond, args...).Scan(&page.Total); err != nil {
		return page, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT g.service, g.group_key, g.objects, g.cached_bytes, g.total_bytes,
			g.bytes_served, g.hits, g.first_cached, g.last_access,
			EXISTS (SELECT 1 FROM store_pinned_groups p WHERE p.service = g.service AND p.group_key = g.group_key)
		FROM store_groups g`+cond+order+` LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var g GroupUsage
		var first, last int64
		if err := rows.Scan(&g.Service, &g.GroupKey, &g.Objects, &g.CachedBytes, &g.TotalBytes, &g.BytesServed, &g.Hits,
			&first, &last, &g.Pinned); err != nil {
			return page, err
		}
		g.FirstCached, g.LastAccess = db.Time(first), db.Time(last)
		g.ExpiresAt = expires(g.LastAccess, q.Retention, g.Pinned)
		page.Items = append(page.Items, g)
	}
	return page, rows.Err()
}

// Objects lists objects.
func (s *Store) Objects(ctx context.Context, q ObjectQuery) (listing.Page[Object], error) {
	page := listing.Page[Object]{Items: []Object{}}
	if !s.enter() {
		return page, ErrClosed
	}
	defer s.leave()
	limit, offset, err := pageBounds(q.Limit, q.Offset)
	if err != nil {
		return page, err
	}
	order, err := orderBy(objectSorts, q.Sort, "lastAccess", q.Desc, "o.id")
	if err != nil {
		return page, err
	}
	var where []string
	var args []any
	if q.Service != "" {
		where, args = append(where, "o.service = ?"), append(args, q.Service)
	}
	if q.GroupKey != "" {
		where, args = append(where, "o.group_key = ?"), append(args, q.GroupKey)
	}
	if q.Search != "" {
		if err := validSearch(q.Search); err != nil {
			return page, err
		}
		where, args = append(where, "instr(lower(o.path), lower(?)) > 0"), append(args, q.Search)
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	if err := s.db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM store_objects o`+cond, args...).Scan(&page.Total); err != nil {
		return page, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+objectCols+` FROM store_objects o`+cond+order+` LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var o Object
		if _, err := scanObject(rows, &o); err != nil {
			return page, err
		}
		o.ExpiresAt = expires(o.LastAccess, q.Retention, o.Pinned)
		page.Items = append(page.Items, o)
	}
	return page, rows.Err()
}

// SetPinned pins/unpins one object (pinned objects are never evicted).
func (s *Store) SetPinned(ctx context.Context, id string, pinned bool) error {
	if !s.enter() {
		return ErrClosed
	}
	defer s.leave()
	if !ValidObjectID(id) {
		return errInvalidID
	}
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if err := s.flush(ctx, false); err != nil { // a just-created object must exist in the index
		return err
	}
	res, err := s.db.W.ExecContext(ctx, `UPDATE store_objects SET pinned = ? WHERE id = ?`, pinned, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return apperr.NotFound("object", id)
	}
	return nil
}

// SetGroupPinned pins/unpins a group persistently (existing and future objects).
func (s *Store) SetGroupPinned(ctx context.Context, service, groupKey string, pinned bool) error {
	if !s.enter() {
		return ErrClosed
	}
	defer s.leave()
	if !validService(service) {
		return apperr.Invalid("service", "invalid service id")
	}
	if !validGroupKey(groupKey) {
		return apperr.Invalid("key", "invalid group key")
	}
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	return s.db.Tx(ctx, func(tx *sql.Tx) error {
		if pinned {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM store_pinned_groups`).Scan(&n); err != nil {
				return err
			}
			if n >= maxPinnedGroups {
				return apperr.Conflict("at most %d groups can be pinned", maxPinnedGroups)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO store_pinned_groups (service, group_key, created_at)
				VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, service, groupKey, db.NowMs()); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, `DELETE FROM store_pinned_groups WHERE service = ? AND group_key = ?`,
			service, groupKey); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE store_objects SET pinned = ? WHERE service = ? AND group_key = ?`,
			pinned, service, groupKey)
		return err
	})
}
