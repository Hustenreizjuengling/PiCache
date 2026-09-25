package clients

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// Configuration limits.
const (
	maxGroups           = 1024
	maxClients          = 10000
	maxIdentifiers      = 32
	maxGroupsPerClient  = 64
	maxNameLen          = 64
	maxCommentLen       = 512
	defaultGroupComment = "Clients without another group"
)

// migrations of component "clients". Append only: a released step is never
// edited (v1 keeps the column name of 0.1.x, v2 renames it).
var migrations = []string{
	// v1
	`CREATE TABLE client_groups (
		id         INTEGER PRIMARY KEY,
		name       TEXT    NOT NULL UNIQUE COLLATE NOCASE,
		comment    TEXT    NOT NULL DEFAULT '',
		enabled    INTEGER NOT NULL DEFAULT 1,
		created_at INTEGER NOT NULL
	);
	INSERT INTO client_groups (id, name, comment, enabled, created_at)
		VALUES (1, 'Default', '` + defaultGroupComment + `', 1, CAST(strftime('%s', 'now') AS INTEGER) * 1000);
	CREATE TABLE client_clients (
		id              INTEGER PRIMARY KEY,
		name            TEXT    NOT NULL,
		comment         TEXT    NOT NULL DEFAULT '',
		lancache_bypass INTEGER NOT NULL DEFAULT 0,
		ignore_logs     INTEGER NOT NULL DEFAULT 0,
		created_at      INTEGER NOT NULL,
		updated_at      INTEGER NOT NULL
	);
	CREATE TABLE client_identifiers (
		value     TEXT    PRIMARY KEY,
		client_id INTEGER NOT NULL REFERENCES client_clients(id) ON DELETE CASCADE,
		kind      TEXT    NOT NULL,
		pos       INTEGER NOT NULL
	);
	CREATE INDEX client_identifiers_client ON client_identifiers(client_id);
	CREATE TABLE client_memberships (
		client_id INTEGER NOT NULL REFERENCES client_clients(id) ON DELETE CASCADE,
		group_id  INTEGER NOT NULL REFERENCES client_groups(id) ON DELETE CASCADE,
		PRIMARY KEY (client_id, group_id)
	) WITHOUT ROWID;
	CREATE INDEX client_memberships_group ON client_memberships(group_id);`,
	// v2 (0.2.0): the per-client bypass flag is named after the download
	// cache. No index uses the column, so restored backups of older versions
	// keep matching the live index definitions (app.checkPlainSchema).
	`ALTER TABLE client_clients RENAME COLUMN lancache_bypass TO download_cache_bypass;`,
}

// reload rebuilds the identification snapshot from the database and the
// learned MACs that depend on it.
func (r *Registry) reload(ctx context.Context) error {
	groups, err := r.Groups(ctx)
	if err != nil {
		return err
	}
	cl, err := r.Clients(ctx)
	if err != nil {
		return err
	}
	r.snap.Store(newSnapshot(groups, cl))
	r.rebuildLearned() // callers invalidate the identity cache afterwards
	return nil
}

// write runs fn in a transaction, then reloads the snapshot and notifies
// listeners. Writes are serialised so snapshots are never stored out of order.
func (r *Registry) write(ctx context.Context, fn func(*sql.Tx) error) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if err := r.db.Tx(ctx, fn); err != nil {
		return err
	}
	if err := r.reload(ctx); err != nil {
		return fmt.Errorf("clients: reload: %w", err)
	}
	r.changed()
	return nil
}

// cleanText trims s and checks its length and characters.
func cleanText(field, s string, required bool, max int) (string, error) {
	s = strings.TrimSpace(s)
	if required && s == "" {
		return "", apperr.Invalid(field, "required")
	}
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
		return "", apperr.Invalid(field, "must be valid text of at most %d characters", max)
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return "", apperr.Invalid(field, "must not contain control characters")
		}
	}
	return s, nil
}

// --- groups ---

