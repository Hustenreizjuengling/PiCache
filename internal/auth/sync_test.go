package auth

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// Sync tokens: admins only, the scope sync (never read), deleted with the
// admin tokens on a demotion.
func TestSyncTokens(t *testing.T) {
	e := newUsersEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	admin, _ := e.principal(t, "admin", testPassword)
	tok, info, err := e.a.CreateToken(ctx, admin, testPassword, "follower", ScopeSync, 0)
	if err != nil || info.Scope != ScopeSync {
		t.Fatalf("sync token %+v %v", info, err)
	}
	p, err := e.a.Authenticate(bearerRequest(tok))
	if err != nil || p.Scope != ScopeSync || p.Role != RoleAdmin || p.TokenID != info.ID {
		t.Fatalf("sync principal %+v %v", p, err)
	}
	if _, err := e.a.CreateUser(ctx, admin, testPassword, "viewer1", "viewer password", RoleViewer); err != nil {
		t.Fatal(err)
	}
	vp, _ := e.principal(t, "viewer1", "viewer password")
	_, _, err = e.a.CreateToken(ctx, vp, "viewer password", "x", ScopeSync, 0)
	wantErr(t, err, apperr.KindInvalid, "scope", "viewers can create read tokens only")
	_, _, err = e.a.CreateToken(ctx, admin, testPassword, "x", Scope("root"), 0)
	wantErr(t, err, apperr.KindInvalid, "scope", "must be read, admin or sync")

	// A sync token of an account that lost admin rights behind the
	// service's back keeps the sync scope (the API checks the role).
	if _, err := e.d.W.Exec(`UPDATE auth_users SET role = 'viewer' WHERE username = 'admin'`); err != nil {
		t.Fatal(err)
	}
	if p, err := e.a.Authenticate(bearerRequest(tok)); err != nil || p.Scope != ScopeSync || p.Role != RoleViewer {
		t.Fatalf("sync token of a viewer: %+v %v", p, err)
	}
	if _, err := e.d.W.Exec(`UPDATE auth_users SET role = 'admin' WHERE username = 'admin'`); err != nil {
		t.Fatal(err)
	}

	// A demotion deletes the admin and sync tokens, read tokens stay.
	if _, err := e.a.CreateUser(ctx, admin, testPassword, "second", "second password", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	sp, _ := e.principal(t, "second", "second password")
	stok, _, err := e.a.CreateToken(ctx, sp, "second password", "sync", ScopeSync, 0)
	if err != nil {
		t.Fatal(err)
	}
	rtok, _, err := e.a.CreateToken(ctx, sp, "second password", "read", ScopeRead, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.a.UpdateUser(ctx, admin, sp.UserID, UserUpdate{Role: strp(RoleViewer)}, testPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.Authenticate(bearerRequest(stok)); err == nil {
		t.Fatal("the sync token of a demoted account still works")
	}
	if _, err := e.a.Authenticate(bearerRequest(rtok)); err != nil {
		t.Fatalf("the read token of a demoted account: %v", err)
	}
}

// Auth v3 rebuilds auth_tokens for the scope sync: the tokens of v1 and v2
// keep their rows and scopes, the schema equals a fresh one, and backups of
// every version pass CheckBackupSchema.
func TestAuthMigrationV3(t *testing.T) {
	ctx := context.Background()
	for _, from := range []int{1, 2} {
		d, err := db.Open(filepath.Join(t.TempDir(), "picache.db"), 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Migrate(ctx, "auth", migrations[:from]); err != nil {
			t.Fatal(err)
		}
		if err := CheckBackupSchema(ctx, d.R); err != nil {
			t.Fatalf("auth v%d backup refused: %v", from, err)
		}
		if _, err := d.W.Exec(`INSERT INTO auth_users (id, username, password_hash, created_at) VALUES (7, 'old', 'x', 1)`); err != nil {
			t.Fatal(err)
		}
		if _, err := d.W.Exec(`INSERT INTO auth_tokens (id, hash, user_id, name, scope, prefix, created_at, expires_at, last_used)
			VALUES (3, x'01', 7, 'ci', 'admin', 'pc_a', 10, 20, 30), (4, x'02', 7, 'grafana', 'read', 'pc_b', 11, 0, 0)`); err != nil {
			t.Fatal(err)
		}
		if _, err := d.W.Exec(`INSERT INTO auth_tokens (hash, user_id, name, scope, prefix, created_at) VALUES (x'03', 7, 's', 'sync', 'p', 1)`); err == nil {
			t.Fatalf("auth v%d accepts the scope sync", from)
		}
		if err := d.Migrate(ctx, "auth", migrations); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := d.R.QueryRow(`SELECT group_concat(id || ':' || scope || ':' || name || ':' || expires_at || ':' || last_used, ' ')
			FROM (SELECT * FROM auth_tokens ORDER BY id)`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != "3:admin:ci:20:30 4:read:grafana:0:0" {
			t.Fatalf("tokens after v3: %s", got)
		}
		if _, err := d.W.Exec(`INSERT INTO auth_tokens (hash, user_id, name, scope, prefix, created_at) VALUES (x'03', 7, 's', 'sync', 'p', 1)`); err != nil {
			t.Fatalf("v3 refuses the scope sync: %v", err)
		}
		if _, err := d.W.Exec(`INSERT INTO auth_tokens (hash, user_id, name, scope, prefix, created_at) VALUES (x'04', 7, 's', 'root', 'p', 1)`); err == nil {
			t.Fatal("v3 accepts another scope")
		}
		if _, err := d.W.Exec(`DELETE FROM auth_users WHERE id = 7`); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := d.R.QueryRow(`SELECT COUNT(*) FROM auth_tokens`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("tokens of a deleted user must cascade: %d %v", n, err)
		}
		got2, err := schemaObjects(ctx, d.R, authObjectsQuery)
		if err != nil {
			t.Fatal(err)
		}
		want, err := referenceAuthSchema(ctx, len(migrations))
		if err != nil {
			t.Fatal(err)
		}
		if len(got2) != len(want) {
			t.Fatalf("schema after the upgrade %v, fresh %v", got2, want)
		}
		for i := range want {
			if got2[i] != want[i] {
				t.Fatalf("schema after the upgrade %v, fresh %v", got2[i], want[i])
			}
		}
		if err := CheckBackupSchema(ctx, d.R); err != nil {
			t.Fatalf("auth v3 backup refused: %v", err)
		}
		d.Close()
	}
}
