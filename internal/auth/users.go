package auth

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// maxAccounts is the number of accounts that can exist.
const maxAccounts = 32

// errLastAdmin refuses a role change or delete that would leave no admin.
func errLastAdmin() error { return apperr.Conflict("at least one admin must remain") }

// errSelfCredentials refuses a change of the caller's own password or TOTP
// through the user management (field: the member).
func errSelfCredentials(field string) error {
	return apperr.Invalid(field, "change your own password and two-factor authentication under Your account")
}

func validateRole(role string) error {
	if role != RoleAdmin && role != RoleViewer {
		return apperr.Invalid("role", "must be admin or viewer")
	}
	return nil
}

// checkAdmins logs an error at start when accounts exist but none is an
// admin (only the host can repair that).
func (a *Service) checkAdmins(ctx context.Context) error {
	var users, admins int
	if err := a.db.R.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE role = 'admin') FROM auth_users`).
		Scan(&users, &admins); err != nil {
		return err
	}
	if users > 0 && admins == 0 {
		a.log.Error("no account is an admin: run `picache reset-password --admin <user>` on the host")
	}
	return nil
}

// Users returns all accounts, oldest first.
func (a *Service) Users(ctx context.Context) ([]User, error) {
	rows, err := a.db.R.QueryContext(ctx, `SELECT `+userColumns+` FROM auth_users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u.User)
	}
	return out, rows.Err()
}

// requireAdmin verifies inside a transaction that the caller is still an
// admin with the session and the password just confirmed (a concurrent
// demotion or reset may have ended that after the request was
// authenticated; recheckCaller).
func requireAdmin(ctx context.Context, tx *sql.Tx, p *Principal, passwordHash string) error {
	role, err := recheckCaller(ctx, tx, p, "currentPassword", passwordHash)
	if err != nil {
		return err
	}
	if role != RoleAdmin {
		return apperr.Forbidden("this action requires admin rights")
	}
	return nil
}

