package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/app"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/version"
)

// `picache web-access --reset` creates the marker exclusively (0600, never
// through a symbolic link) and never touches picache.db.
func TestWebAccessCommand(t *testing.T) {
	path, _ := resetEnv(t)
	dir := filepath.Dir(path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := capture(t, func() int { return run([]string{"web-access"}) }); code != 2 || !strings.Contains(errOut, "usage: picache web-access --reset") {
		t.Fatalf("without --reset: %d %q", code, errOut)
	}
	code, out, errOut := capture(t, func() int { return run([]string{"web-access", "--reset"}) })
	if code != 0 || !strings.Contains(out, "Web access reset requested. PiCache applies it within a minute") ||
		!strings.Contains(out, "systemctl kill -s HUP --kill-whom=main picache") {
		t.Fatalf("reset: %d %q %q", code, out, errOut)
	}
	marker := filepath.Join(dir, app.WebAccessResetMarker)
	fi, err := os.Lstat(marker)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != 0 {
		t.Fatalf("marker: %v %v", fi, err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("marker mode %v", fi.Mode().Perm())
	}
	if code, out, _ := capture(t, func() int { return run([]string{"web-access", "--reset"}) }); code != 0 || !strings.Contains(out, "A web access reset is already pending.") {
		t.Fatalf("pending: %d %q", code, out)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("the CLI must not write picache.db")
	}
	// A symbolic link in place of the marker is never followed.
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(target, marker); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	code, _, _ = capture(t, func() int { return run([]string{"web-access", "--reset"}) })
	if _, err := os.Lstat(target); err == nil {
		t.Fatalf("a file was created through the link (exit %d)", code)
	}
}

// `picache users` lists the accounts read-only.
func TestUsersCommand(t *testing.T) {
	path, _ := resetEnv(t)
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`INSERT INTO auth_users (username, role, password_hash, totp_secret, created_at) VALUES ('watcher', 'viewer', 'x', 'sealed', 1)`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	code, out, errOut := capture(t, func() int { return run([]string{"users"}) })
	if code != 0 {
		t.Fatalf("users: %d %q", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[1], "owner") ||
		!strings.Contains(lines[1], "admin") || !strings.Contains(lines[2], "watcher") || !strings.Contains(lines[2], "viewer") ||
		!strings.Contains(lines[2], " on ") || !strings.Contains(lines[2], "never") {
		t.Fatalf("output:\n%s", out)
	}
	if code, _, _ := capture(t, func() int { return run([]string{"users", "x"}) }); code != 2 {
		t.Fatalf("extra argument: %d", code)
	}
}

// reset-password keeps the role, --admin makes the account an admin (flag
// before or after the name), and the summary names the role.
func TestResetPasswordRoles(t *testing.T) {
	path, _ := resetEnv(t)
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`INSERT INTO auth_users (username, role, password_hash, created_at) VALUES ('watcher', 'viewer', 'x', 1)`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	withStdin(t, "a new password\n")
	code, _, errOut := capture(t, func() int { return run([]string{"reset-password", "watcher"}) })
	if code != 0 || !strings.Contains(errOut, "Role: viewer (run again with --admin to make it an admin).") {
		t.Fatalf("viewer: %d %q", code, errOut)
	}
	withStdin(t, "a new password\n")
	code, _, errOut = capture(t, func() int { return run([]string{"reset-password", "watcher", "--admin"}) })
	if code != 0 || !strings.Contains(errOut, "Role: admin (it was a viewer).") {
		t.Fatalf("--admin after the name: %d %q", code, errOut)
	}
	withStdin(t, "a new password\n")
	code, _, errOut = capture(t, func() int { return run([]string{"reset-password", "--admin", "owner"}) })
	if code != 0 || !strings.Contains(errOut, "Role: admin.") {
		t.Fatalf("admin: %d %q", code, errOut)
	}
	for _, args := range [][]string{{"reset-password", "a", "b"}, {"reset-password", "--force"}} {
		if code, _, errOut := capture(t, func() int { return run(args) }); code != 2 || !strings.Contains(errOut, "usage: picache reset-password") {
			t.Fatalf("%v: %d %q", args, code, errOut)
		}
	}
}

// reset-password run with a new binary before the service's first start
// migrates the auth schema, so it makes the pre-upgrade copy first (with
// the schema of the previous version) and records the new version, so the
// service does not copy the migrated database under the old name.
func TestResetPasswordKeepsPreUpgradeCopy(t *testing.T) {
	path, _ := resetEnv(t)
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Back to the state 0.10 left: auth v1 (no role column), binary v0.10.0.
	for _, q := range []string{`ALTER TABLE auth_users DROP COLUMN role`,
		`DELETE FROM schema_migrations WHERE component = 'auth' AND version > 1`,
		`CREATE TABLE app_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO app_meta (key, value) VALUES ('binary_version', 'v0.10.0')`,
		`INSERT INTO schema_migrations (component, version, applied_at) VALUES ('app', 1, 1)`} {
		if _, err := d.W.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	d.Close()
	old := version.Version
	version.Version = "v0.11.0"
	t.Cleanup(func() { version.Version = old })

	withStdin(t, "a new password\n")
	if code, _, errOut := capture(t, func() int { return run([]string{"reset-password", "owner"}) }); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	state := func(path string) (authV int, hasRole bool, bin string) {
		t.Helper()
		d, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if err := d.QueryRow(`SELECT (SELECT MAX(version) FROM schema_migrations WHERE component = 'auth'),
			EXISTS (SELECT 1 FROM pragma_table_info('auth_users') WHERE name = 'role'),
			(SELECT value FROM app_meta WHERE key = 'binary_version')`).Scan(&authV, &hasRole, &bin); err != nil {
			t.Fatal(err)
		}
		return
	}
	copies, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "backups", "picache-v0.10.0-*.db"))
	if len(copies) != 1 {
		t.Fatalf("copies %v", copies)
	}
	if authV, hasRole, bin := state(copies[0]); authV != 1 || hasRole || bin != "v0.10.0" {
		t.Fatalf("the copy has auth v%d, role column %v, version %s", authV, hasRole, bin)
	}
	if authV, hasRole, bin := state(path); authV != 2 || !hasRole || bin != "v0.11.0" {
		t.Fatalf("live: auth v%d, role column %v, version %s", authV, hasRole, bin)
	}
}
