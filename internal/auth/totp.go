package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// TOTP parameters (RFC 6238 defaults understood by every authenticator app).
const (
	totpPeriod     = 30 // seconds
	totpSkew       = 1  // accepted steps before/after the current one
	totpSecretLen  = 20 // bytes (160 bit, RFC 4226 recommendation)
	totpPendingTTL = 10 * time.Minute
	totpIssuer     = "PiCache"
)

var totpB32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// totpAAD binds a sealed TOTP secret to its user.
func totpAAD(userID int64) string { return "picache/auth/totp/" + strconv.FormatInt(userID, 10) }

// totpCode computes the 6-digit code for a time step (RFC 4226 HOTP).
func totpCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1_000_000)
}

// totpMatch returns the step within ±totpSkew of now whose code equals code
// (the latest if several match), or -1. All candidates are compared in
// constant time.
func totpMatch(secret []byte, code string, now time.Time) int64 {
	cur := now.Unix() / totpPeriod
	match := int64(-1)
	for s := cur - totpSkew; s <= cur+totpSkew; s++ {
		if subtle.ConstantTimeCompare([]byte(totpCode(secret, s)), []byte(code)) == 1 {
			match = s
		}
	}
	return match
}

// normalizeTOTP strips spaces and reports whether code has 6 digits.
func normalizeTOTP(code string) (string, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 {
		return "", false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < '0' || code[i] > '9' {
			return "", false
		}
	}
	return code, true
}

// openTOTP decrypts a sealed TOTP secret.
func (a *Service) openTOTP(userID int64, sealed string) ([]byte, error) {
	secret, err := a.box.Open(sealed, totpAAD(userID))
	if err != nil {
		return nil, fmt.Errorf("auth: cannot decrypt the TOTP secret (master key changed?); "+
			"run `picache reset-password` to disable two-factor authentication: %w", err)
	}
	return secret, nil
}

// consumeTOTP verifies code for a login and marks its step as used. It
// reports false for wrong codes and for steps that were already used.
func (a *Service) consumeTOTP(ctx context.Context, userID int64, sealed, code string) (bool, error) {
	c, ok := normalizeTOTP(code)
	if !ok {
		return false, nil
	}
	secret, err := a.openTOTP(userID, sealed)
	if err != nil {
		return false, err
	}
	step := totpMatch(secret, c, a.now())
	if step < 0 {
		return false, nil
	}
	res, err := a.db.W.ExecContext(ctx,
		`UPDATE auth_users SET totp_last_step = ? WHERE id = ? AND totp_last_step < ?`, step, userID, step)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// TOTPBegin generates a new (unconfirmed) TOTP secret; returns it and the
// otpauth:// URI. It requires the password: someone with a stolen session
// must not be able to put their own authenticator on the account.
func (a *Service) TOTPBegin(ctx context.Context, p *Principal, currentPassword string) (secret, uri string, err error) {
	u, err := a.userByID(ctx, p.UserID)
	if err != nil {
		return "", "", err
	}
	if u.TOTPEnabled {
		return "", "", apperr.Conflict("two-factor authentication is already enabled; disable it first")
	}
	if err := a.verifyUserPassword(ctx, p, "currentPassword", currentPassword); err != nil {
		return "", "", err
	}
	raw := make([]byte, totpSecretLen)
	rand.Read(raw)
	sealed, err := a.box.Seal(raw, totpAAD(u.ID))
	if err != nil {
		return "", "", err
	}
	if _, err := a.db.W.ExecContext(ctx, `UPDATE auth_users SET totp_pending = ?, totp_pending_at = ? WHERE id = ?`,
		sealed, a.now().UnixMilli(), u.ID); err != nil {
		return "", "", err
	}
	secret = totpB32.EncodeToString(raw)
	return secret, otpauthURI(u.Username, secret), nil
}

// otpauthURI builds the Key URI Format understood by authenticator apps.
func otpauthURI(username, secret string) string {
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", totpIssuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", strconv.Itoa(totpPeriod))
	u := url.URL{Scheme: "otpauth", Host: "totp", Path: "/" + totpIssuer + ":" + username, RawQuery: q.Encode()}
	return u.String()
}

// TOTPConfirm enables TOTP after verifying a code for the pending secret and
// signs out the user's other sessions (they were created without a code).
func (a *Service) TOTPConfirm(ctx context.Context, p *Principal, code string) error {
	c, ok := normalizeTOTP(code)
	if !ok {
		return apperr.Invalid("code", "enter the 6-digit code from your authenticator app")
	}
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		var (
			enabled           bool
			pending           sql.NullString
			pendingAt, lastSt int64
		)
		err := tx.QueryRowContext(ctx, `SELECT totp_secret IS NOT NULL, totp_pending, totp_pending_at, totp_last_step
			FROM auth_users WHERE id = ?`, p.UserID).Scan(&enabled, &pending, &pendingAt, &lastSt)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotAuthenticated
		}
		if err != nil {
			return err
		}
		if enabled {
			return apperr.Conflict("two-factor authentication is already enabled")
		}
		if !pending.Valid || a.now().Sub(db.Time(pendingAt)) > totpPendingTTL {
			return apperr.Conflict("no pending two-factor setup (it expires after 10 minutes); start again")
		}
		secret, err := a.openTOTP(p.UserID, pending.String)
		if err != nil {
			return err
		}
		step := totpMatch(secret, c, a.now())
		if step < 0 || step <= lastSt {
			return apperr.Invalid("code", "wrong code; check that the time on your device is correct")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE auth_users SET totp_secret = totp_pending, totp_pending = NULL,
			totp_pending_at = 0, totp_last_step = ? WHERE id = ?`, step, p.UserID); err != nil {
			return err
		}
		_, err = deleteVerified(ctx, tx, "auth_sessions", "user_id = ? AND id != ?", p.UserID, p.SessionID)
		return err
	})
}

// TOTPDisable disables TOTP (requires the password).
func (a *Service) TOTPDisable(ctx context.Context, p *Principal, password string) error {
	if err := a.verifyUserPassword(ctx, p, "password", password); err != nil {
		return err
	}
	_, err := a.db.W.ExecContext(ctx,
		`UPDATE auth_users SET totp_secret = NULL, totp_pending = NULL, totp_pending_at = 0 WHERE id = ?`, p.UserID)
	return err
}
