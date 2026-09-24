package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

const (
	sessionTokenLen     = 43 // base64url of 32 random bytes
	maxSessionsPerUser  = 32 // the oldest sessions are ended beyond this
	lastSeenGranularity = time.Minute
	maxUserAgentLen     = 256
)

var errNotAuthenticated = apperr.Unauthorized("not authenticated")

// hashSecret returns the SHA-256 of a session or token secret (the stored form).
func hashSecret(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

// userRow is an auth_users row.
type userRow struct {
	User
	passwordHash string
	totpSealed   sql.NullString
}

const userColumns = `id, username, password_hash, totp_secret, created_at, last_login_at`

func scanUser(row interface{ Scan(...any) error }) (userRow, error) {
	var u userRow
	var created, lastLogin int64
	if err := row.Scan(&u.ID, &u.Username, &u.passwordHash, &u.totpSealed, &created, &lastLogin); err != nil {
		return u, err
	}
	u.TOTPEnabled = u.totpSealed.Valid
	u.CreatedAt, u.LastLoginAt = db.Time(created), db.Time(lastLogin)
	return u, nil
}

// userByID loads a user; a missing user is reported as unauthenticated (the
// principal's account was deleted).
func (a *Service) userByID(ctx context.Context, id int64) (userRow, error) {
	u, err := scanUser(a.db.R.QueryRowContext(ctx, `SELECT `+userColumns+` FROM auth_users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return u, errNotAuthenticated
	}
	return u, err
}

// Login verifies credentials (+ TOTP if enabled) with throttling per client
// and per username (see throttle). A browser with a device cookie issued for
// this username is throttled by its device instead of the username
// (device.go); its failures still count for the username, and its success
// leaves the username delay as it is (it says nothing about the other
// browsers that failed).
func (a *Service) Login(ctx context.Context, username, password, totp string, meta ReqMeta) (*Session, error) {
	ckey, ukey := clientThrottleKey(meta.IP), userThrottleKey(username)
	checked, counted := []string{ckey, ukey}, []string{ckey, ukey}
	if dev, ok := a.knownDevice(meta.Devices, username); ok {
		dkey := deviceThrottleKey(dev.id)
		checked, counted = []string{ckey, dkey}, []string{ckey, ukey, dkey}
	}
	if err := a.throttle.allow(a.now(), checked...); err != nil {
		return nil, err
	}
	fail := func(reason string, err error) (*Session, error) {
		a.recordFailure(counted...)
		a.auditFailure(ctx, meta, reason)
		return nil, err
	}
	errBadCredentials := apperr.Unauthorized("wrong username or password")
	if len(password) > maxPasswordBytes {
		return fail("password", errBadCredentials)
	}

	u, err := scanUser(a.db.R.QueryRowContext(ctx, `SELECT `+userColumns+` FROM auth_users WHERE username = ?`, username))
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	encoded := dummyHash()
	if found {
		encoded = u.passwordHash
	}
	ok, newHash, err := a.checkPassword(ctx, encoded, password)
	if err != nil {
		return nil, err
	}
	if !ok || !found {
		return fail("password", errBadCredentials)
	}

	if u.TOTPEnabled {
		if strings.TrimSpace(totp) == "" {
			// The password was right: asking for the code is not a failure.
			return nil, ErrTOTPRequired()
		}
		valid, err := a.consumeTOTP(ctx, u.ID, u.totpSealed.String, totp)
		if err != nil {
			return nil, err
		}
		if !valid {
			return fail("totp", errTOTPWrong())
		}
	}
	a.throttle.succeed(checked...)

	now := a.now()
	if newHash != "" {
		if _, err := a.db.W.ExecContext(ctx, `UPDATE auth_users SET password_hash = ? WHERE id = ?`, newHash, u.ID); err != nil {
			a.log.Warn("could not upgrade password hash", slog.Any("err", err))
		}
	}
	if _, err := a.db.W.ExecContext(ctx, `UPDATE auth_users SET last_login_at = ? WHERE id = ?`, now.UnixMilli(), u.ID); err != nil {
		return nil, err
	}
	s, err := a.createSession(ctx, u.ID, meta)
	if err != nil {
		return nil, err
	}
	s.Device = a.issueDevice(u.ID, u.Username)
	method := "password"
	if u.TOTPEnabled {
		method = "password+totp"
	}
	a.Audit(ctx, &Principal{UserID: u.ID, Username: u.Username, SessionID: s.ID, Scope: ScopeAdmin},
		meta.IP, "auth.login", u.Username, map[string]string{"method": method})
	return s, nil
}

// createSession stores a new session for userID and ends the user's oldest
// sessions beyond maxSessionsPerUser.
func (a *Service) createSession(ctx context.Context, userID int64, meta ReqMeta) (*Session, error) {
	raw := make([]byte, 32)
	rand.Read(raw)
	tok := base64.RawURLEncoding.EncodeToString(raw)
	h := hashSecret(tok)
	id := hex.EncodeToString(h[:8])
	now := a.now()
	ua := truncateUTF8(meta.UserAgent, maxUserAgentLen)
	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO auth_sessions (id, hash, user_id, created_at, last_seen, ip, user_agent) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, h, userID, now.UnixMilli(), now.UnixMilli(), meta.IP, ua); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM auth_sessions WHERE user_id = ? AND id NOT IN
			(SELECT id FROM auth_sessions WHERE user_id = ? ORDER BY last_seen DESC, created_at DESC LIMIT ?)`,
			userID, userID, maxSessionsPerUser)
		return err
	})
	if err != nil {
		return nil, err
	}
	maxAge := time.Duration(a.set.Get().Web.SessionMaxHours) * time.Hour
	return &Session{ID: id, Token: tok, UserID: userID, ExpiresAt: now.Add(maxAge)}, nil
}

