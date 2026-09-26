package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

const (
	tokenPrefix       = "pc_"
	tokenLen          = len(tokenPrefix) + 64 // "pc_" + hex of 32 random bytes
	tokenDisplayChars = 7                     // "pc_ab12"
	maxTokens         = 100
	maxTokensPerUser  = 20
	maxTokenNameLen   = 64
	maxTokenTTL       = 3650 * 24 * time.Hour
)

// authToken resolves an API token and records its use (at most once per
// minute).
func (a *Service) authToken(ctx context.Context, tok string) (*Principal, error) {
	if len(tok) != tokenLen {
		return nil, errNotAuthenticated
	}
	var (
		p                 Principal
		scope             string
		expires, lastUsed int64
	)
	err := a.db.R.QueryRowContext(ctx, `SELECT t.id, t.user_id, t.scope, t.expires_at, t.last_used, u.username, u.role
		FROM auth_tokens t JOIN auth_users u ON u.id = t.user_id WHERE t.hash = ?`, hashSecret(tok)).
		Scan(&p.TokenID, &p.UserID, &scope, &expires, &lastUsed, &p.Username, &p.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotAuthenticated
	}
	if err != nil {
		return nil, err
	}
	now := a.now()
	if expires != 0 && now.UnixMilli() >= expires {
		return nil, apperr.Unauthorized("API token expired")
	}
	// An admin token acts with admin rights only while its owner is an
	// admin (read on every request, like the role of a session).
	p.Scope = ScopeRead
	if Scope(scope) == ScopeAdmin && p.Role == RoleAdmin {
		p.Scope = ScopeAdmin
	}
	if now.Sub(db.Time(lastUsed)) >= lastSeenGranularity {
		if _, err := a.db.W.ExecContext(ctx, `UPDATE auth_tokens SET last_used = ? WHERE id = ?`, now.UnixMilli(), p.TokenID); err != nil {
			return nil, err
		}
	}
	return &p, nil
}

// Tokens lists API tokens with their owners: all of them for an admin,
// the caller's own for a viewer.
func (a *Service) Tokens(ctx context.Context, p *Principal) ([]TokenInfo, error) {
	if p == nil {
		return nil, errNotAuthenticated
	}
	q := `SELECT t.id, t.name, t.scope, t.prefix, t.created_at, t.expires_at, t.last_used, t.user_id, u.username
		FROM auth_tokens t JOIN auth_users u ON u.id = t.user_id`
	var args []any
	if p.Scope != ScopeAdmin {
		q, args = q+` WHERE t.user_id = ?`, append(args, p.UserID)
	}
	rows, err := a.db.R.QueryContext(ctx, q+` ORDER BY t.created_at DESC, t.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TokenInfo{}
	for rows.Next() {
		var t TokenInfo
		var created, expires, lastUsed int64
		if err := rows.Scan(&t.ID, &t.Name, &t.Scope, &t.Prefix, &created, &expires, &lastUsed, &t.UserID, &t.Username); err != nil {
			return nil, err
		}
		t.CreatedAt, t.ExpiresAt, t.LastUsed = db.Time(created), db.Time(expires), db.Time(lastUsed)
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateToken creates an API token of the signed-in user and returns the
// secret once. It requires the user's password: a token outlives sessions
// and password changes that keep tokens, so a stolen session alone must not
// be enough to create one. Viewers can create read tokens only; an account
// has at most 20 tokens, all accounts together 100.
func (a *Service) CreateToken(ctx context.Context, p *Principal, currentPassword, name string, scope Scope, ttl time.Duration) (string, TokenInfo, error) {
	if p == nil {
		return "", TokenInfo{}, errNotAuthenticated
	}
	name = strings.TrimSpace(name)
	if err := validateTokenName(name); err != nil {
		return "", TokenInfo{}, err
	}
	if scope != ScopeRead && scope != ScopeAdmin {
		return "", TokenInfo{}, apperr.Invalid("scope", "must be read or admin")
	}
	if scope == ScopeAdmin {
		u, err := a.userByID(ctx, p.UserID)
		if err != nil {
			return "", TokenInfo{}, err
		}
		if u.Role != RoleAdmin {
			return "", TokenInfo{}, apperr.Invalid("scope", "viewers can create read tokens only")
		}
	}
	if ttl < 0 || ttl > maxTokenTTL {
		return "", TokenInfo{}, apperr.Invalid("expiresInDays", "must be between 0 (never) and 3650 days")
	}
	verified, err := a.verifyUserPassword(ctx, p, "currentPassword", currentPassword)
	if err != nil {
		return "", TokenInfo{}, err
	}
	raw := make([]byte, 32)
	rand.Read(raw)
	secret := tokenPrefix + hex.EncodeToString(raw)
	now := a.now()
	info := TokenInfo{Name: name, Scope: scope, Prefix: secret[:tokenDisplayChars], CreatedAt: now.UTC().Truncate(time.Millisecond),
		UserID: p.UserID}
	if ttl > 0 {
		info.ExpiresAt = now.Add(ttl).UTC().Truncate(time.Millisecond)
	}
	err = a.db.Tx(ctx, func(tx *sql.Tx) error {
		// The role, the session and the password again, serialised with
		// demotions and resets: a token requested while one of them
		// committed must not outlive it.
		role, err := recheckCaller(ctx, tx, p, "currentPassword", verified)
		if err != nil {
			return err
		}
		if scope == ScopeAdmin && role != RoleAdmin {
			return apperr.Invalid("scope", "viewers can create read tokens only")
		}
		var mine, n int
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT username FROM auth_users WHERE id = ?),
			(SELECT COUNT(*) FROM auth_tokens WHERE user_id = ?), (SELECT COUNT(*) FROM auth_tokens)`, p.UserID, p.UserID).
			Scan(&info.Username, &mine, &n); err != nil {
			return err
		}
		if mine >= maxTokensPerUser {
			return apperr.Conflict("at most %d API tokens per account; delete unused ones first", maxTokensPerUser)
		}
		if n >= maxTokens {
			return apperr.Conflict("at most %d API tokens can exist; delete unused ones first", maxTokens)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO auth_tokens (hash, user_id, name, scope, prefix, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			hashSecret(secret), p.UserID, name, string(scope), info.Prefix, db.Ms(info.CreatedAt), db.Ms(info.ExpiresAt))
		if err != nil {
			return err
		}
		info.ID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return "", TokenInfo{}, err
	}
	return secret, info, nil
}

// DeleteToken revokes an API token: any token for an admin, only the
// caller's own for a viewer (another user's token is "not found").
func (a *Service) DeleteToken(ctx context.Context, p *Principal, id int64) error {
	if p == nil {
		return errNotAuthenticated
	}
	where, args := "id = ?", []any{id}
	if p.Scope != ScopeAdmin {
		where, args = "id = ? AND user_id = ?", append(args, p.UserID)
	}
	n, err := deleteVerified(ctx, a.db.W, "auth_tokens", where, args...)
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.NotFound("API token", id)
	}
	return nil
}

func validateTokenName(name string) error {
	if name == "" || utf8.RuneCountInString(name) > maxTokenNameLen || !utf8.ValidString(name) {
		return apperr.Invalid("name", "must be 1 to %d characters", maxTokenNameLen)
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return apperr.Invalid("name", "must not contain control characters")
		}
	}
	return nil
}
