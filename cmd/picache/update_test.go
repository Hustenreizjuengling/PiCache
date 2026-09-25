package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/update"
	"github.com/hustenreizjuengling/picache/internal/version"
)

// fakeReleases serves a release list with one release of every binary.
func fakeReleases(t *testing.T, status int, tag string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = io.WriteString(w, `[{"tag_name":"`+tag+`","published_at":"2026-09-20T10:00:00Z","body":"notes",
			"assets":[{"name":"picache-linux-amd64"},{"name":"picache-linux-arm64"},{"name":"picache-linux-armv7"},
			{"name":"SHA256SUMS"},{"name":"SHA256SUMS.sig"}]}]`)
	}))
	t.Cleanup(srv.Close)
	old := newReleaseClient
	newReleaseClient = func() *update.Client {
		return &update.Client{HTTP: srv.Client(), APIBase: srv.URL, DownloadBase: "https://github.com", Arch: "amd64"}
	}
	t.Cleanup(func() { newReleaseClient = old })
}

func setVersion(t *testing.T, v string) {
	t.Helper()
	old := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = old })
}

// --check needs no root: exit code 0 = up to date, 10 = update available,
// 1 = error.
func TestUpdateCheck(t *testing.T) {
	t.Setenv("PICACHE_ENV_FILE", "")
	setVersion(t, "v0.9.0")
	fakeReleases(t, http.StatusOK, "v0.9.1")
	code, out, errOut := capture(t, func() int { return run([]string{"update", "--check"}) })
	if code != exitUpdateAvailable || !strings.Contains(out, "running: v0.9.0\nlatest:  v0.9.1 (published 2026-09-20)\n") ||
		!strings.Contains(out, "release: https://github.com/Hustenreizjuengling/PiCache/releases/tag/v0.9.1") ||
		!strings.Contains(out, "sudo picache update --version v0.9.1") {
		t.Fatalf("update available: exit %d\n%s%s", code, out, errOut)
	}

	setVersion(t, "v0.9.1")
	if code, out, _ := capture(t, func() int { return run([]string{"update", "--check"}) }); code != exitUpToDate || !strings.Contains(out, "up to date") {
		t.Fatalf("up to date: exit %d\n%s", code, out)
	}
	// A development build of the same base counts as newer.
	setVersion(t, "v0.9.1-3-gabcdef0-dirty")
	if code, _, _ := capture(t, func() int { return run([]string{"update", "--check"}) }); code != exitUpToDate {
		t.Fatalf("development build: exit %d", code)
	}

	fakeReleases(t, http.StatusNotFound, "")
	code, _, errOut = capture(t, func() int { return run([]string{"update", "--check"}) })
	if code != 1 || !strings.Contains(errOut, "release information is not reachable (HTTP 404") {
		t.Fatalf("private repository: exit %d, %q", code, errOut)
	}
}

func TestUpdateUsage(t *testing.T) {
	t.Setenv("PICACHE_ENV_FILE", "")
	for _, args := range [][]string{
		{"update", "extra"},
		{"update", "--check", "--yes"},
		{"update", "--check", "--version", "v1.0.0"},
		{"update", "--from", "/tmp", "--prerelease"},
		{"update", "--version", "1.0"},
		{"update", "--version", "v1.0.0/../x"},
		{"update", "--nope"},
		{"update", "apply-pending", "x"},
	} {
		if code, _, errOut := capture(t, func() int { return run(args) }); code != 2 || errOut == "" {
			t.Errorf("picache %s: exit %d, %q", strings.Join(args, " "), code, errOut)
		}
	}
	if code, out, _ := capture(t, func() int { return run([]string{"update", "-h"}) }); code != 0 || !strings.HasPrefix(out, "usage: picache update") {
		t.Errorf("update -h: exit %d, %q", code, out)
	}
	if code, out, _ := capture(t, func() int { return run([]string{"help"}) }); code != 0 || !strings.Contains(out, "update apply-pending") {
		t.Errorf("help lacks the update commands: %q", out)
	}
}

// Installing needs root (and Linux); nothing is asked or downloaded before.
func TestUpdateInstallNeedsRoot(t *testing.T) {
	if runtime.GOOS == "linux" && isRoot() {
		t.Skip("running as root")
	}
	t.Setenv("PICACHE_ENV_FILE", "")
	t.Setenv("PICACHE_DATA_DIR", t.TempDir())
	fakeReleases(t, http.StatusInternalServerError, "")
	code, _, errOut := capture(t, func() int { return run([]string{"update", "--yes"}) })
	if code != 1 || !(strings.Contains(errOut, "must be run as root") || strings.Contains(errOut, "only supported on Linux")) {
		t.Fatalf("exit %d, %q", code, errOut)
	}
}

// Release notes are untrusted: they cannot send terminal escape sequences.
func TestPrintRelease(t *testing.T) {
	setVersion(t, "v0.9.0")
	var b bytes.Buffer
	notes := "## Changes\n- \x1b]0;pwned\x07fix\r\n" + strings.Repeat("line\n", 30)
	printRelease(&b, &update.Release{Version: "v0.9.1", URL: "https://github.com/x", Notes: notes,
		PublishedAt: time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)})
	out := b.String()
	if strings.ContainsAny(out, "\x1b\x07\r") || !strings.Contains(out, "- ?]0;pwned?fix\n") ||
		!strings.HasPrefix(out, "Update PiCache v0.9.0 → v0.9.1 (published 2026-09-20)") || !strings.Contains(out, "  …\n") ||
		strings.Count(out, "  line") != maxNoteLines-2 || !strings.HasSuffix(out, "Release notes: https://github.com/x\n") {
		t.Fatalf("printRelease:\n%s", out)
	}
}

func isRoot() bool { return os.Geteuid() == 0 }
