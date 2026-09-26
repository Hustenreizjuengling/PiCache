package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/version"
)

const maxRestoreBytes = 512 << 20

var appMigrations = []string{
	`CREATE TABLE app_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);`,
}

// restoreMu lets only one upload be staged at a time.
var restoreMu sync.Mutex

// Backup writes a consistent copy of picache.db (VACUUM INTO) to w. Accounts
// (users with their password hashes and TOTP secrets, sessions, API tokens)
// are always removed: a restore never replaces them, and a download (also
// possible with an admin API token) must not give a password hash to crack
// offline. Sealed NAS passwords and notification secrets only survive with
// includeSecrets (they need the master key, which is never part of a
// backup).
func (a *App) Backup(ctx context.Context, w io.Writer, includeSecrets bool) error {
	tmp := filepath.Join(a.cfg.DataDir, "tmp", fmt.Sprintf("backup-%d.db", time.Now().UnixNano()))
	defer os.Remove(tmp)
	defer os.Remove(tmp + "-journal")
	if _, err := a.cdb.W.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if err := scrubBackup(ctx, tmp, includeSecrets); err != nil {
		return fmt.Errorf("backup: scrub: %w", err)
	}
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

func scrubBackup(ctx context.Context, path string, includeSecrets bool) error {
	d, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(DELETE)&_pragma=trusted_schema(0)")
	if err != nil {
		return err
	}
	defer d.Close()
	if err := auth.ScrubBackup(ctx, d); err != nil {
		return err
	}
	if !includeSecrets {
		for _, c := range sealedColumns {
			if err := clearColumn(ctx, d, c.table, c.column, c.what); err != nil {
				return err
			}
		}
	}
	// VACUUM rewrites the file, so deleted secrets are not left in free pages.
	_, err = d.ExecContext(ctx, `VACUUM`)
	return err
}

// sealedColumns hold secrets sealed with the master key; backups keep them
// only with includeSecrets.
var sealedColumns = []struct{ table, column, what string }{
	{"storage_targets", "password_sealed", "sealed NAS passwords"},
	{"notify_channels", "secret_sealed", "sealed notification secrets"},
}

// clearColumn sets column to NULL in every row of table and verifies it (a
// missing table, e.g. in a copy of an older version, is fine).
func clearColumn(ctx context.Context, d *sql.DB, table, column, what string) error {
	_, err := d.ExecContext(ctx, `UPDATE `+table+` SET `+column+` = NULL`)
	switch {
	case err != nil && strings.Contains(err.Error(), "no such table"):
		return nil
	case err != nil:
		return err
	}
	var left int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE `+column+` IS NOT NULL`).Scan(&left); err != nil {
		return err
	}
	if left != 0 {
		return fmt.Errorf("%d %s could not be removed", left, what)
	}
	return nil
}

// StageRestore validates an uploaded picache.db against the live database
// and stages it; it replaces the configuration on the next start (see
// applyStagedRestore). It returns the staged settings as that start will
// load them (nil when they cannot be decoded). Only one upload is handled
// at a time, each in its own temporary file.
func (a *App) StageRestore(ctx context.Context, r io.Reader) (*settings.All, error) {
	if !restoreMu.TryLock() {
		return nil, apperr.Conflict("another backup is being uploaded; try again when it is done")
	}
	defer restoreMu.Unlock()
	if a.cdb == nil {
		return nil, apperr.Unavailable("the configuration database is not open")
	}
	live, err := schemaNames(ctx, a.cdb.R)
	if err != nil {
		return nil, fmt.Errorf("restore: read the current schema: %w", err)
	}
	staged := a.paths.ConfigDB + ".restore"
	f, err := os.CreateTemp(filepath.Dir(staged), filepath.Base(staged)+"-*.tmp")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer func() {
		for _, p := range []string{tmp, tmp + "-journal", tmp + "-wal", tmp + "-shm"} {
			_ = os.Remove(p)
		}
	}()
	n, err := io.Copy(f, io.LimitReader(r, maxRestoreBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if n > maxRestoreBytes {
		return nil, apperr.Invalid("file", "backup is larger than 512 MiB")
	}
	if err := validateBackup(ctx, tmp, live); err != nil {
		return nil, apperr.Wrap(apperr.KindInvalid, err, "not a usable PiCache backup: %v", err)
	}
	// The settings as the next start will load them, so the API can warn a
	// requester the restored web access would lock out.
	set, err := stagedSettings(ctx, tmp)
	if err != nil {
		a.log.Warn("restore: the staged settings cannot be decoded; the restore will be refused at the next start", slog.Any("err", err))
	}
	if err := os.Rename(tmp, staged); err != nil {
		return nil, err
	}
	a.log.Warn("configuration restore staged; it will be applied on the next restart")
	return set, nil
}

// stagedSettings decodes the settings document of an uploaded database
// (read-only) like the next start will (settings.DecodeStored).
func stagedSettings(ctx context.Context, path string) (*settings.All, error) {
	d, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=query_only(1)&_pragma=trusted_schema(0)")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	var doc string
	if err := d.QueryRowContext(ctx, `SELECT doc FROM settings WHERE id = 1`).Scan(&doc); err != nil {
		return nil, err
	}
	return settings.DecodeStored([]byte(doc))
}

// schemaObject is one table or index of sqlite_master.
type schemaObject struct{ typ, name string }

type rowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// schemaNames returns the tables and indexes of a database with their SQL
// ("" for the automatic indexes of UNIQUE and PRIMARY KEY constraints).
func schemaNames(ctx context.Context, q rowsQuerier) (map[schemaObject]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT type, name, COALESCE(sql, '') FROM sqlite_master WHERE type IN ('table', 'index')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[schemaObject]string{}
	for rows.Next() {
		var o schemaObject
		var stmt string
		if err := rows.Scan(&o.typ, &o.name, &stmt); err != nil {
			return nil, err
		}
		out[o] = stmt
	}
	return out, rows.Err()
}

// validateBackup checks an uploaded database: integrity, a PiCache schema
// that is not newer than this binary, and nothing but tables and indexes
// that the live database (live: its schemaNames) has too, indexes with the
// same definition. Triggers and views
// are refused because they would run on every later write with the rights
// of the service (e.g. "BEFORE DELETE ON auth_tokens ... RAISE(IGNORE)"
// keeps revoked API tokens alive); PiCache creates none, nor virtual tables
// or generated columns. The account tables must match the schema PiCache
// creates exactly (auth.CheckBackupSchema). The file is opened read-only
// with trusted_schema off.
func validateBackup(ctx context.Context, path string, live map[schemaObject]string) error {
	d, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=query_only(1)&_pragma=trusted_schema(0)")
	if err != nil {
		return err
	}
	defer d.Close()
	var res string
	if err := d.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return errors.New("integrity check failed")
	}
	if err := checkSchemaVersions(ctx, d); err != nil {
		return err
	}
	if err := checkPlainSchema(ctx, d, live); err != nil {
		return err
	}
	return auth.CheckBackupSchema(ctx, d)
}

func checkSchemaVersions(ctx context.Context, d *sql.DB) error {
	rows, err := d.QueryContext(ctx, `SELECT component, MAX(version) FROM schema_migrations GROUP BY component`)
	if err != nil {
		return errors.New("missing PiCache schema")
	}
	defer rows.Close()
	known := db.SchemaVersions()
	found := false
	for rows.Next() {
		var comp string
		var v int
		if err := rows.Scan(&comp, &v); err != nil {
			return err
		}
		if comp == "settings" {
			found = true
		}
		if max, ok := known[comp]; ok && v > max {
			return fmt.Errorf("backup was made by a newer PiCache (component %s v%d > v%d); update PiCache first", comp, v, max)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !found {
		return errors.New("missing PiCache settings")
	}
	return nil
}

// sqliteInternalTables are tables SQLite itself may create (AUTOINCREMENT,
// ANALYZE); they are allowed even if the live database lacks them.
var sqliteInternalTables = []string{"sqlite_sequence", "sqlite_stat1", "sqlite_stat4"}

// checkPlainSchema refuses every schema object that PiCache never creates:
// triggers, views, virtual tables, generated columns, tables or indexes
// whose names the live database does not have, and indexes that differ
// from the live index of the same name. Index definitions never change once
// created (migrations are append-only, and the only rename, a column of
// client_clients in clients v2, touches no index), so a PiCache backup of
// any version has the live definition.
// Tables are matched by name only: an older backup is migrated like any
// older database at the start that applies it. This includes the
// automatic indexes of UNIQUE and PRIMARY KEY constraints
// (sqlite_autoindex_*, no SQL): a named index renamed to look like one
// (writable_schema) has SQL or a name the live database lacks and is
// refused, so an upload cannot add e.g. a UNIQUE index to a known table.
func checkPlainSchema(ctx context.Context, d *sql.DB, live map[schemaObject]string) error {
	rows, err := d.QueryContext(ctx, `SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var typ, name, table, stmt string
		if err := rows.Scan(&typ, &name, &table, &stmt); err != nil {
			return err
		}
		liveSQL, known := live[schemaObject{typ, name}]
		switch {
		case typ != "table" && typ != "index":
			return fmt.Errorf("the database contains a %s (%q); PiCache backups have none", typ, name)
		case typ == "table" && strings.HasPrefix(strings.ToUpper(normalizeSQL(stmt)), "CREATE VIRTUAL TABLE"):
			return fmt.Errorf("the database contains a virtual table (%q); PiCache backups have none", name)
		case typ == "index" && known:
			// Automatic indexes have no SQL on both sides; a named index
			// renamed to an automatic index's name has SQL and differs.
			if normalizeSQL(stmt) != normalizeSQL(liveSQL) {
				return fmt.Errorf("the index %q differs from the one this PiCache has", name)
			}
		case known:
		case typ == "table" && slices.Contains(sqliteInternalTables, name):
		default:
			return fmt.Errorf("the database contains the %s %q, which this PiCache does not have", typ, name)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var table string
	err = d.QueryRowContext(ctx, `SELECT m.name FROM sqlite_master m, pragma_table_xinfo(m.name) x
		WHERE m.type = 'table' AND x.hidden != 0 LIMIT 1`).Scan(&table)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	return fmt.Errorf("table %q has generated columns; PiCache backups have none", table)
}

// normalizeSQL collapses the whitespace of a CREATE statement.
func normalizeSQL(stmt string) string { return strings.Join(strings.Fields(stmt), " ") }

// applyStagedRestore swaps in a staged backup before the database is opened.
// The staged file is checked again (it may have been staged by an older
// PiCache or changed on disk) and gets the accounts of the live database
// (auth.CarryOverAccounts): a restore replaces the configuration, never the
// users, their passwords and TOTP, the API tokens or the audit log, and it
// ends all sessions. A staged file that fails is set aside as
// picache.db.failed-restore-<timestamp> and the live database stays.
//
// A backup of an older version needs no conversion here: it keeps its
// schema versions, and the component migrations that run when build opens
// the swapped-in database bring it up to date (e.g. settings v2 moves the
// download cache section to "downloadCache", clients v2 renames the
// bypass column).
func (a *App) applyStagedRestore() (bool, error) {
	staged := a.paths.ConfigDB + ".restore"
	if _, err := os.Stat(staged); err != nil {
		return false, nil
	}
	if err := a.prepareRestore(context.Background(), staged); err != nil {
		failed := fmt.Sprintf("%s.failed-restore-%s", a.paths.ConfigDB, time.Now().UTC().Format("20060102T150405"))
		if rerr := os.Rename(staged, failed); rerr != nil {
			_ = os.Remove(staged)
			failed = ""
		}
		for _, sfx := range []string{"-journal", "-wal", "-shm"} {
			_ = os.Remove(staged + sfx)
		}
		a.log.Error("the staged configuration restore was refused; the current configuration stays",
			slog.String("file", failed), slog.Any("err", err))
		return false, nil
	}
	backup := a.paths.ConfigDB + ".before-restore"
	_ = os.Remove(backup)
	if _, err := os.Stat(a.paths.ConfigDB); err == nil {
		if err := os.Rename(a.paths.ConfigDB, backup); err != nil {
			return false, fmt.Errorf("restore: keep old database: %w", err)
		}
	}
	for _, sfx := range []string{"-wal", "-shm"} {
		_ = os.Remove(a.paths.ConfigDB + sfx)
	}
	if err := os.Rename(staged, a.paths.ConfigDB); err != nil {
		return false, fmt.Errorf("restore: %w", err)
	}
	a.restoredAt = time.Now()
	a.log.Warn("applied staged configuration restore (accounts, API tokens and audit log kept, sessions ended)",
		slog.String("previous", backup))
	return true, nil
}

// prepareRestore validates the staged file against the live database and
// carries the live accounts over into it. Attaching the live database also
// folds a leftover WAL into it, so picache.db.before-restore is complete.
func (a *App) prepareRestore(ctx context.Context, staged string) error {
	if _, err := os.Stat(a.paths.ConfigDB); err != nil {
		return fmt.Errorf("no current database to check the backup against: %w", err)
	}
	live, err := liveSchemaNames(ctx, a.paths.ConfigDB)
	if err != nil {
		return err
	}
	if err := validateBackup(ctx, staged, live); err != nil {
		return err
	}
	sdb, err := sql.Open("sqlite", "file:"+staged+"?_pragma=busy_timeout(5000)&_pragma=trusted_schema(0)"+
		"&_pragma=foreign_keys(0)&_pragma=journal_mode(DELETE)")
	if err != nil {
		return err
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1) // ATTACH and the migration must share one connection
	sdb.SetMaxIdleConns(1)
	sdb.SetConnMaxLifetime(0)
	if err := auth.CarryOverAccounts(ctx, &db.DB{W: sdb, R: sdb, Path: staged}, a.paths.ConfigDB); err != nil {
		return err
	}
	return sdb.Close()
}

// liveSchemaNames reads the tables and indexes of the (not yet opened) live
// database.
func liveSchemaNames(ctx context.Context, path string) (map[schemaObject]string, error) {
	d, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=query_only(1)&_pragma=trusted_schema(0)")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := schemaNames(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("read the current schema: %w", err)
	}
	return names, nil
}

// rollbackRestore puts the pre-restore database back after a failed start.
func (a *App) rollbackRestore() error {
	backup := a.paths.ConfigDB + ".before-restore"
	if _, err := os.Stat(backup); err != nil {
		return err
	}
	failed := fmt.Sprintf("%s.failed-restore-%s", a.paths.ConfigDB, time.Now().UTC().Format("20060102T150405"))
	_ = os.Rename(a.paths.ConfigDB, failed)
	for _, sfx := range []string{"-wal", "-shm"} {
		_ = os.Remove(a.paths.ConfigDB + sfx)
	}
	a.restoredAt = time.Time{}
	return os.Rename(backup, a.paths.ConfigDB)
}

// removePlantedSchema drops every trigger and view from picache.db at start.
// PiCache creates neither; one can only come from a restore made before
// uploads were checked, or from an edit on disk, and it could keep revoked
// sessions or API tokens alive or password hashes in backups. The start
// fails if one cannot be removed.
func (a *App) removePlantedSchema(ctx context.Context) error {
	dropped, err := db.DropTriggersAndViews(ctx, a.cdb.W)
	for _, o := range dropped {
		a.log.Warn("removed a trigger or view from the configuration database; PiCache creates none, "+
			"so it was planted (e.g. by a backup restored with an older version): check the audit log, "+
			"and change the password or run `picache reset-password`", slog.String("object", o))
	}
	if err != nil {
		return fmt.Errorf("remove planted triggers and views: %w", err)
	}
	return nil
}

// preUpgradeBackup keeps a copy of picache.db whenever the binary version
// changes (newest 3 kept in <data>/backups), so a rollback is possible
// (docs/ARCHITECTURE.md 14.4). The copy is made before this version
// migrates anything, its own app_meta table included: a rollback puts it
// back for the previous version, which refuses newer schema versions. An
// error fails the start, so nothing is migrated without a copy.
func (a *App) preUpgradeBackup(ctx context.Context) error {
	return PreUpgradeBackup(ctx, a.cdb, a.cfg.DataDir, a.log)
}

// PreUpgradeBackup is the pre-upgrade copy of the service start
// (preUpgradeBackup) for d, the picache.db of dataDir. A CLI command that
// migrates picache.db (`picache reset-password`) calls it first: run with a
// new binary before the service's first start, the command would otherwise
// migrate the database, and the copy made at that start would no longer
// open with the previous version. The new version is recorded, so the
// service does not copy the migrated database again under the previous
// version's name.
func PreUpgradeBackup(ctx context.Context, d *db.DB, dataDir string, log *slog.Logger) error {
	var hadSchema int
	_ = d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'schema_migrations'`).Scan(&hadSchema)
	var prev string // stays "" while app_meta does not exist yet
	_ = d.R.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key = 'binary_version'`).Scan(&prev)
	cur := version.Version
	if hadSchema > 0 && prev != "" && prev != cur && cur != "dev" {
		dir := filepath.Join(dataDir, "backups")
		name := fmt.Sprintf("picache-%s-%s.db", sanitizeFile(prev), time.Now().UTC().Format("20060102T150405"))
		err := os.MkdirAll(dir, 0o750) // the CLI may run before the service ever created it
		if err == nil {
			_, err = d.W.ExecContext(ctx, `VACUUM INTO ?`, filepath.Join(dir, name))
		}
		if err != nil {
			return fmt.Errorf("copy picache.db to %s before the upgrade from %s (is the disk full?): %w", dir, prev, err)
		}
		log.Info("saved configuration backup before upgrade", slog.String("file", filepath.Join(dir, name)))
		pruneBackups(dir, 3)
	}
	if err := d.Migrate(ctx, "app", appMigrations); err != nil {
		return err
	}
	if prev == cur {
		return nil
	}
	_, err := d.W.ExecContext(ctx, `INSERT INTO app_meta (key, value) VALUES ('binary_version', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, cur)
	return err
}

func sanitizeFile(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
			b[i] = '_'
		}
	}
	return string(b)
}

func pruneBackups(dir string, keep int) {
	matches, _ := filepath.Glob(filepath.Join(dir, "picache-*.db"))
	if len(matches) <= keep {
		return
	}
	type fi struct {
		path string
		mod  time.Time
	}
	var files []fi
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil {
			files = append(files, fi{m, st.ModTime()})
		}
	}
	slices.SortFunc(files, func(x, y fi) int { return y.mod.Compare(x.mod) })
	for _, f := range files[min(keep, len(files)):] {
		_ = os.Remove(f.path)
	}
}
