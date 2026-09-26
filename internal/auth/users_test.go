package auth

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/secrets"
)

// principal signs in and returns the session principal as Authenticate
// builds it.
func (e *testEnv) principal(t *testing.T, user, pw string) (*Principal, *Session) {
	t.Helper()
	s, err := e.a.Login(context.Background(), user, pw, "", meta)
	if err != nil {
		t.Fatalf("login %s: %v", user, err)
	}
	p, err := e.a.Authenticate(cookieRequest(s.Token))
	if err != nil {
		t.Fatal(err)
	}
	return p, s
}

func wantErr(t *testing.T, err error, kind apperr.Kind, field, msg string) {
	t.Helper()
	e, ok := apperr.As(err)
	if !ok || e.Kind != kind || e.Field != field || !strings.Contains(e.Message, msg) {
		t.Fatalf("err %v, want kind %v field %q message %q", err, kind, field, msg)
	}
}

func strp(s string) *string { return &s }

// newUsersEnv is newEnv with a clock that moves one second per reading, so
// the global attempt limit (10/s) never throttles the many password
// confirmations of these tests.
func newUsersEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newEnv(t)
	e.a.now = func() time.Time { e.clock.Advance(time.Second); return e.clock.Now() }
	return e
}

// Auth migration v2: existing accounts become admins, an insert that
// forgets the role creates a viewer.
func TestAuthMigrationV2Roles(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "picache.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(ctx, "auth", migrations[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`INSERT INTO auth_users (username, password_hash, created_at) VALUES ('old', 'x', 1), ('older', 'y', 2)`); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx, "auth", migrations); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`INSERT INTO auth_users (username, password_hash, created_at) VALUES ('new', 'z', 3)`); err != nil {
		t.Fatal(err)
	}
	users, err := ListUsers(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	got := ""
	for _, u := range users {
		got += u.Username + "=" + u.Role + " "
	}
	if got != "old=admin older=admin new=viewer " {
		t.Fatalf("roles after migration: %s", got)
	}
	if _, err := d.W.Exec(`UPDATE auth_users SET role = 'root' WHERE username = 'new'`); err == nil {
		t.Fatal("the role column must refuse other values")
	}
}