// countAdmins returns the number of admins.
func countAdmins(ctx context.Context, tx *sql.Tx) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_users WHERE role = 'admin'`).Scan(&n)
	return n, err
}

// CreateUser creates an account (an admin's session; the caller's password
// is checked after the input). At most 32 accounts exist; names are unique
// case-insensitively.
func (a *Service) CreateUser(ctx context.Context, p *Principal, currentPassword, username, password, role string) (User, error) {
	if p == nil {
		return User{}, errNotAuthenticated
	}
	if err := validateUsername(username); err != nil {
		return User{}, err
	}
	if err := validatePassword("password", password); err != nil {
		return User{}, err
	}
	if err := validateRole(role); err != nil {
		return User{}, err
	}
	verified, err := a.verifyUserPassword(ctx, p, "currentPassword", currentPassword)
	if err != nil {
		return User{}, err
	}
	h, err := a.newHash(ctx, password)
	if err != nil {
		return User{}, err
	}
	var id int64
	err = a.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := requireAdmin(ctx, tx, p, verified); err != nil {
			return err
		}
		var n int
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), EXISTS (SELECT 1 FROM auth_users WHERE username = ?) FROM auth_users`,
			username).Scan(&n, &exists); err != nil {
			return err
		}
		if exists {
			return apperr.Conflict("an account named %s already exists", username)
		}
		if n >= maxAccounts {
			return apperr.Conflict("at most %d accounts can exist", maxAccounts)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO auth_users (username, role, password_hash, created_at) VALUES (?, ?, ?, ?)`,
			username, role, h, a.now().UnixMilli())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return User{}, err
	}
	a.log.Info("account created", slog.String("username", username), slog.String("role", role), slog.String("by", p.Username))
	u, err := a.userByID(ctx, id)
	return u.User, err
}

// UserUpdate is a change of another account (PUT /system/users/{id}); nil
// members stay as they are.
type UserUpdate struct {
	Role        *string
	Password    *string
	DisableTOTP bool
}

// UserChange reports what UpdateUser changed (the audit details).
type UserChange struct {
	Role            string `json:"role,omitempty"` // the new role, when one was given
	RoleChanged     bool   `json:"-"`
	PasswordReset   bool   `json:"passwordReset"`
	TOTPDisabled    bool   `json:"totpDisabled"`
	SessionsRevoked int64  `json:"sessionsRevoked"`
	TokensRevoked   int64  `json:"tokensRevoked"`
}

// UpdateUser changes the role of an account, sets another account's
// password (ending all its sessions and deleting all its API tokens; TOTP
// stays) or turns another account's TOTP off (ending all its sessions;
// tokens stay). A role change ends the account's sessions; a demotion also
// deletes its admin tokens (read tokens stay). The admins are counted in
// the same BEGIN IMMEDIATE transaction, so the last admin can never be
// demoted, not even by two admins demoting each other at once. On the
// caller's own account only the role may be given.
func (a *Service) UpdateUser(ctx context.Context, p *Principal, id int64, in UserUpdate, currentPassword string) (User, UserChange, error) {
	var ch UserChange
	if p == nil {
		return User{}, ch, errNotAuthenticated
	}
	if in.Role == nil && in.Password == nil && !in.DisableTOTP {
		return User{}, ch, apperr.Invalid("body", "nothing to change")
	}
	if id == p.UserID {
		if in.Password != nil {
			return User{}, ch, errSelfCredentials("password")
		}
		if in.DisableTOTP {
			return User{}, ch, errSelfCredentials("disableTotp")
		}
	}
	if in.Role != nil {
		if err := validateRole(*in.Role); err != nil {
			return User{}, ch, err
		}
	}
	if in.Password != nil {
		if err := validatePassword("password", *in.Password); err != nil {
			return User{}, ch, err
		}
	}
	verified, err := a.verifyUserPassword(ctx, p, "currentPassword", currentPassword)
	if err != nil {
		return User{}, ch, err
	}
	var hash string
	if in.Password != nil {
		if hash, err = a.newHash(ctx, *in.Password); err != nil {
			return User{}, ch, err
		}
	}
	err = a.db.Tx(ctx, func(tx *sql.Tx) error {
		ch = UserChange{}
		if err := requireAdmin(ctx, tx, p, verified); err != nil {
			return err
		}
		var (
			role    string
			hadTOTP bool
		)
		err := tx.QueryRowContext(ctx, `SELECT role, totp_secret IS NOT NULL FROM auth_users WHERE id = ?`, id).Scan(&role, &hadTOTP)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.NotFound("account", id)
		}
		if err != nil {
			return err
		}
		endSessions := func() error {
			n, err := deleteVerified(ctx, tx, "auth_sessions", "user_id = ?", id)
			ch.SessionsRevoked += n
			return err
		}
		if in.Role != nil {
			ch.Role = *in.Role
		}
		if in.Role != nil && *in.Role != role {
			if role == RoleAdmin {
				n, err := countAdmins(ctx, tx)
				if err != nil {
					return err
				}
				if n <= 1 {
					return errLastAdmin()
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE auth_users SET role = ? WHERE id = ?`, *in.Role, id); err != nil {
				return err
			}
			ch.RoleChanged = true
			if err := endSessions(); err != nil {
				return err
			}
			if *in.Role != RoleAdmin {
				n, err := deleteVerified(ctx, tx, "auth_tokens", "user_id = ? AND scope = 'admin'", id)
				ch.TokensRevoked += n
				if err != nil {
					return err
				}
			}
		}
		if in.Password != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE auth_users SET password_hash = ? WHERE id = ?`, hash, id); err != nil {
				return err
			}
			ch.PasswordReset = true
			if err := endSessions(); err != nil {
				return err
			}
			n, err := deleteVerified(ctx, tx, "auth_tokens", "user_id = ?", id)
			ch.TokensRevoked += n
			if err != nil {
				return err
			}
		}
		if in.DisableTOTP && hadTOTP {
			if _, err := tx.ExecContext(ctx, `UPDATE auth_users SET totp_secret = NULL, totp_pending = NULL,
				totp_pending_at = 0 WHERE id = ?`, id); err != nil {
				return err
			}
			ch.TOTPDisabled = true
			return endSessions()
		}
		return nil
	})
	if err != nil {
		return User{}, ch, err
	}
	u, err := a.userByID(ctx, id)
	return u.User, ch, err
}

// DeleteUser deletes an account with its sessions and API tokens (deleted
// and verified in the same transaction, not left to the foreign key
// cascade). Deleting oneself is allowed unless it removes the last admin.
// It returns the deleted account.
func (a *Service) DeleteUser(ctx context.Context, p *Principal, id int64, currentPassword string) (User, error) {
	if p == nil {
		return User{}, errNotAuthenticated
	}
	verified, err := a.verifyUserPassword(ctx, p, "currentPassword", currentPassword)
	if err != nil {
		return User{}, err
	}
	var u userRow
	err = a.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := requireAdmin(ctx, tx, p, verified); err != nil {
			return err
		}
		var err error
		u, err = scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM auth_users WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.NotFound("account", id)
		}
		if err != nil {
			return err
		}
		if u.Role == RoleAdmin {
			n, err := countAdmins(ctx, tx)
			if err != nil {
				return err
			}
			if n <= 1 {
				return errLastAdmin()
			}
		}
		for _, t := range []string{"auth_sessions", "auth_tokens"} {
			if _, err := deleteVerified(ctx, tx, t, "user_id = ?", id); err != nil {
				return err
			}
		}
		_, err = deleteVerified(ctx, tx, "auth_users", "id = ?", id)
		return err
	})
	if err != nil {
		return User{}, err
	}
	a.log.Info("account deleted", slog.String("username", u.Username), slog.String("by", p.Username))
	return u.User, nil
}

// ListUsers returns all accounts of a database, oldest first, without
// changing it (CLI `picache users`). Accounts of a database from before
// roles (auth schema v1) are all admins.
func ListUsers(ctx context.Context, d *db.DB) ([]User, error) {
	var hasRole bool
	if err := d.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pragma_table_info('auth_users') WHERE name = 'role')`).
		Scan(&hasRole); err != nil {
		return nil, err
	}
	role := "role"
	if !hasRole {
		role = "'admin'"
	}
	rows, err := d.R.QueryContext(ctx, `SELECT id, username, `+role+`, totp_secret IS NOT NULL, created_at, last_login_at
		FROM auth_users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		var created, lastLogin int64
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.TOTPEnabled, &created, &lastLogin); err != nil {
			return nil, err
		}
		u.CreatedAt, u.LastLoginAt = db.Time(created), db.Time(lastLogin)
		out = append(out, u)
	}
	return out, rows.Err()
}
