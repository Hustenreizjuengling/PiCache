package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/update"
)

// countingReleases serves one release with every binary and every .deb and
// counts the requests.
func countingReleases(t *testing.T, tag string) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		v := strings.TrimPrefix(tag, "v")
		_, _ = io.WriteString(w, `[{"tag_name":"`+tag+`","published_at":"2026-09-20T10:00:00Z","body":"notes",
			"assets":[{"name":"picache-linux-amd64"},{"name":"picache-linux-arm64"},{"name":"picache-linux-armv7"},
			{"name":"picache-linux-armv6"},{"name":"picache-linux-386"},{"name":"picache-linux-riscv64"},
			{"name":"picache_`+v+`_amd64.deb"},{"name":"picache_`+v+`_arm64.deb"},{"name":"picache_`+v+`_armhf.deb"},
			{"name":"picache_`+v+`_i386.deb"},{"name":"picache_`+v+`_riscv64.deb"},
			{"name":"SHA256SUMS"},{"name":"SHA256SUMS.sig"}]}]`)
	}))
	t.Cleanup(srv.Close)
	old := newReleaseClient
	newReleaseClient = func() *update.Client {
		return &update.Client{HTTP: srv.Client(), APIBase: srv.URL, DownloadBase: "https://github.com"}
	}
	t.Cleanup(func() { newReleaseClient = old })
	return &n
}

func setPackaged(t *testing.T, v bool) {
	t.Helper()
	old := packaged
	packaged = func() bool { return v }
	t.Cleanup(func() { packaged = old })
}

// In package mode every installing form of `picache update` and the root
// helper's apply-pending exit 1 with the apt steps before anything is
// downloaded (or root is asked for).
func TestUpdateRefusedInPackageMode(t *testing.T) {
	t.Setenv("PICACHE_ENV_FILE", "")
	t.Setenv("PICACHE_DATA_DIR", t.TempDir())
	setVersion(t, "v0.16.0")
	setPackaged(t, true)
	reqs := countingReleases(t, "v0.16.1")
	arch := debArch()
	for _, tc := range []struct {
		args []string
		file string
	}{
		{[]string{"update"}, "picache_<version>_" + arch + ".deb"},
		{[]string{"update", "--yes"}, "picache_<version>_" + arch + ".deb"},
		{[]string{"update", "--channel", "beta", "--yes"}, "picache_<version>_" + arch + ".deb"},
		{[]string{"update", "--version", "v0.16.1", "--yes"}, "picache_0.16.1_" + arch + ".deb"},
		{[]string{"update", "--version", "v0.16.1-rc.1", "--allow-downgrade"}, "picache_0.16.1-rc.1_" + arch + ".deb"},
		{[]string{"update", "--from", t.TempDir(), "--yes"}, "picache_<version>_" + arch + ".deb"},
		{[]string{"update", "apply-pending"}, "picache_<version>_" + arch + ".deb"},
	} {
		code, out, errOut := capture(t, func() int { return run(tc.args) })
		want := "picache update: PiCache was installed as a Debian package: download " + tc.file +
			` from the release, verify it (docs/DEPLOYMENT.md "Debian package") and install it with sudo apt install ./` + tc.file + "\n"
		if code != 1 || errOut != want || out != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q\nwant %q", tc.args, code, out, errOut, want)
		}
	}
	if n := reqs.Load(); n != 0 {
		t.Fatalf("%d requests to the release API before the refusal", n)
	}
	// Usage errors stay usage errors.
	if code, _, _ := capture(t, func() int { return run([]string{"update", "apply-pending", "x"}) }); code != 2 {
		t.Errorf("usage: exit %d", code)
	}
}

// --check works in package mode and names the apt steps when an update is
// available; outside package mode it shows the usual command.
func TestUpdateCheckPackageMode(t *testing.T) {
	t.Setenv("PICACHE_ENV_FILE", "")
	setVersion(t, "v0.16.0")
	countingReleases(t, "v0.16.1")
	setPackaged(t, true)
	arch := debArch()
	code, out, errOut := capture(t, func() int { return run([]string{"update", "--check"}) })
	file := "picache_0.16.1_" + arch + ".deb"
	if code != exitUpdateAvailable || !strings.Contains(out, "latest:  v0.16.1") ||
		!strings.Contains(out, "PiCache was installed as a Debian package: download "+file) ||
		!strings.Contains(out, "sudo apt install ./"+file+"\n") || strings.Contains(out, "sudo picache update") {
		t.Fatalf("package mode: exit %d\n%s%s", code, out, errOut)
	}
	setPackaged(t, false)
	code, out, _ = capture(t, func() int { return run([]string{"update", "--check"}) })
	if code != exitUpdateAvailable || !strings.Contains(out, "sudo picache update --version v0.16.1") || strings.Contains(out, "apt install") {
		t.Fatalf("helper mode: exit %d\n%s", code, out)
	}
	// A release without the .deb of this architecture is not offered in
	// package mode.
	fakeReleases(t, http.StatusOK, "v0.16.1")
	setPackaged(t, true)
	code, out, _ = capture(t, func() int { return run([]string{"update", "--check"}) })
	if code != exitUpToDate || !strings.Contains(out, "latest:  none") {
		t.Fatalf("no .deb: exit %d\n%s", code, out)
	}
}