// ListUsers reads a database from before roles without changing it.
func TestListUsersAuthV1(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "picache.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(ctx, "auth", migrations[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`INSERT INTO auth_users (username, password_hash, totp_secret, created_at, last_login_at)
		VALUES ('old', 'x', 'sealed', 1000, 2000)`); err != nil {
		t.Fatal(err)
	}
	users, err := ListUsers(ctx, d)
	if err != nil || len(users) != 1 || users[0].Role != RoleAdmin || !users[0].TOTPEnabled || users[0].LastLoginAt.UnixMilli() != 2000 {
		t.Fatalf("users %+v, %v", users, err)
	}
	var v int
	if err := d.R.QueryRow(`SELECT MAX(version) FROM schema_migrations WHERE component = 'auth'`).Scan(&v); err != nil || v != 1 {
		t.Fatalf("ListUsers must not migrate: v%d %v", v, err)
	}
}

// Sessions of viewers have the read scope; an admin token of a viewer
// counts as read; the role is read on every request.
func TestRoleScopes(t *testing.T) {
	e := newUsersEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	admin, _ := e.principal(t, "admin", testPassword)
	if admin.Scope != ScopeAdmin || admin.Role != RoleAdmin {
		t.Fatalf("admin principal %+v", admin)
	}
	adminTok, _, err := e.a.CreateToken(ctx, admin, testPassword, "ci", ScopeAdmin, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.CreateUser(ctx, admin, testPassword, "watcher", "viewer password", RoleViewer); err != nil {
		t.Fatal(err)
	}
	viewer, vs := e.principal(t, "watcher", "viewer password")
	if viewer.Scope != ScopeRead || viewer.Role != RoleViewer {
		t.Fatalf("viewer principal %+v", viewer)
	}
	u, err := e.a.Me(ctx, viewer)
	if err != nil || u.Role != RoleViewer || u.Username != "watcher" {
		t.Fatalf("Me = %+v, %v", u, err)
	}
	// A role changed behind the service's back applies at the next request.
	if _, err := e.d.W.Exec(`UPDATE auth_users SET role = 'viewer' WHERE username = 'admin'`); err != nil {
		t.Fatal(err)
	}
	if p, err := e.a.Authenticate(bearerRequest(adminTok)); err != nil || p.Scope != ScopeRead {
		t.Fatalf("admin token of a viewer: %+v, %v", p, err)
	}
	if p, err := e.a.Authenticate(cookieRequest(vs.Token)); err != nil || p.Scope != ScopeRead {
		t.Fatalf("viewer session: %+v %v", p, err)
	}
	if _, err := e.d.W.Exec(`UPDATE auth_users SET role = 'admin' WHERE username = 'admin'`); err != nil {
		t.Fatal(err)
	}
	if p, err := e.a.Authenticate(bearerRequest(adminTok)); err != nil || p.Scope != ScopeAdmin {
		t.Fatalf("admin token of an admin: %+v, %v", p, err)
	}
}

func TestCreateUser(t *testing.T) {
	e := newUsersEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	admin, _ := e.principal(t, "admin", testPassword)
	for _, tc := range []struct {
		user, pw, role, current string
		field, msg              string
	}{
		{"-bad", "long enough 1", RoleViewer, testPassword, "username", "may only contain"},
		{"", "long enough 1", RoleViewer, testPassword, "username", "must be 1 to 64"},
		{"ok", "short", RoleViewer, testPassword, "password", "at least 10"},
		{"ok", "long enough 1", "root", testPassword, "role", "must be admin or viewer"},
		{"ok", "long enough 1", RoleViewer, "", "currentPassword", "enter your current password"},
		{"ok", "long enough 1", RoleViewer, "wrong password", "currentPassword", "wrong password"},
	} {
		_, err := e.a.CreateUser(ctx, admin, tc.current, tc.user, tc.pw, tc.role)
		wantErr(t, err, apperr.KindInvalid, tc.field, tc.msg)
	}
	u, err := e.a.CreateUser(ctx, admin, testPassword, "Max@home", "viewer password", RoleViewer)
	if err != nil || u.Role != RoleViewer || u.Username != "Max@home" || u.ID == 0 || u.CreatedAt.IsZero() {
		t.Fatalf("create: %+v, %v", u, err)
	}
	_, err = e.a.CreateUser(ctx, admin, testPassword, "max@HOME", "viewer password", RoleAdmin)
	wantErr(t, err, apperr.KindConflict, "", "an account named max@HOME already exists")
	for i := 3; i <= maxAccounts; i++ {
		if _, err := e.d.W.Exec(`INSERT INTO auth_users (username, password_hash, created_at) VALUES (?, 'x', 1)`,
			fmt.Sprintf("user%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	_, err = e.a.CreateUser(ctx, admin, testPassword, "one-more", "viewer password", RoleViewer)
	wantErr(t, err, apperr.KindConflict, "", "at most 32 accounts can exist")
	list, err := e.a.Users(ctx)
	if err != nil || len(list) != maxAccounts || list[0].Username != "admin" || list[1].Username != "Max@home" {
		t.Fatalf("users %d, %v", len(list), err)
	}
}

func TestUpdateUser(t *testing.T) {
	e := newUsersEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	admin, adminSess := e.principal(t, "admin", testPassword)
	bob, err := e.a.CreateUser(ctx, admin, testPassword, "bob", "bob password 1", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	bobP, bobSess := e.principal(t, "bob", "bob password 1")
	bobAdminTok, _, err := e.a.CreateToken(ctx, bobP, "bob password 1", "ci", ScopeAdmin, 0)
	if err != nil {
		t.Fatal(err)
	}
	bobReadTok, _, err := e.a.CreateToken(ctx, bobP, "bob password 1", "grafana", ScopeRead, 0)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = e.a.UpdateUser(ctx, admin, bob.ID, UserUpdate{}, testPassword)
	wantErr(t, err, apperr.KindInvalid, "body", "nothing to change")
	_, _, err = e.a.UpdateUser(ctx, admin, admin.UserID, UserUpdate{Password: strp("new password 1")}, testPassword)
	wantErr(t, err, apperr.KindInvalid, "password", "under Your account")
	_, _, err = e.a.UpdateUser(ctx, admin, admin.UserID, UserUpdate{DisableTOTP: true}, testPassword)
	wantErr(t, err, apperr.KindInvalid, "disableTotp", "under Your account")
	_, _, err = e.a.UpdateUser(ctx, admin, bob.ID, UserUpdate{Role: strp("owner")}, testPassword)
	wantErr(t, err, apperr.KindInvalid, "role", "must be admin or viewer")
	_, _, err = e.a.UpdateUser(ctx, admin, bob.ID, UserUpdate{Password: strp("short")}, testPassword)
	wantErr(t, err, apperr.KindInvalid, "password", "at least 10")
	_, _, err = e.a.UpdateUser(ctx, admin, bob.ID, UserUpdate{Role: strp(RoleViewer)}, "wrong password")
	wantErr(t, err, apperr.KindInvalid, "currentPassword", "wrong password")
	_, _, err = e.a.UpdateUser(ctx, admin, 999, UserUpdate{Role: strp(RoleViewer)}, testPassword)
	wantErr(t, err, apperr.KindNotFound, "", "not found")

	// Demotion: bob's sessions end at once, his admin token is deleted,
	// his read token stays.
	u, ch, err := e.a.UpdateUser(ctx, admin, bob.ID, UserUpdate{Role: strp(RoleViewer)}, testPassword)
	if err != nil || u.Role != RoleViewer || !ch.RoleChanged || ch.Role != RoleViewer || ch.SessionsRevoked != 1 || ch.TokensRevoked != 1 {
		t.Fatalf("demote: %+v %+v %v", u, ch, err)
	}
	if _, err := e.a.Authenticate(cookieRequest(bobSess.Token)); err == nil {
		t.Fatal("the demoted user's session must end")
	}
	if _, err := e.a.Authenticate(bearerRequest(bobAdminTok)); err == nil {
		t.Fatal("the demoted user's admin token must be deleted")
	}
	if p, err := e.a.Authenticate(bearerRequest(bobReadTok)); err != nil || p.Scope != ScopeRead {
		t.Fatalf("the read token must stay: %+v %v", p, err)
	}
	// The same role again changes nothing.
	if _, ch, err := e.a.UpdateUser(ctx, admin, bob.ID, UserUpdate{Role: strp(RoleViewer)}, testPassword); err != nil ||
		ch.RoleChanged || ch.SessionsRevoked != 0 {
		t.Fatalf("same role: %+v %v", ch, err)
	}
	// Password reset of another user: all sessions and tokens end, TOTP stays.
	bobP, bobSess = e.principal(t, "bob", "bob password 1")
	if _, _, err := e.a.TOTPBegin(ctx, bobP, "bob password 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.W.Exec(`UPDATE auth_users SET totp_secret = totp_pending WHERE username = 'bob'`); err != nil {
		t.Fatal(err)
	}
	u, ch, err = e.a.UpdateUser(ctx, admin, bob.ID, UserUpdate{Password: strp("bob password 2")}, testPassword)
	if err != nil || !ch.PasswordReset || ch.SessionsRevoked != 1 || ch.TokensRevoked != 1 || !u.TOTPEnabled || ch.Role != "" {
		t.Fatalf("password reset: %+v %+v %v", u, ch, err)
	}
	if _, err := e.a.Authenticate(bearerRequest(bobReadTok)); err == nil {
		t.Fatal("a password reset must revoke the user's tokens")
	}
	if _, err := e.a.Authenticate(cookieRequest(bobSess.Token)); err == nil {
		t.Fatal("a password reset must end the user's sessions")
	}
	// Disable TOTP: sessions end, tokens stay.
	if _, err := e.d.W.Exec(`INSERT INTO auth_sessions (id, hash, user_id, created_at, last_seen, ip, user_agent)
		VALUES ('00000000000000aa', x'01', ?, 1, 1, '', '')`, bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.W.Exec(`INSERT INTO auth_tokens (hash, user_id, name, scope, prefix, created_at) VALUES (x'02', ?, 't', 'read', 'pc_x', 1)`, bob.ID); err != nil {
		t.Fatal(err)
	}
	u, ch, err = e.a.UpdateUser(ctx, admin, bob.ID, UserUpdate{DisableTOTP: true}, testPassword)
	if err != nil || !ch.TOTPDisabled || u.TOTPEnabled || ch.SessionsRevoked != 1 || ch.TokensRevoked != 0 {
		t.Fatalf("disable TOTP: %+v %+v %v", u, ch, err)
	}
	var tokens int
	if err := e.d.R.QueryRow(`SELECT COUNT(*) FROM auth_tokens WHERE user_id = ?`, bob.ID).Scan(&tokens); err != nil || tokens != 1 {
		t.Fatalf("tokens after disabling TOTP: %d %v", tokens, err)
	}

	// The last admin cannot demote themselves; with a second admin they can,
	// and their own sessions end.
	_, _, err = e.a.UpdateUser(ctx, admin, admin.UserID, UserUpdate{Role: strp(RoleViewer)}, testPassword)
	wantErr(t, err, apperr.KindConflict, "", "at least one admin must remain")
	if _, _, err := e.a.UpdateUser(ctx, admin, bob.ID, UserUpdate{Role: strp(RoleAdmin)}, testPassword); err != nil {
		t.Fatal(err)
	}
	u, ch, err = e.a.UpdateUser(ctx, admin, admin.UserID, UserUpdate{Role: strp(RoleViewer)}, testPassword)
	if err != nil || u.Role != RoleViewer || ch.SessionsRevoked == 0 {
		t.Fatalf("self-demotion: %+v %+v %v", u, ch, err)
	}
	if _, err := e.a.Authenticate(cookieRequest(adminSess.Token)); err == nil {
		t.Fatal("self-demotion ends the own sessions")
	}
}

// Two admins demoting each other at the same time leave exactly one admin.
func TestConcurrentCrossDemotion(t *testing.T) {
	for range 5 {
		e := newUsersEnv(t)
		e.withAdmin(t)
		ctx := context.Background()
		a1, _ := e.principal(t, "admin", testPassword)
		b, err := e.a.CreateUser(ctx, a1, testPassword, "bob", "bob password 1", RoleAdmin)
		if err != nil {
			t.Fatal(err)
		}
		b1, _ := e.principal(t, "bob", "bob password 1")
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Go(func() {
			_, _, errs[0] = e.a.UpdateUser(ctx, a1, b.ID, UserUpdate{Role: strp(RoleViewer)}, testPassword)
		})
		wg.Go(func() {
			_, _, errs[1] = e.a.UpdateUser(ctx, b1, a1.UserID, UserUpdate{Role: strp(RoleViewer)}, "bob password 1")
		})
		wg.Wait()
		var admins int
		if err := e.d.R.QueryRow(`SELECT COUNT(*) FROM auth_users WHERE role = 'admin'`).Scan(&admins); err != nil || admins != 1 {
			t.Fatalf("admins after cross-demotion: %d (%v, %v)", admins, errs[0], errs[1])
		}
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("exactly one demotion must win: %v, %v", errs[0], errs[1])
		}
	}
}

func TestDeleteUser(t *testing.T) {
	e := newUsersEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	admin, adminSess := e.principal(t, "admin", testPassword)
	_, err := e.a.DeleteUser(ctx, admin, admin.UserID, testPassword)
	wantErr(t, err, apperr.KindConflict, "", "at least one admin must remain")
	_, err = e.a.DeleteUser(ctx, admin, 999, testPassword)
	wantErr(t, err, apperr.KindNotFound, "", "not found")
	_, err = e.a.DeleteUser(ctx, admin, 999, "")
	wantErr(t, err, apperr.KindInvalid, "currentPassword", "")
	v, err := e.a.CreateUser(ctx, admin, testPassword, "viewer1", "viewer password", RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	vp, vs := e.principal(t, "viewer1", "viewer password")
	vTok, _, err := e.a.CreateToken(ctx, vp, "viewer password", "ci", ScopeRead, 0)
	if err != nil {
		t.Fatal(err)
	}
	// A viewer can never delete (the API refuses the route; the service
	// checks the caller's role again).
	_, err = e.a.DeleteUser(ctx, vp, admin.UserID, "viewer password")
	wantErr(t, err, apperr.KindForbidden, "", "admin rights")
	u, err := e.a.DeleteUser(ctx, admin, v.ID, testPassword)
	if err != nil || u.Username != "viewer1" || u.Role != RoleViewer {
		t.Fatalf("delete: %+v %v", u, err)
	}
	if _, err := e.a.Authenticate(cookieRequest(vs.Token)); err == nil {
		t.Fatal("the deleted user's session must end")
	}
	if _, err := e.a.Authenticate(bearerRequest(vTok)); err == nil {
		t.Fatal("the deleted user's token must be gone")
	}
	var left int
	if err := e.d.R.QueryRow(`SELECT (SELECT COUNT(*) FROM auth_sessions WHERE user_id = ?) + (SELECT COUNT(*) FROM auth_tokens WHERE user_id = ?)`,
		v.ID, v.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("rows left: %d %v", left, err)
	}
	// Self-delete with another admin left.
	if _, err := e.a.CreateUser(ctx, admin, testPassword, "second", "second password", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.DeleteUser(ctx, admin, admin.UserID, testPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.Authenticate(cookieRequest(adminSess.Token)); err == nil {
		t.Fatal("self-delete ends the own session")
	}
}

// Token listing and deleting are scoped: viewers see and delete only their
// own tokens (another user's token is "not found"), admins all; a viewer
// cannot create an admin token; at most 20 tokens per account.
func TestTokenScoping(t *testing.T) {
	e := newUsersEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	admin, _ := e.principal(t, "admin", testPassword)
	_, adminInfo, err := e.a.CreateToken(ctx, admin, testPassword, "admin-ci", ScopeAdmin, 0)
	if err != nil || adminInfo.Username != "admin" || adminInfo.UserID != admin.UserID {
		t.Fatalf("admin token %+v %v", adminInfo, err)
	}
	if _, err := e.a.CreateUser(ctx, admin, testPassword, "viewer1", "viewer password", RoleViewer); err != nil {
		t.Fatal(err)
	}
	vp, _ := e.principal(t, "viewer1", "viewer password")
	_, _, err = e.a.CreateToken(ctx, vp, "viewer password", "x", ScopeAdmin, 0)
	wantErr(t, err, apperr.KindInvalid, "scope", "viewers can create read tokens only")
	_, vInfo, err := e.a.CreateToken(ctx, vp, "viewer password", "grafana", ScopeRead, 0)
	if err != nil || vInfo.Username != "viewer1" {
		t.Fatalf("viewer token %+v %v", vInfo, err)
	}
	mine, err := e.a.Tokens(ctx, vp)
	if err != nil || len(mine) != 1 || mine[0].ID != vInfo.ID || mine[0].Username != "viewer1" || mine[0].UserID != vp.UserID {
		t.Fatalf("viewer tokens %+v %v", mine, err)
	}
	all, err := e.a.Tokens(ctx, admin)
	if err != nil || len(all) != 2 {
		t.Fatalf("admin tokens %+v %v", all, err)
	}
	wantKind(t, e.a.DeleteToken(ctx, vp, adminInfo.ID), apperr.KindNotFound)
	if err := e.a.DeleteToken(ctx, admin, vInfo.ID); err != nil {
		t.Fatalf("an admin deletes any token: %v", err)
	}
	for i := range maxTokensPerUser {
		if _, err := e.d.W.Exec(`INSERT INTO auth_tokens (hash, user_id, name, scope, prefix, created_at) VALUES (?, ?, 't', 'read', 'pc_x', 1)`,
			[]byte{byte(i), 9}, vp.UserID); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err = e.a.CreateToken(ctx, vp, "viewer password", "one more", ScopeRead, 0)
	wantErr(t, err, apperr.KindConflict, "", "at most 20 API tokens per account")
	if _, _, err := e.a.CreateToken(ctx, admin, testPassword, "other account", ScopeRead, 0); err != nil {
		t.Fatalf("the limit is per account: %v", err)
	}
}

// A revocation that commits while a password is being checked wins: the
// token, session or change that the check was for is refused inside the
// write transaction (the role, the session and the password hash are
// checked again there).
func TestRevocationDuringPasswordCheck(t *testing.T) {
	ctx := context.Background()
	// during runs fn once, right after the next password check.
	during := func(e *testEnv, fn func()) {
		e.a.onPasswordChecked = func() {
			e.a.onPasswordChecked = nil
			fn()
		}
	}
	setup := func(t *testing.T) (e *testEnv, a, b *Principal) {
		e = newUsersEnv(t)
		e.withAdmin(t)
		a, _ = e.principal(t, "admin", testPassword)
		if _, err := e.a.CreateUser(ctx, a, testPassword, "bob", "bob password 1", RoleAdmin); err != nil {
			t.Fatal(err)
		}
		b, _ = e.principal(t, "bob", "bob password 1")
		return e, a, b
	}
	tokens := func(e *testEnv, id int64) int {
		var n int
		if err := e.d.R.QueryRow(`SELECT COUNT(*) FROM auth_tokens WHERE user_id = ?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("token vs password reset", func(t *testing.T) {
		e, a, b := setup(t)
		during(e, func() {
			if _, _, err := e.a.UpdateUser(ctx, b, a.UserID, UserUpdate{Password: strp("reset by bob 1")}, "bob password 1"); err != nil {
				t.Error(err)
			}
		})
		_, _, err := e.a.CreateToken(ctx, a, testPassword, "late", ScopeAdmin, 0)
		wantKind(t, err, apperr.KindUnauthorized)
		if n := tokens(e, a.UserID); n != 0 {
			t.Fatalf("%d tokens survived the reset", n)
		}
	})
	t.Run("token vs demotion", func(t *testing.T) {
		e, a, b := setup(t)
		during(e, func() {
			if _, _, err := e.a.UpdateUser(ctx, b, a.UserID, UserUpdate{Role: strp(RoleViewer)}, "bob password 1"); err != nil {
				t.Error(err)
			}
		})
		_, _, err := e.a.CreateToken(ctx, a, testPassword, "late", ScopeAdmin, 0)
		wantKind(t, err, apperr.KindUnauthorized)
		if n := tokens(e, a.UserID); n != 0 {
			t.Fatalf("a demoted admin got %d tokens", n)
		}
	})
	t.Run("token vs role and hash changed in place", func(t *testing.T) {
		e, a, _ := setup(t)
		during(e, func() {
			if _, err := e.d.W.Exec(`UPDATE auth_users SET role = 'viewer' WHERE id = ?`, a.UserID); err != nil {
				t.Error(err)
			}
		})
		_, _, err := e.a.CreateToken(ctx, a, testPassword, "late", ScopeAdmin, 0)
		wantErr(t, err, apperr.KindInvalid, "scope", "viewers can create read tokens only")
		during(e, func() {
			if _, err := e.d.W.Exec(`UPDATE auth_users SET password_hash = ? WHERE id = ?`, hashPassword("other password 1"), a.UserID); err != nil {
				t.Error(err)
			}
		})
		_, _, err = e.a.CreateToken(ctx, a, testPassword, "late", ScopeRead, 0)
		wantErr(t, err, apperr.KindInvalid, "currentPassword", "changed meanwhile")
		if n := tokens(e, a.UserID); n != 0 {
			t.Fatalf("%d tokens created", n)
		}
	})
	t.Run("sign-in vs password reset", func(t *testing.T) {
		e, a, b := setup(t)
		during(e, func() {
			if _, _, err := e.a.UpdateUser(ctx, b, a.UserID, UserUpdate{Password: strp("reset by bob 1")}, "bob password 1"); err != nil {
				t.Error(err)
			}
		})
		_, err := e.a.Login(ctx, "admin", testPassword, "", meta)
		wantKind(t, err, apperr.KindUnauthorized)
		var n int
		if err := e.d.R.QueryRow(`SELECT COUNT(*) FROM auth_sessions WHERE user_id = ?`, a.UserID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("sessions after the reset: %d %v", n, err)
		}
		if _, err := e.a.Login(ctx, "admin", "reset by bob 1", "", meta); err != nil {
			t.Fatalf("the new password signs in: %v", err)
		}
	})
	t.Run("password change vs password reset", func(t *testing.T) {
		e, a, b := setup(t)
		during(e, func() {
			if _, _, err := e.a.UpdateUser(ctx, b, a.UserID, UserUpdate{Password: strp("reset by bob 1")}, "bob password 1"); err != nil {
				t.Error(err)
			}
		})
		wantKind(t, e.a.ChangePassword(ctx, a, testPassword, "chosen by a 1", false), apperr.KindUnauthorized)
		if _, err := e.a.Login(ctx, "admin", "reset by bob 1", "", meta); err != nil {
			t.Fatalf("the reset password must stay: %v", err)
		}
	})
	t.Run("account change vs demotion", func(t *testing.T) {
		e, a, b := setup(t)
		during(e, func() {
			if _, _, err := e.a.UpdateUser(ctx, b, a.UserID, UserUpdate{Role: strp(RoleViewer)}, "bob password 1"); err != nil {
				t.Error(err)
			}
		})
		_, err := e.a.CreateUser(ctx, a, testPassword, "late", "late password 1", RoleAdmin)
		wantKind(t, err, apperr.KindUnauthorized)
	})
}

