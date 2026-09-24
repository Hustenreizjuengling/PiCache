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
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/version"
)

const maxRestoreBytes = 512 << 20

var appMigrations = []string{
	`CREATE TABLE app_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);`,
}

// Backup writes a consistent copy of picache.db (VACUUM INTO) to w. Sessions
// are always removed; sealed NAS passwords only survive with includeSecrets
// (they need the master key, which is never part of a backup).
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
	d, err := sql.Open("sqlite", path+"?_pragma=journal_mode(DELETE)")
	if err != nil {
		return err
	}
	defer d.Close()
	stmts := []string{`DELETE FROM auth_sessions`}
	if !includeSecrets {
		stmts = append(stmts, `UPDATE storage_targets SET password_sealed = NULL`)
	}
	for _, q := range stmts {
		if _, err := d.ExecContext(ctx, q); err != nil && !strings.Contains(err.Error(), "no such table") {
			return err
		}
	}
	_, err = d.ExecContext(ctx, `VACUUM`)
	return err
}

// StageRestore validates an uploaded picache.db and stages it; it replaces
// the live database on the next start.
func (a *App) StageRestore(ctx context.Context, r io.Reader) error {
	staged := a.paths.ConfigDB + ".restore"
	tmp := staged + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, maxRestoreBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if n > maxRestoreBytes {
		os.Remove(tmp)
		return apperr.Invalid("file", "backup is larger than 512 MiB")
	}
	if err := validateBackup(ctx, tmp); err != nil {
		os.Remove(tmp)
		return apperr.Wrap(apperr.KindInvalid, err, "not a usable PiCache backup: %v", err)
	}
	if err := os.Rename(tmp, staged); err != nil {
		os.Remove(tmp)
		return err
	}
	a.log.Warn("configuration restore staged; it will be applied on the next restart")
	return nil
}

func validateBackup(ctx context.Context, path string) error {
	d, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
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
	if !found {
		return errors.New("missing PiCache settings")
	}
	return rows.Err()
}

// applyStagedRestore swaps in a staged backup before the database is opened.
func (a *App) applyStagedRestore() (bool, error) {
	staged := a.paths.ConfigDB + ".restore"
	if _, err := os.Stat(staged); err != nil {
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
	a.log.Warn("applied staged configuration restore", slog.String("previous", backup))
	return true, nil
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

// preUpgradeBackup keeps a copy of picache.db whenever the binary version
// changes (newest 3 kept in <data>/backups), so a rollback is possible.
func (a *App) preUpgradeBackup(ctx context.Context) error {
	var hadSchema int
	_ = a.cdb.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'schema_migrations'`).Scan(&hadSchema)
	if err := a.cdb.Migrate(ctx, "app", appMigrations); err != nil {
		return err
	}
	var prev string
	_ = a.cdb.R.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key = 'binary_version'`).Scan(&prev)
	cur := version.Version
	if prev == cur {
		return nil
	}
	if hadSchema > 0 && prev != "" && cur != "dev" {
		dir := filepath.Join(a.cfg.DataDir, "backups")
		name := fmt.Sprintf("picache-%s-%s.db", sanitizeFile(prev), time.Now().UTC().Format("20060102T150405"))
		if _, err := a.cdb.W.ExecContext(ctx, `VACUUM INTO ?`, filepath.Join(dir, name)); err != nil {
			return err
		}
		a.log.Info("saved configuration backup before upgrade", slog.String("file", filepath.Join(dir, name)))
		pruneBackups(dir, 3)
	}
	_, err := a.cdb.W.ExecContext(ctx, `INSERT INTO app_meta (key, value) VALUES ('binary_version', ?)
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
