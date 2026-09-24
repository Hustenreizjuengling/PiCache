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
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

const maxRestoreBytes = 512 << 20

// Backup writes a consistent copy of picache.db (VACUUM INTO) to w.
// Secrets inside stay sealed with the master key, which is not included.
func (a *App) Backup(ctx context.Context, w io.Writer) error {
	tmp := filepath.Join(a.cfg.DataDir, "tmp", fmt.Sprintf("backup-%d.db", time.Now().UnixNano()))
	defer os.Remove(tmp)
	if _, err := a.cdb.W.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
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
		return apperr.Wrap(apperr.KindInvalid, err, "not a valid PiCache backup: %v", err)
	}
	if err := os.Rename(tmp, staged); err != nil {
		os.Remove(tmp)
		return err
	}
	a.log.Warn("configuration restore staged; it will be applied on the next restart")
	return nil
}

func validateBackup(ctx context.Context, path string) error {
	d, err := sql.Open("sqlite", path+"?_query_only=1")
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
	var n int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE component = 'settings'`).Scan(&n); err != nil || n == 0 {
		return errors.New("missing PiCache schema")
	}
	return nil
}

// applyStagedRestore swaps in a staged backup before the database is opened.
func (a *App) applyStagedRestore() error {
	staged := a.paths.ConfigDB + ".restore"
	if _, err := os.Stat(staged); err != nil {
		return nil
	}
	backup := fmt.Sprintf("%s.before-restore-%s", a.paths.ConfigDB, time.Now().UTC().Format("20060102T150405"))
	if _, err := os.Stat(a.paths.ConfigDB); err == nil {
		if err := os.Rename(a.paths.ConfigDB, backup); err != nil {
			return fmt.Errorf("restore: keep old database: %w", err)
		}
	}
	for _, sfx := range []string{"-wal", "-shm"} {
		_ = os.Remove(a.paths.ConfigDB + sfx)
	}
	if err := os.Rename(staged, a.paths.ConfigDB); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	a.log.Warn("applied staged configuration restore", slog.String("previous", backup))
	return nil
}
