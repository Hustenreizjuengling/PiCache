package update

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The newest eligible release wins: drafts, pre-releases (unless allowed),
// invalid tags and releases without the binary for this architecture or
// without SHA256SUMS(.sig) are skipped; order in the list does not matter.
func TestLatestSelection(t *testing.T) {
	g := newFakeGitHub(t)
	g.releases = []ghRel{
		{TagName: "v2.0.0", Draft: true, Assets: stdAssets()},
		{TagName: "v1.4.0", Assets: stdAssets("picache-linux-amd64", SumsFile)}, // no signature
		{TagName: "v1.3.0-rc.1", Prerelease: true, Assets: stdAssets(), PublishedAt: "2026-09-21T08:00:00Z"},
		{TagName: "v1.2.9-beta", Assets: stdAssets()}, // a pre-release by its version, not flagged
		{TagName: "latest", Assets: stdAssets()},
		{TagName: "v01.9.0", Assets: stdAssets()},
		{TagName: "v1.1.0", Assets: stdAssets(), Body: "old"},
		{TagName: "v1.2.0", Assets: stdAssets(), Body: "## Changes\n- more", PublishedAt: "2026-09-20T10:00:00+02:00"},
		{TagName: "v1.2.5", Assets: stdAssets("picache-linux-arm64", "picache-linux-armv7", SumsFile, SigFile)}, // no amd64
	}
	c := g.client()
	rel, err := c.Latest(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	want := Release{Version: "v1.2.0", PublishedAt: time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC),
		URL: g.URL + "/" + Repository + "/releases/tag/v1.2.0", Notes: "## Changes\n- more"}
	if rel == nil || *rel != want {
		t.Fatalf("Latest = %+v, want %+v", rel, want)
	}
	if h := g.headers; h.Get("Accept") != "application/vnd.github+json" || !strings.HasPrefix(h.Get("User-Agent"), "PiCache/") {
		t.Errorf("request headers %v", h)
	}
	if reqs := g.requested(); len(reqs) != 1 || reqs[0] != "/repos/"+Repository+"/releases" {
		t.Errorf("requests %v", reqs)
	}

	rel, err = c.Latest(t.Context(), true)
	if err != nil || rel == nil || rel.Version != "v1.3.0-rc.1" || !rel.Prerelease {
		t.Fatalf("Latest with pre-releases = %+v, %v", rel, err)
	}

	// Architectures: arm uses the armv7 binary; arm64 has v1.2.5.
	for arch, want := range map[string]string{"arm64": "v1.2.5", "arm": "v1.2.5", "amd64": "v1.2.0"} {
		c.Arch = arch
		if rel, err := c.Latest(t.Context(), false); err != nil || rel == nil || rel.Version != want {
			t.Errorf("%s: %+v, %v", arch, rel, err)
		}
	}
	c.Arch = "386"
	if _, err := c.Latest(t.Context(), false); err == nil || !strings.Contains(err.Error(), "linux/386") {
		t.Errorf("386: %v", err)
	}

	// Nothing eligible: no release, no error.
	g.releases = []ghRel{{TagName: "v9.0.0", Draft: true, Assets: stdAssets()}}
	c.Arch = "amd64"
	if rel, err := c.Latest(t.Context(), true); rel != nil || err != nil {
		t.Errorf("only drafts: %+v, %v", rel, err)
	}
}

func TestAssetName(t *testing.T) {
	for arch, want := range map[string]string{"amd64": "picache-linux-amd64", "arm64": "picache-linux-arm64", "arm": "picache-linux-armv7", "386": "", "riscv64": ""} {
		if got, ok := AssetName(arch); got != want || ok != (want != "") {
			t.Errorf("AssetName(%s) = %q, %v", arch, got, ok)
		}
	}
}

func TestReleaseNotesAreBounded(t *testing.T) {
	g := newFakeGitHub(t)
	long := strings.Repeat("ä", maxNotes) // 2 bytes each
	g.releases = []ghRel{{TagName: "v1.0.0", Assets: stdAssets(), Body: long}}
	rel, err := g.client().Latest(t.Context(), false)
	if err != nil || rel == nil {
		t.Fatal(rel, err)
	}
	if len(rel.Notes) > maxNotes+4 || !strings.HasSuffix(rel.Notes, "\n…") || !strings.HasPrefix(rel.Notes, "ää") {
		t.Errorf("notes: %d bytes, suffix %q", len(rel.Notes), rel.Notes[len(rel.Notes)-8:])
	}
}

