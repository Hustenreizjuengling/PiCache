package app

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/auth"
)

// OPS-5: a PICACHE_ADMIN_PASSWORD_FILE that no longer exists stops the start
// only while no account exists; once an admin exists it is a warning, so a
// line left in picache.env cannot take DNS down at the next restart.
func TestAdminPasswordFileMissing(t *testing.T) {
	ctx := context.Background()
	a := syncApp(t)
	logBuf := &syncBuffer{}
	a.log = slog.New(slog.NewTextHandler(logBuf, nil))
	var err error
	if a.auth, err = auth.New(ctx, a.cdb, a.set, a.box, filepath.Join(a.cfg.DataDir, "setup-token"), a.log); err != nil {
		t.Fatal(err)
	}
	a.cfg.AdminPasswordFileMissing = "/etc/picache/admin-password"

	err = a.checkAdminPasswordFile(ctx)
	if err == nil || !strings.Contains(err.Error(), "/etc/picache/admin-password does not exist and no account exists yet") {
		t.Fatalf("first start: %v", err)
	}
	if err := a.auth.Provision(ctx, "admin", "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if err := a.checkAdminPasswordFile(ctx); err != nil {
		t.Fatalf("with an account: %v", err)
	}
	if !strings.Contains(logBuf.String(), "PICACHE_ADMIN_PASSWORD_FILE names a file that does not exist") {
		t.Fatalf("no warning: %s", logBuf)
	}
	a.cfg.AdminPasswordFileMissing = ""
	if err := a.checkAdminPasswordFile(ctx); err != nil {
		t.Fatal(err)
	}
}
