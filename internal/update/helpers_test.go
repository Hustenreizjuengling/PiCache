package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// testKey signs the fake releases; useTestKey makes it the only trusted key.
var testKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))

func useTestKey(t *testing.T) {
	t.Helper()
	old := trustedKeys
	trustedKeys = []string{base64.StdEncoding.EncodeToString(testKey.Public().(ed25519.PublicKey))}
	t.Cleanup(func() { trustedKeys = old })
}

// fakeBinary is the content of a fake PiCache binary: the fake RunVersion
// "runs" it by returning its content, which is what `picache version`
// would print.
func fakeBinary(version string) []byte {
	return []byte("picache " + version + " (commit test, built 2026-09-25T00:00:00Z, go1.27, linux/amd64)\n")
}

// releaseFiles returns the assets of a signed release with the binary bin.
func releaseFiles(bin []byte) map[string][]byte {
	sums := fmt.Sprintf("%x  picache-linux-amd64\n%x  picache-deploy.tar.gz\n", sha256.Sum256(bin), sha256.Sum256([]byte("deploy")))
	return map[string][]byte{
		"picache-linux-amd64":   bin,
		"picache-deploy.tar.gz": []byte("deploy"),
		SumsFile:                []byte(sums),
		SigFile:                 []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(testKey, []byte(sums))) + "\n"),
	}
}

// ghRel is a release as the GitHub API returns it.
type ghRel struct {
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt string    `json:"published_at"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	Assets      []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name string `json:"name"`
}

func stdAssets(names ...string) []ghAsset {
	if len(names) == 0 {
		names = []string{"picache-linux-amd64", "picache-linux-arm64", "picache-linux-armv7", "picache-deploy.tar.gz", SumsFile, SigFile}
	}
	var out []ghAsset
	for _, n := range names {
		out = append(out, ghAsset{Name: n})
	}
	return out
}

// fakeGitHub serves the release API and release downloads of Repository.
type fakeGitHub struct {
	*httptest.Server
	mu       sync.Mutex
	releases []ghRel
	files    map[string]map[string][]byte // version → name → content
	requests []string                     // paths requested
	headers  http.Header                  // of the last API request
	status   int                          // non-zero: every API request answers with it
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{files: map[string]map[string][]byte{}}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Close)
	return g
}

func (g *fakeGitHub) client() *Client {
	return &Client{HTTP: g.Server.Client(), APIBase: g.URL, DownloadBase: g.URL, Arch: "amd64"}
}

// addRelease publishes a release with a signed binary for amd64.
func (g *fakeGitHub) addRelease(version string, files map[string][]byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.releases = append(g.releases, ghRel{TagName: version, PublishedAt: "2026-09-20T10:00:00Z", Body: "notes of " + version,
		Assets: stdAssets(), Prerelease: strings.Contains(version, "-")})
	g.files[version] = files
}

func (g *fakeGitHub) requested() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.requests)
}

func (g *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, r.URL.Path)
	api := "/repos/" + Repository + "/releases"
	switch p := r.URL.Path; {
	case p == api || strings.HasPrefix(p, api+"/tags/"):
		g.headers = r.Header.Clone()
		if g.status != 0 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(g.status)
			return
		}
		if p == api {
			_ = json.MarshalWrite(w, g.releases)
			return
		}
		tag := strings.TrimPrefix(p, api+"/tags/")
		for _, rel := range g.releases {
			if rel.TagName == tag {
				_ = json.MarshalWrite(w, rel)
				return
			}
		}
		http.NotFound(w, r)
	case strings.HasPrefix(p, "/"+Repository+"/releases/download/"):
		ver, name, _ := strings.Cut(strings.TrimPrefix(p, "/"+Repository+"/releases/download/"), "/")
		if b, ok := g.files[ver][name]; ok {
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

// fakeHost records systemctl calls. A restart or start "runs" the binary
// installed at bin: healthy reports whether that version passes the health
// probe, and onStart simulates what the version does at its start (for
// example the pre-upgrade database copy).
type fakeHost struct {
	mu      sync.Mutex
	bin     string
	calls   []string
	healthy func(installed string) bool
	onStart func(installed string)
	running string // content of the binary that "runs"
	failCmd string // systemctl command that fails
}

func (f *fakeHost) host() Host {
	return Host{
		Systemctl: f.systemctl,
		Health:    f.health,
		RunVersion: func(_ context.Context, path string) (string, error) {
			b, err := os.ReadFile(path)
			return string(b), err
		},
		HealthTimeout:  300 * time.Millisecond,
		HealthInterval: 10 * time.Millisecond,
	}
}

func (f *fakeHost) systemctl(_ context.Context, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	if cmd == f.failCmd {
		return errors.New("systemctl failed")
	}
	switch args[0] {
	case "stop":
		f.running = ""
	case "restart", "start":
		b, _ := os.ReadFile(f.bin)
		f.running = string(b)
		if f.onStart != nil {
			f.onStart(f.running)
		}
	}
	return nil
}

func (f *fakeHost) health(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running == "" || f.healthy == nil || !f.healthy(f.running) {
		return errors.New("connection refused")
	}
	return nil
}

func (f *fakeHost) systemctlCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// installEnv is an installed PiCache: bin/picache (version current) and a
// data directory with picache.db.
type installEnv struct {
	bin, dataDir string
	host         *fakeHost
	steps        []string
}

func newInstallEnv(t *testing.T, current string) *installEnv {
	t.Helper()
	dir := t.TempDir()
	e := &installEnv{bin: filepath.Join(dir, "bin", "picache"), dataDir: filepath.Join(dir, "data")}
	for _, d := range []string{filepath.Dir(e.bin), filepath.Join(e.dataDir, "backups")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, e.bin, fakeBinary(current))
	writeFile(t, filepath.Join(e.dataDir, "picache.db"), []byte("database of "+current))
	e.host = &fakeHost{bin: e.bin, healthy: func(string) bool { return true }}
	return e
}

func (e *installEnv) options(current, version string, files Files) Options {
	return Options{Version: version, Files: files, BinPath: e.bin, DataDir: e.dataDir, Current: current, Arch: "amd64", Host: e.host.host(),
		Progress: func(step, _ string) {
			if len(e.steps) == 0 || e.steps[len(e.steps)-1] != step {
				e.steps = append(e.steps, step)
			}
		}}
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