// Provision validates the username only when it creates the account, so a
// stored name that the rule refuses never stops the start; sign-in and
// reset-password work for such names.
func TestProvisionAndResetWithUnusualNames(t *testing.T) {
	e := newUsersEnv(t)
	ctx := context.Background()
	h := hashPassword(testPassword)
	if _, err := e.d.W.Exec(`INSERT INTO auth_users (username, role, password_hash, created_at) VALUES
		('Max@home', 'admin', ?, 1), ('.odd name', 'viewer', ?, 2)`, h, h); err != nil {
		t.Fatal(err)
	}
	if err := e.a.Provision(ctx, "-invalid-", "x"); err != nil {
		t.Fatalf("provision with existing accounts must not validate: %v", err)
	}
	for _, name := range []string{"Max@home", ".odd name"} {
		if _, err := e.a.Login(ctx, name, testPassword, "", meta); err != nil {
			t.Fatalf("sign-in %q: %v", name, err)
		}
	}
	res, err := ResetPassword(ctx, e.d, ".ODD NAME", "reset password 1", false)
	if err != nil || res.Username != ".odd name" || res.Role != RoleViewer || res.RoleChanged {
		t.Fatalf("reset of an unusual name: %+v %v", res, err)
	}
	res, err = ResetPassword(ctx, e.d, "max@home", "reset password 2", true)
	if err != nil || res.Role != RoleAdmin || res.RoleChanged {
		t.Fatalf("reset of an admin with --admin: %+v %v", res, err)
	}
	res, err = ResetPassword(ctx, e.d, ".odd name", "reset password 3", true)
	if err != nil || res.Role != RoleAdmin || !res.RoleChanged {
		t.Fatalf("--admin on a viewer: %+v %v", res, err)
	}
	entries, _, err := e.a.AuditLog(ctx, AuditQuery{Search: "password_reset"})
	if err != nil || len(entries) != 3 || !strings.Contains(entries[0].Details, `"role":"admin"`) ||
		!strings.Contains(entries[0].Details, `"roleChanged":true`) {
		t.Fatalf("audit %+v %v", entries, err)
	}
}

