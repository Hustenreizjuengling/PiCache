package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/notify"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/storage"
)

// metaKeyReplaced is the app_meta key that records when a new master key
// was created while the configuration held secrets sealed with the
// previous one (unix ms). The health check "master-key" warns while stored
// secrets cannot be decrypted.
const metaKeyReplaced = "secrets.key_replaced_at"

// sealedSecrets are the stored secrets sealed with the master key, each
// read as (key, sealed) with the additional data it is sealed with: the
// settings secrets (sync token, proxy password), the notification channel
// secrets and the NAS passwords.
var sealedSecrets = []struct {
	query string
	aad   func(key string) string
}{
	{`SELECT name, sealed FROM settings_secrets`, settings.SecretAAD},
	{`SELECT id, secret_sealed FROM notify_channels WHERE COALESCE(secret_sealed, '') != ''`, notify.SecretAAD},
	{`SELECT id, password_sealed FROM storage_targets WHERE COALESCE(password_sealed, '') != ''`, storage.PasswordAAD},
}

// unreadableSecrets counts the stored secrets open cannot decrypt (sealed
// with another master key, or damaged). It opens every one: an edit that
// keeps a secret (a renamed notification channel) changes the row, not the
// sealed value, so only a secret entered again counts as readable. A table
// that does not exist yet counts as empty.
func unreadableSecrets(ctx context.Context, q *sql.DB, open func(sealed, aad string) ([]byte, error)) (int, error) {
	n := 0
	for _, src := range sealedSecrets {
		c, err := countUnreadable(ctx, q, src.query, src.aad, open)
		if err != nil && !strings.Contains(err.Error(), "no such table") {
			return 0, err
		}
		n += c
	}
	return n, nil
}

func countUnreadable(ctx context.Context, q *sql.DB, query string, aad func(string) string,
	open func(sealed, aad string) ([]byte, error)) (int, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var key string
		var sealed []byte
		if err := rows.Scan(&key, &sealed); err != nil {
			return 0, err
		}
		if _, err := open(string(sealed), aad(key)); err != nil {
			n++
		}
	}
	return n, rows.Err()
}

// checkReplacedKey runs when the master key file had to be created: a
// configuration that holds sealed secrets (keys/master.key was deleted or
// lost, not a new installation) gets an ERROR and the marker the health
// check follows. Before, only every use of such a secret failed with a
// WARN (a notification not delivered), so outage alerts stopped silently.
func (a *App) checkReplacedKey(ctx context.Context) {
	n, err := unreadableSecrets(ctx, a.cdb.R, a.box.Open)
	if err != nil {
		a.log.Warn("cannot check the stored secrets against the new master key", slog.Any("err", err))
	}
	var totp int
	_ = a.cdb.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_users WHERE totp_secret IS NOT NULL`).Scan(&totp)
	if n == 0 && totp == 0 {
		return
	}
	a.log.Error("keys/master.key was missing, so a new master key was created, but the configuration holds secrets sealed with "+
		"the previous key: put the previous key back (with PiCache stopped), or enter the secrets again (notification channels, "+
		"NAS passwords, the sync token, the proxy password); accounts with two-factor sign-in need `picache reset-password`",
		slog.String("file", a.paths.MasterKeyFile), slog.Int("secrets", n), slog.Int("twoFactorAccounts", totp))
	if n == 0 {
		return
	}
	if _, err := a.cdb.W.ExecContext(ctx, `INSERT INTO app_meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, metaKeyReplaced, strconv.FormatInt(db.NowMs(), 10)); err != nil {
		a.log.Warn("cannot record the replaced master key", slog.Any("err", err))
	}
}

// masterKeyHealth evaluates the check "master-key" after a new master key
// was created (the marker): it warns while stored secrets cannot be
// decrypted with it; once all were entered again or deleted, the marker
// goes.
func (a *App) masterKeyHealth(ctx context.Context) (status, msg, hint string, show bool) {
	var raw string
	if a.box == nil || a.cdb.R.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key = ?`, metaKeyReplaced).Scan(&raw) != nil {
		return "", "", "", false
	}
	at, _ := strconv.ParseInt(raw, 10, 64)
	day := time.UnixMilli(at).UTC().Format(time.DateOnly)
	hint = "put the previous keys/master.key back (with PiCache stopped), or enter the secrets again: notification channels, NAS passwords, the sync token, the proxy password"
	n, err := unreadableSecrets(ctx, a.cdb.R, a.box.Open)
	switch {
	case err != nil: // the marker stays
		return "warn", fmt.Sprintf("keys/master.key was replaced on %s; the stored secrets cannot be checked: %v", day, err), hint, true
	case n == 0:
		_, _ = a.cdb.W.ExecContext(ctx, `DELETE FROM app_meta WHERE key = ? AND value = ?`, metaKeyReplaced, raw)
		return "", "", "", false
	}
	return "warn", fmt.Sprintf("%d stored secret(s) cannot be decrypted: keys/master.key was replaced on %s", n, day), hint, true
}
