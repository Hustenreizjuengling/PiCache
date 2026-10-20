package services

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

const (
	forzaPkg      = "Microsoft.624F8B84B80"
	forzaLookup   = "/v7.0/products/lookup?alternateId=PackageFamilyName&market=US&languages=en-US&value=Microsoft.624F8B84B80_8wekyb3d8bbwe"
	forzaIcon     = "/image/apps.28758.13734397844529069.box"
	destinyTitle  = "/store/api/chihiro/00_09_000/titlecontainer/US/en/999/CUSA05042_00"
	destinyIcon   = destinyTitle + "/image"
	forzaGroupKey = "xbox:" + forzaPkg
	conceptQuery  = "/concepts?country=US&language=en&age=99"
	destinyConcpt = "/api/catalog/v2/titles/CUSA05042_00" + conceptQuery
	returnalIcon  = "/vulcan/ap/rnd/202011/0415/master.png"
)

// psnConceptBody is a catalog answer (trimmed) naming a concept.
func psnConceptBody(id, name, master string) string {
	return `[{"id":` + id + `,"name":"` + name + `","titleIds":["PPSA01284_00","PPSA01285_00"],"media":{"images":[
		{"type":"LOGO","url":"https://image.api.playstation.com/vulcan/logo.png"},
		{"type":"MASTER","url":"` + master + `"}]}}]`
}

// psnNoConcept is the catalog's answer for a title it does not list.
const psnNoConcept = `[{"titleIds":["X_00"],"country":"US","language":"en","revisionIds":{},"errorCode":3166081}]`

