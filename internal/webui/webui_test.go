package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
)

const indexHTML = `<!doctype html><script type="module" src="./assets/index-abc.js"></script><div id="app"></div>`

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":          {Data: []byte(indexHTML)},
		"theme-init.js":       {Data: []byte("/* theme */")},
		"favicon.svg":         {Data: []byte("<svg/>")},
		"assets/index-abc.js": {Data: []byte("console.log(1)")},
		"assets/app-abc.css":  {Data: []byte("body{}")},
	}
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestKnownFiles(t *testing.T) {
	h := handler(testFS())
	for _, tc := range []struct {
		path, ctype, cache string
	}{
		{"/", "text/html", "no-cache"},
		{"/theme-init.js", "javascript", "no-cache"},
		{"/assets/index-abc.js", "javascript", "immutable"},
		{"/assets/app-abc.css", "text/css", "immutable"},
	} {
		rec := get(t, h, tc.path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", tc.path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, tc.ctype) {
			t.Errorf("%s: Content-Type %q, want %s", tc.path, ct, tc.ctype)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, tc.cache) {
			t.Errorf("%s: Cache-Control %q, want %s", tc.path, cc, tc.cache)
		}
	}
}

// Unknown paths one level deep get the app shell in place: its relative
// asset URLs (./assets/…) resolve to the real assets from there.
func TestUnknownTopLevelServesShell(t *testing.T) {
	h := handler(testFS())
	for _, p := range []string{"/admin", "/dns", "/robots.txt", "/assets"} {
		rec := get(t, h, p)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="app"`) {
			t.Errorf("%s: status %d body %.40q, want the app shell", p, rec.Code, rec.Body.String())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control %q, want no-cache", p, cc)
		}
	}
}

// Deeper unknown paths are redirected to the UI root. Serving the shell there
// broke the page: its ./assets/… URLs resolved to /admin/assets/…, which
// were answered with index.html (text/html), and the browser refused the
// module script (blank page).
func TestNestedUnknownPathRedirectsToRoot(t *testing.T) {
	h := handler(testFS())
	for _, tc := range []struct{ path, loc string }{
		{"/admin/", "../"},
		{"/dns/queries", "../"},
		{"/admin/index.php", "../"},
		{"/a/b/c/", "../../../"},
	} {
		rec := get(t, h, tc.path)
		if rec.Code != http.StatusFound {
			t.Errorf("%s: status %d, want 302", tc.path, rec.Code)
			continue
		}
		loc := rec.Header().Get("Location")
		if loc != tc.loc {
			t.Errorf("%s: Location %q, want %q", tc.path, loc, tc.loc)
		}
		// The relative Location lands on the root, also below a proxy prefix.
		for _, prefix := range []string{"", "/picache"} {
			base, _ := url.Parse("http://picache.lan" + prefix + tc.path)
			if got := base.ResolveReference(&url.URL{Path: loc}).Path; got != prefix+"/" {
				t.Errorf("%s%s: redirect resolves to %q, want %q", prefix, tc.path, got, prefix+"/")
			}
		}
	}
}

// What the browser does after loading a page: every asset URL of the shell,
// resolved against the page URL, must be answered with the asset (not HTML).
func TestShellAssetsResolveFromEveryServedPage(t *testing.T) {
	h := handler(testFS())
	for _, page := range []string{"/", "/admin", "/admin/", "/dns/queries", "/x/y/z"} {
		pageURL, _ := url.Parse("http://picache.lan" + page)
		rec := get(t, h, pageURL.Path)
		for rec.Code == http.StatusFound {
			pageURL = pageURL.ResolveReference(&url.URL{Path: rec.Header().Get("Location")})
			rec = get(t, h, pageURL.Path)
		}
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="app"`) {
			t.Fatalf("%s: final %s status %d, want the app shell", page, pageURL.Path, rec.Code)
		}
		asset := pageURL.ResolveReference(&url.URL{Path: "./assets/index-abc.js"})
		ar := get(t, h, asset.Path)
		if ar.Code != http.StatusOK || !strings.Contains(ar.Header().Get("Content-Type"), "javascript") {
			t.Errorf("%s: script %s answered %d %q, want JavaScript", page, asset.Path, ar.Code, ar.Header().Get("Content-Type"))
		}
	}
}

// A missing hashed asset (e.g. a stale chunk after an upgrade) is a 404, not
// index.html cached for a year under the asset's URL.
func TestMissingAssetIs404(t *testing.T) {
	h := handler(testFS())
	for _, p := range []string{"/assets/index-old.js", "/assets/"} {
		rec := get(t, h, p)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
			t.Errorf("%s: Cache-Control %q must not be immutable", p, cc)
		}
	}
}

func TestNotBuilt(t *testing.T) {
	rec := get(t, handler(fstest.MapFS{".gitkeep": {}}), "/")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
}

func TestHandlerEmbedded(t *testing.T) {
	// Works whether or not the UI was built into dist.
	rec := get(t, Handler(), "/")
	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
}
