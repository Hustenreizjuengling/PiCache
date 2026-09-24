package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// migrations of the "storage" component (append-only).
var migrations = []string{
	`CREATE TABLE storage_targets (
		id                 TEXT    PRIMARY KEY,
		name               TEXT    NOT NULL,
		kind               TEXT    NOT NULL,
		mode               TEXT    NOT NULL,
		path               TEXT    NOT NULL DEFAULT '',
		server             TEXT    NOT NULL DEFAULT '',
		share              TEXT    NOT NULL DEFAULT '',
		export             TEXT    NOT NULL DEFAULT '',
		subdir             TEXT    NOT NULL DEFAULT '',
		username           TEXT    NOT NULL DEFAULT '',
		domain             TEXT    NOT NULL DEFAULT '',
		password_sealed    TEXT    NULL,
		smb_version        TEXT    NOT NULL DEFAULT '',
		smb_seal           INTEGER NOT NULL DEFAULT 0,
		nfs_version        TEXT    NOT NULL DEFAULT '',
		nfs_nconnect       INTEGER NOT NULL DEFAULT 0,
		require_mountpoint INTEGER NOT NULL DEFAULT 0,
		store_id           TEXT    NOT NULL DEFAULT '',
		created_at         INTEGER NOT NULL,
		updated_at         INTEGER NOT NULL
	)`,
}

// targetColumns never include the sealed password itself.
const targetColumns = `id, name, kind, mode, path, server, share, export, subdir, username, domain,
	COALESCE(password_sealed, '') <> '', smb_version, smb_seal, nfs_version, nfs_nconnect,
	require_mountpoint, store_id, created_at, updated_at`

// maxRows bounds how many rows are read (more than maxTargets can only come
// from a tampered or restored database; the surplus is ignored).
const maxRows = 2 * maxTargets

type scanner interface{ Scan(dest ...any) error }

func scanTarget(sc scanner) (Target, error) {
	var t Target
	var kind, mode string
	var created, updated int64
	err := sc.Scan(&t.ID, &t.Name, &kind, &mode, &t.Path, &t.Server, &t.Share, &t.Export, &t.Subdir,
		&t.Username, &t.Domain, &t.HasPassword, &t.SMBVersion, &t.SMBSeal, &t.NFSVersion, &t.NFSNConnect,
		&t.RequireMountpoint, &t.StoreID, &created, &updated)
	t.Kind, t.Mode = Kind(kind), Mode(mode)
	t.CreatedAt, t.UpdatedAt = db.Time(created), db.Time(updated)
	return t, err
}

func loadTargets(ctx context.Context, q *sql.DB) ([]Target, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+targetColumns+` FROM storage_targets ORDER BY created_at, id LIMIT ?`, maxRows)
	if err != nil {
		return nil, fmt.Errorf("storage: load targets: %w", err)
	}
	defer rows.Close()
	var out []Target
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, fmt.Errorf("storage: load targets: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func loadTarget(ctx context.Context, q *sql.DB, id string) (Target, error) {
	t, err := scanTarget(q.QueryRowContext(ctx, `SELECT `+targetColumns+` FROM storage_targets WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Target{}, apperr.NotFound("storage target", id)
	}
	if err != nil {
		return Target{}, fmt.Errorf("storage: load target: %w", err)
	}
	return t, nil
}

// loadSealedPassword returns the sealed password ("" if none).
func loadSealedPassword(ctx context.Context, q *sql.DB, id string) (string, error) {
	var sealed sql.NullString
	err := q.QueryRowContext(ctx, `SELECT password_sealed FROM storage_targets WHERE id = ?`, id).Scan(&sealed)
	if err != nil {
		return "", fmt.Errorf("storage: load password: %w", err)
	}
	return sealed.String, nil
}

func nullable(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func insertTarget(ctx context.Context, w *sql.DB, t Target, sealed string) error {
	_, err := w.ExecContext(ctx, `INSERT INTO storage_targets (id, name, kind, mode, path, server, share,
		export, subdir, username, domain, password_sealed, smb_version, smb_seal, nfs_version, nfs_nconnect,
		require_mountpoint, store_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, string(t.Kind), string(t.Mode), t.Path, t.Server, t.Share, t.Export, t.Subdir,
		t.Username, t.Domain, nullable(sealed), t.SMBVersion, t.SMBSeal, t.NFSVersion, t.NFSNConnect,
		t.RequireMountpoint, t.StoreID, db.Ms(t.CreatedAt), db.Ms(t.UpdatedAt))
	if err != nil {
		return fmt.Errorf("storage: insert target: %w", err)
	}
	return nil
}

// updateTarget writes all fields; the password column only if setPassword.
func updateTarget(ctx context.Context, w *sql.DB, t Target, sealed string, setPassword bool) error {
	_, err := w.ExecContext(ctx, `UPDATE storage_targets SET name = ?, kind = ?, mode = ?, path = ?,
		server = ?, share = ?, export = ?, subdir = ?, username = ?, domain = ?,
		password_sealed = CASE WHEN ? THEN ? ELSE password_sealed END,
		smb_version = ?, smb_seal = ?, nfs_version = ?, nfs_nconnect = ?, require_mountpoint = ?,
		updated_at = ? WHERE id = ?`,
		t.Name, string(t.Kind), string(t.Mode), t.Path, t.Server, t.Share, t.Export, t.Subdir,
		t.Username, t.Domain, setPassword, nullable(sealed), t.SMBVersion, t.SMBSeal, t.NFSVersion,
		t.NFSNConnect, t.RequireMountpoint, db.Ms(t.UpdatedAt), t.ID)
	if err != nil {
		return fmt.Errorf("storage: update target: %w", err)
	}
	return nil
}