func testPNG(t *testing.T, w, h int) string {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// catalogEnv is a loaded registry with the store names on, a fake
// Microsoft catalog (Forza Horizon 5) and PlayStation Store (Returnal and
// Destiny 2 in the catalog, the PS4 title CUSA00001 only in the US title
// container), and a cached file of the Forza package (Xbox Live).
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
	e.cdn.set("/api/catalog/v2/titles/PPSA01284_00"+conceptQuery, fakeResp{status: http.StatusOK,
		body: psnConceptBody(`10000176`, "Returnal", "https://image.api.playstation.com"+returnalIcon)})
	e.cdn.set(returnalIcon+"?w=64", fakeResp{status: http.StatusOK, body: testJPEG(t, 137, 137)})
	e.cdn.set(destinyConcpt, fakeResp{status: http.StatusOK,
		body: psnConceptBody(`"221852"`, "Destiny 2", "https://image.api.playstation.com/vulcan/destiny.png")})
	e.cdn.set("/vulcan/destiny.png?w=64", fakeResp{status: http.StatusOK, body: testPNG(t, 64, 64)})
	for _, id := range []string{"CUSA00001", "CUSA99999", "PPSA99999"} {
		e.cdn.set("/api/catalog/v2/titles/"+id+"_00"+conceptQuery, fakeResp{status: http.StatusOK, body: psnNoConcept})
	}
	e.cdn.set("/store/api/chihiro/00_09_000/titlecontainer/US/en/999/CUSA00001_00", fakeResp{status: http.StatusOK,
		body: `{"id":"UP0001-CUSA00001_00-OLDGAME000000000","name":"Old Game PS4™","title_name":"Old Game"}`})
	e.cdn.set("/store/api/chihiro/00_09_000/titlecontainer/US/en/999/CUSA00001_00/image?w=64&h=64",
		fakeResp{status: http.StatusOK, body: testPNG(t, 64, 64)})
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
		a.DownloadCache.XboxNames, a.DownloadCache.PSNNames, a.DownloadCache.WindowsNames = on, on, on
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

// A PS4 or PS5 title is named after the PlayStation Store's catalog entry
// (its concept); a PS4 title the catalog does not list after the US title
// container; a 404 there is "not found".
func TestCatalogPSN(t *testing.T) {
	r, e := catalogEnv(t)
	ctx := context.Background()
	for key, want := range map[string]string{"psn:CUSA05042": "Destiny 2", "psn:PPSA01284": "Returnal", "psn:CUSA00001": "Old Game"} {
		r.Label(key)
		resolveQueued(t, r)
		id := strings.TrimPrefix(key, "psn:")
		if l := r.Label(key); l != want || r.IconPath(key) != "/cache/icons/psn/"+id {
			t.Fatalf("%s: label %q, icon %q", key, l, r.IconPath(key))
		}
		if _, ok, err := r.CatalogIcon(ctx, key); err != nil || !ok {
			t.Fatalf("%s icon: %v %v", key, ok, err)
		}
	}
	if e.cdn.count(destinyTitle) != 0 || e.cdn.count("/store/api/chihiro/00_09_000/titlecontainer/US/en/999/PPSA01284_00") != 0 {
		t.Fatal("a title the catalog lists was asked in the title container")
	}
	r.Label("psn:CUSA99999")
	r.Label("psn:PPSA99999")
	resolveQueued(t, r)
	for _, id := range []string{"CUSA99999", "PPSA99999"} {
		if got, ok := r.catalog.entries["psn:"+id]; !ok || got.name != "" || !r.catalog.sources["psn"].pausedTo.IsZero() {
			t.Fatalf("%s: %+v %v", id, got, ok)
		}
	}
	// Only PS4 titles are asked in the title container.
	if e.cdn.count("/store/api/chihiro/00_09_000/titlecontainer/US/en/999/CUSA99999_00") != 1 ||
		e.cdn.count("/store/api/chihiro/00_09_000/titlecontainer/US/en/999/PPSA99999_00") != 0 {
		t.Fatalf("requests %v", e.cdn.hits)
	}
}

// Answers are untrusted: a failure pauses the source (not the other one),
// names are cleaned, and an icon must be a small image from the source's
// image host.
func TestCatalogUntrusted(t *testing.T) {
	r, e := catalogEnv(t)
	ctx := context.Background()
	e.cdn.set(destinyConcpt, fakeResp{status: http.StatusServiceUnavailable, body: "busy"})
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

// windowsSearchPage is a (trimmed) Microsoft Update Catalog result page.
func windowsSearchPage(titles ...string) string {
	var b strings.Builder
	b.WriteString(`<html><body><table class="resultsBorder">`)
	for i, title := range titles {
		id := fmt.Sprintf("953645fb-3a1b-4c2d-8e9f-0a1b2c3d4e%02d", i)
		fmt.Fprintf(&b, `<tr><td><a id='%s_link' href= "javascript:void(0);" onclick='goToDetails("%s");' class="contentTextItemSpacerNoBreakLink">
                            %s
                        </a></td><td id="%s_C2_R%d">Windows 11</td></tr>`, id, id, title, id, i)
	}
	b.WriteString(`</table></body></html>`)
	return b.String()
}

// A Windows update is named after the Microsoft Update Catalog result that
// matches its file best; withdrawn updates keep the offline label.
func TestCatalogWindows(t *testing.T) {
	r, e := catalogEnv(t)
	e.cdn.set("/Search.aspx?q=KB5065426", fakeResp{status: http.StatusOK, body: windowsSearchPage(
		"2025-09 Cumulative Update for Windows 11, version 25H2 for arm64-based Systems (KB5065426) (26200.6584)",
		"2025-09 Cumulative Update for Microsoft server operating system version 24H2 for x64-based Systems (KB5065426) (26100.6584)",
		"2025-09 Cumulative Update for Windows 11, version 25H2 for x64-based Systems (KB5065426) (26200.6584)",
		"Unrelated &amp; listed (KB1234567)")})
	e.cdn.set("/Search.aspx?q=KB5066613", fakeResp{status: http.StatusOK, body: windowsSearchPage(
		"2025-09 Cumulative Update for Windows 11, version 24H2 for x64-based Systems (KB5066613)",
		"2025-09 Cumulative Update for .NET Framework 3.5 and 4.8.1 for Windows 11, version 24H2 for x64 (KB5066613)")})
	e.cdn.set("/Search.aspx?q=KB5031356", fakeResp{status: http.StatusOK,
		body: `<html><body><span id="ctl00_catalogBody_noResultText">We did not find any results for "KB5031356".</span></body></html>`})
	for key, want := range map[string]string{
		"win:kb5065426:windows11.0-x64":        "2025-09 Cumulative Update for Windows 11, version 25H2 for x64-based Systems (KB5065426)",
		"win:kb5065426:windows11.0-arm64":      "2025-09 Cumulative Update for Windows 11, version 25H2 for arm64-based Systems (KB5065426)",
		"win:kb5066613:windows11.0-x64-ndp481": "2025-09 Cumulative Update for .NET Framework 3.5 and 4.8.1 for Windows 11, version 24H2 for x64 (KB5066613)",
		"win:kb5066613":                        "2025-09 Cumulative Update for Windows 11, version 24H2 for x64-based Systems (KB5066613)",
	} {
		r.Label(key)
		resolveQueued(t, r)
		if l := r.Label(key); l != want || r.IconPath(key) != "" {
			t.Errorf("%s: %q, icon %q", key, l, r.IconPath(key))
		}
	}
	if e.cdn.count("/Search.aspx?q=KB5065426") != 2 {
		t.Fatalf("requests %v", e.cdn.hits)
	}
	withdrawn := "win:kb5031356:windows10.0-x64"
	r.Label(withdrawn)
	resolveQueued(t, r)
	if l := r.Label(withdrawn); l != "KB5031356 · Windows 10 (x64)" || r.catalog.entries[withdrawn].at.IsZero() || len(r.catalog.queue) != 0 {
		t.Fatalf("withdrawn: %q", l)
	}
	// A built-in label wins and is not looked up; off: the offline label.
	if l := r.Label("win:kb890830"); l != "Windows Malicious Software Removal Tool" || len(r.catalog.queue) != 0 {
		t.Fatalf("MSRT: %q, queued %d", l, len(r.catalog.queue))
	}
	setStoreNames(t, e, false)
	if l := r.Label("win:kb5065426:windows11.0-x64"); l != "KB5065426 · Windows 11 (x64)" || len(r.catalog.queue) != 0 {
		t.Fatalf("off: %q", l)
	}
}

// epicManifest builds a binary Epic manifest whose meta names app and exe
// (UTF-16 strings when wide).
func epicManifest(t *testing.T, app, exe string, compressed, wide bool) []byte {
	t.Helper()
	fstring := func(b *bytes.Buffer, s string) {
		if wide {
			u := utf16.Encode([]rune(s + "\x00"))
			_ = binary.Write(b, binary.LittleEndian, int32(-len(u)))
			_ = binary.Write(b, binary.LittleEndian, u)
			return
		}
		_ = binary.Write(b, binary.LittleEndian, int32(len(s)+1))
		b.WriteString(s + "\x00")
	}
	var meta bytes.Buffer
	_ = binary.Write(&meta, binary.LittleEndian, uint32(0)) // meta size (unused)
	meta.WriteByte(0)                                       // data version
	_ = binary.Write(&meta, binary.LittleEndian, uint32(18))
	meta.WriteByte(1)
	_ = binary.Write(&meta, binary.LittleEndian, uint32(0))
	fstring(&meta, app)
	fstring(&meta, "++Fortnite+Release-1.0-CL-1")
	fstring(&meta, exe)
	fstring(&meta, "")
	meta.Write(bytes.Repeat([]byte{0xAB}, 4096)) // the chunk lists
	body := meta.Bytes()
	var stored byte
	if compressed {
		var z bytes.Buffer
		w := zlib.NewWriter(&z)
		_, _ = w.Write(body)
		_ = w.Close()
		body, stored = z.Bytes(), 1
	}
	var h bytes.Buffer
	_ = binary.Write(&h, binary.LittleEndian, uint32(epicManifestMagic))
	_ = binary.Write(&h, binary.LittleEndian, uint32(41))
	_ = binary.Write(&h, binary.LittleEndian, uint32(meta.Len()))
	_ = binary.Write(&h, binary.LittleEndian, uint32(len(body)))
	h.Write(make([]byte, 20))
	h.WriteByte(stored)
	_ = binary.Write(&h, binary.LittleEndian, uint32(21))
	h.Write(body)
	return h.Bytes()
}

func TestParseEpicManifest(t *testing.T) {
	m := epicManifest(t, "Sugar", "Binaries/Win64/RocketLeague.exe", true, false)
	for name, b := range map[string][]byte{
		"compressed": m,
		"cut off":    m[:len(m)-12], // only the start of the stream is needed
		"plain":      epicManifest(t, "Sugar", "Binaries/Win64/RocketLeague.exe", false, false),
		"utf-16":     epicManifest(t, "Sugar", "Binaries/Win64/RocketLeague.exe", true, true),
		"json":       []byte("\xef\xbb\xbf{\n\t\"ManifestFileVersion\": \"013000000000\",\n\t\"AppNameString\": \"Sugar\",\n\t\"LaunchExeString\": \"Binaries\\\\Win64\\\\RocketLeague.exe\"\n}"),
	} {
		app, exe, err := parseEpicManifest(b)
		if err != nil || app != "Sugar" || epicName(app, exe) != "Rocket League" {
			t.Errorf("%s: %q %q %v", name, app, exe, err)
		}
	}
	long := epicManifest(t, strings.Repeat("a", 2000), "x.exe", false, false)
	for name, b := range map[string][]byte{
		"empty": nil, "not a manifest": []byte("<html>"), "bad magic": append([]byte{1, 2, 3, 4}, m[4:]...),
		"truncated header": m[:30], "header size beyond": append(append([]byte{}, m[:4]...), 0xff, 0xff, 0, 0),
		"bad zlib": append(append([]byte{}, m[:41]...), 0x00, 0x01), "string too long": long,
		"json without names": []byte(`{"ManifestFileVersion":"013000000000"}`),
	} {
		if _, _, err := parseEpicManifest(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEpicName(t *testing.T) {
	for _, tt := range []struct{ app, exe, want string }{
		{"Sugar", "Binaries/Win64/RocketLeague.exe", "Rocket League"},
		{"dc9d2e595d0e4650b35d659f90d41059", "AlanWake2.exe", "Alan Wake 2"},
		{"Fortnite", "FortniteGame/Binaries/Win64/FortniteClient-Win64-Shipping.exe", "Fortnite Client"},
		{"Hydra", `Control\Control_DX12.exe`, "Control"},
		{"Satisfactory", "", "Satisfactory"},
		{"Oak", "OakGame/Binaries/Win64/Borderlands3.exe", "Borderlands 3"},
		{"Ue", "Engine/UE5Editor.exe", "UE5 Editor"},
		{"dc9d2e595d0e4650b35d659f90d41059", "", ""},
	} {
		if got := epicName(tt.app, tt.exe); got != tt.want {
			t.Errorf("epicName(%q, %q) = %q, want %q", tt.app, tt.exe, got, tt.want)
		}
	}
}

// An Epic org build is named after its cached manifest, without a request
// and whatever the store name switches say.
func TestCatalogEpic(t *testing.T) {
	r, e := catalogEnv(t)
	r.SetLookupClient(nil, "")
	setStoreNames(t, e, false)
	requests := e.cdn.total()
	rocket := "epic:o-98larctxyhn55kqjq5xjb9wzjl9hf9/e6bcca5b0123456789abcdef01234567"
	junk := "epic:o-98larctxyhn55kqjq5xjb9wzjl9hf9/00000000000000000000000000000000"
	none := "epic:o-98larctxyhn55kqjq5xjb9wzjl9hf9/11111111111111111111111111111111"
	r.SetContentLookup(func(_ context.Context, service, groupKey, suffix string, limit int) ([]byte, error) {
		if service != "epicgames" || suffix != ".manifest" || limit != epicMaxManifest {
			t.Errorf("content lookup %q %q %q %d", service, groupKey, suffix, limit)
		}
		switch groupKey {
		case rocket:
			return epicManifest(t, "Sugar", "Binaries/Win64/RocketLeague.exe", true, false), nil
		case junk:
			return []byte("<Error>AccessDenied</Error>"), nil
		}
		return nil, nil
	})
	for _, k := range []string{rocket, junk, none} {
		r.Label(k)
	}
	resolveQueued(t, r)
	if l := r.Label(rocket); l != "Rocket League" || r.IconPath(rocket) != "" || e.cdn.total() != requests {
		t.Fatalf("label %q, icon %q, requests %d", l, r.IconPath(rocket), e.cdn.total()-requests)
	}
	if got := r.SearchLabels("rocket"); !slices.Equal(got, []string{rocket}) {
		t.Fatalf("search = %v", got)
	}
	if got, ok := r.catalog.entries[junk]; !ok || got.name != "" || r.Label(junk) != "Epic item 00000000" {
		t.Fatalf("junk manifest: %+v %v", got, ok)
	}
	if _, stored := r.catalog.entries[none]; stored {
		t.Fatal("a build without a cached manifest was stored")
	}
}
