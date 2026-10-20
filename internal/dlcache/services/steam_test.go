package services

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

const testIconHash = "231b8f624ff26721431fe5749fa5ab5bccd75ff4"

// testJPEG returns a w×h JPEG.
func testJPEG(t *testing.T, w, h int) string {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func infoPath(app int) string { return fmt.Sprintf("/v1/info/%d", app) }

func emptyApp(app int) string { return fmt.Sprintf(`{"data":{"%d":{}},"status":"success"}`, app) }

// steamEnv is a loaded registry with Steam names on and a fake mirror: app
// 2567870 (with a shared depot of app 228980, a DLC depot and its workshop
// depot) and app 228980 (with an icon); every other app does not exist.
func steamEnv(t *testing.T) (*Registry, *testEnv, *[]time.Duration) {
	t.Helper()
	r, e := loadedRegistry(t)
	e.cdn.set(infoPath(2567870), fakeResp{status: http.StatusOK, body: `{"data":{"2567870":{
		"common":{"name":"Chained Together","type":"Game","icon":"` + testIconHash + `"},
		"depots":{
			"229100":{"config":{"oslist":"windows"},"depotfromapp":"228980","sharedinstall":"1"},
			"2567871":{"manifests":{"public":{"gid":"7557936190977295836","size":"8159666216"}}},
			"2567872":{"dlcappid":"4948760"},
			"branches":{"public":{"buildid":"25325926"}},
			"privatebranches":"1",
			"workshopdepot":"2567870"}}},"status":"success"}`})
	e.cdn.set(infoPath(228980), fakeResp{status: http.StatusOK, body: `{"data":{"228980":{
		"common":{"name":"Steamworks Common Redistributables","type":"Tool","icon":"` + testIconHash + `"},
		"depots":{"228990":{},"229003":{},"229100":{}}}},"status":"success"}`})
	e.cdn.set("/steamcommunity/public/images/apps/228980/"+testIconHash+".jpg",
		fakeResp{status: http.StatusOK, body: testJPEG(t, 32, 32)})
	e.cdn.set("/steamcommunity/public/images/apps/2567870/"+testIconHash+".jpg",
		fakeResp{status: http.StatusOK, body: testJPEG(t, 32, 32)})
	for _, app := range []int{1, 2, 3, 2567871, 2567872} {
		e.cdn.set(infoPath(app), fakeResp{status: http.StatusOK, body: emptyApp(app)})
	}
	for app := 999999 - steamWalk; app <= 999999; app++ {
		e.cdn.set(infoPath(app), fakeResp{status: http.StatusOK, body: emptyApp(app)})
	}
	setSteamNames(t, e, true)
	r.SetSteamClient(e.cdn.client(), "PiCache/test")
	var slept []time.Duration
	r.steam.sleep = func(ctx context.Context, d time.Duration) bool {
		slept = append(slept, d)
		return ctx.Err() == nil
	}
	return r, e, &slept
}

func setSteamNames(t *testing.T, e *testEnv, on bool) {
	t.Helper()
	if _, err := e.set.Update(context.Background(), func(a *settings.All) error {
		a.DownloadCache.SteamNames = on
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (e *testEnv) infoRequests() int {
	e.cdn.mu.Lock()
	defer e.cdn.mu.Unlock()
	n := 0
	for p, c := range e.cdn.hits {
		if strings.HasPrefix(p, "/v1/info/") {
			n += c
		}
	}
	return n
}

// The depot's app is found by walking down from the depot; every depot the
// app lists is named without a further request.
func TestSteamNamesResolve(t *testing.T) {
	r, e, slept := steamEnv(t)
	ctx := context.Background()
	key := "steam:depot:2567871"
	if l := r.Label(key); l != "Steam depot 2567871" {
		t.Fatalf("before the lookup: %q", l)
	}
	if !r.steam.queued[2567871] || len(r.steam.queue) != 1 {
		t.Fatal("the unknown depot is not queued")
	}
	r.steam.resolve(ctx, <-r.steam.queue)
	if l := r.Label(key); l != "Chained Together" {
		t.Fatalf("label = %q", l)
	}
	if e.cdn.count(infoPath(2567871)) != 1 || e.cdn.count(infoPath(2567870)) != 1 || e.infoRequests() != 2 {
		t.Fatalf("info requests: %v", e.cdn.hits)
	}
	if p := r.IconPath(key); p != "/cache/icons/steam/2567870" {
		t.Fatalf("icon path = %q", p)
	}
	icon, ok, err := r.SteamIcon(ctx, 2567870)
	if err != nil || !ok || !bytes.HasPrefix(icon, []byte{0xFF, 0xD8}) {
		t.Fatalf("icon: %v %v %d bytes", ok, err, len(icon))
	}
	// One request per steamGap (the test clock stands still while waiting).
	if len(*slept) != 3 {
		t.Fatalf("waits = %v", *slept)
	}
	for i, d := range *slept {
		if want := time.Duration(i) * steamGap; d > want || d < want-100*time.Millisecond {
			t.Fatalf("waits = %v", *slept)
		}
	}

	for k, want := range map[string]string{
		"steam:depot:2567870": "Chained Together · Workshop",
		"steam:depot:2567872": "Chained Together · DLC",
	} {
		if l := r.Label(k); l != want {
			t.Errorf("%s = %q, want %q", k, l, want)
		}
	}
	// A shared depot names its owner, which is asked for its name (not the
	// depot's neighbours).
	if l := r.Label("steam:depot:229100"); l != "Steam depot 229100" {
		t.Fatalf("owner not asked yet: %q", l)
	}
	r.steam.resolve(ctx, <-r.steam.queue)
	if l := r.Label("steam:depot:229100"); l != "Steamworks Common Redistributables" {
		t.Fatalf("shared depot = %q", l)
	}
	if e.cdn.count(infoPath(228980)) != 1 || e.infoRequests() != 3 || r.IconPath("steam:depot:229100") != "/cache/icons/steam/228980" {
		t.Fatalf("info requests %v, icon %q", e.cdn.hits, r.IconPath("steam:depot:229100"))
	}
	if len(r.steam.queue) != 0 {
		t.Fatal("a resolved depot is queued again")
	}

	got := r.SearchLabels("chained")
	if !slices.Equal(got, []string{"steam:depot:2567870", "steam:depot:2567871", "steam:depot:2567872"}) {
		t.Fatalf("search = %v", got)
	}
	// A user label wins over the app name.
	if err := r.SetLabel(ctx, key, "Mine"); err != nil {
		t.Fatal(err)
	}
	if l := r.Label(key); l != "Mine" || slices.Contains(r.SearchLabels("chained"), key) {
		t.Fatalf("user label: %q %v", l, r.SearchLabels("chained"))
	}

	// Stored: a new registry names the depots without a request.
	r2 := e.registry(t)
	if l := r2.Label("steam:depot:2567872"); l != "Chained Together · DLC" || len(r2.steam.queue) != 0 {
		t.Fatalf("after a restart: %q, queued %d", l, len(r2.steam.queue))
	}

	// Off: no names, no icons, nothing queued.
	setSteamNames(t, e, false)
	if l := r.Label("steam:depot:2567872"); l != "Steam depot 2567872" || r.IconPath(key) != "" {
		t.Fatalf("off: %q %q", l, r.IconPath(key))
	}
	if _, ok, _ := r.SteamIcon(ctx, 2567870); ok || r.SearchLabels("chained") != nil {
		t.Fatal("off: icon or search still served")
	}
	r.Label("steam:depot:123")
	if len(r.steam.queue) != 0 {
		t.Fatal("off: a depot was queued")
	}
}

// A depot no app near it lists is stored as not found and not asked again
// within steamNotFound; the walk stops after steamWalk apps.
func TestSteamNamesNotFound(t *testing.T) {
	r, e, _ := steamEnv(t)
	ctx := context.Background()
	r.steam.resolve(ctx, 999999)
	if n := e.infoRequests(); n != steamWalk {
		t.Fatalf("%d requests, want %d", n, steamWalk)
	}
	if l := r.Label("steam:depot:999999"); l != "Steam depot 999999" || len(r.steam.queue) != 0 {
		t.Fatalf("not found: %q, queued %d", l, len(r.steam.queue))
	}
	// A neighbour reuses the apps asked within steamNotFound.
	r.steam.resolve(ctx, 999998)
	if n := e.infoRequests(); n != steamWalk+1 {
		t.Fatalf("%d requests after the neighbour, want %d", n, steamWalk+1)
	}
	// After steamNotFound it is asked again.
	r.steam.now = func() time.Time { return time.Now().Add(steamNotFound + time.Hour) }
	r.Label("steam:depot:999999")
	if len(r.steam.queue) != 1 {
		t.Fatal("a stale depot is not queued")
	}
	// Depots near zero walk only down to 1.
	r.steam.resolve(ctx, 3)
	if n := e.infoRequests(); n != steamWalk+1+3 {
		t.Fatalf("%d requests after depot 3", n)
	}
}

// Answers are untrusted: failures pause the worker, names are cleaned, and
// an icon that is not a small JPEG is dropped.
func TestSteamNamesUntrusted(t *testing.T) {
	r, e, slept := steamEnv(t)
	ctx := context.Background()

	e.cdn.set(infoPath(2567871), fakeResp{status: http.StatusTooManyRequests, body: "slow down"})
	r.steam.resolve(ctx, 2567871)
	if e.infoRequests() != 1 || r.steam.pausedTo.IsZero() {
		t.Fatalf("after a 429: %d requests, paused to %v", e.infoRequests(), r.steam.pausedTo)
	}
	if _, known := r.steam.depots[2567871]; known {
		t.Fatal("a failed lookup was stored as not found")
	}
	*slept = nil
	e.cdn.set(infoPath(2567871), fakeResp{status: http.StatusOK, body: `{"status":"error"}`})
	r.steam.resolve(ctx, 2567871)
	if len(*slept) != 1 || (*slept)[0] < steamPause-time.Minute {
		t.Fatalf("the next request did not wait for the pause: %v", *slept)
	}
	if _, known := r.steam.depots[2567871]; known {
		t.Fatal("an error status was stored")
	}
	e.cdn.set(infoPath(2567871), fakeResp{status: http.StatusOK, body: strings.Repeat(" ", steamMaxBody+1)})
	if err := r.steam.fetchApp(ctx, 2567871); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized answer: %v", err)
	}

	// Control characters and over-long names; a broken icon hash is ignored.
	long := strings.Repeat("é", 100)
	e.cdn.set(infoPath(5), fakeResp{status: http.StatusOK, body: `{"data":{"5":{"common":{"name":"Evil\u001b[31m\nGame ` + long +
		`","icon":"../../x"},"depots":{"6":{}}}},"status":"success"}`})
	if err := r.steam.fetchApp(ctx, 5); err != nil {
		t.Fatal(err)
	}
	a := r.steam.apps[5]
	if strings.ContainsAny(a.name, "\x1b\n") || len(a.name) > steamMaxName || !strings.HasPrefix(a.name, "Evil [31m Game é") || a.iconHash != "" {
		t.Fatalf("app = %+v", a)
	}

	// Not a JPEG, or too large: the hash is cleared, so it is not asked again.
	for i, body := range []string{"<svg/>", testJPEG(t, 300, 20)} {
		app := uint32(2567870)
		r.steam.mu.Lock()
		r.steam.apps[app] = steamApp{name: "Chained Together", iconHash: testIconHash, at: time.Now()}
		r.steam.mu.Unlock()
		e.cdn.set("/steamcommunity/public/images/apps/2567870/"+testIconHash+".jpg", fakeResp{status: http.StatusOK, body: body})
		if err := r.steam.fetchIcon(ctx, app, testIconHash); err != nil {
			t.Fatal(i, err)
		}
		if a := r.steam.apps[app]; a.icon || a.iconHash != "" {
			t.Fatalf("%d: icon kept: %+v", i, a)
		}
	}

	// The daily budget.
	r.steam.dayCount = steamPerDay
	r.steam.dayStart = time.Now()
	if err := r.steam.wait(ctx); err != errSteamBudget {
		t.Fatalf("budget: %v", err)
	}
}

// The Steamworks redistributables keep their built-in label (the component);
// only the app's name and icon are looked up, without a walk, also for a
// depot stored as not found before.
func TestSteamNamesRedists(t *testing.T) {
	r, e, _ := steamEnv(t)
	ctx := context.Background()
	key, want := "steam:depot:229033", "PhysX 9.14.0702 · Steamworks Common Redistributables"
	if err := r.steam.store(ctx, nil, map[uint32]steamDepot{229033: {at: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if l := r.Label(key); l != want || len(r.steam.queue) != 1 {
		t.Fatalf("before the lookup: %q, queued %d", l, len(r.steam.queue))
	}
	r.steam.resolve(ctx, <-r.steam.queue)
	if l := r.Label(key); l != want || r.IconPath(key) != "/cache/icons/steam/228980" {
		t.Fatalf("after the lookup: %q, icon %q", l, r.IconPath(key))
	}
	if e.infoRequests() != 1 || e.cdn.count(infoPath(228980)) != 1 || len(r.steam.queue) != 0 {
		t.Fatalf("info requests %v, queued %d", e.cdn.hits, len(r.steam.queue))
	}
	// The app's other depots cost no request; search finds each component
	// once, also by the app's name.
	if l := r.Label("steam:depot:229003"); l != ".NET Framework 4.0 Client Profile · Steamworks Common Redistributables" ||
		r.IconPath("steam:depot:229003") == "" || len(r.steam.queue) != 0 {
		t.Fatalf("sibling: %q, queued %d", l, len(r.steam.queue))
	}
	// (229100: a depot of the app without a built-in label.)
	keys := r.SearchLabels("steamworks")
	if len(keys) != len(steamRedists)+1 || len(slices.Compact(slices.Clone(keys))) != len(keys) || !slices.Contains(keys, "steam:depot:229100") {
		t.Fatalf("search = %v", keys)
	}
	if keys := r.SearchLabels("physx 9.14"); !slices.Equal(keys, []string{key}) {
		t.Fatalf("search = %v", keys)
	}
	// Off: the built-in label stays, nothing is queued and no icon is shown.
	setSteamNames(t, e, false)
	if l := r.Label("steam:depot:228981"); l != "Visual C++ 2005 · Steamworks Common Redistributables" ||
		r.IconPath(key) != "" || len(r.steam.queue) != 0 {
		t.Fatalf("off: %q %q", l, r.IconPath(key))
	}
}

func TestSteamDepotID(t *testing.T) {
	for key, want := range map[string]uint32{"steam:depot:228990": 228990, "steam:depot:4294967295": 4294967295,
		"steam:depot:4294967296": 0, "steam:depot:0": 0, "steam:depot:-1": 0, "steam:depot:": 0, "epic:x": 0, "steam:depot:12a": 0} {
		if got, ok := steamDepotID(key); got != want || ok != (want != 0) {
			t.Errorf("%s = %d %v", key, got, ok)
		}
	}
}

// The worker resolves what Label queued, off the caller's path.
func TestSteamNamesWorker(t *testing.T) {
	r, _, _ := steamEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.steam.run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(5 * time.Second)
	for r.Label("steam:depot:2567871") != "Chained Together" || r.IconPath("steam:depot:2567871") == "" {
		if time.Now().After(deadline) {
			t.Fatalf("not resolved: %q", r.Label("steam:depot:2567871"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}