// Groups lists all groups.
func (r *Registry) Groups(ctx context.Context) ([]Group, error) {
	rows, err := r.db.R.QueryContext(ctx, `SELECT g.id, g.name, g.comment, g.enabled, g.created_at,
		(SELECT COUNT(*) FROM client_memberships m WHERE m.group_id = g.id)
		FROM client_groups g ORDER BY g.id`)
	if err != nil {
		return nil, fmt.Errorf("clients: list groups: %w", err)
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		var g Group
		var created int64
		if err := rows.Scan(&g.ID, &g.Name, &g.Comment, &g.Enabled, &created, &g.ClientCount); err != nil {
			return nil, fmt.Errorf("clients: scan group: %w", err)
		}
		g.CreatedAt = db.Time(created)
		out = append(out, g)
	}
	return out, rows.Err()
}

func (r *Registry) group(ctx context.Context, id int64) (Group, error) {
	var g Group
	var created int64
	err := r.db.R.QueryRowContext(ctx, `SELECT g.id, g.name, g.comment, g.enabled, g.created_at,
		(SELECT COUNT(*) FROM client_memberships m WHERE m.group_id = g.id)
		FROM client_groups g WHERE g.id = ?`, id).Scan(&g.ID, &g.Name, &g.Comment, &g.Enabled, &created, &g.ClientCount)
	if errors.Is(err, sql.ErrNoRows) {
		return g, apperr.NotFound("group", id)
	}
	if err != nil {
		return g, fmt.Errorf("clients: get group: %w", err)
	}
	g.CreatedAt = db.Time(created)
	return g, nil
}

func validateGroup(in GroupInput) (GroupInput, error) {
	var err error
	if in.Name, err = cleanText("name", in.Name, true, maxNameLen); err != nil {
		return in, err
	}
	if in.Comment, err = cleanText("comment", in.Comment, false, maxCommentLen); err != nil {
		return in, err
	}
	return in, nil
}

// groupNameTaken reports whether another group (not id) already uses name.
func groupNameTaken(ctx context.Context, tx *sql.Tx, name string, id int64) error {
	var other int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM client_groups WHERE name = ? COLLATE NOCASE AND id != ?`, name, id).Scan(&other)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	return apperr.Conflict("a group named %q already exists", name)
}

// CreateGroup creates a group.
func (r *Registry) CreateGroup(ctx context.Context, in GroupInput) (Group, error) {
	in, err := validateGroup(in)
	if err != nil {
		return Group{}, err
	}
	var id int64
	err = r.write(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM client_groups`).Scan(&n); err != nil {
			return err
		}
		if n >= maxGroups {
			return apperr.Conflict("at most %d groups are allowed", maxGroups)
		}
		if err := groupNameTaken(ctx, tx, in.Name, 0); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO client_groups (name, comment, enabled, created_at) VALUES (?, ?, ?, ?)`,
			in.Name, in.Comment, in.Enabled, db.NowMs())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return Group{}, err
	}
	return r.group(ctx, id)
}

// UpdateGroup updates a group.
func (r *Registry) UpdateGroup(ctx context.Context, id int64, in GroupInput) (Group, error) {
	in, err := validateGroup(in)
	if err != nil {
		return Group{}, err
	}
	err = r.write(ctx, func(tx *sql.Tx) error {
		if err := groupNameTaken(ctx, tx, in.Name, id); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE client_groups SET name = ?, comment = ?, enabled = ? WHERE id = ?`,
			in.Name, in.Comment, in.Enabled, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return apperr.NotFound("group", id)
		}
		return nil
	})
	if err != nil {
		return Group{}, err
	}
	return r.group(ctx, id)
}

