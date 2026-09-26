package parental

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/clients"
)

// The safe search table: exact lower-case A-label names, no name in two
// engines (init panics), every engine present, targets are host names
// outside the table.
func TestSafeSearchTable(t *testing.T) {
	perEngine := map[searchEngine]int{}
	for name, e := range safeSearchTable {
		if !validDomain(name) || strings.ToLower(name) != name || !strings.Contains(name, ".") {
			t.Errorf("%q is not a lower-case A-label name", name)
		}
		perEngine[e]++
	}
	for e := range numEngines {
		if perEngine[e] == 0 {
			t.Errorf("engine %d has no name", e)
		}
	}
	if perEngine[engineGoogle] != 2*len(googleDomains) || len(googleDomains) != 187 {
		t.Errorf("google: %d names for %d domains", perEngine[engineGoogle], len(googleDomains))
	}
	targets := []string{targetGoogle, targetYouTubeMod, targetYouTubeStrict, targetBing, targetDuckDuckGo, targetEcosia,
		targetYandex, targetPixabay}
	for _, target := range targets {
		if _, ok := safeSearchTable[target]; ok || !validDomain(target) || !strings.Contains(target, ".") {
			t.Errorf("target %q must be a host name outside the table (no loops)", target)
		}
	}
	for e := range numEngines {
		if e != engineYouTube && (engineTargets[e] == "" || engineLabels[e] == "") {
			t.Errorf("engine %d lacks a target or label", e)
		}
	}
	// Exact names only: no suffix walk.
	for _, name := range []string{"mail.google.com", "google", "images.google.de", "music.youtube.com", "bing.com",
		"www.google.com.evil.example", "xn--google.com"} {
		if _, ok := safeSearchTable[name]; ok {
			t.Errorf("%q must not be rewritten", name)
		}
	}
	for _, name := range []string{"google.com", "www.google.de", "www.google.co.uk", "google.cat", "www.youtube.com",
		"youtubei.googleapis.com", "www.bing.com", "duckduckgo.com", "start.duckduckgo.com", "www.ecosia.org",
		"ya.ru", "www.yandex.com.tr", "pixabay.com"} {
		if _, ok := safeSearchTable[name]; !ok {
			t.Errorf("%q must be rewritten", name)
		}
	}
}

