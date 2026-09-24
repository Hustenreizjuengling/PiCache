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
	err := a.db.R.QueryRowContext(ctx, `SELECT t.id, t.user_id, t.scope, t.expires_at, t.last_used, u.username
		FROM auth_tokens t JOIN auth_users u ON u.id = t.user_id WHERE t.hash = ?`, hashSecret(tok)).
		Scan(&p.TokenID, &p.UserID, &scope, &expires, &lastUsed, &p.Username)
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
	p.Scope = Scope(scope)
	if now.Sub(db.Time(lastUsed)) >= lastSeenGranularity {
		if _, err := a.db.W.ExecContext(ctx, `UPDATE auth_tokens SET last_used = ? WHERE id = ?`, now.UnixMilli(), p.TokenID); err != nil {
			return nil, err
		}
	}
	return &p, nil
}

// Tokens lists API tokens.
func (a *Service) Tokens(ctx context.Context) ([]TokenInfo, error) {
	rows, err := a.db.R.QueryContext(ctx, `SELECT id, name, scope, prefix, created_at, expires_at, last_used
		FROM auth_tokens ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TokenInfo{}
	for rows.Next() {
		var t TokenInfo
		var created, expires, lastUsed int64
		if err := rows.Scan(&t.ID, &t.Name, &t.Scope, &t.Prefix, &created, &expires, &lastUsed); err != nil {
			return nil, err
		}
		t.CreatedAt, t.ExpiresAt, t.LastUsed = db.Time(created), db.Time(expires), db.Time(lastUsed)
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateToken creates an API token and returns the secret once. It requires
// the user's password: a token outlives sessions and password changes that
// keep tokens, so a stolen session alone must not be enough to create one.
func (a *Service) CreateToken(ctx context.Context, p *Principal, currentPassword, name string, scope Scope, ttl time.Duration) (string, TokenInfo, error) {
	name = strings.TrimSpace(name)
	if err := validateTokenName(name); err != nil {
		return "", TokenInfo{}, err
	}
	if scope != ScopeRead && scope != ScopeAdmin {
		return "", TokenInfo{}, apperr.Invalid("scope", "must be read or admin")
	}
	if ttl < 0 || ttl > maxTokenTTL {
		return "", TokenInfo{}, apperr.Invalid("expiresInDays", "must be between 0 (never) and 3650 days")
	}
	if err := a.verifyUserPassword(ctx, p, "currentPassword", currentPassword); err != nil {
		return "", TokenInfo{}, err
	}
	raw := make([]byte, 32)
	rand.Read(raw)
	secret := tokenPrefix + hex.EncodeToString(raw)
	now := a.now()
	info := TokenInfo{Name: name, Scope: scope, Prefix: secret[:tokenDisplayChars], CreatedAt: now.UTC().Truncate(time.Millisecond)}
	if ttl > 0 {
		info.ExpiresAt = now.Add(ttl).UTC().Truncate(time.Millisecond)
	}
	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_tokens`).Scan(&n); err != nil {
			return err
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

// DeleteToken revokes an API token.
func (a *Service) DeleteToken(ctx context.Context, id int64) error {
	n, err := deleteVerified(ctx, a.db.W, "auth_tokens", "id = ?", id)
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