// DeleteGroup deletes a group (not the Default group). Clients that would be
// left without any group are moved into the Default group, so a device never
// silently loses its filtering.
func (r *Registry) DeleteGroup(ctx context.Context, id int64) error {
	if id == DefaultGroupID {
		return apperr.Forbidden("the Default group cannot be deleted")
	}
	return r.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM client_groups WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return apperr.NotFound("group", id)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO client_memberships (client_id, group_id)
			SELECT c.id, ? FROM client_clients c
			WHERE NOT EXISTS (SELECT 1 FROM client_memberships m WHERE m.client_id = c.id)`, DefaultGroupID)
		return err
	})
}

// --- clients ---

// Clients lists configured clients.
func (r *Registry) Clients(ctx context.Context) ([]Client, error) {
	return r.queryClients(ctx, 0)
}

// queryClients loads all clients (id == 0) or one client.
func (r *Registry) queryClients(ctx context.Context, id int64) ([]Client, error) {
	where, args := "", []any{}
	if id != 0 {
		where, args = " WHERE id = ?", []any{id}
	}
	rows, err := r.db.R.QueryContext(ctx, `SELECT id, name, comment, download_cache_bypass, ignore_logs, created_at, updated_at
		FROM client_clients`+where+` ORDER BY name COLLATE NOCASE, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("clients: list clients: %w", err)
	}
	out := []Client{}
	index := map[int64]int{}
	for rows.Next() {
		var c Client
		var created, updated int64
		if err := rows.Scan(&c.ID, &c.Name, &c.Comment, &c.DownloadCacheBypass, &c.IgnoreLogs, &created, &updated); err != nil {
			rows.Close()
			return nil, fmt.Errorf("clients: scan client: %w", err)
		}
		c.CreatedAt, c.UpdatedAt = db.Time(created), db.Time(updated)
		c.Identifiers, c.GroupIDs = []string{}, []int64{}
		index[c.ID] = len(out)
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	idRows, err := r.db.R.QueryContext(ctx, `SELECT client_id, value FROM client_identifiers ORDER BY client_id, pos`)
	if err != nil {
		return nil, fmt.Errorf("clients: list identifiers: %w", err)
	}
	for idRows.Next() {
		var cid int64
		var v string
		if err := idRows.Scan(&cid, &v); err != nil {
			idRows.Close()
			return nil, err
		}
		if i, ok := index[cid]; ok {
			out[i].Identifiers = append(out[i].Identifiers, v)
		}
	}
	idRows.Close()
	if err := idRows.Err(); err != nil {
		return nil, err
	}

	mRows, err := r.db.R.QueryContext(ctx, `SELECT client_id, group_id FROM client_memberships ORDER BY client_id, group_id`)
	if err != nil {
		return nil, fmt.Errorf("clients: list memberships: %w", err)
	}
	defer mRows.Close()
	for mRows.Next() {
		var cid, gid int64
		if err := mRows.Scan(&cid, &gid); err != nil {
			return nil, err
		}
		if i, ok := index[cid]; ok {
			out[i].GroupIDs = append(out[i].GroupIDs, gid)
		}
	}
	return out, mRows.Err()
}

func (r *Registry) client(ctx context.Context, id int64) (Client, error) {
	cl, err := r.queryClients(ctx, id)
	if err != nil {
		return Client{}, err
	}
	if len(cl) == 0 {
		return Client{}, apperr.NotFound("client", id)
	}
	return cl[0], nil
}