// sessionExpiry is when a session ends: the earlier of the absolute and the
// idle limit (current settings, so lowering them affects existing sessions).
func (a *Service) sessionExpiry(created, lastSeen time.Time) time.Time {
	w := a.set.Get().Web
	abs := created.Add(time.Duration(w.SessionMaxHours) * time.Hour)
	idle := lastSeen.Add(time.Duration(w.SessionIdleMinutes) * time.Minute)
	if idle.Before(abs) {
		return idle
	}
	return abs
}

// Logout ends the session with the given cookie token.
func (a *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	_, err := deleteVerified(ctx, a.db.W, "auth_sessions", "hash = ?", hashSecret(token))
	return err
}

// Authenticate resolves the principal from the session cookie or a Bearer
// token and refreshes the session idle timer. Returns apperr.Unauthorized.
//
// A Bearer value starting with "pc_" is an API token, any other Bearer value
// a session token. Authorization headers with other schemes are ignored (a
// reverse proxy may use Basic auth in front of PiCache). Both session cookie
// names are accepted, the __Host- one first (cookies.go).
func (a *Service) Authenticate(r *http.Request) (*Principal, error) {
	p, err := a.authenticate(r)
	if err != nil {
		return nil, err
	}
	if ip := netutil.AddrFromRemote(r.RemoteAddr); ip.IsValid() {
		p.IP = ip.String()
	}
	return p, nil
}

func (a *Service) authenticate(r *http.Request) (*Principal, error) {
	if tok, ok := bearerToken(r); ok {
		if strings.HasPrefix(tok, tokenPrefix) {
			return a.authToken(r.Context(), tok)
		}
		return a.authSession(r.Context(), tok)
	}
	err := errNotAuthenticated
	for _, v := range sessionCookies(r) {
		var p *Principal
		if p, err = a.authSession(r.Context(), v); err == nil {
			return p, nil
		}
		if apperr.KindOf(err) != apperr.KindUnauthorized {
			return nil, err
		}
	}
	return nil, err
}

func bearerToken(r *http.Request) (string, bool) {
	scheme, val, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	val = strings.TrimSpace(val)
	return val, val != ""
}

