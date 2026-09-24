package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// setupTokenLen is the length of crypto/rand.Text output (26 base32 chars,
// 130 bits).
const setupTokenLen = 26

// initSetup prepares the one-time setup token if no user exists (reusing a
// well-formed token file from an earlier start) or removes a stale file.
func (a *Service) initSetup(ctx context.Context) error {
	exists, err := userExists(ctx, a.db.R)
	if err != nil {
		return err
	}
	if exists {
		a.setupDone.Store(true)
		a.removeSetupFile()
		return nil
	}
	tok := ""
	if a.setupFile != "" {
		if b, err := os.ReadFile(a.setupFile); err == nil && validSetupToken(strings.TrimSpace(string(b))) {
			tok = strings.TrimSpace(string(b))
		}
	}
	if tok == "" {
		tok = rand.Text()
	}
	if a.setupFile != "" {
		if err := writeSetupToken(a.setupFile, tok); err != nil {
			// Not fatal: the token is still in the log below.
			a.log.Warn("could not write the setup token file", slog.String("file", a.setupFile), slog.Any("err", err))
		}
	}
	a.setupToken = tok
	// Logging the token is intended: it is the documented way to find it
	// (docs/ARCHITECTURE.md 6.1). It is logged once per start.
	a.log.Warn("first-run setup required: open the web UI and enter this setup token (also: `picache setup-token`)",
		slog.String("setupToken", tok), slog.String("file", a.setupFile))
	return nil
}

func validSetupToken(s string) bool {
	if len(s) != setupTokenLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= 'A' && c <= 'Z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

// writeSetupToken writes the token with mode 0600 (temp file + rename, so a
// pre-existing file with wider permissions is never reused).
func writeSetupToken(path, tok string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(tok + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

func (a *Service) removeSetupFile() {
	if a.setupFile == "" {
		return
	}
	if err := os.Remove(a.setupFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		a.log.Warn("could not delete the setup token file", slog.String("file", a.setupFile), slog.Any("err", err))
	}
}

// finishSetup forgets the setup token once a user exists.
func (a *Service) finishSetup() {
	a.setupMu.Lock()
	a.setupToken = ""
	a.setupMu.Unlock()
	a.setupDone.Store(true)
	a.removeSetupFile()
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func userExists(ctx context.Context, q queryRower) (bool, error) {
	var exists bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM auth_users)`).Scan(&exists)
	return exists, err
}

// SetupRequired reports whether no user exists yet.
func (a *Service) SetupRequired(ctx context.Context) (bool, error) {
	if a.setupDone.Load() {
		return false, nil
	}
	exists, err := userExists(ctx, a.db.R)
	if err != nil {
		return false, err
	}
	if exists {
		a.finishSetup()
	}
	return !exists, nil
}

// Setup creates the first admin (requires the setup token) and logs in.
func (a *Service) Setup(ctx context.Context, token, username, password string, meta ReqMeta) (*Session, error) {
	ckey := clientThrottleKey(meta.IP)
	if err := a.throttle.allow(a.now(), ckey); err != nil {
		return nil, err
	}
	required, err := a.SetupRequired(ctx)
	if err != nil {
		return nil, err
	}
	if !required {
		return nil, apperr.Forbidden("setup has already been completed")
	}
	a.setupMu.Lock()
	expected := a.setupToken
	a.setupMu.Unlock()
	if expected == "" || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(expected)) != 1 {
		a.recordFailure(ckey)
		a.auditFailure(ctx, meta, "setup-token")
		return nil, apperr.Forbidden("invalid setup token")
	}
	if err := validateUsername(username); err != nil {
		return nil, err
	}
	if err := validatePassword("password", password); err != nil {
		return nil, err
	}
	h, err := a.newHash(ctx, password)
	if err != nil {
		return nil, err
	}
	id, err := createFirstUser(ctx, a.db, username, h, a.now().UnixMilli())
	if err != nil {
		return nil, err
	}
	a.throttle.succeed(ckey)
	a.finishSetup()
	a.log.Info("initial setup completed", slog.String("username", username))
	s, err := a.createSession(ctx, id, meta)
	if err != nil {
		return nil, err
	}
	a.Audit(ctx, &Principal{UserID: id, Username: username, SessionID: s.ID, Scope: ScopeAdmin},
		meta.IP, "auth.setup", username, nil)
	return s, nil
}

// createFirstUser inserts the first user in one BEGIN IMMEDIATE transaction
// that verifies that no user exists (two concurrent setups cannot both win).
func createFirstUser(ctx context.Context, d *db.DB, username, hash string, nowMs int64) (int64, error) {
	var id int64
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		exists, err := userExists(ctx, tx)
		if err != nil {
			return err
		}
		if exists {
			return apperr.Forbidden("setup has already been completed")
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO auth_users (username, password_hash, created_at) VALUES (?, ?, ?)`,
			username, hash, nowMs)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// Provision creates the first admin from bootstrap config if no user exists.
func (a *Service) Provision(ctx context.Context, username, password string) error {
	if err := validateUsername(username); err != nil {
		return err
	}
	if err := validatePassword("password", password); err != nil {
		return err
	}
	required, err := a.SetupRequired(ctx)
	if err != nil {
		return err
	}
	if !required {
		a.log.Info("an admin account already exists; the provisioned admin password is ignored (use `picache reset-password` to change it)")
		return nil
	}
	h, err := a.newHash(ctx, password)
	if err != nil {
		return err
	}
	if _, err := createFirstUser(ctx, a.db, username, h, a.now().UnixMilli()); err != nil {
		return err
	}
	a.finishSetup()
	a.log.Info("admin account provisioned from the bootstrap configuration", slog.String("username", username))
	a.Audit(ctx, &Principal{Username: "bootstrap"}, "", "auth.provision", username, nil)
	return nil
}

// ResetPassword sets a user's password (CLI `picache reset-password`),
// creating the user if missing, disables TOTP and revokes all sessions.
func ResetPassword(ctx context.Context, d *db.DB, username, password string) error {
	if err := validateUsername(username); err != nil {
		return err
	}
	if err := validatePassword("password", password); err != nil {
		return err
	}
	if err := d.Migrate(ctx, "auth", migrations); err != nil {
		return err
	}
	h := hashPassword(password)
	now := db.NowMs()
	return d.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE auth_users SET password_hash = ?, totp_secret = NULL,
			totp_pending = NULL, totp_pending_at = 0 WHERE username = ?`, h, username)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO auth_users (username, password_hash, created_at) VALUES (?, ?, ?)`,
				username, h, now); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM auth_sessions`); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO auth_audit (time, username, ip, action, target, details)
			VALUES (?, 'cli', '', 'auth.password_reset', ?, '')`, now, username)
		return err
	})
}

// PurgeCredentials deletes all sessions and API tokens (after a restore).
func PurgeCredentials(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{`DELETE FROM auth_sessions`, `DELETE FROM auth_tokens`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
