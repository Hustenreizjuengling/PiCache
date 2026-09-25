package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/secrets"
)

const testID = "0123456789abcdef0123456789abcdef"

// testConfig returns a configuration with data, cache and mount root in a
// temp dir (all created).
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := longTempDir(t)
	cfg := &config.Config{
		DataDir:   filepath.Join(dir, "data"),
		CacheDir:  filepath.Join(dir, "cache"),
		MountRoot: filepath.Join(dir, "mnt"),
	}
	for _, d := range []string{cfg.DataDir, cfg.CacheDir, cfg.MountRoot, requestsDir(cfg)} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

// newTestManager opens picache.db and the master key in cfg.DataDir and
// creates a manager (the built-in store is initialised by New).
func newTestManager(t *testing.T, cfg *config.Config) (*Manager, *db.DB, *secrets.Box) {
	t.Helper()
	d, err := db.Open(cfg.Paths().ConfigDB, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	box, err := secrets.Open(cfg.Paths().MasterKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(context.Background(), d, box, cfg, func() int64 { return 1 << 20 }, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.hostApplyFlag = filepath.Join(cfg.DataDir, "host-apply.enabled")
	return m, d, box
}

func mkdir(t *testing.T, p string) string {
	t.Helper()
	if err := os.MkdirAll(p, 0o750); err != nil {
		t.Fatal(err)
	}
	return p
}

func ptr[T any](v T) *T { return &v }

// field returns the field of an apperr error ("" if none) and fails the
// test for non-apperr errors.
func wantKind(t *testing.T, err error, kind apperr.Kind) *apperr.Error {
	t.Helper()
	ae, ok := apperr.As(err)
	if !ok {
		t.Fatalf("want apperr kind %d, got %v", kind, err)
	}
	if ae.Kind != kind {
		t.Fatalf("want apperr kind %d, got %d (%v)", kind, ae.Kind, err)
	}
	return ae
}

// check runs a fresh guard check synchronously.
func check(t *testing.T, m *Manager, id string) checkResult {
	t.Helper()
	res, err := m.freshCheck(context.Background(), id, testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func smbInput(pw *string) TargetInput {
	return TargetInput{Name: "NAS", Kind: KindSMB, Mode: ModeHostApply, Server: "192.168.1.10", Share: "picache",
		Username: "picache", Domain: "WORKGROUP", Password: pw}
}

func localInput(path string) TargetInput {
	return TargetInput{Name: "Disk", Kind: KindLocal, Mode: ModeExternal, Path: path}
}

// longTempDir returns t.TempDir() with symbolic links and Windows 8.3 short
// names (C:\Users\RUNNER~1 on CI runners) resolved: validatePath rightly
// refuses "~", and macOS temp dirs live below the /var symlink.
func longTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	long, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return long
}
