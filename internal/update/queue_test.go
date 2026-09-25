package update

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func readStatusFile(t *testing.T, dataDir string) Status {
	t.Helper()
	var st Status
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(RequestsDir(dataDir), statusFile))), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// helperOptions is the host part of Options the CLI gives ApplyPending.
func (e *installEnv) helperOptions(current string) Options {
	o := e.options(current, "", nil)
	o.Progress = nil
	return o
}

// The service queues a request; the helper claims it, installs exactly
// that release and reports the result in status.json.
func TestApplyPending(t *testing.T) {
	useTestKey(t)
	g := newFakeGitHub(t)
	g.addRelease("v0.9.1", releaseFiles(fakeBinary("v0.9.1")))
	e := newInstallEnv(t, "v0.9.0")
	now := time.Now()

	if st := ReadStatus(e.dataDir, "v0.9.0", now); st != nil {
		t.Fatalf("status before any request: %+v", st)
	}
	if err := QueueRequest(e.dataDir, Request{Version: "v0.9.1", RequestedAt: now.UTC(), RequestedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	st := ReadStatus(e.dataDir, "v0.9.0", time.Now())
	if st == nil || st.State != StateRunning || st.Step != StepDownload || st.Version != "v0.9.1" || st.From != "v0.9.0" ||
		st.Message != msgWaiting || !st.Busy() {
		t.Fatalf("queued status: %+v", st)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(filepath.Join(RequestsDir(e.dataDir), requestFile)); err != nil || fi.Mode().Perm() != 0o640 {
			t.Errorf("request file: %v, %v", fi, err)
		}
	}
	// Not picked up for 3 minutes: the UI stops waiting.
	if st := ReadStatus(e.dataDir, "v0.9.0", time.Now().Add(staleRequest+time.Second)); st.State != StateFailed || st.Message != msgNotPickedUp || st.Busy() {
		t.Fatalf("stale request: %+v", st)
	}

	if err := ApplyPending(t.Context(), g.client(), e.helperOptions("v0.9.0")); err != nil {
		t.Fatal(err)
	}
	st = ReadStatus(e.dataDir, "v0.9.1", time.Now())
	if st == nil || st.State != StateSucceeded || st.Step != StepDone || st.Version != "v0.9.1" || st.From != "v0.9.0" ||
		st.FinishedAt.IsZero() || st.StartedAt.IsZero() || st.Busy() {
		t.Fatalf("status after the update: %+v", st)
	}
	if readFile(t, e.bin) != string(fakeBinary("v0.9.1")) {
		t.Error("binary not updated")
	}
	entries, _ := os.ReadDir(RequestsDir(e.dataDir))
	if len(entries) != 1 || entries[0].Name() != statusFile {
		t.Errorf("requests directory after the run: %v", entries)
	}
	// Nothing queued: nothing to do.
	if err := ApplyPending(t.Context(), g.client(), e.helperOptions("v0.9.1")); err != nil || len(e.host.systemctlCalls()) != 1 {
		t.Fatalf("empty queue: %v, calls %v", err, e.host.systemctlCalls())
	}
}

// Only a version string is taken from the request. Anything else, and
// every string that is not exactly vX.Y.Z[-pre], is refused before any
// network access.
func TestApplyPendingRefusesMaliciousRequests(t *testing.T) {
	useTestKey(t)
	for _, body := range []string{
		`{"version":"v1.0.0/../../../etc/passwd","requestedAt":"2026-09-25T10:00:00Z","requestedBy":"x"}`,
		`{"version":"https://evil.example/picache","requestedAt":"2026-09-25T10:00:00Z","requestedBy":"x"}`,
		`{"version":"v1.0.0; rm -rf /","requestedAt":"2026-09-25T10:00:00Z","requestedBy":"x"}`,
		`{"version":"v1.0.0\n","requestedAt":"2026-09-25T10:00:00Z","requestedBy":"x"}`,
		`{"version":"v01.0.0","requestedAt":"2026-09-25T10:00:00Z","requestedBy":"x"}`,
		`{"version":"","requestedAt":"2026-09-25T10:00:00Z","requestedBy":"x"}`,
		`{"version":"v1.0.0","url":"https://evil.example/","requestedAt":"2026-09-25T10:00:00Z","requestedBy":"x"}`,
		`{"version":"v1.0.0","requestedAt":"2026-09-25T10:00:00Z","requestedBy":"x","command":"sh"}`,
		`not json`,
		strings.Repeat(" ", maxRequestSize+1) + `{"version":"v1.0.0"}`,
	} {
		g := newFakeGitHub(t)
		g.addRelease("v1.0.0", releaseFiles(fakeBinary("v1.0.0")))
		e := newInstallEnv(t, "v0.9.0")
		dir := RequestsDir(e.dataDir)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, requestFile), []byte(body))
		err := ApplyPending(t.Context(), g.client(), e.helperOptions("v0.9.0"))
		st := readStatusFile(t, e.dataDir)
		if err == nil || st.State != StateFailed || !strings.HasPrefix(st.Message, "invalid update request") || st.Version != "" {
			t.Errorf("%q: err %v, status %+v", body, err, st)
		}
		if reqs := g.requested(); len(reqs) != 0 {
			t.Errorf("%q: requests to GitHub %v", body, reqs)
		}
		if exists(filepath.Join(dir, claimFile)) || exists(filepath.Join(dir, requestFile)) || readFile(t, e.bin) != string(fakeBinary("v0.9.0")) {
			t.Errorf("%q: claim or request left, or binary changed", body)
		}
	}
}

