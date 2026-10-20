package services

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

const (
	forzaPkg      = "Microsoft.624F8B84B80"
	forzaLookup   = "/v7.0/products/lookup?alternateId=PackageFamilyName&market=US&languages=en-US&value=Microsoft.624F8B84B80_8wekyb3d8bbwe"
	forzaIcon     = "/image/apps.28758.13734397844529069.box"
	destinyTitle  = "/store/api/chihiro/00_09_000/titlecontainer/US/en/999/CUSA05042_00"
	destinyIcon   = destinyTitle + "/image"
	forzaGroupKey = "xbox:" + forzaPkg
)

func testPNG(t *testing.T, w, h int) string {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// catalogEnv is a loaded registry with both store names on, a fake
// Microsoft catalog (Forza Horizon 5) and PlayStation Store (Destiny 2),
// and a cached file of the Forza package (Xbox Live).
func catalogEnv(t *testing.T) (*Registry, *testEnv) {
	t.Helper()
	r, e := loadedRegistry(t)
	e.cdn.set(forzaLookup, fakeResp{status: http.StatusOK, body: `{"Products":[{"LocalizedProperties":[{
		"ProductTitle":"Forza Horizon 5","Images":[
			{"ImagePurpose":"Screenshot","Uri":"//store-images.s-microsoft.com/image/apps.1.shot","Width":3840,"Height":2160},
			{"ImagePurpose":"Poster","Uri":"//store-images.s-microsoft.com/image/apps.1.poster","Width":1440,"Height":2160},
			{"ImagePurpose":"BoxArt","Uri":"//store-images.s-microsoft.com` + forzaIcon + `","Width":2160,"Height":2160}]}]}],
		"TotalResultCount":1}`})
	e.cdn.set(forzaIcon+"?w=64&h=64", fakeResp{status: http.StatusOK, body: testJPEG(t, 64, 64)})
	e.cdn.set(destinyTitle, fakeResp{status: http.StatusOK, body: `{"id":"UP0002-CUSA05042_00-DESTINYTHEGAME02",
		"name":"Destiny 2 PS4™ & PS5™","title_name":"Destiny 2","images":[{"type":10,"url":"https://vulcan.example/x.png"}]}`})
	e.cdn.set(destinyIcon+"?w=64&h=64", fakeResp{status: http.StatusOK, body: testPNG(t, 64, 64)})
	setStoreNames(t, e, true)
	r.SetLookupClient(e.cdn.client(), "PiCache/test")
	r.SetPathLookup(func(_ context.Context, service, groupKey string) (string, error) {
		if service == "xboxlive" && groupKey == forzaGroupKey {
			return "/7/5c3e2bd7-0d8a-4cf1-8a76-0b0f1a3aeae2/" + forzaPkg + "_3.410.25.0_x64__8wekyb3d8bbwe.msixvc", nil
		}
		return "", nil
	})
	for _, src := range r.catalog.sources {
		src.sleep = func(ctx context.Context, d time.Duration) bool { return ctx.Err() == nil }
	}
	return r, e
}

func setStoreNames(t *testing.T, e *testEnv, on bool) {
	t.Helper()
	if _, err := e.set.Update(context.Background(), func(a *settings.All) error {
		a.DownloadCache.XboxNames, a.DownloadCache.PSNNames = on, on
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// resolveQueued resolves what Label queued.
func resolveQueued(t *testing.T, r *Registry) {
	t.Helper()
	for len(r.catalog.queue) > 0 {
		key := <-r.catalog.queue
		src, id := r.catalog.source(key)
		if src == nil {
			t.Fatalf("queued %q without a source", key)
		}
		r.catalog.resolve(context.Background(), src, key, id)
		delete(r.catalog.queued, key)
	}
}

// An Xbox package is named after its store entry: the package family name
// comes from a cached file, the icon is scaled by the image host.
func TestCatalogXbox(t *testing.T) {
	r, e := catalogEnv(t)
	ctx := context.Background()
	def := defaultLabel(forzaGroupKey, displayName)
	if l := r.Label(forzaGroupKey); l != def || len(r.catalog.queue) != 1 {
		t.Fatalf("before the lookup: %q, queued %d", l, len(r.catalog.queue))
	}
	resolveQueued(t, r)
	if l := r.Label(forzaGroupKey); l != "Forza Horizon 5" {
		t.Fatalf("label = %q", l)
	}
	if e.cdn.count(forzaLookup) != 1 || e.cdn.count(forzaIcon+"?w=64&h=64") != 1 || len(r.catalog.queue) != 0 {
		t.Fatalf("requests %v, queued %d", e.cdn.hits, len(r.catalog.queue))
	}
	if p := r.IconPath(forzaGroupKey); p != "/cache/icons/xbox/"+forzaPkg {
		t.Fatalf("icon path = %q", p)
	}
	icon, ok, err := r.CatalogIcon(ctx, forzaGroupKey)
	if err != nil || !ok || !bytes.HasPrefix(icon, []byte{0xFF, 0xD8}) {
		t.Fatalf("icon: %v %v %d bytes", ok, err, len(icon))
	}
	if got := r.SearchLabels("forza"); !slices.Equal(got, []string{forzaGroupKey}) {
		t.Fatalf("search = %v", got)
	}

	// Stored: a new registry names it without a request.
	r2 := e.registry(t)
	if l := r2.Label(forzaGroupKey); l != "Forza Horizon 5" || len(r2.catalog.queue) != 0 {
		t.Fatalf("after a restart: %q, queued %d", l, len(r2.catalog.queue))
	}

	// No entry in the catalog: stored as not found, not asked again.
	other := "xbox:Contoso.Game"
	r.SetPathLookup(func(_ context.Context, service, _ string) (string, error) {
		return "/x/Contoso.Game_1.0.0.0_x64__abcdefghjk123.msixvc", nil
	})
	e.cdn.set("/v7.0/products/lookup?alternateId=PackageFamilyName&market=US&languages=en-US&value=Contoso.Game_abcdefghjk123",
		fakeResp{status: http.StatusOK, body: `{"BigIds":[],"Products":[],"TotalResultCount":0}`})
	r.Label(other)
	resolveQueued(t, r)
	if l := r.Label(other); l != defaultLabel(other, displayName) || len(r.catalog.queue) != 0 || r.catalog.entries[other].at.IsZero() {
		t.Fatalf("not found: %q, queued %d", l, len(r.catalog.queue))
	}

	// No cached file names the publisher: nothing asked, nothing stored.
	r.SetPathLookup(func(context.Context, string, string) (string, error) { return "", nil })
	n := e.cdn.total()
	r.Label("xbox:Gone.Package")
	resolveQueued(t, r)
	if _, stored := r.catalog.entries["xbox:Gone.Package"]; stored || e.cdn.total() != n {
		t.Fatal("a package without a cached file was looked up or stored")
	}

	// Off: no names, no icons, nothing queued.
	setStoreNames(t, e, false)
	if l := r.Label(forzaGroupKey); l != def || r.IconPath(forzaGroupKey) != "" || len(r.catalog.queue) != 0 {
		t.Fatalf("off: %q %q", l, r.IconPath(forzaGroupKey))
	}
	if _, ok, _ := r.CatalogIcon(ctx, forzaGroupKey); ok || r.SearchLabels("forza") != nil {
		t.Fatal("off: icon or search still served")
	}
}

// A PS4 title is named after its PlayStation Store entry; PS5 titles are
// not looked up; a 404 is "not found".
func TestCatalogPSN(t *testing.T) {
	r, e := catalogEnv(t)
	ctx := context.Background()
	key := "psn:CUSA05042"
	r.Label(key)
	resolveQueued(t, r)
	if l := r.Label(key); l != "Destiny 2" || r.IconPath(key) != "/cache/icons/psn/CUSA05042" {
		t.Fatalf("label %q, icon %q", l, r.IconPath(key))
	}
	if icon, ok, err := r.CatalogIcon(ctx, key); err != nil || !ok || !bytes.HasPrefix(icon, []byte("\x89PNG")) {
		t.Fatalf("icon: %v %v", ok, err)
	}
	if l := r.Label("psn:PPSA01284"); l != "PPSA01284" || len(r.catalog.queue) != 0 {
		t.Fatalf("PS5 title: %q, queued %d", l, len(r.catalog.queue))
	}
	r.Label("psn:CUSA99999")
	resolveQueued(t, r)
	if got, ok := r.catalog.entries["psn:CUSA99999"]; !ok || got.name != "" || !r.catalog.sources["psn"].pausedTo.IsZero() ||
		e.cdn.count("/store/api/chihiro/00_09_000/titlecontainer/US/en/999/CUSA99999_00") != 1 {
		t.Fatalf("404: %+v %v", got, ok)
	}
}

// Answers are untrusted: a failure pauses the source (not the other one),
// names are cleaned, and an icon must be a small image from the source's
// image host.
func TestCatalogUntrusted(t *testing.T) {
	r, e := catalogEnv(t)
	ctx := context.Background()
	e.cdn.set(destinyTitle, fakeResp{status: http.StatusServiceUnavailable, body: "busy"})
	r.Label("psn:CUSA05042")
	resolveQueued(t, r)
	if _, stored := r.catalog.entries["psn:CUSA05042"]; stored || r.catalog.sources["psn"].pausedTo.IsZero() ||
		!r.catalog.sources["xbox"].pausedTo.IsZero() {
		t.Fatal("a failed lookup was stored or did not pause only its source")
	}

	e.cdn.set(forzaLookup, fakeResp{status: http.StatusOK, body: `{"Products":[{"LocalizedProperties":[{
		"ProductTitle":"Evil\u001b[31m\nGame ` + strings.Repeat("é", 100) + `","Images":[
			{"ImagePurpose":"Logo","Uri":"//evil.example/x.png","Width":64,"Height":64}]}]}]}`})
	r.Label(forzaGroupKey)
	resolveQueued(t, r)
	got := r.catalog.entries[forzaGroupKey]
	if strings.ContainsAny(got.name, "\x1b\n") || len(got.name) > catalogMaxName || !strings.HasPrefix(got.name, "Evil [31m Game é") ||
		got.icon || e.cdn.count("/x.png") != 0 {
		t.Fatalf("entry = %+v, foreign icon requests %d", got, e.cdn.count("/x.png"))
	}

	src := r.catalog.sources["psn"]
	for i, body := range []string{"<svg/>", testPNG(t, 300, 20)} {
		e.cdn.set(destinyIcon+"?w=64&h=64", fakeResp{status: http.StatusOK, body: body})
		if _, err := r.catalog.fetchIcon(ctx, src, "https://store.playstation.com"+destinyIcon+"?w=64&h=64"); !errors.Is(err, errBadIcon) {
			t.Fatalf("%d: %v", i, err)
		}
	}
	if _, err := r.catalog.fetchIcon(ctx, src, "http://store.playstation.com/x"); !errors.Is(err, errBadIcon) {
		t.Fatalf("plain http icon: %v", err)
	}
}

// The worker resolves what Label queued, off the caller's path.
func TestCatalogWorker(t *testing.T) {
	r, _ := catalogEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.catalog.run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(5 * time.Second)
	for r.Label("psn:CUSA05042") != "Destiny 2" || r.IconPath("psn:CUSA05042") == "" {
		if time.Now().After(deadline) {
			t.Fatalf("not resolved: %q", r.Label("psn:CUSA05042"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}
