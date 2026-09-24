//go:build linux

package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/config"
)

// TestApplyHostStrictAsRoot exercises the ownership checks and chown of a
// real root run. It needs root and a root-owned base directory, so it only
// runs as root (e.g. in a container).
func TestApplyHostStrictAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	base, err := os.MkdirTemp("/", ".picache-test-")
	if err != nil {
		t.Skip("cannot create a root-owned directory:", err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	if err := checkRootOwnedChain(base); err != nil {
		t.Skip("base directory chain is not root-owned:", err)
	}
	const svc = 65534
	cfg := &config.Config{DataDir: filepath.Join(base, "data"), CacheDir: filepath.Join(base, "cache"), MountRoot: filepath.Join(base, "mnt")}
	for _, d := range []string{cfg.DataDir, cfg.CacheDir, requestsDir(cfg)} {
		mkdir(t, d)
	}
	m, _, _ := newTestManager(t, cfg)
	if err := os.Chown(cfg.DataDir, svc, svc); err != nil {
		t.Fatal(err)
	}
	tg, err := m.Create(context.Background(), smbInput(ptr(nasPassword)))
	if err != nil {
		t.Fatal(err)
	}
	h := newFakeHost(t)
	h.env.strict = true
	h.env.serviceOwner = ownerOf
	h.env.credDir = filepath.Join(base, "etc", "credentials")
	h.env.unitDir = mkdir(t, filepath.Join(base, "systemd"))

	if err := applyHost(context.Background(), h.env, cfg, tg.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	owner := func(p string) (uint32, os.FileMode) {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Sys().(*syscall.Stat_t).Uid, fi.Mode().Perm()
	}
	if uid, _ := owner(filepath.Join(cfg.MountRoot, tg.ID)); uid != svc {
		t.Errorf("mountpoint owner %d", uid)
	}
	if uid, mode := owner(h.env.credDir); uid != 0 || mode != 0o700 {
		t.Errorf("credentials dir %d %v", uid, mode)
	}
	if uid, mode := owner(filepath.Join(h.env.credDir, tg.ID+".cred")); uid != 0 || mode != 0o600 {
		t.Errorf("credentials file %d %v", uid, mode)
	}
	mustContain(t, "unit", h.unit(t, filepath.Join(cfg.MountRoot, tg.ID)), ",uid=65534,gid=65534,")

	// A mount root the service could modify is refused.
	if err := os.Chown(cfg.MountRoot, svc, svc); err != nil {
		t.Fatal(err)
	}
	err = applyHost(context.Background(), h.env, cfg, tg.ID, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "owned by root") {
		t.Fatalf("service-owned mount root accepted: %v", err)
	}
	if err := os.Chown(cfg.MountRoot, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg.MountRoot, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := applyHost(context.Background(), h.env, cfg, tg.ID, nil, nil); err == nil {
		t.Fatal("world-writable mount root accepted")
	}
	if err := os.Chmod(cfg.MountRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// A symbolic link as mountpoint is refused.
	where := filepath.Join(cfg.MountRoot, tg.ID)
	if err := os.Remove(where); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", where); err != nil {
		t.Fatal(err)
	}
	if err := applyHost(context.Background(), h.env, cfg, tg.ID, nil, nil); err == nil {
		t.Fatal("symbolic link mountpoint accepted")
	}
}

// TestOpenConfigDBRefusesFIFO: a FIFO in place of picache.db would block the
// root helper in open(2); it is refused before SQLite opens it.
func TestOpenConfigDBRefusesFIFO(t *testing.T) {
	p := filepath.Join(t.TempDir(), "picache.db")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skip("cannot create a FIFO:", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := openConfigDB(p)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("FIFO accepted: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("blocked on a FIFO")
	}
}