func (a *Service) authSession(ctx context.Context, tok string) (*Principal, error) {
	if len(tok) != sessionTokenLen {
		return nil, errNotAuthenticated
	}
	var (
		p                 = Principal{Scope: ScopeAdmin}
		created, lastSeen int64
	)
	err := a.db.R.QueryRowContext(ctx, `SELECT s.id, s.user_id, s.created_at, s.last_seen, u.username
		FROM auth_sessions s JOIN auth_users u ON u.id = s.user_id WHERE s.hash = ?`, hashSecret(tok)).
		Scan(&p.SessionID, &p.UserID, &created, &lastSeen, &p.Username)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotAuthenticated
	}
	if err != nil {
		return nil, err
	}
	now := a.now()
	if !now.Before(a.sessionExpiry(db.Time(created), db.Time(lastSeen))) {
		if _, err := a.db.W.ExecContext(ctx, `DELETE FROM auth_sessions WHERE id = ?`, p.SessionID); err != nil {
			a.log.Warn("delete expired session", slog.Any("err", err))
		}
		return nil, apperr.Unauthorized("session expired; please sign in again")
	}
	if now.Sub(db.Time(lastSeen)) >= lastSeenGranularity {
		if _, err := a.db.W.ExecContext(ctx, `UPDATE auth_sessions SET last_seen = ? WHERE id = ?`, now.UnixMilli(), p.SessionID); err != nil {
			return nil, err
		}
	}
	return &p, nil
}

// Valid reports whether p's session or token is still active (used by
// long-lived SSE streams every 15 s).
func (a *Service) Valid(ctx context.Context, p *Principal) bool {
	if p == nil {
		return false
	}
	now := a.now()
	if p.TokenID != 0 {
		var expires int64
		err := a.db.R.QueryRowContext(ctx, `SELECT expires_at FROM auth_tokens WHERE id = ? AND user_id = ?`, p.TokenID, p.UserID).Scan(&expires)
		return err == nil && (expires == 0 || now.UnixMilli() < expires)
	}
	var created, lastSeen int64
	err := a.db.R.QueryRowContext(ctx, `SELECT created_at, last_seen FROM auth_sessions WHERE id = ? AND user_id = ?`,
		p.SessionID, p.UserID).Scan(&created, &lastSeen)
	return err == nil && now.Before(a.sessionExpiry(db.Time(created), db.Time(lastSeen)))
}

// Me returns the user of a principal.
func (a *Service) Me(ctx context.Context, p *Principal) (User, error) {
	if p == nil {
		return User{}, errNotAuthenticated
	}
	u, err := a.userByID(ctx, p.UserID)
	return u.User, err
}

