package app

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
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
	"github.com/hustenreizjuengling/picache/internal/storage"
	"github.com/hustenreizjuengling/picache/internal/update"
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
		// The secrets of the settings (the sync token, the proxy
		// password) are rows of their own.
		if err := clearTable(ctx, d, "settings_secrets", "sealed settings secrets (sync token, proxy password)"); err != nil {
			return err
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

// clearTable deletes every row of table and verifies it (a missing table,
// e.g. in a copy of an older version, is fine).
func clearTable(ctx context.Context, d *sql.DB, table, what string) error {
	_, err := d.ExecContext(ctx, `DELETE FROM `+table)
	switch {
	case err != nil && strings.Contains(err.Error(), "no such table"):
		return nil
	case err != nil:
		return err
	}
	var left int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&left); err != nil {
		return err
	}
	if left != 0 {
		return fmt.Errorf("%d %s could not be removed", left, what)
	}
	return nil
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
func (a *App) StageRestore(ctx context.Context, r io.Reader, sections []string) (*settings.All, error) {
	if !restoreMu.TryLock() {
		return nil, apperr.Conflict("another backup is being uploaded; try again when it is done")
	}
	defer restoreMu.Unlock()
	if a.cdb == nil {
		return nil, apperr.Unavailable("the configuration database is not open")
	}
	if sections != nil {
		var err error
		if sections, err = settings.CheckSections("sections", sections, settings.RestoreSections, "restore"); err != nil {
			return nil, err
		}
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
	if sections != nil {
		// The merge of a partial restore as a dry run: its errors (a group
		// the live data lacks, a missing storage target) come now.
		if err := dryRunRestore(ctx, a.paths.ConfigDB, tmp, sections); err != nil {
			if _, ok := apperr.As(err); ok {
				return nil, err
			}
			return nil, fmt.Errorf("restore: dry run: %w", err)
		}
	}
	if err := writeRestoreMeta(ctx, tmp, sections, ""); err != nil {
		return nil, fmt.Errorf("restore: %w", err)
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
	ctx := context.Background()
	sections, by, err := readRestoreMeta(ctx, staged)
	if err == nil && sections != nil {
		err = a.applyPartialRestore(ctx, staged, sections)
		if err == nil {
			a.restoreSections, a.restoreBy = sections, by
			a.log.Warn("applied a staged partial configuration restore (accounts and sessions kept)",
				slog.Any("sections", sections), slog.String("previous", a.paths.ConfigDB+".before-restore"))
			return true, nil
		}
	}
	if err == nil {
		err = a.prepareRestore(ctx, staged)
	}
	if err != nil {
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
	a.restoreSections, a.restoreBy = slices.Clone(settings.RestoreSections), by
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
	if err := keepLocalStore(ctx, sdb, a.paths.ConfigDB); err != nil {
		return fmt.Errorf("keep the local cache store: %w", err)
	}
	return sdb.Close()
}

// keepLocalStore gives the built-in cache target (storage.LocalTargetID)
// of the restored database staged the store id of the live database ("" if
// it has none): that store lives in this machine's cache directory, not in
// the backup. A backup of another machine (a move, a full restore on a new
// instance) names that machine's store, and the local cache stayed offline
// ("a different cache store … was found") until an admin adopted it.
func keepLocalStore(ctx context.Context, staged *sql.DB, livePath string) error {
	conn, err := staged.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var has bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM main.sqlite_master WHERE type = 'table' AND name = 'storage_targets')`).
		Scan(&has); err != nil || !has {
		return err // a backup from before storage targets: storage.New creates the row
	}
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS live`, livePath); err != nil { // as CarryOverAccounts (only read)
		return err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), `DETACH DATABASE live`)
	storeID := ""
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM live.sqlite_master WHERE type = 'table' AND name = 'storage_targets')`).
		Scan(&has); err != nil {
		return err
	}
	if has {
		err := conn.QueryRowContext(ctx, `SELECT store_id FROM live.storage_targets WHERE id = ?`, storage.LocalTargetID).Scan(&storeID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	_, err = conn.ExecContext(ctx, `UPDATE main.storage_targets SET store_id = ? WHERE id = ?`, storeID, storage.LocalTargetID)
	return err
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
	// The restore never took effect: the next build neither ends sessions,
	// forgets the sync state nor audits system.restore for it.
	a.restoredAt, a.restoreSections, a.restoreBy = time.Time{}, nil, ""
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
	if hadSchema > 0 {
		// An older binary on a newer database (a downgrade without the copy)
		// neither copies it under its own, older name nor records itself as
		// the database's version: the next upgrade would copy the migrated
		// database under that name and prune a genuine older copy.
		if err := refuseNewerSchema(ctx, d); err != nil {
			return err
		}
	}
	var prev string // stays "" while app_meta does not exist yet
	_ = d.R.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key = 'binary_version'`).Scan(&prev)
	cur := version.Version
	if hadSchema > 0 && prev != "" && prev != cur && cur != "dev" {
		if err := copyBeforeUpgrade(ctx, d, filepath.Join(dataDir, "backups"), prev, cur, log); err != nil {
			return err
		}
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

// copyBeforeUpgrade makes the pre-upgrade copy of d in dir, named after the
// version whose schema the database has (copyVersion), and prunes the
// copies. It makes none when that version is cur itself (a version before
// 1.0.0 was started on this database and failed without migrating it) and
// a copy of the same schema named after cur exists already: such an older
// version copies the database under cur's name itself before it fails, so
// another copy would only fill a place of the three that version keeps and
// get the one copy it can open pruned sooner. A previous version that
// opens the database's schema (0.17.x on the database of 1.0.x, which
// added no schema step) ran on it and may have changed it: the copy is
// named after that version, as after every upgrade.
func copyBeforeUpgrade(ctx context.Context, d *db.DB, dir, prev, cur string, log *slog.Logger) error {
	owner, schemaOf := copyVersion(ctx, d, prev)
	switch {
	case owner != prev:
		log.Warn("the previous version recorded itself without migrating this database; the copy is named after the version that wrote its schema",
			slog.String("previous", prev), slog.String("schemaOf", owner))
	case schemaOf != "" && schemaOf != prev:
		log.Info("the previous version ran on this database with the schema of a later version (going back needed no copy); "+
			"the copy is named after the previous version", slog.String("previous", prev), slog.String("schemaOf", schemaOf))
	}
	if owner == cur {
		if existing := copyOfSchema(ctx, d, dir, owner); existing != "" {
			log.Info("no configuration backup before this start: a copy of this database's schema named after this version exists",
				slog.String("file", existing), slog.String("previous", prev))
			return nil
		}
	}
	name := fmt.Sprintf("picache-%s-%s.db", sanitizeFile(owner), time.Now().UTC().Format(copyTimeLayout))
	err := os.MkdirAll(dir, 0o750) // the CLI may run before the service ever created it
	if err == nil {
		_, err = d.W.ExecContext(ctx, `VACUUM INTO ?`, filepath.Join(dir, name))
	}
	if err != nil {
		return fmt.Errorf("copy picache.db to %s before the upgrade from %s (is the disk full?): %w", dir, prev, err)
	}
	log.Info("saved configuration backup before upgrade", slog.String("file", filepath.Join(dir, name)))
	pruneBackups(dir, 3)
	return nil
}

// copyTimeLayout is the time in the name of a pre-upgrade copy
// (picache-<version>-<time>.db, UTC).
const copyTimeLayout = "20060102T150405"

// copyOfSchema returns a pre-upgrade copy in dir named after version
// (exactly picache-<version>-<time>.db) whose schema equals d's ("" if
// there is none).
func copyOfSchema(ctx context.Context, d *db.DB, dir, version string) string {
	v, err := schemaVersions(ctx, d.R)
	if err != nil {
		return ""
	}
	want := schemaKey(v)
	entries, err := os.ReadDir(dir)
	if want == "" || err != nil {
		return ""
	}
	prefix := "picache-" + sanitizeFile(version) + "-"
	for _, e := range entries {
		ts, named := strings.CutPrefix(e.Name(), prefix)
		ts, isDB := strings.CutSuffix(ts, ".db")
		if !named || !isDB || !e.Type().IsRegular() {
			continue
		}
		if _, err := time.Parse(copyTimeLayout, ts); err != nil {
			continue // another version's name that starts the same (v1.0.0-rc.2 for v1.0.0)
		}
		if p := filepath.Join(dir, e.Name()); copySchema(p) == want {
			return p
		}
	}
	return ""
}

// metaBinarySchema is the app_meta key with the schema versions of
// picache.db and the version that brought it there
// ({"version":…,"schema":{component: version}}), written once every
// migration of a start succeeded (recordSchema). A version before 1.0.0
// started on a newer database records itself as binary_version before it
// fails, but never this key.
const metaBinarySchema = "binary_schema"

type binarySchema struct {
	Version string         `json:"version"`
	Schema  map[string]int `json:"schema"`
}

// schemaVersions returns the schema version of every component of a
// picache.db (schema_migrations).
func schemaVersions(ctx context.Context, q rowsQuerier) (map[string]int, error) {
	rows, err := q.QueryContext(ctx, `SELECT component, MAX(version) FROM schema_migrations GROUP BY component`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var c string
		var v int
		if err := rows.Scan(&c, &v); err != nil {
			return nil, err
		}
		out[c] = v
	}
	return out, rows.Err()
}

// recordSchema writes metaBinarySchema for this version after every
// migration of the start succeeded.
func recordSchema(ctx context.Context, d *db.DB) error {
	schema, err := schemaVersions(ctx, d.R)
	if err != nil {
		return err
	}
	b, err := json.Marshal(binarySchema{Version: version.Version, Schema: schema}, json.Deterministic(true))
	if err != nil {
		return err
	}
	_, err = d.W.ExecContext(ctx, `INSERT INTO app_meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, metaBinarySchema, string(b))
	return err
}

// copyVersion returns the version a pre-upgrade copy is named after: the
// recorded previous version prev, unless another version recorded the
// database's current schema (metaBinarySchema) and prev did not: a version
// before 1.0.0 started on a newer database records itself before it fails,
// and a copy named after it would not open with it (going back picks the
// copy by its name). The schema's version is then named, unless prev opens
// that schema (openedBy: 0.17.x on the database of 1.0.x); at worst a copy
// that prev could open too is named after that newer version. schemaOf is
// the version that recorded the database's current schema ("" if none
// did).
func copyVersion(ctx context.Context, d *db.DB, prev string) (owner, schemaOf string) {
	var raw string
	if err := d.R.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key = ?`, metaBinarySchema).Scan(&raw); err != nil {
		return prev, ""
	}
	var rec binarySchema
	if json.Unmarshal([]byte(raw), &rec) != nil || rec.Version == "" {
		return prev, ""
	}
	cur, err := schemaVersions(ctx, d.R)
	if err != nil || !maps.Equal(cur, rec.Schema) {
		return prev, ""
	}
	if rec.Version == prev || openedBy(prev, cur) {
		return prev, rec.Version
	}
	return rec.Version, rec.Version
}

// schemaV017 is the picache.db schema of 0.17.x (component → version),
// the last release before 1.0.0. 1.0.0 added no step: 0.17.x opens the
// database of 1.0.x (DEPLOYMENT "Going back to an earlier version").
var schemaV017 = map[string]int{"app": 1, "auth": 3, "clients": 4, "dhcp": 2, "dns": 4, "filter": 3, "notify": 1,
	"parental": 2, "proxy": 1, "services": 1, "settings": 7, "storage": 1}

// openedBy reports whether version v, recorded as binary_version while
// another version recorded the database's schema, opens a database with
// schema: a version before 1.0.0 records itself before it checks the
// schema, so its record alone does not say whether it ran. Of those only
// 0.17.x opens a later version's database, and only while no component it
// knows has a newer schema than its own (a component it does not know is
// left alone). Versions from 1.0.0 on refuse a newer schema before they
// record themselves.
func openedBy(v string, schema map[string]int) bool {
	pv, err := update.ParseVersion(v)
	if err != nil || pv.Major != 0 || pv.Minor != 17 {
		return false
	}
	for c, n := range schema {
		if own, known := schemaV017[c]; known && n > own {
			return false
		}
	}
	return true
}

// refuseNewerSchema returns db.Migrate's downgrade refusal when a component
// of picache.db this binary knows has a newer schema version than this
// binary (components it does not know are left to their own version).
func refuseNewerSchema(ctx context.Context, d *db.DB) error {
	rows, err := d.R.QueryContext(ctx, `SELECT component, MAX(version) FROM schema_migrations GROUP BY component ORDER BY component`)
	if err != nil {
		return err
	}
	defer rows.Close()
	known := ConfigSchemaVersions()
	for rows.Next() {
		var component string
		var version int
		if err := rows.Scan(&component, &version); err != nil {
			return err
		}
		if steps, ok := known[component]; ok && version > steps {
			return fmt.Errorf("db: %s schema version %d is newer than this binary (%d); refusing to downgrade "+
				"(start the newer version again, or restore the copy made before the upgrade: "+
				"docs/DEPLOYMENT.md \"Going back to an earlier version\")", component, version, steps)
		}
	}
	return rows.Err()
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

// pruneBackups keeps the newest keep pre-upgrade copies and, beyond them,
// the newest copy of every other schema (the component versions of the
// copy): the copy an older version can open is never pruned only because
// newer copies of other schemas were made, such as copies a version before
// 1.0.0 made of a newer database before it failed. A copy whose schema
// cannot be read counts as none.
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
	seen := map[string]bool{}
	for i, f := range files {
		schema := copySchema(f.path)
		if i < keep || (schema != "" && !seen[schema]) {
			seen[schema] = true
			continue
		}
		_ = os.Remove(f.path)
	}
}

// copySchema returns the component versions of a database copy as one
// string ("" when they cannot be read).
func copySchema(path string) string {
	d, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=query_only(1)&_pragma=trusted_schema(0)")
	if err != nil {
		return ""
	}
	defer d.Close()
	v, err := schemaVersions(context.Background(), d)
	if err != nil {
		return ""
	}
	return schemaKey(v)
}

// schemaKey returns component versions as one string ("" for none).
func schemaKey(v map[string]int) string {
	if len(v) == 0 {
		return ""
	}
	b, _ := json.Marshal(v, json.Deterministic(true))
	return string(b)
}
