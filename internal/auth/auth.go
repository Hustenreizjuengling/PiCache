// Package auth implements web authentication: users with argon2id password
// hashes, sessions, API tokens, optional TOTP, login throttling, the
// first-run setup token and the audit log (docs/ARCHITECTURE.md 6.1, 12).
//
// Tables (picache.db, component "auth"): auth_users, auth_sessions,
// auth_tokens, auth_audit. Sessions and tokens are stored as SHA-256 hashes
// of the random secret. Authenticate reads the session row from the DB on
// every request (no in-memory cache), so revocations by the CLI or another
// session take effect immediately.
//
// Rules:
//   - Passwords: argon2id m=19456 KiB, t=2, p=1, PHC string, rehash on login
//     when parameters change; ≥ 10 characters; max 2 concurrent hash
//     computations (semaphore).
//   - Throttling (throttle.go): per netutil.ClientKey 5 failures → 15 min
//     lockout; per username a progressive delay after 5 failures (1 s,
//     doubling, at most 30 s; reset on success); TOTP failures count too;
//     global cap of 10 attempts/s. A browser that signed in before presents
//     a sealed device cookie (device.go) and is throttled by its device key
//     (5 failures → 15 min) instead of the username delay, so failures that
//     other hosts cause for the username cannot keep it out. Password
//     confirmations of signed-in users (password change, API tokens, TOTP,
//     restore) are throttled per client and per session (5 failures →
//     15 min each) and the global cap, not by the username delay.
//   - Account changes: creating an API token and starting TOTP enrolment
//     require the current password; a password change revokes the other
//     sessions and, unless kept explicitly, the API tokens; the CLI reset
//     revokes all sessions and tokens; enabling TOTP revokes the other
//     sessions.
//   - Backup and restore (backup.go): backups never contain accounts,
//     sessions or API tokens (the copy's triggers and views are dropped
//     first, and the tables are verified to be empty); a restore keeps the
//     accounts, API tokens and audit log of the running instance and ends
//     all sessions. Every revocation verifies that the rows are gone.
//   - Cookies (cookies.go): "__Host-picache_session" for requests over TLS,
//     "picache_session" over plain HTTP (Secure when PICACHE_WEB_SECURE_COOKIES
//     is set); Authenticate accepts both. The device cookie is named alike
//     ("__Host-picache_device" / "picache_device").
//   - TOTP (RFC 6238, SHA-1, 6 digits, 30 s, ±1 step): the secret is sealed
//     with the secrets.Box (AAD "picache/auth/totp/<userID>"); each step can
//     be used only once (auth_users.totp_last_step).
//   - Setup: token compared in constant time; the first user is created in
//     one BEGIN IMMEDIATE transaction that checks that no user exists; the
//     setup-token file is deleted on success.
//   - Audit: details are marshalled to JSON and every object member whose
//     lower-cased name is one of password, currentpassword, newpassword,
//     setuptoken, token, secret, code, totp is replaced by "[redacted]"
//     (recursively); details are truncated to 4 KiB. Retention: 365 days or
//     100 000 rows; failed logins are aggregated per client key per 10 min.
package auth

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Scope of a principal.
type Scope string

// Scopes. Browser sessions always have ScopeAdmin; API tokens have either.
const (
	ScopeAdmin Scope = "admin"
	ScopeRead  Scope = "read"
)

// ErrTOTPRequired is returned by Login when TOTP is enabled and the code is
// missing or wrong (the UI shows the code field when Field == "totp").
func ErrTOTPRequired() error {
	return &apperr.Error{Kind: apperr.KindUnauthorized, Field: "totp", Message: "enter the code from your authenticator app"}
}

// errTOTPWrong is returned by Login for a wrong or reused TOTP code.
func errTOTPWrong() error {
	return &apperr.Error{Kind: apperr.KindUnauthorized, Field: "totp", Message: "wrong or already used code"}
}

// User is an account (v1: a single admin, the model allows more).
type User struct {
	ID          int64     `json:"id"`
	Username    string    `json:"username"`
	TOTPEnabled bool      `json:"totpEnabled"`
	CreatedAt   time.Time `json:"createdAt"`
	LastLoginAt time.Time `json:"lastLoginAt,omitzero"`
}

// Principal is the authenticated caller of a request.
type Principal struct {
	UserID    int64
	Username  string
	SessionID string // session id (hash prefix) for "current" marking; "" for tokens
	TokenID   int64  // API token id, 0 for browser sessions
	Scope     Scope
	IP        string // client address of the request (throttles password confirmations)
}

// ReqMeta carries request metadata for throttling and audit.
type ReqMeta struct {
	IP        string
	UserAgent string
	Devices   []string // device cookie values of the request (DeviceCookieValues)
}

