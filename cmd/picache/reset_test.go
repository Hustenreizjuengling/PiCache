package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// withStdin makes os.Stdin read input for the rest of the test.
func withStdin(t *testing.T, input string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old; f.Close() })
}

// resetEnv creates a data directory with the account "owner" (a session and
// an API token) and returns the database path and the token.
func resetEnv(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PICACHE_ENV_FILE", "")
	t.Setenv("PICACHE_DATA_DIR", dir)
	path := filepath.Join(dir, "picache.db")
	ctx := context.Background()
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	set, err := settings.Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.New(make([]byte, 32))
	a, err := auth.New(ctx, d, set, box, "", log)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Provision(ctx, "owner", "owner password"); err != nil {
		t.Fatal(err)
	}
	s, err := a.Login(ctx, "owner", "owner password", "", auth.ReqMeta{IP: "192.168.1.10"})
	if err != nil {
		t.Fatal(err)
	}
	p := &auth.Principal{UserID: s.UserID, Username: "owner", SessionID: s.ID, Scope: auth.ScopeAdmin}
	tok, _, err := a.CreateToken(ctx, p, "owner password", "ci", auth.ScopeAdmin, 0)
	if err != nil {
		t.Fatal(err)
	}
	return path, tok
}

// tokenWorks reports whether the API token still authenticates.
func tokenWorks(t *testing.T, path, tok string) bool {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	set, err := settings.Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.New(make([]byte, 32))
	a, err := auth.New(ctx, d, set, box, "", log)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	_, err = a.Authenticate(r)
	return err == nil
}

// SEC-04: `picache reset-password` never adds a second admin: a name (or
// the default "admin") that matches no account is refused with the existing
// names. A reset says exactly what it changed and also revokes API tokens.
func TestResetPasswordCommand(t *testing.T) {
	path, tok := resetEnv(t)

	withStdin(t, "a new password\n")
	code, _, errOut := capture(t, func() int { return run([]string{"reset-password"}) })
	if code != 2 || !strings.Contains(errOut, `no account named "admin"`) || !strings.Contains(errOut, "owner") {
		t.Fatalf("default name: exit %d, stderr %q", code, errOut)
	}
	if strings.Contains(errOut, "New password") {
		t.Fatal("an unknown name must be refused before the password is asked for")
	}
	if !tokenWorks(t, path, tok) {
		t.Fatal("a refused reset must change nothing")
	}

	withStdin(t, "a new password\n")
	code, _, errOut = capture(t, func() int { return run([]string{"reset-password", "owner"}) })
	if code != 0 {
		t.Fatalf("reset owner: exit %d, stderr %q", code, errOut)
	}
	for _, want := range []string{`Password set for "owner"`, "Two-factor authentication was not enabled",
		"1 session(s) signed out and 1 API token(s) revoked"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("summary lacks %q: %q", want, errOut)
		}
	}
	if tokenWorks(t, path, tok) {
		t.Fatal("reset-password must revoke API tokens")
	}
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if names, err := auth.Usernames(context.Background(), d); err != nil || strings.Join(names, ",") != "owner" {
		t.Fatalf("accounts after reset = %v, %v", names, err)
	}
}

// A trigger planted in the database (e.g. by a backup restored with an old
// version) cannot make the recovery fail or keep tokens: reset-password
// removes it, says so, and revokes the tokens.
func TestResetPasswordRemovesPlantedTriggers(t *testing.T) {
	path, tok := resetEnv(t)
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`CREATE TRIGGER keep BEFORE DELETE ON auth_tokens BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	d.Close()

	withStdin(t, "a new password\n")
	code, _, errOut := capture(t, func() int { return run([]string{"reset-password", "owner"}) })
	if code != 0 {
		t.Fatalf("reset owner: exit %d, stderr %q", code, errOut)
	}
	for _, want := range []string{`Removed the trigger "keep"`, "1 API token(s) revoked"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("output lacks %q: %q", want, errOut)
		}
	}
	if tokenWorks(t, path, tok) {
		t.Fatal("reset-password must revoke API tokens despite the trigger")
	}
}