// The helper never installs an older release, even if the service asks.
func TestApplyPendingRefusesDowngrade(t *testing.T) {
	useTestKey(t)
	g := newFakeGitHub(t)
	g.addRelease("v0.8.0", releaseFiles(fakeBinary("v0.8.0")))
	e := newInstallEnv(t, "v0.9.0")
	if err := QueueRequest(e.dataDir, Request{Version: "v0.8.0", RequestedAt: time.Now().UTC(), RequestedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	err := ApplyPending(t.Context(), g.client(), e.helperOptions("v0.9.0"))
	st := readStatusFile(t, e.dataDir)
	if err == nil || st.State != StateFailed || !strings.Contains(st.Message, "not newer than the installed version") {
		t.Fatalf("downgrade: %v, %+v", err, st)
	}
	if slices.ContainsFunc(g.requested(), func(p string) bool { return strings.Contains(p, "/download/") }) {
		t.Error("files of the older release were downloaded")
	}
}

// A release that does not exist (or is a draft) fails with GitHub's answer.
func TestApplyPendingUnknownRelease(t *testing.T) {
	useTestKey(t)
	g := newFakeGitHub(t)
	e := newInstallEnv(t, "v0.9.0")
	if err := QueueRequest(e.dataDir, Request{Version: "v0.9.5", RequestedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyPending(t.Context(), g.client(), e.helperOptions("v0.9.0")); err == nil {
		t.Fatal("unknown release installed")
	}
	if st := readStatusFile(t, e.dataDir); st.State != StateFailed || st.Version != "v0.9.5" || !strings.Contains(st.Message, "there is no release v0.9.5") {
		t.Fatalf("status %+v", st)
	}
}

// A claim left by a helper that was killed is reported as failed, PiCache
// is started (the run may have died while it was stopped), and a request
// queued since is handled.
func TestApplyPendingInterruptedClaim(t *testing.T) {
	useTestKey(t)
	g := newFakeGitHub(t)
	e := newInstallEnv(t, "v0.9.0")
	dir := RequestsDir(e.dataDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, claimFile), []byte(`{"version":"v0.9.1","requestedAt":"2026-09-25T10:00:00Z","requestedBy":"admin"}`))
	running, _ := json.Marshal(Status{State: StateRunning, Step: StepHealth, Version: "v0.9.1", From: "v0.9.0", StartedAt: time.Now().UTC()})
	writeFile(t, filepath.Join(dir, statusFile), running)
	if err := ApplyPending(t.Context(), g.client(), e.helperOptions("v0.9.0")); err != nil {
		t.Fatal(err)
	}
	st := readStatusFile(t, e.dataDir)
	if st.State != StateFailed || st.Step != StepHealth || st.Version != "v0.9.1" || st.Message != msgInterrupted || st.FinishedAt.IsZero() {
		t.Fatalf("status %+v", st)
	}
	if exists(filepath.Join(dir, claimFile)) || !slices.Equal(e.host.systemctlCalls(), []string{"start picache.service"}) {
		t.Fatalf("claim left or PiCache not started: %v", e.host.systemctlCalls())
	}

	// With a new request, the stale claim is reported and the request handled.
	g.addRelease("v0.9.1", releaseFiles(fakeBinary("v0.9.1")))
	writeFile(t, filepath.Join(dir, claimFile), []byte("{}"))
	if err := QueueRequest(e.dataDir, Request{Version: "v0.9.1", RequestedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyPending(t.Context(), g.client(), e.helperOptions("v0.9.0")); err != nil {
		t.Fatal(err)
	}
	if st := readStatusFile(t, e.dataDir); st.State != StateSucceeded {
		t.Fatalf("status %+v", st)
	}
}

// A run that stays "running" far longer than the helper may run died.
func TestReadStatusStaleRun(t *testing.T) {
	e := newInstallEnv(t, "v0.9.0")
	dir := RequestsDir(e.dataDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-time.Hour).UTC()
	b, _ := json.Marshal(Status{State: StateRunning, Step: StepHealth, Version: "v0.9.1", From: "v0.9.0", StartedAt: started})
	writeFile(t, filepath.Join(dir, statusFile), b)
	if st := ReadStatus(e.dataDir, "v0.9.0", started.Add(10*time.Minute)); st.State != StateRunning || !st.Busy() {
		t.Fatalf("running: %+v", st)
	}
	if st := ReadStatus(e.dataDir, "v0.9.0", time.Now()); st.State != StateFailed || st.Message != msgInterrupted {
		t.Fatalf("stale run: %+v", st)
	}
	for _, bad := range []string{`{"state":"hacked"}`, `not json`, strings.Repeat("x", maxStatusSize+1)} {
		writeFile(t, filepath.Join(dir, statusFile), []byte(bad))
		if st := ReadStatus(e.dataDir, "v0.9.0", time.Now()); st != nil {
			t.Errorf("status from %.20q: %+v", bad, st)
		}
	}
}

func TestQueueRequestValidatesVersion(t *testing.T) {
	e := newInstallEnv(t, "v0.9.0")
	if err := QueueRequest(e.dataDir, Request{Version: "../x"}); err == nil {
		t.Fatal("invalid version queued")
	}
	if exists(RequestsDir(e.dataDir)) {
		t.Fatal("request directory created for an invalid request")
	}
}

// The requests directory belongs to the service: a request that is a
// symbolic link is never followed.
func TestApplyPendingRefusesLinkedRequest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	useTestKey(t)
	g := newFakeGitHub(t)
	e := newInstallEnv(t, "v0.9.0")
	dir := RequestsDir(e.dataDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "request")
	writeFile(t, target, []byte(`{"version":"v1.0.0","requestedAt":"2026-09-25T10:00:00Z","requestedBy":"x"}`))
	if err := os.Symlink(target, filepath.Join(dir, requestFile)); err != nil {
		t.Fatal(err)
	}
	if err := ApplyPending(t.Context(), g.client(), e.helperOptions("v0.9.0")); err == nil || len(g.requested()) != 0 {
		t.Fatalf("linked request: %v, requests %v", err, g.requested())
	}
	if st := readStatusFile(t, e.dataDir); st.State != StateFailed || !strings.Contains(st.Message, "not a regular file") {
		t.Fatalf("status %+v", st)
	}
}

// A release lookup that gets no answer ends after lookupTimeout instead of
// keeping the helper (and the UI's "running") for 30 minutes.
func TestApplyPendingLookupTimeout(t *testing.T) {
	useTestKey(t)
	old := lookupTimeout
	lookupTimeout = 200 * time.Millisecond
	t.Cleanup(func() { lookupTimeout = old })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(srv.Close)
	e := newInstallEnv(t, "v0.9.0")
	if err := QueueRequest(e.dataDir, Request{Version: "v0.9.1", RequestedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := ApplyPending(t.Context(), &Client{HTTP: srv.Client(), APIBase: srv.URL, DownloadBase: srv.URL, Arch: "amd64"}, e.helperOptions("v0.9.0"))
	if err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("ApplyPending = %v after %s", err, time.Since(start))
	}
	if st := readStatusFile(t, e.dataDir); st.State != StateFailed || !strings.Contains(st.Message, "not reachable") {
		t.Fatalf("status %+v", st)
	}
}