// ChangePassword changes the password and revokes all other sessions and,
// unless keepTokens, the user's API tokens: changing the password is how an
// owner ends access that someone else may have set up (a stolen session
// could have been used to create a token).
func (a *Service) ChangePassword(ctx context.Context, p *Principal, current, next string, keepTokens bool) error {
	if err := validatePassword("newPassword", next); err != nil {
		return err
	}
	if current == next {
		return apperr.Invalid("newPassword", "must differ from the current password")
	}
	if err := a.verifyUserPassword(ctx, p, "currentPassword", current); err != nil {
		return err
	}
	h, err := a.newHash(ctx, next)
	if err != nil {
		return err
	}
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE auth_users SET password_hash = ? WHERE id = ?`, h, p.UserID); err != nil {
			return err
		}
		if _, err := deleteVerified(ctx, tx, "auth_sessions", "user_id = ? AND id != ?", p.UserID, p.SessionID); err != nil {
			return err
		}
		if keepTokens {
			return nil
		}
		_, err := deleteVerified(ctx, tx, "auth_tokens", "user_id = ?", p.UserID)
		return err
	})
}

// checkUserPassword re-checks the password of an authenticated user with
// the login throttle (per client, the global limit, and per session: 5
// failures → 15 min), so a stolen session cannot be used to guess the
// password faster than a single client can at the sign-in form. The session
// key replaces the username delay: failed sign-ins that other hosts cause
// for the username must not keep the signed-in owner from changing the
// password (and a success does not reset that delay either). It reports
// false for a wrong password (the failure is recorded and audited like a
// failed sign-in).
func (a *Service) checkUserPassword(ctx context.Context, p *Principal, pw string) (bool, error) {
	if p == nil {
		return false, errNotAuthenticated
	}
	u, err := a.userByID(ctx, p.UserID)
	if err != nil {
		return false, err
	}
	ckey, ukey := clientThrottleKey(p.IP), userThrottleKey(u.Username)
	akey := ukey // only sessions confirm passwords (permSession); the username otherwise
	if p.SessionID != "" {
		akey = sessionThrottleKey(p.SessionID)
	}
	if err := a.throttle.allow(a.now(), ckey, akey); err != nil {
		return false, err
	}
	ok := false
	if len(pw) <= maxPasswordBytes {
		if ok, _, err = a.checkPassword(ctx, u.passwordHash, pw); err != nil {
			return false, err
		}
	}
	if !ok {
		a.recordFailure(ckey, akey)
		a.auditFailure(ctx, ReqMeta{IP: p.IP}, "password confirmation")
		return false, nil
	}
	a.throttle.succeed(ckey, akey)
	return true, nil
}

// verifyUserPassword re-checks the password of an authenticated user
// (password change, creating an API token, starting or disabling TOTP) and
// reports a missing or wrong password as invalid input of field.
func (a *Service) verifyUserPassword(ctx context.Context, p *Principal, field, pw string) error {
	if pw == "" {
		return apperr.Invalid(field, "enter your current password")
	}
	ok, err := a.checkUserPassword(ctx, p, pw)
	if err != nil {
		return err
	}
	if !ok {
		return apperr.Invalid(field, "wrong password")
	}
	return nil
}

// ConfirmPassword re-checks the password of a signed-in user before an
// action that replaces the configuration (restore). A missing or wrong
// password is reported as 401 with field "password"; attempts are throttled
// like sign-ins.
func (a *Service) ConfirmPassword(ctx context.Context, p *Principal, pw string) error {
	if pw == "" {
		return &apperr.Error{Kind: apperr.KindUnauthorized, Field: "password", Message: "enter your password to confirm"}
	}
	ok, err := a.checkUserPassword(ctx, p, pw)
	if err != nil {
		return err
	}
	if !ok {
		return &apperr.Error{Kind: apperr.KindUnauthorized, Field: "password", Message: "wrong password"}
	}
	return nil
}

// Sessions lists the user's sessions.
func (a *Service) Sessions(ctx context.Context, p *Principal) ([]SessionInfo, error) {
	rows, err := a.db.R.QueryContext(ctx, `SELECT id, created_at, last_seen, ip, user_agent
		FROM auth_sessions WHERE user_id = ? ORDER BY last_seen DESC`, p.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := a.now()
	out := []SessionInfo{}
	for rows.Next() {
		var s SessionInfo
		var created, lastSeen int64
		if err := rows.Scan(&s.ID, &created, &lastSeen, &s.IP, &s.UserAgent); err != nil {
			return nil, err
		}
		s.CreatedAt, s.LastSeen = db.Time(created), db.Time(lastSeen)
		s.ExpiresAt = a.sessionExpiry(s.CreatedAt, s.LastSeen)
		if !now.Before(s.ExpiresAt) {
			continue
		}
		s.Current = s.ID == p.SessionID
		out = append(out, s)
	}
	return out, rows.Err()
}

// RevokeSession ends one of the user's sessions.
func (a *Service) RevokeSession(ctx context.Context, p *Principal, id string) error {
	if len(id) != 16 || strings.Trim(id, "0123456789abcdef") != "" {
		return apperr.Invalid("id", "invalid session id")
	}
	n, err := deleteVerified(ctx, a.db.W, "auth_sessions", "id = ? AND user_id = ?", id, p.UserID)
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.NotFound("session", id)
	}
	return nil
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