func TestCheckErrors(t *testing.T) {
	g := newFakeGitHub(t)
	c := g.client()
	for status, want := range map[int]string{
		http.StatusNotFound:            "release information is not reachable (HTTP 404: the repository " + Repository + " is private",
		http.StatusForbidden:           "HTTP 403: GitHub rate limit",
		http.StatusInternalServerError: "release information is not reachable (HTTP 500)",
	} {
		g.status = status
		if _, err := c.Latest(t.Context(), false); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("HTTP %d: %v", status, err)
		}
	}

	// A response larger than 2 MiB is refused.
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"tag_name":"v1.0.0","body":"`)
		_, _ = io.WriteString(w, strings.Repeat("x", maxAPIResponse))
		_, _ = io.WriteString(w, `"}]`)
	}))
	defer big.Close()
	if _, err := (&Client{APIBase: big.URL}).Latest(t.Context(), false); err == nil || !strings.Contains(err.Error(), "larger than 2 MiB") {
		t.Errorf("large response: %v", err)
	}
	// Not JSON.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "<html>") }))
	defer bad.Close()
	if _, err := (&Client{APIBase: bad.URL}).Latest(t.Context(), false); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("not JSON: %v", err)
	}
	// No network.
	bad.Close()
	if _, err := (&Client{APIBase: bad.URL}).Latest(t.Context(), false); err == nil || !strings.HasPrefix(err.Error(), "release information is not reachable") {
		t.Errorf("no network: %v", err)
	}
	// A cancelled check returns at once.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := g.client().Latest(ctx, false); err == nil {
		t.Error("cancelled check succeeded")
	}
}

func TestReleaseByVersion(t *testing.T) {
	g := newFakeGitHub(t)
	g.addRelease("v1.2.0", nil)
	g.addRelease("v1.3.0-rc.1", nil)
	g.releases = append(g.releases, ghRel{TagName: "v1.4.0", Draft: true, Assets: stdAssets()},
		ghRel{TagName: "v1.5.0", Assets: stdAssets("picache-linux-arm64", SumsFile, SigFile)})
	c := g.client()
	if rel, err := c.Release(t.Context(), "v1.3.0-rc.1"); err != nil || rel.Version != "v1.3.0-rc.1" || !rel.Prerelease {
		t.Errorf("pre-release by version: %+v, %v", rel, err)
	}
	for v, want := range map[string]string{
		"v1.1.0":      "there is no release v1.1.0",
		"v1.4.0":      "is a draft",
		"v1.5.0":      "has no picache-linux-amd64",
		"v1.2":        "invalid version",
		"../../evil":  "invalid version",
		"v1.2.0/x?y=": "invalid version",
	} {
		if _, err := c.Release(t.Context(), v); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Release(%q): %v", v, err)
		}
	}
	for _, p := range g.requested() {
		if strings.Contains(p, "..") || strings.Contains(p, "evil") || strings.HasSuffix(p, "/v1.2") {
			t.Errorf("an invalid version reached GitHub: %s", p)
		}
	}
}

// Downloads never follow a redirect from https to http and stop after 5.
func TestCheckRedirect(t *testing.T) {
	req := func(u string) *http.Request { r, _ := http.NewRequest("GET", u, nil); return r }
	if err := checkRedirect(req("http://objects.example/x"), []*http.Request{req("https://github.com/x")}); err == nil {
		t.Error("https → http redirect allowed")
	}
	if err := checkRedirect(req("https://objects.example/x"), []*http.Request{req("https://github.com/x")}); err != nil {
		t.Errorf("https redirect: %v", err)
	}
	via := make([]*http.Request, maxRedirects)
	for i := range via {
		via[i] = req("https://github.com/x")
	}
	if err := checkRedirect(req("https://objects.example/x"), via); err == nil {
		t.Error("too many redirects allowed")
	}
}
