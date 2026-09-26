package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/db"
)

// Backups and restores (docs/ARCHITECTURE.md 3, 6.1). A backup never
// contains accounts, and a restore replaces the configuration, never the
// accounts: whoever can upload a backup must not be able to plant a password
// hash, drop TOTP, revive API tokens or erase the audit log with it, and a
// backup download (also possible with an admin API token) never contains a
// password hash that could be cracked offline.

type execQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ScrubBackup removes the accounts from a backup copy: users (password
// hashes, TOTP secrets), sessions and API tokens. The audit log stays.
//
// The copy's triggers and views go first (PiCache creates none; VACUUM INTO
// copies whatever the live database has), and the tables are verified to
// be empty afterwards, so a planted "BEFORE DELETE … RAISE(IGNORE)" cannot
// leave password hashes in a backup.
func ScrubBackup(ctx context.Context, x execQuerier) error {
	if _, err := db.DropTriggersAndViews(ctx, x); err != nil {
		return err
	}
	for _, t := range []string{"auth_sessions", "auth_tokens", "auth_users"} {
		if _, err := x.ExecContext(ctx, `DELETE FROM `+t); err != nil {
			if strings.Contains(err.Error(), "no such table") {
				continue
			}
			return err
		}
		if err := checkEmpty(ctx, x, t); err != nil {
			return err
		}
	}
	return nil
}

// authObjectsQuery lists the schema objects that belong to the auth tables
// (tables, their indexes and automatic indexes; LIKE is case-insensitive, so
// look-alike names are included and then refused).
const authObjectsQuery = `SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master
	WHERE name LIKE 'auth\_%' ESCAPE '\' OR tbl_name LIKE 'auth\_%' ESCAPE '\' ORDER BY type, name`

type schemaObject struct{ typ, name, table, sql string }

func schemaObjects(ctx context.Context, q execQuerier, query string) ([]schemaObject, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []schemaObject
	for rows.Next() {
		var o schemaObject
		if err := rows.Scan(&o.typ, &o.name, &o.table, &o.sql); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// referenceAuthSchema returns the auth schema objects that migrations 1..version create.
func referenceAuthSchema(ctx context.Context, version int) ([]schemaObject, error) {
	ref, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	defer ref.Close()
	ref.SetMaxOpenConns(1) // one in-memory database
	for _, step := range migrations[:version] {
		if _, err := ref.ExecContext(ctx, step); err != nil {
			return nil, err
		}
	}
	return schemaObjects(ctx, ref, authObjectsQuery)
}

// CheckBackupSchema verifies the auth part of an uploaded database: its auth
// tables and indexes must be exactly those that the auth migrations create
// for the version recorded in the upload (no added columns, constraints,
// generated columns or indexes), and no other table may reference an auth
// table (a foreign key with ON DELETE RESTRICT could keep API tokens alive).
// The caller refuses triggers and views separately.
func CheckBackupSchema(ctx context.Context, q execQuerier) error {
	var version int
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations WHERE component = 'auth'`).
		Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("the account tables are from a newer PiCache (v%d > v%d)", version, len(migrations))
	}
	got, err := schemaObjects(ctx, q, authObjectsQuery)
	if err != nil {
		return err
	}
	want, err := referenceAuthSchema(ctx, version)
	if err != nil {
		return err
	}
	if !slices.Equal(got, want) {
		return errors.New("the account tables differ from those PiCache creates")
	}
	rows, err := q.QueryContext(ctx, `SELECT m.name, f."table" FROM sqlite_master m, pragma_foreign_key_list(m.name) f
		WHERE m.type = 'table'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			return err
		}
		if strings.HasPrefix(strings.ToLower(to), "auth_") && !strings.HasPrefix(strings.ToLower(from), "auth_") {
			return fmt.Errorf("table %q refers to the account table %q", from, to)
		}
	}
	return rows.Err()
}

// CarryOverAccounts makes a staged restore keep the accounts of the running
// instance. It migrates the auth tables of the staged database d (opened
// with a single connection) to the current version and replaces its users,
// API tokens and audit log with those of the live database at livePath (""
// when there is none: the instance then starts in setup mode). All sessions
// end, so everyone signs in again, and the restore is recorded in the audit
// log.
//
// Columns are copied by name (the live database may use an older or newer
// auth schema version than the backup), so the accounts keep their roles;
// the accounts of a live database from before roles (auth v1) were all
// admins and stay admins.
func CarryOverAccounts(ctx context.Context, d *db.DB, livePath string) error {
	if err := d.Migrate(ctx, "auth", migrations); err != nil {
		return err
	}
	conn, err := d.W.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	live := false
	if livePath != "" {
		if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS live`, livePath); err != nil {
			return fmt.Errorf("open the current database: %w", err)
		}
		defer conn.ExecContext(context.WithoutCancel(ctx), `DETACH DATABASE live`)
		if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM live.sqlite_master
			WHERE type = 'table' AND name = 'auth_users')`).Scan(&live); err != nil {
			return err
		}
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	kept := []string{"auth_users", "auth_tokens", "auth_audit"}
	for _, t := range append([]string{"auth_sessions"}, kept...) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM main.`+t); err != nil {
			return err
		}
	}
	if err := checkEmpty(ctx, tx, "main.auth_sessions", "main.auth_users", "main.auth_tokens", "main.auth_audit"); err != nil {
		return err
	}
	if live {
		for _, t := range kept {
			if err := copyLiveTable(ctx, tx, t); err != nil {
				return fmt.Errorf("keep %s: %w", t, err)
			}
			if err := sameCount(ctx, tx, t); err != nil {
				return err
			}
		}
		// Accounts of a live database before roles (auth v1) were all
		// admins; copied by name they would get the column default.
		var hasRole bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pragma_table_info('auth_users', 'live')
			WHERE name = 'role')`).Scan(&hasRole); err != nil {
			return err
		}
		if !hasRole {
			if _, err := tx.ExecContext(ctx, `UPDATE main.auth_users SET role = 'admin'`); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO main.auth_audit (time, username, ip, action, target, details)
		VALUES (?, 'system', '', 'system.restore.apply', '', '')`, db.NowMs()); err != nil {
		return err
	}
	return tx.Commit()
}

// sameCount verifies that main.<table> holds as many rows as live.<table>.
func sameCount(ctx context.Context, tx *sql.Tx, table string) error {
	var got, want int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM main.`+table+`), (SELECT COUNT(*) FROM live.`+table+`)`).
		Scan(&got, &want); err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("keep %s: %d of %d rows copied", table, got, want)
	}
	return nil
}

// copyLiveTable copies the rows of live.<table> into main.<table> (the
// columns both have).
func copyLiveTable(ctx context.Context, tx *sql.Tx, table string) error {
	cols := func(schema string) ([]string, error) {
		rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?, ?) ORDER BY cid`, table, schema)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return nil, err
			}
			out = append(out, n)
		}
		return out, rows.Err()
	}
	mainCols, err := cols("main")
	if err != nil {
		return err
	}
	liveCols, err := cols("live")
	if err != nil {
		return err
	}
	var common []string
	for _, c := range mainCols {
		if slices.Contains(liveCols, c) {
			common = append(common, `"`+strings.ReplaceAll(c, `"`, `""`)+`"`)
		}
	}
	if len(common) == 0 {
		return nil // no such table in the live database
	}
	list := strings.Join(common, ", ")
	_, err = tx.ExecContext(ctx, `INSERT INTO main.`+table+` (`+list+`) SELECT `+list+` FROM live.`+table)
	return err
}