// Safe search takes the union of the client's groups; YouTube strict beats
// moderate, reported for the first group that enables the chosen level.
func TestSafeSearchUnion(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	kids := env.group("Kids")
	teens := env.group("Teens")
	strict, moderate, on := YouTubeStrict, YouTubeModerate, true
	if _, err := env.e.Update(ctx, teens, UpdateInput{SafeSearch: &SafeSearchInput{YouTube: &moderate, Bing: &on}}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Update(ctx, kids, UpdateInput{SafeSearch: &SafeSearchInput{YouTube: &strict, Google: &on}}); err != nil {
		t.Fatal(err)
	}
	third := env.group("Third")
	if _, err := env.e.Update(ctx, third, UpdateInput{SafeSearch: &SafeSearchInput{YouTube: &strict}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		groups        []int64
		target, label string
		group         string
	}{
		{"www.youtube.com", []int64{teens}, targetYouTubeMod, labelYouTubeModerate, "Teens"},
		{"m.youtube.com", []int64{teens, kids}, targetYouTubeStrict, labelYouTubeStrict, "Kids"},
		{"m.youtube.com", []int64{teens, third, kids}, targetYouTubeStrict, labelYouTubeStrict, "Third"},
		{"www.google.de", []int64{teens, kids}, targetGoogle, "Google safe search", "Kids"},
		{"www.bing.com", []int64{kids, teens}, targetBing, "Bing safe search", "Teens"},
		{"www.bing.com", []int64{kids}, "", "", ""},
		{"duckduckgo.com", []int64{kids, teens}, "", "", ""},
		{"www.google.de", []int64{clients.DefaultGroupID}, "", "", ""},
		{"example.com", []int64{kids}, "", "", ""},
	} {
		rw, ok := env.e.SafeSearch(tc.name, tc.groups, env.now)
		if ok != (tc.target != "") || rw.Target != tc.target || rw.Label != tc.label || rw.Group != tc.group {
			t.Errorf("%s %v: %+v %v, want %s %q %s", tc.name, tc.groups, rw, ok, tc.target, tc.label, tc.group)
		}
	}
	rw, _ := env.e.SafeSearch("www.youtube.com", []int64{kids}, env.now)
	if rw.Reason() != "Kids: YouTube restricted mode (strict)" || rw.GroupID != kids {
		t.Errorf("reason %q", rw.Reason())
	}
	// Neither the allow override nor a pause lifts it.
	env.override(kids, OverrideAllow, 60)
	minutes := 30
	if _, err := env.e.SetPause(ctx, kids, PauseInput{Minutes: &minutes}); err != nil {
		t.Fatal(err)
	}
	if _, ok := env.e.SafeSearch("google.com", []int64{kids}, env.now); !ok {
		t.Error("safe search must survive an allow override and a pause")
	}
	// Hot path: names outside the table cost one map lookup, nothing allocates.
	for _, name := range []string{"www.example.com", "www.youtube.com", "www.google.de"} {
		if n := testing.AllocsPerRun(200, func() { env.e.SafeSearch(name, []int64{teens, kids}, env.now) }); n != 0 {
			t.Errorf("SafeSearch(%s) allocates %v times", name, n)
		}
	}
}

// A PUT without safeSearch keeps it; members change one by one; the stored
// JSON of an older version (without safeSearch) loads as all off.
func TestSafeSearchUpdate(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	kids := env.group("Kids")
	on, strict := true, YouTubeStrict
	gc, err := env.e.Update(ctx, kids, UpdateInput{BlockedServices: []string{"tiktok"},
		SafeSearch: &SafeSearchInput{Google: &on, YouTube: &strict}})
	if err != nil || !gc.SafeSearch.Google || gc.SafeSearch.YouTube != YouTubeStrict || gc.SafeSearch.Bing {
		t.Fatalf("set: %+v %v", gc.SafeSearch, err)
	}
	gc = env.update(kids, Config{BlockedServices: []string{"roblox"}}) // a v0.9-shaped body
	if !gc.SafeSearch.Google || gc.SafeSearch.YouTube != YouTubeStrict || !slices.Equal(gc.BlockedServices, []string{"roblox"}) {
		t.Fatalf("a body without safeSearch must keep it: %+v", gc)
	}
	off := false
	gc, err = env.e.Update(ctx, kids, UpdateInput{SafeSearch: &SafeSearchInput{Google: &off, Pixabay: &on}})
	if err != nil || gc.SafeSearch.Google || !gc.SafeSearch.Pixabay || gc.SafeSearch.YouTube != YouTubeStrict {
		t.Fatalf("member by member: %+v %v", gc.SafeSearch, err)
	}
	bad := "sometimes"
	_, err = env.e.Update(ctx, kids, UpdateInput{SafeSearch: &SafeSearchInput{YouTube: &bad}})
	wantField(t, err, "safeSearch.youtube")
	if err := ValidateUpdate(UpdateInput{SafeSearch: &SafeSearchInput{YouTube: &bad}}); err == nil {
		t.Error("ValidateUpdate must reject the level")
	}
	if err := ValidateUpdate(UpdateInput{BlockedServices: []string{"nope"}}); err == nil {
		t.Error("ValidateUpdate must reject unknown services")
	}
	// A configuration of v0.9 and a stored unknown level load as off.
	for _, stored := range []string{`{"blockedServices":["youtube"],"schedules":[]}`,
		`{"blockedServices":[],"schedules":[],"safeSearch":{"youtube":"extreme","bing":true}}`} {
		if _, err := env.db.W.Exec(`UPDATE parental_groups SET config = ? WHERE group_id = ?`, stored, kids); err != nil {
			t.Fatal(err)
		}
		gc, err := env.e.Get(ctx, kids)
		if err != nil || gc.SafeSearch.YouTube != YouTubeOff || gc.SafeSearch.Google {
			t.Fatalf("stored %s: %+v %v", stored, gc.SafeSearch, err)
		}
	}
	// A group without a row: everything off, categories off.
	gc, err = env.e.Get(ctx, clients.DefaultGroupID)
	if err != nil || gc.SafeSearch != (SafeSearch{YouTube: YouTubeOff}) || gc.Categories.Adult.State != "off" || gc.Categories.Bypass.On {
		t.Fatalf("default group: %+v %+v %v", gc.SafeSearch, gc.Categories, err)
	}
}

func TestCategoriesInput(t *testing.T) {
	var nilInput *CategoriesInput
	if len(nilInput.Want()) != 0 {
		t.Error("nil input wants nothing")
	}
	on, off := true, false
	want := (&CategoriesInput{Adult: &on, Bypass: &off}).Want()
	if len(want) != 2 || !want["adult"] || want["bypass"] {
		t.Errorf("want %v", want)
	}
	var c Categories
	for _, name := range CategorySwitchNames {
		if c.Switch(name) == nil {
			t.Errorf("switch %s missing", name)
		}
	}
	if c.Switch("sports") != nil {
		t.Error("unknown switch")
	}
}

// The pause of a group's filtering: validation, expiry, FilterGroups, and
// coexistence with a block override.
func TestPause(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	kids := env.group("Kids")
	teens := env.group("Teens")
	minutes := func(n int) *int { return &n }
	until := func(d time.Duration) *time.Time { t := env.now.Add(d); return &t }
	for _, tc := range []struct {
		in    PauseInput
		field string
	}{
		{PauseInput{}, "minutes"},
		{PauseInput{Minutes: minutes(0)}, "minutes"},
		{PauseInput{Minutes: minutes(10081)}, "minutes"},
		{PauseInput{Minutes: minutes(5), Until: until(time.Hour)}, "minutes"},
		{PauseInput{Until: until(-time.Minute)}, "pause.until"},
		{PauseInput{Until: until(7*24*time.Hour + 2*time.Minute)}, "pause.until"},
	} {
		_, err := env.e.SetPause(ctx, kids, tc.in)
		wantField(t, err, tc.field)
	}
	if _, err := env.e.SetPause(ctx, 999, PauseInput{Minutes: minutes(5)}); err == nil {
		t.Fatal("unknown group")
	}
	if _, err := env.e.ClearPause(ctx, 999); err == nil {
		t.Fatal("unknown group")
	}

	groups := []int64{clients.DefaultGroupID, kids, teens}
	if got := env.e.FilterGroups(groups, env.now); &got[0] != &groups[0] {
		t.Error("without pauses FilterGroups returns its input")
	}
	if n := testing.AllocsPerRun(200, func() { env.e.FilterGroups(groups, env.now) }); n != 0 {
		t.Errorf("FilterGroups allocates %v times without pauses", n)
	}

	// Pause and a block override at the same time: both are in force.
	env.override(kids, OverrideBlock, 120)
	gc, err := env.e.SetPause(ctx, kids, PauseInput{Minutes: minutes(60)})
	if err != nil || !gc.State.Paused || !gc.State.PausedUntil.Equal(env.now.Add(time.Hour)) || !gc.State.BlockAll ||
		gc.Override == nil {
		t.Fatalf("pause with override: %+v %v", gc.State, err)
	}
	if got := env.e.FilterGroups(groups, env.now); !slices.Equal(got, []int64{clients.DefaultGroupID, teens}) {
		t.Errorf("filter groups %v", got)
	}
	if got := env.e.FilterGroups([]int64{teens}, env.now); !slices.Equal(got, []int64{teens}) {
		t.Errorf("unpaused groups only: %v", got)
	}
	if d := env.e.Check("example.com", []int64{kids}, env.now); !d.Blocked || d.Kind != KindOverride {
		t.Errorf("the pause must not lift the block override: %+v", d)
	}
	if p := env.e.PausedGroups(groups, env.now); len(p) != 1 || p[0].Group != "Kids" || !p[0].Until.Equal(env.now.Add(time.Hour)) {
		t.Errorf("paused groups %+v", p)
	}
	// until replaces the pause; a pause survives a config update.
	gc, err = env.e.SetPause(ctx, kids, PauseInput{Until: until(3 * time.Hour)})
	if err != nil || !gc.State.PausedUntil.Equal(env.now.Add(3*time.Hour)) {
		t.Fatalf("replace: %+v %v", gc.State, err)
	}
	if gc = env.update(kids, Config{BlockedServices: []string{"youtube"}}); !gc.State.Paused {
		t.Fatal("an update must keep the pause")
	}
	// Expiry: ignored at once, removed with the next write.
	env.now = env.now.Add(3*time.Hour + time.Second)
	if got := env.e.FilterGroups(groups, env.now); len(got) != 3 {
		t.Errorf("elapsed pause still applies: %v", got)
	}
	if gc, _ := env.e.Get(ctx, kids); gc.State.Paused {
		t.Error("elapsed pause shown")
	}
	env.update(teens, Config{})
	var pause sql.NullInt64
	if err := env.db.R.QueryRow(`SELECT pause_until FROM parental_groups WHERE group_id = ?`, kids).Scan(&pause); err != nil || pause.Valid {
		t.Fatalf("elapsed pause not removed: %v %v", pause, err)
	}
	// Clear.
	if _, err := env.e.SetPause(ctx, teens, PauseInput{Minutes: minutes(10080)}); err != nil {
		t.Fatal(err)
	}
	if gc, err := env.e.ClearPause(ctx, teens); err != nil || gc.State.Paused {
		t.Fatalf("clear: %+v %v", gc.State, err)
	}
	if gc, err := env.e.ClearPause(ctx, teens); err != nil || gc.State.Paused {
		t.Fatalf("clear without pause: %+v %v", gc.State, err)
	}
}
