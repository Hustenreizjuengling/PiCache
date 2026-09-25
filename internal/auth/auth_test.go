package auth

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type testEnv struct {
	a         *Service
	d         *db.DB
	set       *settings.Store
	clock     *fakeClock
	setupFile string
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "picache.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	set, err := settings.Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	setupFile := filepath.Join(dir, "setup-token")
	a, err := New(ctx, d, set, box, setupFile, log)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	a.now = clock.Now
	return &testEnv{a: a, d: d, set: set, clock: clock, setupFile: setupFile}
}

const testPassword = "correct horse battery"

var meta = ReqMeta{IP: "192.168.1.10", UserAgent: "test"}

// withAdmin provisions the user "admin".
func (e *testEnv) withAdmin(t *testing.T) {
	t.Helper()
	if err := e.a.Provision(context.Background(), "admin", testPassword); err != nil {
		t.Fatal(err)
	}
}

func (e *testEnv) login(t *testing.T) *Session {
	t.Helper()
	s, err := e.a.Login(context.Background(), "admin", testPassword, "", meta)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return s
}

func cookieRequest(tok string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: tok})
	return r
}

func bearerRequest(tok string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	return r
}

func wantKind(t *testing.T, err error, k apperr.Kind) {
	t.Helper()
	if err == nil || apperr.KindOf(err) != k {
		t.Fatalf("got error %v (kind %v), want kind %v", err, apperr.KindOf(err), k)
	}
}

