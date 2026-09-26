package app

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/config"
)

// testConfig binds only loopback listeners on free ports.
func testConfig(t *testing.T) *config.Config {
	dir := t.TempDir()
	return &config.Config{DataDir: dir, CacheDir: filepath.Join(dir, "cache"), DNSListen: []string{"127.0.0.1:0"},
		WebListen: []string{"127.0.0.1:0"}, LogFormat: "text"}
}

// Fail closed: when CAP_NET_RAW could not be dropped, Run returns an error
// naming the cause before anything else starts; when it could not be
// verified, the raw socket is closed and PiCache runs on.
func TestRunRefusesNetRaw(t *testing.T) {
	old := dropNetRawFn
	t.Cleanup(func() { dropNetRawFn = old })
	dropNetRawFn = func() (error, error) { return nil, errors.New("a raw socket can still be opened") }
	err := Run(context.Background(), testConfig(t), slog.New(slog.DiscardHandler), nil)
	if err == nil || !strings.Contains(err.Error(), "CAP_NET_RAW could not be dropped: a raw socket can still be opened; refusing to run with it") {
		t.Fatalf("Run = %v", err)
	}

	dropNetRawFn = func() (error, error) { return errors.New("no CapAmb"), nil }
	a := newApp(testConfig(t), slog.New(slog.DiscardHandler))
	if err := a.bindListeners(); err != nil {
		t.Fatal(err)
	}
	defer a.ln.closeAll()
	if err := a.dropRawCapability(); err != nil {
		t.Fatal(err)
	}
	if a.ln.dhcp.HasRaw() {
		t.Fatal("raw socket kept")
	}
	if _, _, raw := a.ln.dhcp.Errors(); !strings.Contains(raw, "could not verify") {
		t.Fatalf("reason %q", raw)
	}
}