// validateClient normalises the input and returns the parsed identifiers.
func validateClient(in ClientInput) (ClientInput, []identifier, error) {
	var err error
	if in.Name, err = cleanText("name", in.Name, true, maxNameLen); err != nil {
		return in, nil, err
	}
	if in.Comment, err = cleanText("comment", in.Comment, false, maxCommentLen); err != nil {
		return in, nil, err
	}
	if len(in.Identifiers) == 0 {
		return in, nil, apperr.Invalid("identifiers", "at least one IP address, CIDR or MAC address is required")
	}
	if len(in.Identifiers) > maxIdentifiers {
		return in, nil, apperr.Invalid("identifiers", "at most %d identifiers are allowed", maxIdentifiers)
	}
	ids := make([]identifier, 0, len(in.Identifiers))
	seen := map[string]bool{}
	for i, raw := range in.Identifiers {
		id, ok := parseIdentifier(raw)
		if !ok {
			return in, nil, apperr.Invalid(fmt.Sprintf("identifiers[%d]", i), "must be an IP address, a CIDR (e.g. 192.168.1.0/24) or a MAC address")
		}
		if seen[id.value] {
			continue
		}
		seen[id.value] = true
		ids = append(ids, id)
	}
	in.Identifiers = make([]string, 0, len(ids))
	for _, id := range ids {
		in.Identifiers = append(in.Identifiers, id.value)
	}
	if len(in.GroupIDs) == 0 {
		in.GroupIDs = []int64{DefaultGroupID}
	}
	if len(in.GroupIDs) > maxGroupsPerClient {
		return in, nil, apperr.Invalid("groupIds", "at most %d groups per client are allowed", maxGroupsPerClient)
	}
	groups := slices.Clone(in.GroupIDs)
	slices.Sort(groups)
	in.GroupIDs = slices.Compact(groups)
	return in, ids, nil
}

// saveClientLinks replaces the identifiers and memberships of client id.
func saveClientLinks(ctx context.Context, tx *sql.Tx, id int64, in ClientInput, ids []identifier) error {
	for _, ident := range ids {
		var owner int64
		err := tx.QueryRowContext(ctx, `SELECT client_id FROM client_identifiers WHERE value = ?`, ident.value).Scan(&owner)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case owner != id:
			return apperr.Conflict("identifier %s is already used by another client", ident.value)
		}
	}
	for _, g := range in.GroupIDs {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM client_groups WHERE id = ?`, g).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.Invalid("groupIds", "group %d does not exist", g)
		}
		if err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM client_identifiers WHERE client_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM client_memberships WHERE client_id = ?`, id); err != nil {
		return err
	}
	for i, ident := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT INTO client_identifiers (value, client_id, kind, pos) VALUES (?, ?, ?, ?)`,
			ident.value, id, ident.kind, i); err != nil {
			return err
		}
	}
	for _, g := range in.GroupIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO client_memberships (client_id, group_id) VALUES (?, ?)`, id, g); err != nil {
			return err
		}
	}
	return nil
}

// CreateClient creates a client.
func (r *Registry) CreateClient(ctx context.Context, in ClientInput) (Client, error) {
	in, ids, err := validateClient(in)
	if err != nil {
		return Client{}, err
	}
	var id int64
	err = r.write(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM client_clients`).Scan(&n); err != nil {
			return err
		}
		if n >= maxClients {
			return apperr.Conflict("at most %d clients are allowed", maxClients)
		}
		now := db.NowMs()
		res, err := tx.ExecContext(ctx, `INSERT INTO client_clients (name, comment, download_cache_bypass, ignore_logs, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, in.Name, in.Comment, in.DownloadCacheBypass, in.IgnoreLogs, now, now)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		return saveClientLinks(ctx, tx, id, in, ids)
	})
	if err != nil {
		return Client{}, err
	}
	return r.client(ctx, id)
}

// UpdateClient updates a client.
func (r *Registry) UpdateClient(ctx context.Context, id int64, in ClientInput) (Client, error) {
	in, ids, err := validateClient(in)
	if err != nil {
		return Client{}, err
	}
	err = r.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE client_clients SET name = ?, comment = ?, download_cache_bypass = ?, ignore_logs = ?, updated_at = ?
			WHERE id = ?`, in.Name, in.Comment, in.DownloadCacheBypass, in.IgnoreLogs, db.NowMs(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return apperr.NotFound("client", id)
		}
		return saveClientLinks(ctx, tx, id, in, ids)
	})
	if err != nil {
		return Client{}, err
	}
	return r.client(ctx, id)
}

// DeleteClient deletes a client.
func (r *Registry) DeleteClient(ctx context.Context, id int64) error {
	return r.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM client_clients WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return apperr.NotFound("client", id)
		}
		return nil
	})
}
