package app

import (
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/notify"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/update"
)

func events(ms []notify.Message) string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Event+" "+m.Title)
	}
	return strings.Join(out, "; ")
}

func checks(kv ...string) []api.HealthCheck {
	var out []api.HealthCheck
	for i := 0; i < len(kv); i += 2 {
		out = append(out, api.HealthCheck{Name: kv[i], Status: kv[i+1], Message: kv[i] + " is " + kv[i+1], Hint: "fix " + kv[i]})
	}
	return out
}

// A problem is reported after two consecutive evaluations with the same
// status, once; "recovered" follows only a reported problem.
func TestHealthWatchDebounce(t *testing.T) {
	var w healthWatch
	steps := []struct {
		checks []api.HealthCheck
		want   string
	}{
		{checks("upstreams", "ok", "blocklists", "fail"), ""},
		{checks("upstreams", "fail", "blocklists", "fail"), "health.failed Health check failed: blocklists"},
		{checks("upstreams", "ok", "blocklists", "fail"), ""}, // one failed evaluation: nothing
		{checks("upstreams", "fail", "blocklists", "fail"), ""},
		{checks("upstreams", "ok", "blocklists", "warn"), ""},
		{checks("upstreams", "ok", "blocklists", "warn"), "health.warning Health check warning: blocklists"},
		{checks("upstreams", "ok", "blocklists", "warn"), ""},
		{checks("upstreams", "ok", "blocklists", "ok"), "health.recovered Health check recovered: blocklists"},
		{checks("upstreams", "ok", "blocklists", "ok"), ""},
		{checks("upstreams", "warn", "blocklists", "ok", "data-disk", "fail"), ""},
		{checks("upstreams", "warn", "blocklists", "ok", "data-disk", "fail"),
			"health.warning Health check warning: upstreams; health.failed Health check failed: data-disk"},
		// A check that disappears (data-disk only exists while space is
		// low) counts as recovered; one that was never reported does not.
		{checks("upstreams", "warn", "blocklists", "ok"), "health.recovered Health check recovered: data-disk"},
		{checks("blocklists", "ok"), "health.recovered Health check recovered: upstreams"},
		{checks("blocklists", "ok", "logs", "warn"), ""},
		{checks("blocklists", "ok"), ""},
	}
	for i, st := range steps {
		if got := events(w.observe(st.checks)); got != st.want {
			t.Fatalf("step %d: %q, want %q", i, got, st.want)
		}
	}
	m := healthProblem(api.HealthCheck{Name: "cache-store", Status: "fail", Message: "cache storage offline: not mounted", Hint: "mount it"})
	if m.Message != "cache storage offline: not mounted\nHint: mount it" || m.Event != notify.EventHealthFailed {
		t.Fatalf("message %+v", m)
	}
}

func TestStoreWatch(t *testing.T) {
	var w storeWatch
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if _, ok := w.observe(true, "NAS", "", t0); ok {
		t.Fatal("online at the start")
	}
	for _, d := range []time.Duration{0, 15 * time.Second, storeOfflineDelay - time.Second} {
		if m, ok := w.observe(false, "NAS", "not mounted", t0.Add(d)); ok {
			t.Fatalf("after %s: %+v", d, m)
		}
	}
	m, ok := w.observe(false, "NAS", "not mounted", t0.Add(storeOfflineDelay))
	if !ok || m.Event != notify.EventStorageOffline || !strings.Contains(m.Message, `"NAS" has been offline for 2 minutes: not mounted`) {
		t.Fatalf("offline %+v", m)
	}
	if _, ok := w.observe(false, "NAS", "not mounted", t0.Add(time.Hour)); ok {
		t.Fatal("offline reported twice")
	}
	if m, ok := w.observe(true, "NAS", "", t0.Add(time.Hour)); !ok || m.Event != notify.EventStorageOnline {
		t.Fatalf("online %+v", m)
	}
	// A short outage is not reported, and so is its end.
	w.observe(false, "NAS", "x", t0.Add(2*time.Hour))
	if _, ok := w.observe(true, "NAS", "", t0.Add(2*time.Hour+time.Minute)); ok {
		t.Fatal("online without a reported offline")
	}
}

// webhookSink receives the notifications of a real notify.Service.
type webhookSink struct {
	mu  sync.Mutex
	got []map[string]any
}