// Without any account, reset-password creates an admin (the name is
// validated on that path only).
func TestResetPasswordCreatesAdmin(t *testing.T) {
	e := newUsersEnv(t)
	ctx := context.Background()
	if _, err := ResetPassword(ctx, e.d, "-bad", "reset password 1", false); apperr.KindOf(err) != apperr.KindInvalid {
		t.Fatalf("create path validates the name: %v", err)
	}
	res, err := ResetPassword(ctx, e.d, "owner", "reset password 1", false)
	if err != nil || !res.Created || res.Role != RoleAdmin {
		t.Fatalf("create: %+v %v", res, err)
	}
}

// A start with accounts but no admin logs how to repair it.
func TestNoAdminLogged(t *testing.T) {
	e := newUsersEnv(t)
	e.withAdmin(t)
	if _, err := e.d.W.Exec(`UPDATE auth_users SET role = 'viewer'`); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	box, _ := secrets.New(make([]byte, 32))
	if _, err := New(context.Background(), e.d, e.set, box, "", slog.New(slog.NewTextHandler(&buf, nil))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "level=ERROR") || !strings.Contains(buf.String(), "picache reset-password --admin") {
		t.Fatalf("log: %s", buf.String())
	}
}

// Backups of auth schema v1 (0.10) and v2 are accepted; a restore keeps
// the roles of the running instance, and the accounts of a live database
// from before roles stay admins.
func TestBackupSchemaAndCarryOverRoles(t *testing.T) {
	ctx := context.Background()
	open := func(t *testing.T, steps int) (*db.DB, string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "picache.db")
		d, err := db.Open(path, 1)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Close() })
		if err := d.Migrate(ctx, "auth", migrations[:steps]); err != nil {
			t.Fatal(err)
		}
		return d, path
	}
	for _, steps := range []int{1, 2} {
		d, _ := open(t, steps)
		if err := CheckBackupSchema(ctx, d.R); err != nil {
			t.Fatalf("auth v%d backup refused: %v", steps, err)
		}
	}
	for _, tc := range []struct {
		name      string
		liveSteps int
		want      string
	}{
		{"live v2", 2, "admin=admin,watcher=viewer"},
		{"live v1", 1, "admin=admin,watcher=admin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, livePath := open(t, tc.liveSteps)
			if _, err := live.W.Exec(`INSERT INTO auth_users (username, password_hash, created_at) VALUES ('admin', 'x', 1), ('watcher', 'y', 2)`); err != nil {
				t.Fatal(err)
			}
			if tc.liveSteps == 2 {
				if _, err := live.W.Exec(`UPDATE auth_users SET role = CASE username WHEN 'admin' THEN 'admin' ELSE 'viewer' END`); err != nil {
					t.Fatal(err)
				}
			}
			live.Close()
			staged, _ := open(t, 1) // a backup of 0.10
			sdb, err := sql.Open("sqlite", "file:"+staged.Path+"?_pragma=foreign_keys(0)")
			if err != nil {
				t.Fatal(err)
			}
			defer sdb.Close()
			sdb.SetMaxOpenConns(1)
			if err := CarryOverAccounts(ctx, &db.DB{W: sdb, R: sdb, Path: staged.Path}, livePath); err != nil {
				t.Fatal(err)
			}
			rows, err := sdb.Query(`SELECT username || '=' || role FROM auth_users ORDER BY id`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var got []string
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					t.Fatal(err)
				}
				got = append(got, s)
			}
			if strings.Join(got, ",") != tc.want {
				t.Fatalf("roles after restore: %v, want %s", got, tc.want)
			}
		})
	}
}
