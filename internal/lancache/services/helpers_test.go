package services

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

const testSource = "https://cdn.test/cache-domains/"

// fakeCDN serves canned responses by URL path without any network access.
type fakeCDN struct {
	mu    sync.Mutex
	files map[string]fakeResp
	hits  map[string]int
	down  bool // every request fails with a transport error
}

type fakeResp struct {
	status   int
	body     string
	location string
}

func newFakeCDN(files map[string]string) *fakeCDN {
	f := &fakeCDN{files: map[string]fakeResp{}, hits: map[string]int{}}
	for p, body := range files {
		f.files[p] = fakeResp{status: http.StatusOK, body: body}
	}
	return f
}

func (f *fakeCDN) set(path string, r fakeResp) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = r
}

func (f *fakeCDN) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

func (f *fakeCDN) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *fakeCDN) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, io.ErrUnexpectedEOF
	}
	f.hits[req.URL.Path]++
	r, ok := f.files[req.URL.Path]
	if !ok {
		r = fakeResp{status: http.StatusNotFound, body: "not found"}
	}
	h := http.Header{}
	if r.location != "" {
		h.Set("Location", r.location)
	}
	return &http.Response{
		StatusCode: r.status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Request:    req,
	}, nil
}

// client returns an http.Client that follows redirects (the registry must
// disable that itself).
func (f *fakeCDN) client() *http.Client { return &http.Client{Transport: f} }

// standardSource is a small cache-domains source.
func standardSource() map[string]string {
	return map[string]string{
		"/cache-domains/cache_domains.json": `{"cache_domains":[
			{"name":"steam","description":"Translation of Steam CDN","domain_files":["steam.txt"]},
			{"name":"blizzard","description":"Blizzard CDN","domain_files":["blizzard.txt"]},
			{"name":"epicgames","description":"Epic CDN","domain_files":["epicgames.txt","epic-extra.txt"]},
			{"name":"origin","description":"Origin","domain_files":["origin.txt"],"notes":"HTTP only","mixed_content":true},
			{"name":"wsus","description":"Windows updates","domain_files":["windowsupdates.txt"]},
			{"name":"test","description":"Test","domain_files":["test.txt"]}
		]}`,
		"/cache-domains/steam.txt":          "lancache.steamcontent.com\n",
		"/cache-domains/blizzard.txt":       "*.cdn.blizzard.com\r\nlevel3.blizzard.com\r\n# comment\r\n\r\nDIST.Blizzard.com.\r\n",
		"/cache-domains/epicgames.txt":      "download.epicgames.com\ncdn1.epicgames.com\n",
		"/cache-domains/epic-extra.txt":     "egdownload.fastly-edge.com\ndownload.epicgames.com\n",
		"/cache-domains/origin.txt":         "origin-a.akamaihd.net\n",
		"/cache-domains/windowsupdates.txt": "*.windowsupdate.com\n*.dl.delivery.mp.microsoft.com\n*.com\nfoo bar.com\n",
		"/cache-domains/test.txt":           "trigger.lancache.net\n",
	}
}

type testEnv struct {
	db  *db.DB
	set *settings.Store
	dir string
	cdn *fakeCDN
	log *slog.Logger
}

func newEnv(t *testing.T, cdn *fakeCDN) *testEnv {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "picache.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	set, err := settings.Open(context.Background(), d, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Update(context.Background(), func(a *settings.All) error {
		a.LanCache.DomainsSource = testSource
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return &testEnv{db: d, set: set, dir: filepath.Join(dir, "cache-domains"), cdn: cdn, log: log}
}

func (e *testEnv) registry(t *testing.T) *Registry {
	t.Helper()
	r, err := New(context.Background(), e.db, e.set, e.cdn.client(), e.dir, e.log)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// loadedRegistry returns a registry that fetched standardSource.
func loadedRegistry(t *testing.T) (*Registry, *testEnv) {
	t.Helper()
	e := newEnv(t, newFakeCDN(standardSource()))
	r := e.registry(t)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r, e
}