// newNotifySink returns a running notify.Service on d with one webhook
// channel (all events, all severities) to a test server.
func newNotifySink(t *testing.T, d *db.DB) (*notify.Service, *webhookSink) {
	t.Helper()
	sink := &webhookSink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &p); err == nil {
			sink.mu.Lock()
			sink.got = append(sink.got, p)
			sink.mu.Unlock()
		}
	}))
	t.Cleanup(srv.Close)
	box, _ := secrets.New(make([]byte, 32))
	svc, err := notify.New(context.Background(), d, box, notify.Options{InstanceID: "picache-test", Hostname: "pi", Version: "v0.4.0"},
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(context.Background(), notify.ChannelInput{Name: "sink", Kind: notify.KindWebhook, URL: srv.URL + "/hook",
		Enabled: true, MinSeverity: notify.SeverityInfo}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc.Start(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return svc, sink
}

// wait returns the first n notifications (event and title), waiting up to 10 s.
func (s *webhookSink) wait(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		var out []string
		for _, p := range s.got {
			out = append(out, p["event"].(string)+" "+p["title"].(string))
		}
		s.mu.Unlock()
		if len(out) >= n {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d notifications, want %d: %v", len(out), n, out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The store loop's watcher: storage.offline after 2 minutes, storage.online
// when back, only while the download cache is enabled.
func TestWatchStoreNotifies(t *testing.T) {
	a := newTestApp(t)
	var sink *webhookSink
	a.notify, sink = newNotifySink(t, a.cdb)
	ctx := t.Context()
	var w storeWatch
	t0 := time.Now()
	offline := &api.StoreState{TargetID: "local", PassThrough: true, Reason: "the disk is not mounted"}
	a.storeState.Store(offline)
	a.watchStore(ctx, &w, t0)
	a.watchStore(ctx, &w, t0.Add(time.Hour))
	if !w.offlineSince.IsZero() {
		t.Fatal("watched while the download cache is disabled")
	}
	if _, err := a.set.Update(ctx, func(s *settings.All) error { s.DownloadCache.Enabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	a.watchStore(ctx, &w, t0)
	a.watchStore(ctx, &w, t0.Add(storeOfflineDelay))
	a.storeState.Store(&api.StoreState{TargetID: "local", Online: true})
	a.watchStore(ctx, &w, t0.Add(storeOfflineDelay+15*time.Second))
	got := sink.wait(t, 2)
	if got[0] != "storage.offline Cache storage offline: Local disk" || got[1] != "storage.online Cache storage online: Local disk" {
		t.Fatalf("notifications %v", got)
	}
}

// A sign-in lockout reaches the channels as security.lockout with the
// client address and without the username.
func TestLockoutNotification(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "picache.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	log := slog.New(slog.DiscardHandler)
	set, err := settings.Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.New(make([]byte, 32))
	svc, err := auth.New(ctx, d, set, box, filepath.Join(t.TempDir(), "setup-token"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Provision(ctx, "admin", "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	n, sink := newNotifySink(t, d)
	svc.OnLockout(func(l auth.Lockout) { n.Emit(lockoutMessage(l)) }) // as in build
	const typed = "hunter2-typed-as-username"
	for range 5 {
		_, _ = svc.Login(ctx, typed, "wrong password", "", auth.ReqMeta{IP: "192.168.1.66"})
	}
	got := sink.wait(t, 2)
	joined := strings.Join(got, "; ")
	if !strings.Contains(joined, "security.lockout Sign-in lockout: 192.168.1.66/32") ||
		!strings.Contains(joined, "security.lockout Sign-ins for a user name delayed") {
		t.Fatalf("notifications %v", got)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, p := range sink.got {
		if strings.Contains(p["message"].(string), typed) || strings.Contains(p["title"].(string), typed) {
			t.Fatalf("the username reached a notification: %v", p)
		}
		if p["severity"] != "warning" {
			t.Fatalf("severity %v", p)
		}
	}
}

func TestLockoutMessages(t *testing.T) {
	for _, l := range []auth.Lockout{
		{Kind: auth.LockoutClient, Client: "10.0.0.5/32", For: 15 * time.Minute},
		{Kind: auth.LockoutDevice, Client: "10.0.0.5/32", For: 15 * time.Minute},
		{Kind: auth.LockoutSession, Client: "10.0.0.5/32", For: 15 * time.Minute},
		{Kind: auth.LockoutUsername, Client: "", For: time.Second},
	} {
		m := lockoutMessage(l)
		if m.Event != notify.EventSecurityLockout || m.Title == "" || m.Message == "" {
			t.Fatalf("%+v: %+v", l, m)
		}
		if l.Client != "" && !strings.Contains(m.Message, l.Client) {
			t.Fatalf("%+v: no client in %q", l, m.Message)
		}
	}
}

// writeUpdateStatus writes status.json like the root helper does.
func writeUpdateStatus(t *testing.T, dataDir string, st update.Status) {
	t.Helper()
	dir := update.RequestsDir(dataDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(dir, "status.json"), b, 0o640); err != nil {
		t.Fatal(err)
	}
}

// update.available once per version (also across restarts);
// update.installed from the new version after the restart; update.failed
// for failed and rolled-back runs; every run once.
func TestUpdateNotifications(t *testing.T) {
	f := &fakeReleases{rel: &update.Release{Version: "v0.9.1", URL: "https://github.com/x/releases/v0.9.1"}}
	u, d, set := newTestUpdater(t, "v0.9.0", update.ModeHelper, f)
	var got []notify.Message
	u.emit = func(m notify.Message) { got = append(got, m) }
	now := time.Now()
	u.now = func() time.Time { return now }
	check := func() {
		now = now.Add(time.Minute) // past the 30 s floor
		u.CheckUpdate(t.Context())
	}
	check()
	check()
	if len(got) != 1 || got[0].Event != notify.EventUpdateAvailable || got[0].Title != "PiCache v0.9.1 is available" ||
		!strings.Contains(got[0].Message, "running v0.9.0") {
		t.Fatalf("available %s", events(got))
	}

	// After a restart the same version is not reported again, a newer one is.
	u2 := newUpdater(u.dataDir, "v0.9.0", d, set, func() string { return update.ModeHelper }, f.latest, slog.New(slog.DiscardHandler))
	u2.emit = u.emit
	u2.load(t.Context())
	u2.CheckUpdate(t.Context())
	f.rel = &update.Release{Version: "v0.9.2"}
	u2.lastTry = time.Time{}
	u2.CheckUpdate(t.Context())
	if len(got) != 2 || got[1].Title != "PiCache v0.9.2 is available" {
		t.Fatalf("after restart %s", events(got))
	}

	// A failed run is reported once by the running (old) version.
	start := time.Now().UTC().Add(-time.Minute)
	writeUpdateStatus(t, u.dataDir, update.Status{State: update.StateFailed, Step: update.StepVerify, Version: "v0.9.2",
		From: "v0.9.0", StartedAt: start, FinishedAt: start.Add(time.Second), Message: "signature check failed"})
	u2.watchRun(t.Context())
	u2.watchRun(t.Context())
	if len(got) != 3 || got[2].Event != notify.EventUpdateFailed || got[2].Title != "Update to v0.9.2 failed" ||
		!strings.Contains(got[2].Message, "signature check failed") || !strings.Contains(got[2].Message, "step verify") {
		t.Fatalf("failed run %s", events(got))
	}

	// The new version reports the successful run after the restart, once.
	writeUpdateStatus(t, u.dataDir, update.Status{State: update.StateSucceeded, Step: update.StepDone, Version: "v0.9.2",
		From: "v0.9.0", StartedAt: start.Add(time.Second), FinishedAt: start.Add(2 * time.Second)})
	u3 := newUpdater(u.dataDir, "v0.9.2", d, set, func() string { return update.ModeHelper }, f.latest, slog.New(slog.DiscardHandler))
	u3.emit = u.emit
	u3.load(t.Context())
	u3.watchRun(t.Context())
	u3.watchRun(t.Context())
	if len(got) != 4 || got[3].Event != notify.EventUpdateInstalled || got[3].Title != "PiCache v0.9.2 installed" {
		t.Fatalf("installed %s", events(got))
	}
	u4 := newUpdater(u.dataDir, "v0.9.2", d, set, func() string { return update.ModeHelper }, f.latest, slog.New(slog.DiscardHandler))
	u4.emit = u.emit
	u4.load(t.Context())
	u4.watchRun(t.Context())
	if len(got) != 4 {
		t.Fatalf("reported again after a restart: %s", events(got))
	}

	// A rolled-back run is reported by the old version that runs again.
	writeUpdateStatus(t, u.dataDir, update.Status{State: update.StateRolledBack, Step: update.StepRollback, Version: "v0.9.3",
		From: "v0.9.2", StartedAt: start.Add(time.Hour), FinishedAt: start.Add(time.Hour + time.Minute), Message: "health check failed"})
	u4.watchRun(t.Context())
	if len(got) != 5 || got[4].Event != notify.EventUpdateFailed || got[4].Title != "Update to v0.9.3 rolled back" {
		t.Fatalf("rolled back %s", events(got))
	}
	// A running update is not reported.
	writeUpdateStatus(t, u.dataDir, update.Status{State: update.StateRunning, Step: update.StepDownload, Version: "v0.9.4",
		From: "v0.9.2", StartedAt: time.Now().UTC()})
	u4.watchRun(t.Context())
	if len(got) != 5 {
		t.Fatalf("running %s", events(got))
	}
}