// Session is a newly created session (Token is the cookie value; only ever
// returned once).
type Session struct {
	ID        string
	Token     string
	UserID    int64
	ExpiresAt time.Time
	Device    string // new device cookie value for the browser (device.go); "" if none
}

// SessionInfo describes an active session.
type SessionInfo struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	LastSeen  time.Time `json:"lastSeen"`
	ExpiresAt time.Time `json:"expiresAt"`
	IP        string    `json:"ip"`
	UserAgent string    `json:"userAgent"`
	Current   bool      `json:"current"`
}

// TokenInfo describes an API token (never includes the secret).
type TokenInfo struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Scope     Scope     `json:"scope"`
	Prefix    string    `json:"prefix"` // first chars for recognition, e.g. "pc_ab12"
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
	LastUsed  time.Time `json:"lastUsed,omitzero"`
}

// AuditEntry is one audit log record.
type AuditEntry struct {
	ID       int64     `json:"id"`
	Time     time.Time `json:"time"`
	Username string    `json:"username"`
	IP       string    `json:"ip"`
	Action   string    `json:"action"` // e.g. "list.create", "settings.update", "auth.login"
	Target   string    `json:"target"`
	Details  string    `json:"details,omitempty"` // JSON, secrets redacted
}

// AuditQuery filters the audit log (newest first).
type AuditQuery struct {
	Search string
	Limit  int
	Offset int
}

// cleanupInterval is how often Start prunes expired sessions, stale TOTP
// enrolments, old audit rows and throttle records.
const cleanupInterval = 10 * time.Minute

// Service is safe for concurrent use.
type Service struct {
	db  *db.DB
	set *settings.Store
	box *secrets.Box
	log *slog.Logger

	now      func() time.Time // clock (tests replace it)
	hashSem  chan struct{}    // bounds concurrent argon2id computations
	throttle *throttle

	setupFile  string
	setupMu    sync.Mutex
	setupToken string      // "" once setup is done (guarded by setupMu)
	setupDone  atomic.Bool // a user exists (never becomes false again)
}

// New creates the service. setupTokenFile is where the one-time setup token
// is written (0600) while no user exists; the token is also logged at WARN.
func New(ctx context.Context, d *db.DB, set *settings.Store, box *secrets.Box, setupTokenFile string, log *slog.Logger) (*Service, error) {
	if err := d.Migrate(ctx, "auth", migrations); err != nil {
		return nil, err
	}
	a := &Service{
		db:        d,
		set:       set,
		box:       box,
		log:       log.With(slog.String("component", "auth")),
		now:       time.Now,
		hashSem:   make(chan struct{}, maxConcurrentHashes),
		throttle:  newThrottle(),
		setupFile: setupTokenFile,
	}
	if err := a.initSetup(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// Start runs session and audit cleanup until ctx ends (blocks until done).
func (a *Service) Start(ctx context.Context) {
	// Compute the timing-equalisation hash now, so that the first login with
	// an unknown username is not measurably slower than later ones.
	dummyHash()
	t := time.NewTicker(cleanupInterval)
	defer t.Stop()
	for {
		a.cleanup(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// cleanup deletes expired sessions and stale TOTP enrolments, applies the
// audit retention and forgets expired throttle records.
func (a *Service) cleanup(ctx context.Context) {
	now := a.now()
	w := a.set.Get().Web
	stmts := []struct {
		q    string
		args []any
	}{
		{`DELETE FROM auth_sessions WHERE created_at < ? OR last_seen < ?`, []any{
			now.Add(-time.Duration(w.SessionMaxHours) * time.Hour).UnixMilli(),
			now.Add(-time.Duration(w.SessionIdleMinutes) * time.Minute).UnixMilli(),
		}},
		{`UPDATE auth_users SET totp_pending = NULL, totp_pending_at = 0
		  WHERE totp_pending IS NOT NULL AND totp_pending_at < ?`, []any{now.Add(-totpPendingTTL).UnixMilli()}},
		{`DELETE FROM auth_audit WHERE time < ?`, []any{now.Add(-auditRetention).UnixMilli()}},
		{`DELETE FROM auth_audit WHERE id < (SELECT id FROM auth_audit ORDER BY id DESC LIMIT 1 OFFSET ?)`, []any{auditMaxRows - 1}},
	}
	for _, s := range stmts {
		if _, err := a.db.W.ExecContext(ctx, s.q, s.args...); err != nil {
			if ctx.Err() == nil {
				a.log.Warn("cleanup failed", slog.Any("err", err))
			}
			return
		}
	}
	a.throttle.sweep(now)
}
