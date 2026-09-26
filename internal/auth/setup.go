package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
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

func errSetupDone() error { return apperr.Forbidden("setup has already been completed") }

// Setup creates the first admin (requires the setup token) and logs in.
//
// Once setup is done the answer is a constant 403 that takes nothing from
// the shared attempt budget (a client calling it in a loop must not starve
// sign-ins), but every call counts as a failure of the client, so it is
// locked out like a password guesser.
func (a *Service) Setup(ctx context.Context, token, username, password string, meta ReqMeta) (*Session, error) {
	ckey := clientThrottleKey(meta.IP)
	if a.setupDone.Load() {
		a.recordFailure(ckey)
		return nil, errSetupDone()
	}
	if err := a.throttle.allow(a.now(), ckey); err != nil {
		return nil, err
	}
	required, err := a.SetupRequired(ctx)
	if err != nil {
		return nil, err
	}
	if !required {
		a.recordFailure(ckey)
		return nil, errSetupDone()
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
	s, err := a.createSession(ctx, id, h, meta)
	if err != nil {
		return nil, err
	}
	s.Device = a.issueDevice(id, username)
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
			return errSetupDone()
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO auth_users (username, role, password_hash, created_at) VALUES (?, 'admin', ?, ?)`,
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
// The username and password are validated only when the account is
// created, so a stored name that a later rule would refuse never stops
// PiCache (and DNS) from starting.
func (a *Service) Provision(ctx context.Context, username, password string) error {
	required, err := a.SetupRequired(ctx)
	if err != nil {
		return err
	}
	if !required {
		a.log.Info("an admin account already exists; the provisioned admin password is ignored (use `picache reset-password` to change it)")
		return nil
	}
	if err := validateUsername(username); err != nil {
		return err
	}
	if err := validatePassword("password", password); err != nil {
		return err
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

// ResetResult reports what ResetPassword changed.
type ResetResult struct {
	Username        string // the account whose password was set
	Role            string // its role after the reset
	RoleChanged     bool   // --admin made a viewer an admin
	Created         bool   // no account existed, so this one was created (an admin)
	TOTPDisabled    bool   // two-factor authentication was on and is now off
	SessionsRevoked int64  // sessions ended (all sessions of all accounts)
	TokensRevoked   int64  // API tokens deleted (all tokens of all accounts)
}

// Usernames returns the names of all accounts (oldest first).
func Usernames(ctx context.Context, d *db.DB) ([]string, error) {
	return accountNames(ctx, d.R)
}

// ResetPassword sets the password of an account (CLI `picache reset-password`),
// disables its TOTP, signs out all sessions and revokes all API tokens: it is
// the recovery path after a compromise, so it ends every credential that
// someone else may hold.
//
// The account must exist; it is found by name case-insensitively, whatever
// its form. It is created (as an admin, the name validated) only while
// there is no account at all; otherwise a name that matches no account is
// refused with the existing names, so a typo or the default name never adds
// a second admin while the real, possibly compromised account stays as it
// was. The account keeps its role; makeAdmin (--admin) makes it an admin.
func ResetPassword(ctx context.Context, d *db.DB, username, password string, makeAdmin bool) (ResetResult, error) {
	var res ResetResult
	if err := validatePassword("password", password); err != nil {
		return res, err
	}
	if err := d.Migrate(ctx, "auth", migrations); err != nil {
		return res, err
	}
	h := hashPassword(password)
	now := db.NowMs()
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		res = ResetResult{}
		names, err := accountNames(ctx, tx)
		if err != nil {
			return err
		}
		var (
			id      int64
			hadTOTP bool
		)
		// auth_users.username is COLLATE NOCASE: the name is found
		// case-insensitively.
		err = tx.QueryRowContext(ctx, `SELECT id, username, role, totp_secret IS NOT NULL FROM auth_users WHERE username = ?`, username).
			Scan(&id, &res.Username, &res.Role, &hadTOTP)
		switch {
		case errors.Is(err, sql.ErrNoRows) && len(names) > 0:
			return apperr.Invalid("username", "there is no account named %q; existing accounts: %s",
				username, strings.Join(names, ", "))
		case errors.Is(err, sql.ErrNoRows):
			if err := validateUsername(username); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO auth_users (username, role, password_hash, created_at) VALUES (?, 'admin', ?, ?)`,
				username, h, now); err != nil {
				return err
			}
			res.Username, res.Role, res.Created = username, RoleAdmin, true
		case err != nil:
			return err
		default:
			if _, err := tx.ExecContext(ctx, `UPDATE auth_users SET password_hash = ?, totp_secret = NULL,
				totp_pending = NULL, totp_pending_at = 0 WHERE id = ?`, h, id); err != nil {
				return err
			}
			res.TOTPDisabled = hadTOTP
			if makeAdmin && res.Role != RoleAdmin {
				if _, err := tx.ExecContext(ctx, `UPDATE auth_users SET role = 'admin' WHERE id = ?`, id); err != nil {
					return err
				}
				res.Role, res.RoleChanged = RoleAdmin, true
			}
		}
		if res.SessionsRevoked, err = execCount(ctx, tx, `DELETE FROM auth_sessions`); err != nil {
			return err
		}
		if res.TokensRevoked, err = execCount(ctx, tx, `DELETE FROM auth_tokens`); err != nil {
			return err
		}
		if err := checkEmpty(ctx, tx, "auth_sessions", "auth_tokens"); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO auth_audit (time, username, ip, action, target, details)
			VALUES (?, 'cli', '', 'auth.password_reset', ?, ?)`, now, res.Username, auditDetails(map[string]any{
			"created": res.Created, "totpDisabled": res.TOTPDisabled,
			"sessionsRevoked": res.SessionsRevoked, "tokensRevoked": res.TokensRevoked,
			"role": res.Role, "roleChanged": res.RoleChanged,
		}))
		return err
	})
	return res, err
}

type rowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// accountNames returns all usernames (oldest first).
func accountNames(ctx context.Context, q rowsQuerier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT username FROM auth_users ORDER BY id`)
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

func execCount(ctx context.Context, tx *sql.Tx, q string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// checkEmpty verifies that the tables have no rows after they were emptied:
// a trigger such as "BEFORE DELETE … RAISE(IGNORE)" would otherwise turn the
// delete into a silent no-op.
func checkEmpty(ctx context.Context, q queryRower, tables ...string) error {
	for _, t := range tables {
		var n int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("auth: %d rows of %s could not be deleted (the database schema keeps them)", n, t)
		}
	}
	return nil
}

// deleteVerified deletes the rows of table that match where and verifies
// that none of them is left (see checkEmpty). It returns the number of
// deleted rows.
func deleteVerified(ctx context.Context, x execQuerier, table, where string, args ...any) (int64, error) {
	res, err := x.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+where, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	var left int
	if err := x.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE `+where, args...).Scan(&left); err != nil {
		return n, err
	}
	if left != 0 {
		return n, fmt.Errorf("auth: %d rows of %s could not be deleted (the database schema keeps them)", left, table)
	}
	return n, nil
}

// PurgeSessions deletes all sessions (after a restore: everyone signs in
// again) and verifies that none is left.
func PurgeSessions(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_sessions`); err != nil {
		return err
	}
	return checkEmpty(ctx, tx, "auth_sessions")
}
