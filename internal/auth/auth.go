// Package auth implements web authentication: users with argon2id password
// hashes, sessions, API tokens, optional TOTP, login throttling, the
// first-run setup token and the audit log (docs/ARCHITECTURE.md 6.1, 12).
package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

var errNotImplemented = errors.New("auth: not implemented")

// SessionCookie is the name of the session cookie.
const SessionCookie = "picache_session"

// Scope of a principal.
type Scope string

const (
	ScopeAdmin Scope = "admin"
	ScopeRead  Scope = "read"
)

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
	SessionID string // hashed session id (for revocation / "current" marking), "" for tokens
	TokenID   int64  // API token id, 0 for sessions
	Scope     Scope
}

// ReqMeta carries request metadata for throttling and audit.
type ReqMeta struct {
	IP        string
	UserAgent string
}

// Session is a newly created session (Token is the cookie value; only ever
// returned once).
type Session struct {
	ID        string
	Token     string
	UserID    int64
	ExpiresAt time.Time
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

// Service is safe for concurrent use.
type Service struct {
	db  *db.DB
	set *settings.Store
	log *slog.Logger
}

// New creates the service. setupTokenFile is where the one-time setup token
// is written (0600) while no user exists.
func New(ctx context.Context, d *db.DB, set *settings.Store, setupTokenFile string, log *slog.Logger) (*Service, error) {
	return &Service{db: d, set: set, log: log}, nil
}

// Start runs session cleanup until ctx ends.
func (a *Service) Start(ctx context.Context) {}

// Provision creates the first admin from bootstrap config if no user exists.
func (a *Service) Provision(ctx context.Context, username, password string) error {
	return errNotImplemented
}

// SetupRequired reports whether no user exists yet.
func (a *Service) SetupRequired(ctx context.Context) (bool, error) { return true, nil }

// Setup creates the first admin (requires the setup token) and logs in.
func (a *Service) Setup(ctx context.Context, token, username, password string, meta ReqMeta) (*Session, error) {
	return nil, errNotImplemented
}

// Login verifies credentials (+ TOTP if enabled) with throttling.
func (a *Service) Login(ctx context.Context, username, password, totp string, meta ReqMeta) (*Session, error) {
	return nil, errNotImplemented
}

// Logout ends the session with the given cookie token.
func (a *Service) Logout(ctx context.Context, token string) error { return errNotImplemented }

// Authenticate resolves the principal from the session cookie or a Bearer
// token and refreshes the session idle timer. Returns apperr.Unauthorized.
func (a *Service) Authenticate(r *http.Request) (*Principal, error) {
	return nil, errNotImplemented
}

// Cookie builds the session cookie (secure = request came over HTTPS).
func (a *Service) Cookie(s *Session, secure bool) *http.Cookie { return &http.Cookie{} }

// ClearCookie builds a cookie that deletes the session cookie.
func (a *Service) ClearCookie(secure bool) *http.Cookie { return &http.Cookie{} }

// Me returns the user of a principal.
func (a *Service) Me(ctx context.Context, p *Principal) (User, error) { return User{}, errNotImplemented }

// ChangePassword changes the password and revokes all other sessions.
func (a *Service) ChangePassword(ctx context.Context, p *Principal, current, next string) error {
	return errNotImplemented
}

// Sessions lists the user's sessions.
func (a *Service) Sessions(ctx context.Context, p *Principal) ([]SessionInfo, error) {
	return nil, errNotImplemented
}

// RevokeSession ends one of the user's sessions.
func (a *Service) RevokeSession(ctx context.Context, p *Principal, id string) error {
	return errNotImplemented
}

// Tokens lists API tokens.
func (a *Service) Tokens(ctx context.Context) ([]TokenInfo, error) { return nil, errNotImplemented }

// CreateToken creates an API token and returns the secret once.
func (a *Service) CreateToken(ctx context.Context, p *Principal, name string, scope Scope, ttl time.Duration) (string, TokenInfo, error) {
	return "", TokenInfo{}, errNotImplemented
}

// DeleteToken revokes an API token.
func (a *Service) DeleteToken(ctx context.Context, id int64) error { return errNotImplemented }

// TOTPBegin generates a new (unconfirmed) TOTP secret; returns it and the otpauth:// URI.
func (a *Service) TOTPBegin(ctx context.Context, p *Principal) (secret, uri string, err error) {
	return "", "", errNotImplemented
}

// TOTPConfirm enables TOTP after verifying a code for the pending secret.
func (a *Service) TOTPConfirm(ctx context.Context, p *Principal, code string) error {
	return errNotImplemented
}

// TOTPDisable disables TOTP (requires the password).
func (a *Service) TOTPDisable(ctx context.Context, p *Principal, password string) error {
	return errNotImplemented
}

// Audit records an action (never blocks the request for long; errors are logged).
func (a *Service) Audit(ctx context.Context, p *Principal, ip, action, target string, details any) {}

// AuditLog returns audit entries.
func (a *Service) AuditLog(ctx context.Context, q AuditQuery) ([]AuditEntry, int, error) {
	return nil, 0, errNotImplemented
}

// ResetPassword sets a user's password (CLI `picache reset-password`), creating
// the user if missing, and revokes all sessions.
func ResetPassword(ctx context.Context, d *db.DB, username, password string) error {
	return errNotImplemented
}
