package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/parental"
	"github.com/hustenreizjuengling/picache/internal/logs"
)

// newProtectionServer is the filter test server with the parental engine
// and the group Kids (2).
func newProtectionServer(t *testing.T) (*Server, *filter.Engine, *parental.Engine) {
	t.Helper()
	s, eng, p, _ := newProtectionServerDB(t)
	return s, eng, p
}

// newProtectionServerDB is newProtectionServer that also returns the database.
func newProtectionServerDB(t *testing.T) (*Server, *filter.Engine, *parental.Engine, *db.DB) {
	t.Helper()
	s, eng, d := newFilterTestServerDB(t)
	ctx := context.Background()
	if _, err := s.d.Clients.CreateGroup(ctx, clients.GroupInput{Name: "Kids", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	p, err := parental.New(ctx, d, s.d.Clients, s.log)
	if err != nil {
		t.Fatal(err)
	}
	s.d.Parental = p
	return s, eng, p, d
}

// listsState is the part of the lists a failed parental PUT must not change.
func listsState(t *testing.T, eng *filter.Engine) string {
	t.Helper()
	lists, err := eng.Lists(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, l := range lists {
		fmt.Fprintf(&b, "%d %s %s %v %v;", l.ID, l.Kind, l.Category, l.Enabled, l.GroupIDs)
	}
	return b.String()
}

// auditActions returns the audit actions of s, newest first.
func auditActions(t *testing.T, s *Server, search string) []auth.AuditEntry {
	t.Helper()
	entries, _, err := s.d.Auth.AuditLog(context.Background(), auth.AuditQuery{Search: search, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// The PUT body has four members; safeSearch and categories are optional,
// member by member; category switches are list assignments.
func TestParentalUpdateCategories(t *testing.T) {
	s, eng, _ := newProtectionServer(t)
	path := "/api/v1/parental/groups/2"
	var gc parental.GroupControls

	w := callFilter(s, s.parentalUpdate, "PUT", path, "2", `{"blockedServices":["tiktok"],"schedules":[],`+
		`"safeSearch":{"google":true,"youtube":"strict","bing":null},"categories":{"adult":true,"gambling":true,"dating":null}}`)
	wantStatus(t, "four members", w, http.StatusOK, "")
	filterDecode(t, w, &gc)
	if !gc.SafeSearch.Google || gc.SafeSearch.YouTube != "strict" || gc.SafeSearch.Bing || !gc.Categories.Adult.On ||
		gc.Categories.Adult.State != "pending" || !gc.Categories.Gambling.On || gc.Categories.Dating.On || gc.Categories.Bypass.State != "off" {
		t.Fatalf("controls %s", w.Body)
	}
	lists, err := eng.Lists(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var created []filter.List
	for _, l := range lists {
		if l.CatalogKey == "oisd-nsfw" || l.CatalogKey == "hagezi-gambling-medium" {
			created = append(created, l)
			if !slices.Equal(l.GroupIDs, []int64{2}) || !l.Enabled || !filter.IsProtection(l.Category) {
				t.Errorf("created list %+v", l)
			}
		}
	}
	if len(created) != 2 {
		t.Fatalf("created lists %+v", lists)
	}
	var actions []string
	for _, a := range auditActions(t, s, "") {
		actions = append(actions, a.Action)
	}
	if got := strings.Join(actions, ","); got != "parental.update,filter.list.create,filter.list.create" {
		t.Fatalf("audit %s", got)
	}
	upd := auditActions(t, s, "parental.update")[0]
	if !strings.Contains(upd.Details, `"categories":{"adult":true,"bypass":false,"dating":false,"gambling":true,"piracy":false}`) ||
		!strings.Contains(upd.Details, `"safeSearch":{"google":true,"youtube":"strict"`) || !strings.Contains(upd.Details, `"blockedServices":["tiktok"]`) {
		t.Errorf("audit details %s", upd.Details)
	}

	// A v0.9-shaped body keeps safe search and the switches.
	w = callFilter(s, s.parentalUpdate, "PUT", path, "2", `{"blockedServices":["youtube"],"schedules":[]}`)
	filterDecode(t, w, &gc)
	if w.Code != http.StatusOK || !gc.SafeSearch.Google || !gc.Categories.Adult.On || !gc.Categories.Gambling.On ||
		!slices.Equal(gc.BlockedServices, []string{"youtube"}) {
		t.Fatalf("v0.9 body %d %s", w.Code, w.Body)
	}
	// One member at a time: adult off, gambling kept; the list stays.
	w = callFilter(s, s.parentalUpdate, "PUT", path, "2", `{"blockedServices":[],"schedules":[],"categories":{"adult":false}}`)
	filterDecode(t, w, &gc)
	if w.Code != http.StatusOK || gc.Categories.Adult.On || gc.Categories.Adult.State != "off" || !gc.Categories.Gambling.On {
		t.Fatalf("adult off %d %s", w.Code, w.Body)
	}
	if lists, _ := eng.Lists(context.Background()); len(lists) != 3 {
		t.Errorf("switching off keeps the list: %d lists", len(lists))
	}
	if a := auditActions(t, s, "filter.list.update"); len(a) != 1 || !strings.Contains(a[0].Details, `"groupIds":[]`) {
		t.Errorf("list update audit %+v", a)
	}
	// Read routes fill the switches too.
	w = callFilter(s, s.parentalGroups, "GET", "/api/v1/parental/groups", "", "")
	if !strings.Contains(w.Body.String(), `"gambling":{"on":true,"state":"pending"}`) {
		t.Errorf("list %s", w.Body)
	}
	w = callFilter(s, s.parentalGroup, "GET", path, "2", "")
	if !strings.Contains(w.Body.String(), `"gambling":{"on":true,"state":"pending"}`) {
		t.Errorf("one %s", w.Body)
	}

	for _, tc := range []struct{ body, field string }{
		{`{"blockedServices":[],"schedules":[],"safeSearch":{"youtube":"loud"}}`, "safeSearch.youtube"},
		{`{"blockedServices":[],"schedules":[],"safeSearch":{"altavista":true}}`, "body"},
		{`{"blockedServices":[],"schedules":[],"categories":{"sports":true}}`, "body"},
		{`{"blockedServices":["myspace"],"schedules":[],"categories":{"piracy":true}}`, "blockedServices"},
	} {
		wantStatus(t, tc.body, callFilter(s, s.parentalUpdate, "PUT", path, "2", tc.body), http.StatusBadRequest, tc.field)
	}
	if st := eng.Presets(2)["piracy"]; st.On {
		t.Error("a rejected body must not switch anything")
	}
	wantStatus(t, "unknown group", callFilter(s, s.parentalUpdate, "PUT", "/api/v1/parental/groups/9", "9",
		`{"blockedServices":[],"schedules":[],"categories":{"adult":true}}`), http.StatusNotFound, "")
}

// An allowlist at a bound URL and the list limit: 409, nothing changed (no
// list, no parental setting, no audit entry).
func TestParentalUpdateCategoryConflicts(t *testing.T) {
	s, eng, p := newProtectionServer(t)
	ctx := context.Background()
	path, body := "/api/v1/parental/groups/2", `{"blockedServices":[],"schedules":[],"safeSearch":{"bing":true},"categories":{"adult":true,"piracy":true}}`

	// The piracy switch's list, turned into an allowlist.
	piracy, err := eng.CreateList(ctx, filter.ListInput{Name: "Piracy fixes", URL: "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/anti.piracy.txt",
		Enabled: true, GroupIDs: []int64{1}})
	if err != nil || piracy.CatalogKey != "hagezi-anti-piracy" {
		t.Fatalf("piracy list %+v %v", piracy, err)
	}
	if _, err := eng.UpdateList(ctx, piracy.ID, filter.ListInput{Name: piracy.Name, URL: piracy.URL, Kind: "allow", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	before := listsState(t, eng)
	w := callFilter(s, s.parentalUpdate, "PUT", path, "2", body)
	wantStatus(t, "allowlist", w, http.StatusConflict, "")
	if !strings.Contains(w.Body.String(), "Piracy fixes") || !strings.Contains(w.Body.String(), "allowlist") {
		t.Errorf("message %s", w.Body)
	}
	if got := listsState(t, eng); got != before {
		t.Errorf("lists changed on a 409:\n%s\n%s", before, got)
	}
	if gc, _ := p.Get(ctx, 2); gc.SafeSearch.Bing {
		t.Error("the parental configuration must not change on a 409")
	}
	if a := auditActions(t, s, ""); len(a) != 0 {
		t.Errorf("audit on a 409: %+v", a)
	}

	for i := range 98 { // + the default list and the piracy list = 100
		if _, err := eng.CreateList(ctx, filter.ListInput{URL: fmt.Sprintf("https://lists.example/%d.txt", i)}); err != nil {
			t.Fatal(err)
		}
	}
	before = listsState(t, eng)
	w = callFilter(s, s.parentalUpdate, "PUT", path, "2",
		`{"blockedServices":[],"schedules":[],"safeSearch":{"bing":true},"categories":{"bypass":true,"dating":true}}`)
	wantStatus(t, "list limit", w, http.StatusConflict, "")
	if !strings.Contains(w.Body.String(), "at most 100 lists") {
		t.Errorf("message %s", w.Body)
	}
	if got := listsState(t, eng); got != before {
		t.Errorf("lists changed on a 409:\n%s\n%s", before, got)
	}
	if gc, _ := p.Get(ctx, 2); gc.SafeSearch.Bing {
		t.Error("the parental configuration must not change on a 409")
	}
	if a := auditActions(t, s, ""); len(a) != 0 {
		t.Errorf("audit on a 409: %+v", a)
	}
}

// When the parental configuration cannot be saved after the switches were
// applied, the list changes are undone exactly and both steps are audited.
func TestParentalUpdateRevert(t *testing.T) {
	s, eng, p, d := newProtectionServerDB(t)
	ctx := context.Background()
	if _, err := s.d.Clients.CreateGroup(ctx, clients.GroupInput{Name: "Teens", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// The gambling list exists, disabled on purpose, for Teens (3).
	gambling, err := eng.CreateList(ctx, filter.ListInput{URL: "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/gambling.medium.txt",
		Enabled: false, GroupIDs: []int64{3}})
	if err != nil {
		t.Fatal(err)
	}
	before := listsState(t, eng)
	// Saving the parental configuration fails.
	if _, err := d.W.Exec(`CREATE TRIGGER fail_parental BEFORE INSERT ON parental_groups BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	w := callFilter(s, s.parentalUpdate, "PUT", "/api/v1/parental/groups/2", "2",
		`{"blockedServices":[],"schedules":[],"safeSearch":{"google":true},"categories":{"gambling":true,"piracy":true}}`)
	wantStatus(t, "save fails", w, http.StatusInternalServerError, "")
	if got := listsState(t, eng); got != before {
		t.Errorf("the undo must restore the lists exactly:\n%s\n%s", before, got)
	}
	if st := eng.Presets(2); st["gambling"].On || st["piracy"].On {
		t.Errorf("switches after the undo %+v", st)
	}
	if gc, _ := p.Get(ctx, 2); gc.SafeSearch.Google {
		t.Error("safe search must not be saved")
	}
	var actions []string
	for _, a := range auditActions(t, s, "filter.list.") {
		actions = append(actions, a.Action+" "+a.Target)
	}
	g := fmt.Sprint(gambling.ID)
	created := fmt.Sprint(gambling.ID + 1)
	want := "filter.list.update " + g + ",filter.list.delete " + created + ",filter.list.create " + created + ",filter.list.update " + g
	if got := strings.Join(actions, ","); got != want {
		t.Errorf("audit %s, want %s", got, want)
	}
	if a := auditActions(t, s, "parental.update"); len(a) != 0 {
		t.Errorf("a failed save is not audited as parental.update: %+v", a)
	}
}

func TestParentalPauseRoutes(t *testing.T) {
	s, _, _ := newProtectionServer(t)
	path := "/api/v1/parental/groups/2/pause"
	var gc parental.GroupControls
	w := callFilter(s, s.parentalPause, "PUT", path, "2", `{"minutes":30}`)
	filterDecode(t, w, &gc)
	if w.Code != http.StatusOK || !gc.State.Paused || time.Until(gc.State.PausedUntil) < 29*time.Minute || gc.Categories.Adult.State != "off" {
		t.Fatalf("pause %d %s", w.Code, w.Body)
	}
	until := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	w = callFilter(s, s.parentalPause, "PUT", path, "2", `{"until":"`+until.Format(time.RFC3339)+`"}`)
	filterDecode(t, w, &gc)
	if w.Code != http.StatusOK || !gc.State.PausedUntil.Equal(until) {
		t.Fatalf("until %d %s", w.Code, w.Body)
	}
	for _, tc := range []struct{ body, field string }{
		{`{}`, "minutes"},
		{`{"minutes":0}`, "minutes"},
		{`{"minutes":10081}`, "minutes"},
		{`{"minutes":5,"until":"` + until.Format(time.RFC3339) + `"}`, "minutes"},
		{`{"until":"2000-01-01T00:00:00Z"}`, "pause.until"},
		{`{"until":"` + time.Now().Add(8*24*time.Hour).UTC().Format(time.RFC3339) + `"}`, "pause.until"},
		{`{"mode":"block","minutes":5}`, "body"},
	} {
		wantStatus(t, tc.body, callFilter(s, s.parentalPause, "PUT", path, "2", tc.body), http.StatusBadRequest, tc.field)
	}
	wantStatus(t, "unknown group", callFilter(s, s.parentalPause, "PUT", "/api/v1/parental/groups/9/pause", "9", `{"minutes":5}`),
		http.StatusNotFound, "")
	wantStatus(t, "unknown group", callFilter(s, s.parentalPauseClear, "DELETE", "/api/v1/parental/groups/9/pause", "9", ""),
		http.StatusNotFound, "")
	w = callFilter(s, s.parentalPauseClear, "DELETE", path, "2", "")
	filterDecode(t, w, &gc)
	if w.Code != http.StatusOK || gc.State.Paused || strings.Contains(w.Body.String(), "pausedUntil") {
		t.Fatalf("clear %d %s", w.Code, w.Body)
	}
	if w := callFilter(s, s.parentalPauseClear, "DELETE", path, "2", ""); w.Code != http.StatusOK {
		t.Fatalf("clear without a pause %d", w.Code)
	}
	var actions []string
	for _, a := range auditActions(t, s, "parental.") {
		actions = append(actions, a.Action)
		if a.Target != "2" {
			t.Errorf("target %+v", a)
		}
	}
	if got := strings.Join(actions, ","); got != "parental.pause_clear,parental.pause_clear,parental.pause,parental.pause" {
		t.Fatalf("audit %s", got)
	}
	if d := auditActions(t, s, "parental.pause")[2].Details; !strings.Contains(d, `"until":"`+until.Format(time.RFC3339)) {
		t.Errorf("pause audit details %s", d)
	}
}

func TestFilterListCategoryRoutes(t *testing.T) {
	s, _ := newFilterTestServer(t)
	w := callFilter(s, s.filterCreateList, "POST", "/api/v1/filter/lists", "",
		`{"url":"https://lists.example/kids.txt","kind":"block","plainDomains":"exact","category":"gambling","enabled":true,"groupIds":[1],"comment":""}`)
	wantStatus(t, "create", w, http.StatusCreated, "")
	var l filter.List
	filterDecode(t, w, &l)
	if l.Category != "gambling" || l.CatalogKey != "" || !strings.Contains(w.Body.String(), `"category":"gambling","catalogKey":""`) {
		t.Fatalf("created %s", w.Body)
	}
	for _, tc := range []struct{ body, field string }{
		{`{"url":"https://lists.example/x.txt","category":"sports"}`, "category"},
		{`{"url":"https://lists.example/x.txt","kind":"allow","category":"adult"}`, "category"},
		{`{"url":"https://nsfw.oisd.nl/","kind":"allow"}`, "kind"},
	} {
		wantStatus(t, tc.body, callFilter(s, s.filterCreateList, "POST", "/api/v1/filter/lists", "", tc.body), http.StatusBadRequest, tc.field)
	}
	// An old client that omits the category changes nothing.
	w = callFilter(s, s.filterUpdateList, "PUT", "/api/v1/filter/lists/x", fmt.Sprint(l.ID),
		`{"name":"Renamed","url":"https://lists.example/kids.txt","kind":"block","plainDomains":"exact","enabled":true,"groupIds":[1],"comment":""}`)
	filterDecode(t, w, &l)
	if w.Code != http.StatusOK || l.Category != "gambling" || l.Name != "Renamed" {
		t.Fatalf("update %d %s", w.Code, w.Body)
	}
	w = callFilter(s, s.filterCatalog, "GET", "/api/v1/filter/catalog", "", "")
	var cat []filter.CatalogEntry
	filterDecode(t, w, &cat)
	if len(cat) < 50 || cat[0].Key != "hagezi-multi" {
		t.Fatalf("catalog %d entries", len(cat))
	}
	for _, member := range []string{`"key":`, `"name":`, `"description":`, `"descriptionDe":`, `"url":`, `"kind":"block"`,
		`"category":`, `"plainDomains":`, `"recommended":`, `"entries":`, `"maintainer":`, `"license":`, `"homepage":`, `"kind":"allow"`} {
		if !strings.Contains(w.Body.String(), member) {
			t.Errorf("catalog lacks %s", member)
		}
	}
}

func TestStatsPurposesRoute(t *testing.T) {
	s, st, closeDB := logsTestServer(t, t.TempDir())
	defer closeDB()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { st.Start(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	now := time.Now()
	for i, p := range []string{"adult", "adult", "safesearch", ""} {
		st.LogQuery(logs.QueryEvent{Time: now, ClientIP: "10.0.0.1", QName: fmt.Sprint(i, ".example"), QType: "A", Status: "blocked-list",
			RCode: "NOERROR", Protocol: "udp", Purpose: p})
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		w := logsGet(s, s.logsPurposes, "/api/v1/stats/purposes?range=1h")
		if w.Code != http.StatusOK {
			t.Fatalf("purposes %d %s", w.Code, w.Body)
		}
		ps := logsDecode[logs.PurposeStats](t, w)
		if len(ps.Purposes) == 2 {
			if ps.Purposes[0] != (logs.PurposeCount{Purpose: "adult", Count: 2}) || ps.Purposes[1].Purpose != "safesearch" ||
				!ps.From.Equal(logs.TopFrom(now.Add(-time.Hour), now)) && !ps.From.Equal(logs.TopFrom(time.Now().Add(-time.Hour), time.Now())) {
				t.Fatalf("purposes %s", w.Body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("purposes not counted: %s", w.Body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if w := logsGet(s, s.logsPurposes, "/api/v1/stats/purposes"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"from":`) {
		t.Fatalf("default range %d %s", w.Code, w.Body)
	}
	w := logsGet(s, s.logsPurposes, "/api/v1/stats/purposes?range=forever")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"field":"range"`) {
		t.Fatalf("bad range %d %s", w.Code, w.Body)
	}
}
