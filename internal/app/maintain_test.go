package app

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// resetApp is an App with settings, auth, the web ACL and a web
// certificate manager (an HTTPS listener, the local CA) on a temporary
// data directory, with the web access restricted, a trusted proxy, TLS 1.3
// and an uploaded certificate.
func resetApp(t *testing.T) (*App, *syncBuffer) {
	t.Helper()
	e := newTLSEnv(t, "", "")
	ctx := context.Background()
	d, err := db.Open(filepath.Join(e.dir, "picache.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	box, _ := secrets.New(make([]byte, 32))
	logBuf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logBuf, nil))
	svc, err := auth.New(ctx, d, e.set, box, "", log)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(&config.Config{DataDir: e.dir, ConfigLocked: true}, log)
	a.set, a.auth, a.webTLS, a.cdb = e.set, svc, e.m, d
	a.web = netutil.NewWebAccess(e.set, log)
	e.m.start()
	ca := newTestCA(t, "Public CA", nil)
	c, k, _ := ca.leaf(t, leafOpts{dns: []string{"picache.lan"}})
	if _, err := e.m.Upload(string(c), string(k)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.set.Update(ctx, func(s *settings.All) error {
		s.Web.RestrictToNetworks, s.Web.TrustedProxies, s.Web.TLSMinVersion = true, []string{"192.168.1.5", "::1"}, "1.3"
		s.Web.AllowedNetworks = []string{"203.0.113.0/24"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return a, logBuf
}

func createMarker(t *testing.T, a *App) string {
	t.Helper()
	p := filepath.Join(a.cfg.DataDir, WebAccessResetMarker)
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func wantReset(t *testing.T, a *App, marker string) {
	t.Helper()
	w := a.set.Get().Web
	if w.RestrictToNetworks || len(w.TrustedProxies) != 0 || w.TLSMinVersion != "1.2" || len(w.AllowedNetworks) != 1 {
		t.Fatalf("web settings after the reset: %+v", w)
	}
	if st := a.webTLS.Status(); st.UploadStored || st.Source != sourceLocalCA {
		t.Fatalf("the uploaded certificate must be gone: %+v", st)
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the marker must be deleted: %v", err)
	}
	entries, _, err := a.auth.AuditLog(context.Background(), auth.AuditQuery{Search: "web.access_reset"})
	if err != nil || len(entries) != 1 || entries[0].Username != "cli" ||
		entries[0].Details != `{"restrictToNetworks":false,"tlsMinVersion":"1.2","trustedProxiesCleared":2,"uploadedCertificateRemoved":true}` {
		t.Fatalf("audit %+v %v", entries, err)
	}
}

// The service applies `picache web-access --reset` (also with
// PICACHE_CONFIG_LOCKED on): open web access, no trusted proxies, TLS 1.2,
// the uploaded certificate deleted, an audit row, the marker deleted; the
// allowed networks stay. A later settings save does not undo it.
func TestWebAccessResetApplied(t *testing.T) {
	a, logBuf := resetApp(t)
	marker := createMarker(t, a)
	a.applyWebAccessReset(context.Background())
	wantReset(t, a, marker)
	if !strings.Contains(logBuf.String(), "web access was reset from the host") {
		t.Fatalf("log %s", logBuf)
	}
	if _, err := a.set.Update(context.Background(), func(s *settings.All) error { s.Logs.QueryLogRetentionHours = 12; return nil }); err != nil {
		t.Fatal(err)
	}
	if w := a.set.Get().Web; w.RestrictToNetworks || w.TLSMinVersion != "1.2" {
		t.Fatalf("a later save undid the reset: %+v", w)
	}
	if !a.web.Get().Allowed(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("the web ACL must follow the reset")
	}
}

// On the tick and at once on SIGHUP (the maintenance loop).
func TestWebAccessResetOnSIGHUP(t *testing.T) {
	a, _ := resetApp(t)
	hup := make(chan os.Signal, 1)
	a.hup = hup
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.maintenanceLoop(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	marker := createMarker(t, a)
	hup <- os.Interrupt // any value: the channel carries SIGHUP
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Lstat(marker); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("SIGHUP must apply the marker at once")
		}
		time.Sleep(10 * time.Millisecond)
	}
	wantReset(t, a, marker)
}

// A marker that is no regular file is logged once and left alone; one that
// cannot be deleted is applied once per process start.
func TestWebAccessResetMarkerEdgeCases(t *testing.T) {
	a, logBuf := resetApp(t)
	marker := filepath.Join(a.cfg.DataDir, WebAccessResetMarker)
	if err := os.Mkdir(marker, 0o700); err != nil {
		t.Fatal(err)
	}
	a.applyWebAccessReset(context.Background())
	a.applyWebAccessReset(context.Background())
	if !a.set.Get().Web.RestrictToNetworks {
		t.Fatal("a directory must not reset the web access")
	}
	if n := strings.Count(logBuf.String(), "not a regular file"); n != 1 {
		t.Fatalf("logged %d times: %s", n, logBuf)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	old := removeMarker
	removeMarker = func(string) error { return errors.New("read-only file system") }
	defer func() { removeMarker = old }()
	createMarker(t, a)
	a.applyWebAccessReset(context.Background())
	if a.set.Get().Web.RestrictToNetworks || !a.resetApplied || !strings.Contains(logBuf.String(), "could not delete the web access reset marker") {
		t.Fatalf("an undeletable marker must be applied once: %s", logBuf)
	}
	// The admin restricts again: the undeletable marker does not reset it
	// again during this process.
	if _, err := a.set.Update(context.Background(), func(s *settings.All) error { s.Web.RestrictToNetworks = true; return nil }); err != nil {
		t.Fatal(err)
	}
	a.applyWebAccessReset(context.Background())
	if !a.set.Get().Web.RestrictToNetworks {
		t.Fatal("applied twice in one process")
	}
}

// Stored settings that Validate refuses in an unrelated member (Open keeps
// them; a stricter rule of a newer version or a restore) do not block the
// reset: it is saved as a recovery update.
func TestWebAccessResetWithInvalidSettings(t *testing.T) {
	a, logBuf := resetApp(t)
	ctx := context.Background()
	if _, err := a.cdb.W.Exec(`UPDATE settings SET doc = json_set(doc, '$.logs.maxDbSizeMiB', 1)`); err != nil {
		t.Fatal(err)
	}
	set, err := settings.Open(ctx, a.cdb, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := set.Get().Validate(); err == nil {
		t.Fatal("the stored settings must be invalid")
	}
	if _, err := set.Update(ctx, func(s *settings.All) error { s.Web.RestrictToNetworks = false; return nil }); err == nil {
		t.Fatal("Update must refuse the invalid document")
	}
	a.set = set
	marker := createMarker(t, a)
	a.applyWebAccessReset(ctx)
	wantReset(t, a, marker)
	if strings.Contains(logBuf.String(), "could not apply the web access reset") {
		t.Fatalf("log %s", logBuf)
	}
	reopened, err := settings.Open(ctx, a.cdb, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if w := reopened.Get().Web; w.RestrictToNetworks || len(w.TrustedProxies) != 0 || w.TLSMinVersion != "1.2" ||
		reopened.Get().Logs.MaxDBSizeMiB != 1 {
		t.Fatalf("stored after the reset: %+v, maxDbSizeMiB %d", w, reopened.Get().Logs.MaxDBSizeMiB)
	}
}

// A marker that could not be deleted and was then removed by hand does
// not keep a later reset from being applied.
func TestWebAccessResetAfterStuckMarkerRemoved(t *testing.T) {
	a, _ := resetApp(t)
	ctx := context.Background()
	old := removeMarker
	removeMarker = func(string) error { return errors.New("read-only file system") }
	marker := createMarker(t, a)
	a.applyWebAccessReset(ctx)
	removeMarker = old
	if !a.resetApplied {
		t.Fatal("the undeletable marker must be applied once")
	}
	if err := os.Remove(marker); err != nil { // the admin removes it by hand
		t.Fatal(err)
	}
	a.applyWebAccessReset(ctx)
	if _, err := a.set.Update(ctx, func(s *settings.All) error { s.Web.RestrictToNetworks = true; return nil }); err != nil {
		t.Fatal(err)
	}
	createMarker(t, a)
	a.applyWebAccessReset(ctx)
	if a.set.Get().Web.RestrictToNetworks {
		t.Fatal("a new marker must be applied again")
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the new marker must be deleted: %v", err)
	}
}