func TestSetup(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b, err := os.ReadFile(e.setupFile)
	if err != nil {
		t.Fatal(err)
	}
	tok := strings.TrimSpace(string(b))
	if !validSetupToken(tok) {
		t.Fatalf("setup token %q malformed", tok)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(e.setupFile); fi.Mode().Perm() != 0o600 {
			t.Fatalf("setup token mode %v, want 0600", fi.Mode().Perm())
		}
	}
	if req, _ := e.a.SetupRequired(ctx); !req {
		t.Fatal("setup must be required without users")
	}

	_, err = e.a.Setup(ctx, "WRONGWRONGWRONGWRONGWRONG2", "admin", testPassword, meta)
	wantKind(t, err, apperr.KindForbidden)
	_, err = e.a.Setup(ctx, tok, "admin", "short", meta)
	wantKind(t, err, apperr.KindInvalid)
	_, err = e.a.Setup(ctx, tok, "-bad", testPassword, meta)
	wantKind(t, err, apperr.KindInvalid)

	s, err := e.a.Setup(ctx, tok, "admin", testPassword, meta)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := e.a.Authenticate(cookieRequest(s.Token)); err != nil || p.Username != "admin" || p.Scope != ScopeAdmin {
		t.Fatalf("authenticate after setup: %+v, %v", p, err)
	}
	if _, err := os.Stat(e.setupFile); !os.IsNotExist(err) {
		t.Fatalf("setup token file must be deleted, stat err = %v", err)
	}
	if req, _ := e.a.SetupRequired(ctx); req {
		t.Fatal("setup must not be required after setup")
	}
	_, err = e.a.Setup(ctx, tok, "other", testPassword, meta)
	wantKind(t, err, apperr.KindForbidden)

	entries, _, err := e.a.AuditLog(ctx, AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, en := range entries {
		actions = append(actions, en.Action)
	}
	if strings.Join(actions, ",") != "auth.setup,auth.login_failed" {
		t.Fatalf("audit actions = %v", actions)
	}
}

func TestSetupTokenReusedAcrossRestarts(t *testing.T) {
	e := newEnv(t)
	first := e.a.setupToken
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a2, err := New(context.Background(), e.d, e.set, e.a.box, e.setupFile, log)
	if err != nil {
		t.Fatal(err)
	}
	if a2.setupToken != first {
		t.Fatal("a valid setup token file must be reused")
	}
	e.withAdmin(t)
	if _, err := os.Stat(e.setupFile); !os.IsNotExist(err) {
		t.Fatal("provisioning must delete the setup token file")
	}
	if err := e.a.Provision(context.Background(), "admin", "another password"); err != nil {
		t.Fatal(err)
	}
	e.login(t) // the original password still works: Provision never overwrites
}

func TestLoginAndAuthenticate(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	ctx := context.Background()

	_, err := e.a.Login(ctx, "admin", "wrong password", "", meta)
	wantKind(t, err, apperr.KindUnauthorized)
	_, err = e.a.Login(ctx, "nobody", testPassword, "", meta)
	wantKind(t, err, apperr.KindUnauthorized)

	s, err := e.a.Login(ctx, "ADMIN", testPassword, "", meta) // usernames are case-insensitive
	if err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]*http.Request{"cookie": cookieRequest(s.Token), "bearer": bearerRequest(s.Token)} {
		p, err := e.a.Authenticate(r)
		if err != nil || p.SessionID != s.ID || p.TokenID != 0 || p.Scope != ScopeAdmin {
			t.Fatalf("%s: principal %+v, err %v", name, p, err)
		}
	}
	for name, r := range map[string]*http.Request{
		"none":        httptest.NewRequest(http.MethodGet, "/", nil),
		"bad cookie":  cookieRequest("x"),
		"bad bearer":  bearerRequest(strings.Repeat("a", sessionTokenLen)),
		"bad api tok": bearerRequest(tokenPrefix + strings.Repeat("0", 64)),
	} {
		if _, err := e.a.Authenticate(r); apperr.KindOf(err) != apperr.KindUnauthorized {
			t.Fatalf("%s: err = %v, want unauthorized", name, err)
		}
	}
	// A non-Bearer Authorization header (reverse proxy basic auth) falls back to the cookie.
	r := cookieRequest(s.Token)
	r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	if _, err := e.a.Authenticate(r); err != nil {
		t.Fatalf("basic auth header must be ignored: %v", err)
	}
	me, err := e.a.Me(ctx, &Principal{UserID: s.UserID})
	if err != nil || me.Username != "admin" || me.LastLoginAt.IsZero() {
		t.Fatalf("me = %+v, %v", me, err)
	}
	if err := e.a.Logout(ctx, s.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.Authenticate(cookieRequest(s.Token)); err == nil {
		t.Fatal("session must be gone after logout")
	}
}

func TestLoginThrottle(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	for range maxFailures {
		_, err := e.a.Login(ctx, "admin", "wrong password", "", meta)
		wantKind(t, err, apperr.KindUnauthorized)
		e.clock.Advance(time.Second)
	}
	// The failing client is locked out: even the right password is refused,
	// for other usernames too.
	_, err := e.a.Login(ctx, "admin", testPassword, "", meta)
	wantKind(t, err, apperr.KindTooMany)
	_, err = e.a.Login(ctx, "someone", testPassword, "", meta)
	wantKind(t, err, apperr.KindTooMany)
	// Another client only waits for the short username delay (already over).
	if _, err := e.a.Login(ctx, "admin", testPassword, "", ReqMeta{IP: "192.168.1.99"}); err != nil {
		t.Fatalf("another client must not be locked out: %v", err)
	}

	e.clock.Advance(lockoutDuration)
	e.login(t)

	entries, total, err := e.a.AuditLog(ctx, AuditQuery{Search: "login_failed"})
	if err != nil || total != 1 {
		t.Fatalf("failed logins must be aggregated into one row: total %d, err %v", total, err)
	}
	if !strings.Contains(entries[0].Details, `"attempts":5`) || entries[0].Target != "192.168.1.10/32" {
		t.Fatalf("aggregated entry = %+v", entries[0])
	}
}

// New lockouts are reported once each (security.lockout notification),
// with the client but never the username.
func TestLockoutCallback(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	var got []Lockout
	e.a.OnLockout(func(l Lockout) { got = append(got, l) })
	for range maxFailures {
		_, err := e.a.Login(ctx, "admin", "wrong password", "", meta)
		wantKind(t, err, apperr.KindUnauthorized)
	}
	// Further attempts are refused while locked and report nothing new.
	for range 3 {
		_, err := e.a.Login(ctx, "admin", "wrong password", "", meta)
		wantKind(t, err, apperr.KindTooMany)
	}
	if len(got) != 2 {
		t.Fatalf("lockouts %+v", got)
	}
	byKind := map[string]Lockout{}
	for _, l := range got {
		byKind[l.Kind] = l
	}
	if c := byKind[LockoutClient]; c.Client != "192.168.1.10/32" || c.For != lockoutDuration {
		t.Fatalf("client lockout %+v", c)
	}
	if u := byKind[LockoutUsername]; u.Client != "192.168.1.10/32" || u.For != userDelayBase {
		t.Fatalf("username delay %+v", u)
	}
	// After the lockout (and 15 minutes without failures for the
	// username), the next five failures start both again.
	e.clock.Advance(lockoutDuration)
	for range maxFailures {
		e.clock.Advance(userDelayMax)
		_, _ = e.a.Login(ctx, "admin", "wrong password", "", meta)
	}
	if len(got) != 4 || got[2].Kind != LockoutClient || got[3].Kind != LockoutUsername {
		t.Fatalf("second lockout %+v", got)
	}

	// Password confirmations of a session lock the session and the client.
	e.clock.Advance(lockoutDuration)
	s := e.login(t)
	p := &Principal{UserID: 1, Username: "admin", SessionID: s.ID, Scope: ScopeAdmin, IP: "192.168.1.77"}
	for range maxFailures {
		_ = e.a.ConfirmPassword(ctx, p, "wrong password")
	}
	kinds := map[string]string{}
	for _, l := range got[4:] {
		kinds[l.Kind] = l.Client
	}
	if len(got) != 6 || kinds[LockoutSession] != "192.168.1.77/32" || kinds[LockoutClient] != "192.168.1.77/32" {
		t.Fatalf("session lockout %+v", got[4:])
	}
}

// SEC-05: failed sign-ins for a username from other LAN hosts slow sign-ins
// for that username down, but never lock the owner out: the delay starts at
// 1 s, is capped at 30 s and ends with a successful sign-in.
func TestLoginUsernameDelayIsNoLockout(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	owner := ReqMeta{IP: "192.168.1.50"}
	attempt := 0
	// failOnce fails once from a fresh address, waiting out the delay like a
	// patient attacker (each address has its own client key).
	failOnce := func() {
		t.Helper()
		for {
			ip := ReqMeta{IP: fmt.Sprintf("10.0.%d.%d", attempt/200, attempt%200+1)}
			_, err := e.a.Login(ctx, "admin", "wrong password", "", ip)
			if apperr.KindOf(err) == apperr.KindUnauthorized {
				attempt++
				return
			}
			wantKind(t, err, apperr.KindTooMany)
			e.clock.Advance(time.Second)
		}
	}
	for range maxFailures {
		failOnce()
	}
	_, err := e.a.Login(ctx, "admin", testPassword, "", owner)
	wantKind(t, err, apperr.KindTooMany)
	e.clock.Advance(userDelayBase)
	if _, err := e.a.Login(ctx, "admin", testPassword, "", owner); err != nil {
		t.Fatalf("owner after 1 s: %v", err)
	}

	// The success reset the count: four failures do not delay anyone.
	for range maxFailures - 1 {
		failOnce()
	}
	if _, err := e.a.Login(ctx, "admin", testPassword, "", owner); err != nil {
		t.Fatalf("the delay must be reset by a successful sign-in: %v", err)
	}

	// However long the attack runs, the owner waits at most 30 s.
	for range 30 {
		failOnce()
	}
	_, err = e.a.Login(ctx, "admin", testPassword, "", owner)
	if ae, ok := apperr.As(err); !ok || ae.Kind != apperr.KindTooMany || !strings.Contains(ae.Message, "second") {
		t.Fatalf("during the attack: %v", err)
	}
	e.clock.Advance(userDelayMax)
	if _, err := e.a.Login(ctx, "admin", testPassword, "", owner); err != nil {
		t.Fatalf("owner after at most 30 s: %v", err)
	}
}

// SEC-06: /auth/setup after setup neither uses the global attempt budget nor
// goes unpunished: the caller is locked out like a password guesser, and
// sign-ins keep working.
func TestSetupAfterCompletionDoesNotStarveLogins(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	caller := ReqMeta{IP: "192.168.1.99"}
	for range 50 {
		_, err := e.a.Setup(ctx, "AAAAAAAAAAAAAAAAAAAAAAAAAA", "x", testPassword, caller)
		wantKind(t, err, apperr.KindForbidden)
	}
	e.login(t) // same instant: the global limit (10/s) was not consumed
	_, err := e.a.Login(ctx, "admin", testPassword, "", caller)
	wantKind(t, err, apperr.KindTooMany)
}

func TestSessionTimeouts(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	lastSeen := func(id string) int64 {
		var v int64
		if err := e.d.R.QueryRowContext(ctx, `SELECT last_seen FROM auth_sessions WHERE id = ?`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	s := e.login(t)
	created := lastSeen(s.ID)
	e.clock.Advance(30 * time.Second)
	if _, err := e.a.Authenticate(cookieRequest(s.Token)); err != nil {
		t.Fatal(err)
	}
	if lastSeen(s.ID) != created {
		t.Fatal("last_seen must not be written more than once per minute")
	}
	e.clock.Advance(59 * time.Minute) // 59.5 min after login, idle limit 60 min
	if _, err := e.a.Authenticate(cookieRequest(s.Token)); err != nil {
		t.Fatalf("session within idle limit: %v", err)
	}
	if lastSeen(s.ID) == created {
		t.Fatal("last_seen must slide")
	}
	e.clock.Advance(61 * time.Minute)
	if _, err := e.a.Authenticate(cookieRequest(s.Token)); apperr.KindOf(err) != apperr.KindUnauthorized {
		t.Fatalf("idle session must expire, err = %v", err)
	}

	// Absolute limit: active every 30 minutes, still ends after SessionMaxHours.
	s = e.login(t)
	p := &Principal{UserID: s.UserID, SessionID: s.ID}
	for elapsed := time.Duration(0); elapsed < 167*time.Hour; elapsed += 30 * time.Minute {
		e.clock.Advance(30 * time.Minute)
		if _, err := e.a.Authenticate(cookieRequest(s.Token)); err != nil {
			t.Fatalf("after %v: %v", elapsed, err)
		}
	}
	if !e.a.Valid(ctx, p) {
		t.Fatal("session must still be valid")
	}
	e.clock.Advance(90 * time.Minute)
	if e.a.Valid(ctx, p) {
		t.Fatal("Valid must honour the absolute limit")
	}
	if _, err := e.a.Authenticate(cookieRequest(s.Token)); err == nil {
		t.Fatal("session must end after the absolute limit")
	}
}

func TestSessionsChangePasswordRevoke(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	s1, s2, s3 := e.login(t), e.login(t), e.login(t)
	p1 := &Principal{UserID: s1.UserID, Username: "admin", SessionID: s1.ID, Scope: ScopeAdmin}

	list, err := e.a.Sessions(ctx, p1)
	if err != nil || len(list) != 3 {
		t.Fatalf("sessions = %v, %v", list, err)
	}
	current := 0
	for _, s := range list {
		if s.Current {
			current++
			if s.ID != s1.ID {
				t.Fatal("wrong current session")
			}
		}
	}
	if current != 1 {
		t.Fatalf("%d current sessions", current)
	}

	wantKind(t, e.a.RevokeSession(ctx, p1, "zz"), apperr.KindInvalid)
	wantKind(t, e.a.RevokeSession(ctx, p1, "0123456789abcdef"), apperr.KindNotFound)
	if err := e.a.RevokeSession(ctx, p1, s3.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.Authenticate(cookieRequest(s3.Token)); err == nil {
		t.Fatal("revoked session still valid")
	}

	tok, _, err := e.a.CreateToken(ctx, p1, testPassword, "ci", ScopeAdmin, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantKind(t, e.a.ChangePassword(ctx, p1, "wrong password", "a new password", false), apperr.KindInvalid)
	wantKind(t, e.a.ChangePassword(ctx, p1, testPassword, "short", false), apperr.KindInvalid)
	if err := e.a.ChangePassword(ctx, p1, testPassword, "a new password", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.Authenticate(cookieRequest(s1.Token)); err != nil {
		t.Fatal("the current session must survive a password change")
	}
	if _, err := e.a.Authenticate(cookieRequest(s2.Token)); err == nil {
		t.Fatal("other sessions must be revoked by a password change")
	}
	// SEC-03: a token created with a stolen session ends with the password change.
	if _, err := e.a.Authenticate(bearerRequest(tok)); err == nil {
		t.Fatal("API tokens must be revoked by a password change")
	}
	if _, err := e.a.Login(ctx, "admin", "a new password", "", meta); err != nil {
		t.Fatal(err)
	}

	// keepTokens keeps them (sessions still end).
	tok, _, err = e.a.CreateToken(ctx, p1, "a new password", "ci", ScopeAdmin, 0)
	if err != nil {
		t.Fatal(err)
	}
	s4, err := e.a.Login(ctx, "admin", "a new password", "", meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.a.ChangePassword(ctx, p1, "a new password", testPassword, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.Authenticate(bearerRequest(tok)); err != nil {
		t.Fatalf("keepTokens must keep the API tokens: %v", err)
	}
	if _, err := e.a.Authenticate(cookieRequest(s4.Token)); err == nil {
		t.Fatal("other sessions must be revoked with keepTokens too")
	}
}

func TestSessionCap(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	first := e.login(t)
	for range maxSessionsPerUser {
		e.clock.Advance(time.Second)
		e.login(t)
	}
	var n int
	if err := e.d.R.QueryRow(`SELECT COUNT(*) FROM auth_sessions`).Scan(&n); err != nil || n != maxSessionsPerUser {
		t.Fatalf("sessions = %d, %v", n, err)
	}
	if _, err := e.a.Authenticate(cookieRequest(first.Token)); err == nil {
		t.Fatal("the oldest session must be ended beyond the cap")
	}
}

func TestAPITokens(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	s := e.login(t)
	p := &Principal{UserID: s.UserID, Username: "admin", SessionID: s.ID, Scope: ScopeAdmin}

	for _, tc := range []struct {
		name  string
		scope Scope
		ttl   time.Duration
	}{
		{"", ScopeRead, 0},
		{"x", "root", 0},
		{"x", ScopeRead, -time.Hour},
		{"bad\nname", ScopeRead, 0},
	} {
		_, _, err := e.a.CreateToken(ctx, p, testPassword, tc.name, tc.scope, tc.ttl)
		wantKind(t, err, apperr.KindInvalid)
	}
	// SEC-03: a session alone is not enough to create a token.
	for _, pw := range []string{"", "wrong password"} {
		_, _, err := e.a.CreateToken(ctx, p, pw, "x", ScopeAdmin, 0)
		if ae, ok := apperr.As(err); !ok || ae.Kind != apperr.KindInvalid || ae.Field != "currentPassword" {
			t.Fatalf("password %q: err = %v", pw, err)
		}
	}

	secret, info, err := e.a.CreateToken(ctx, p, testPassword, "grafana", ScopeRead, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, tokenPrefix) || len(secret) != tokenLen || info.Prefix != secret[:tokenDisplayChars] {
		t.Fatalf("secret %q info %+v", secret, info)
	}
	tp, err := e.a.Authenticate(bearerRequest(secret))
	if err != nil || tp.TokenID != info.ID || tp.Scope != ScopeRead || tp.SessionID != "" || tp.Username != "admin" {
		t.Fatalf("token principal %+v, %v", tp, err)
	}
	if !e.a.Valid(ctx, tp) {
		t.Fatal("token must be valid")
	}
	list, err := e.a.Tokens(ctx)
	if err != nil || len(list) != 1 || list[0].LastUsed.IsZero() || list[0].ExpiresAt.IsZero() {
		t.Fatalf("tokens = %+v, %v", list, err)
	}

	e.clock.Advance(25 * time.Hour)
	if _, err := e.a.Authenticate(bearerRequest(secret)); apperr.KindOf(err) != apperr.KindUnauthorized {
		t.Fatalf("expired token: err = %v", err)
	}
	if e.a.Valid(ctx, tp) {
		t.Fatal("expired token must not be valid")
	}

	admin, info2, err := e.a.CreateToken(ctx, p, testPassword, "ci", ScopeAdmin, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ap, err := e.a.Authenticate(bearerRequest(admin)); err != nil || ap.Scope != ScopeAdmin {
		t.Fatalf("admin token: %+v, %v", ap, err)
	}
	if err := e.a.DeleteToken(ctx, info2.ID); err != nil {
		t.Fatal(err)
	}
	wantKind(t, e.a.DeleteToken(ctx, info2.ID), apperr.KindNotFound)
	if _, err := e.a.Authenticate(bearerRequest(admin)); err == nil {
		t.Fatal("deleted token still valid")
	}
}

func TestTOTP(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	s := e.login(t)
	p := &Principal{UserID: s.UserID, Username: "admin", SessionID: s.ID, Scope: ScopeAdmin}
	code := func(secret []byte) string { return totpCode(secret, e.clock.Now().Unix()/totpPeriod) }
	other := e.login(t)

	// SEC-03: enrolling an authenticator needs the password, not just a session.
	for _, pw := range []string{"", "wrong password"} {
		_, _, err := e.a.TOTPBegin(ctx, p, pw)
		if ae, ok := apperr.As(err); !ok || ae.Kind != apperr.KindInvalid || ae.Field != "currentPassword" {
			t.Fatalf("password %q: err = %v", pw, err)
		}
	}
	b32secret, uri, err := e.a.TOTPBegin(ctx, p, testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(uri, "otpauth://totp/PiCache:admin?") || !strings.Contains(uri, "secret="+b32secret) {
		t.Fatalf("uri = %q", uri)
	}
	secret, err := totpB32.DecodeString(b32secret)
	if err != nil || len(secret) != totpSecretLen {
		t.Fatalf("secret %q: %v", b32secret, err)
	}
	var sealed string
	if err := e.d.R.QueryRow(`SELECT totp_pending FROM auth_users`).Scan(&sealed); err != nil || strings.Contains(sealed, b32secret) {
		t.Fatalf("pending secret must be sealed: %q, %v", sealed, err)
	}

	wantKind(t, e.a.TOTPConfirm(ctx, p, "12345"), apperr.KindInvalid)
	// A code that is wrong for every step this test accepts codes in.
	wrong := ""
	for c := 0; wrong == ""; c++ {
		cand, cur := fmt.Sprintf("%06d", c), e.clock.Now().Unix()/totpPeriod
		if !slices.ContainsFunc([]int64{cur - 1, cur, cur + 1, cur + 2}, func(s int64) bool { return totpCode(secret, s) == cand }) {
			wrong = cand
		}
	}
	wantKind(t, e.a.TOTPConfirm(ctx, p, wrong), apperr.KindInvalid)
	if err := e.a.TOTPConfirm(ctx, p, code(secret)); err != nil {
		t.Fatal(err)
	}
	if me, _ := e.a.Me(ctx, p); !me.TOTPEnabled {
		t.Fatal("TOTP must be enabled")
	}
	// Sessions created without a code end; the enrolling one stays.
	if _, err := e.a.Authenticate(cookieRequest(other.Token)); err == nil {
		t.Fatal("enabling TOTP must sign out the other sessions")
	}
	if _, err := e.a.Authenticate(cookieRequest(s.Token)); err != nil {
		t.Fatalf("the current session must survive enabling TOTP: %v", err)
	}
	_, _, err = e.a.TOTPBegin(ctx, p, testPassword)
	wantKind(t, err, apperr.KindConflict)

	// The step used for confirmation cannot be replayed for a login.
	_, err = e.a.Login(ctx, "admin", testPassword, code(secret), meta)
	if ae, ok := apperr.As(err); !ok || ae.Kind != apperr.KindUnauthorized || ae.Field != "totp" {
		t.Fatalf("replayed code: err = %v", err)
	}
	_, err = e.a.Login(ctx, "admin", testPassword, "", meta)
	if ae, ok := apperr.As(err); !ok || ae.Field != "totp" {
		t.Fatalf("missing code: err = %v", err)
	}

	e.clock.Advance(totpPeriod * time.Second)
	if _, err := e.a.Login(ctx, "admin", testPassword, code(secret), meta); err != nil {
		t.Fatalf("login with fresh code: %v", err)
	}
	_, err = e.a.Login(ctx, "admin", testPassword, code(secret), meta)
	wantKind(t, err, apperr.KindUnauthorized) // single use

	// Wrong codes count towards the username delay (one reuse above + 4 here).
	for range maxFailures - 1 {
		_, _ = e.a.Login(ctx, "admin", testPassword, wrong, ReqMeta{IP: "10.0.0.1"})
	}
	_, err = e.a.Login(ctx, "admin", testPassword, code(secret), ReqMeta{IP: "10.0.0.2"})
	wantKind(t, err, apperr.KindTooMany)
	e.clock.Advance(lockoutDuration)

	wantKind(t, e.a.TOTPDisable(ctx, p, "wrong password"), apperr.KindInvalid)
	if err := e.a.TOTPDisable(ctx, p, testPassword); err != nil {
		t.Fatal(err)
	}
	e.login(t)
}

// SEC-04 + SEC-03: the CLI reset only resets an existing account (a
// mistyped or default name never adds a second admin), disables its TOTP
// and ends every session and API token, and reports what it changed.
func TestResetPassword(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	s := e.login(t)
	p := &Principal{UserID: s.UserID, Username: "admin", SessionID: s.ID, Scope: ScopeAdmin}
	if _, _, err := e.a.TOTPBegin(ctx, p, testPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.W.Exec(`UPDATE auth_users SET totp_secret = totp_pending`); err != nil {
		t.Fatal(err)
	}
	secret, _, err := e.a.CreateToken(ctx, p, testPassword, "t", ScopeAdmin, 0)
	if err != nil {
		t.Fatal(err)
	}

	_, err = ResetPassword(ctx, e.d, "root", "reset password 1")
	if ae, ok := apperr.As(err); !ok || ae.Kind != apperr.KindInvalid || !strings.Contains(ae.Message, "admin") {
		t.Fatalf("unknown name: err = %v (want the existing names)", err)
	}
	var users int
	if err := e.d.R.QueryRow(`SELECT COUNT(*) FROM auth_users`).Scan(&users); err != nil || users != 1 {
		t.Fatalf("users after a refused reset = %d, %v", users, err)
	}
	if _, err := e.a.Authenticate(bearerRequest(secret)); err != nil {
		t.Fatal("a refused reset must change nothing")
	}
	_, err = ResetPassword(ctx, e.d, "admin", "short")
	wantKind(t, err, apperr.KindInvalid)

	res, err := ResetPassword(ctx, e.d, "ADMIN", "reset password 1")
	if err != nil {
		t.Fatal(err)
	}
	want := ResetResult{Username: "admin", TOTPDisabled: true, SessionsRevoked: 1, TokensRevoked: 1}
	if res != want {
		t.Fatalf("result = %+v, want %+v", res, want)
	}
	if _, err := e.a.Authenticate(cookieRequest(s.Token)); err == nil {
		t.Fatal("reset must revoke sessions")
	}
	if _, err := e.a.Authenticate(bearerRequest(secret)); err == nil {
		t.Fatal("reset must revoke API tokens")
	}
	if _, err := e.a.Login(ctx, "admin", "reset password 1", "", meta); err != nil {
		t.Fatalf("login after reset (TOTP must be off): %v", err)
	}
	entries, _, err := e.a.AuditLog(ctx, AuditQuery{Search: "password_reset"})
	if err != nil || len(entries) != 1 || !strings.Contains(entries[0].Details, `"tokensRevoked":1`) {
		t.Fatalf("audit = %+v, %v", entries, err)
	}
}

// The reset creates an account only while there is none at all.
func TestResetPasswordCreatesOnlyTheFirstAccount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res, err := ResetPassword(ctx, e.d, "owner", "reset password 1")
	if err != nil || !res.Created || res.Username != "owner" {
		t.Fatalf("reset without accounts = %+v, %v", res, err)
	}
	if _, err := e.a.Login(ctx, "owner", "reset password 1", "", meta); err != nil {
		t.Fatal(err)
	}
	_, err = ResetPassword(ctx, e.d, "admin", "reset password 2")
	wantKind(t, err, apperr.KindInvalid)
	if names, err := Usernames(ctx, e.d); err != nil || strings.Join(names, ",") != "owner" {
		t.Fatalf("accounts = %v, %v", names, err)
	}
}

// SEC-02: a trigger planted in the database (e.g. by a restored backup)
// cannot make session and token revocation a silent no-op: the operation
// fails and changes nothing.
func TestPlantedTriggerCannotKeepCredentials(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	e.login(t)
	if _, err := e.d.W.Exec(`CREATE TRIGGER keep BEFORE DELETE ON auth_sessions BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if err := e.d.Tx(ctx, func(tx *sql.Tx) error { return PurgeSessions(ctx, tx) }); err == nil {
		t.Fatal("PurgeSessions must fail when sessions survive the delete")
	}
	if _, err := ResetPassword(ctx, e.d, "admin", "reset password 1"); err == nil {
		t.Fatal("ResetPassword must fail when sessions survive the delete")
	}
	e.login(t) // rolled back: the old password still works

	if _, err := e.d.W.Exec(`DROP TRIGGER keep`); err != nil {
		t.Fatal(err)
	}
	if err := e.d.Tx(ctx, func(tx *sql.Tx) error { return PurgeSessions(ctx, tx) }); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.d.R.QueryRow(`SELECT COUNT(*) FROM auth_sessions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("sessions after purge = %d, %v", n, err)
	}
}

// Revoking a token or session, and the revocations of a password change,
// fail instead of doing nothing when a planted trigger keeps the rows (the
// service removes such triggers at start; this is the second line).
func TestPlantedTriggerCannotKeepRevokedCredentials(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	s1, s2 := e.login(t), e.login(t)
	p := &Principal{UserID: s1.UserID, Username: "admin", SessionID: s1.ID, Scope: ScopeAdmin, IP: meta.IP}
	tok, info, err := e.a.CreateToken(ctx, p, testPassword, "ci", ScopeAdmin, 0)
	if err != nil {
		t.Fatal(err)
	}
	plant := func(q string) {
		t.Helper()
		if _, err := e.d.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	wantKept := func(name string, err error) {
		t.Helper()
		if err == nil || apperr.KindOf(err) == apperr.KindNotFound || !strings.Contains(err.Error(), "could not be deleted") {
			t.Errorf("%s with a planted trigger: %v", name, err)
		}
	}
	plant(`CREATE TRIGGER keep_tokens BEFORE DELETE ON auth_tokens BEGIN SELECT RAISE(IGNORE); END`)
	wantKept("password change (tokens)", e.a.ChangePassword(ctx, p, testPassword, "a new password", false))
	wantKept("delete token", e.a.DeleteToken(ctx, info.ID))
	plant(`CREATE TRIGGER keep_sessions BEFORE DELETE ON auth_sessions BEGIN SELECT RAISE(IGNORE); END`)
	wantKept("password change (sessions)", e.a.ChangePassword(ctx, p, testPassword, "a new password", true))
	wantKept("revoke session", e.a.RevokeSession(ctx, p, s2.ID))
	wantKept("logout", e.a.Logout(ctx, s2.Token))
	e.login(t) // the password changes were rolled back
	if _, err := e.a.Authenticate(bearerRequest(tok)); err != nil {
		t.Fatal(err)
	}

	if _, err := db.DropTriggersAndViews(ctx, e.d.W); err != nil {
		t.Fatal(err)
	}
	if err := e.a.ChangePassword(ctx, p, testPassword, "a new password", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.Authenticate(bearerRequest(tok)); err == nil {
		t.Fatal("the token must be revoked once the trigger is gone")
	}
	if _, err := e.a.Authenticate(cookieRequest(s2.Token)); err == nil {
		t.Fatal("the other session must be revoked once the trigger is gone")
	}
}

// SEC-01: re-confirming the password (restore) answers 401 with field
// "password" and is throttled like a sign-in.
func TestConfirmPassword(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	s := e.login(t)
	p := &Principal{UserID: s.UserID, Username: "admin", SessionID: s.ID, Scope: ScopeAdmin, IP: "192.168.1.20"}
	for _, pw := range []string{"", "wrong password"} {
		err := e.a.ConfirmPassword(ctx, p, pw)
		if ae, ok := apperr.As(err); !ok || ae.Kind != apperr.KindUnauthorized || ae.Field != "password" {
			t.Fatalf("password %q: err = %v", pw, err)
		}
	}
	if err := e.a.ConfirmPassword(ctx, p, testPassword); err != nil {
		t.Fatal(err)
	}
	for range maxFailures {
		_ = e.a.ConfirmPassword(ctx, p, "wrong password")
	}
	wantKind(t, e.a.ConfirmPassword(ctx, p, testPassword), apperr.KindTooMany)
	if _, total, err := e.a.AuditLog(ctx, AuditQuery{Search: "password confirmation"}); err != nil || total != 1 {
		t.Fatalf("wrong confirmations must be audited: total %d, %v", total, err)
	}
}

// SEC-09: over TLS the session cookie is "__Host-picache_session"; the
// Secure flag can be forced for plain HTTP behind a TLS proxy; both names
// are accepted.
func TestSessionCookies(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	s := e.login(t)
	for _, tc := range []struct {
		tls, secure bool
		name        string
		wantSecure  bool
	}{
		{true, false, SecureSessionCookie, true},
		{true, true, SecureSessionCookie, true},
		{false, true, SessionCookie, true},
		{false, false, SessionCookie, false},
	} {
		c := e.a.Cookie(s, tc.tls, tc.secure)
		if c.Name != tc.name || c.Secure != tc.wantSecure || c.Path != "/" || c.Domain != "" || !c.HttpOnly ||
			c.SameSite != http.SameSiteStrictMode || c.Value != s.Token {
			t.Fatalf("tls %v secure %v: cookie %+v", tc.tls, tc.secure, c)
		}
	}
	for _, name := range []string{SessionCookie, SecureSessionCookie} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
		r.RemoteAddr = "192.168.1.77:40000"
		r.AddCookie(&http.Cookie{Name: name, Value: s.Token})
		p, err := e.a.Authenticate(r)
		if err != nil || p.SessionID != s.ID || p.IP != "192.168.1.77" {
			t.Fatalf("%s: %+v, %v", name, p, err)
		}
	}
	// A stale __Host- cookie does not hide a valid plain one.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	r.AddCookie(&http.Cookie{Name: SecureSessionCookie, Value: strings.Repeat("x", sessionTokenLen)})
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: s.Token})
	if _, err := e.a.Authenticate(r); err != nil {
		t.Fatalf("stale secure cookie: %v", err)
	}
	if got := e.a.ClearCookies(true, true); len(got) != 2 || got[0].MaxAge >= 0 || got[1].Name != SecureSessionCookie {
		t.Fatalf("clear over TLS = %+v", got)
	}
	if got := e.a.ClearCookies(false, false); len(got) != 1 || got[0].Name != SessionCookie || got[0].Secure {
		t.Fatalf("clear over HTTP = %+v", got)
	}
}

func TestCleanupRetention(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	e.login(t)
	e.a.Audit(ctx, nil, "", "old.action", "", nil)
	e.clock.Advance(auditRetention + time.Hour)
	e.a.Audit(ctx, nil, "", "new.action", "", nil)
	e.a.cleanup(ctx)

	var sessions int
	if err := e.d.R.QueryRow(`SELECT COUNT(*) FROM auth_sessions`).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("expired sessions must be deleted: %d, %v", sessions, err)
	}
	entries, _, err := e.a.AuditLog(ctx, AuditQuery{Search: "action"})
	if err != nil || len(entries) != 1 || entries[0].Action != "new.action" {
		t.Fatalf("audit after retention = %+v, %v", entries, err)
	}
}

func TestAuditLogSearchAndPaging(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := &Principal{Username: "admin", TokenID: 7}
	for i := range 5 {
		e.clock.Advance(time.Second)
		e.a.Audit(ctx, p, "10.0.0.1", "list.create", "list-"+string(rune('a'+i)), map[string]any{"url": "https://x/100%_"})
	}
	entries, total, err := e.a.AuditLog(ctx, AuditQuery{Search: "list.", Limit: 2, Offset: 1})
	if err != nil || total != 5 || len(entries) != 2 || entries[0].Target != "list-d" {
		t.Fatalf("page = %+v total %d err %v", entries, total, err)
	}
	if entries[0].Username != "admin (API token #7)" {
		t.Fatalf("username = %q", entries[0].Username)
	}
	// LIKE wildcards in the search are literal.
	if _, total, _ := e.a.AuditLog(ctx, AuditQuery{Search: "100%_"}); total != 5 {
		t.Fatalf("literal search total = %d", total)
	}
	if _, total, _ := e.a.AuditLog(ctx, AuditQuery{Search: "1%0"}); total != 0 {
		t.Fatalf("wildcard must not match, total = %d", total)
	}
}

func TestStartCleansUpAndStops(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	e.login(t)
	e.clock.Advance(2 * time.Hour) // past the idle limit
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.a.Start(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := e.d.R.QueryRow(`SELECT COUNT(*) FROM auth_sessions`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Start did not clean up expired sessions")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
}
