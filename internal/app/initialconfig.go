package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// PICACHE_INITIAL_CONFIG (docs/DEPLOYMENT.md "Declarative configuration"):
// a settings document applied once, on the first start (no account exists
// and app_meta has no config.initial_applied), after the migrations and
// before any listener serves or PICACHE_ADMIN_PASSWORD_FILE is
// provisioned. The file is read like --token-file (a regular file, never
// through a link, at most 1 MiB), decoded strictly on top of the current
// document and applied through settings.Store.Update with the normal
// validation in one step. A file that cannot be read or applied refuses
// the start (the variable was set on purpose).
const (
	metaInitialConfig = "config.initial_applied"
	maxInitialConfig  = 1 << 20
)

// initialApplied is the app_meta document of an applied initial config.
type initialApplied struct {
	SHA256    string    `json:"sha256"`
	AppliedAt time.Time `json:"appliedAt"`
}

// applyInitialConfig applies PICACHE_INITIAL_CONFIG on the first start.
func (a *App) applyInitialConfig(ctx context.Context) error {
	path := a.cfg.InitialConfig
	if path == "" {
		return nil
	}
	first, err := firstStart(ctx, a.cdb)
	if err != nil {
		return fmt.Errorf("PICACHE_INITIAL_CONFIG: %w", err)
	}
	if !first {
		a.log.Warn("PICACHE_INITIAL_CONFIG ignored: not the first start")
		return nil
	}
	b, worldReadable, err := readInitialConfig(path)
	if err != nil {
		return fmt.Errorf("PICACHE_INITIAL_CONFIG: %w", err)
	}
	var storeIgnored, secrets bool
	_, err = a.set.Update(ctx, func(s *settings.All) error {
		store := s.Cache.ActiveStoreID
		if err := json.Unmarshal(b, s, json.RejectUnknownMembers(true)); err != nil {
			return apperr.Invalid("", "invalid JSON: %s", jsonMessage(err))
		}
		if s.Cache.ActiveStoreID != store {
			s.Cache.ActiveStoreID, storeIgnored = store, true
		}
		secrets = (s.Sync.Token != nil && *s.Sync.Token != "") || (s.Network.Proxy.Password != nil && *s.Network.Proxy.Password != "")
		return nil
	})
	if err != nil {
		var ae *apperr.Error
		if errors.As(err, &ae) && ae.Field != "" {
			return fmt.Errorf("PICACHE_INITIAL_CONFIG: %s: %s", ae.Field, ae.Message)
		}
		return fmt.Errorf("PICACHE_INITIAL_CONFIG: %w", err)
	}
	if storeIgnored {
		a.log.Warn("PICACHE_INITIAL_CONFIG: cache.activeStoreId is ignored (choose the cache storage in the web UI)")
	}
	if secrets && worldReadable {
		a.log.Warn("PICACHE_INITIAL_CONFIG holds a secret and can be read by other users (chmod 600 it)",
			slog.String("file", path))
	}
	sum := sha256.Sum256(b)
	doc, err := json.Marshal(initialApplied{SHA256: hex.EncodeToString(sum[:]), AppliedAt: time.Now().UTC()})
	if err == nil {
		_, err = a.cdb.W.ExecContext(ctx, `INSERT INTO app_meta (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, metaInitialConfig, string(doc))
	}
	if err != nil {
		return fmt.Errorf("PICACHE_INITIAL_CONFIG: record that it was applied: %w", err)
	}
	a.log.Info("initial configuration applied", slog.String("file", path))
	return nil
}

// firstStart reports whether no account exists and no initial config was
// applied (app_meta exists: preUpgradeBackup created it).
func firstStart(ctx context.Context, d *db.DB) (bool, error) {
	var applied int
	if err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM app_meta WHERE key = ?`, metaInitialConfig).Scan(&applied); err != nil {
		return false, err
	}
	if applied > 0 {
		return false, nil
	}
	var tables int
	if err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'auth_users'`).Scan(&tables); err != nil {
		return false, err
	}
	if tables == 0 {
		return true, nil
	}
	var exists bool
	err := d.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM auth_users)`).Scan(&exists)
	return !exists, err
}

// readInitialConfig reads the file: regular, not a link, at most 1 MiB;
// worldReadable reports group or other read permission.
func readInitialConfig(path string) ([]byte, bool, error) {
	if !filepath.IsAbs(path) {
		return nil, false, fmt.Errorf("%s: must be an absolute path", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if lfi, err := os.Lstat(path); err != nil || lfi.Mode()&os.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("%s is a symbolic link", path)
	}
	if !fi.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Size() > maxInitialConfig {
		return nil, false, fmt.Errorf("%s is larger than 1 MiB", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxInitialConfig+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) > maxInitialConfig {
		return nil, false, fmt.Errorf("%s is larger than 1 MiB", path)
	}
	return b, filepath.Separator == '/' && fi.Mode().Perm()&0o044 != 0, nil
}

// jsonMessage is a JSON error without the package prefix, cut to 200
// bytes.
func jsonMessage(err error) string {
	msg := strings.TrimPrefix(err.Error(), "json: ")
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
