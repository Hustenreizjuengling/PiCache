package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// noNetwork fails every request: filter route tests never download.
type noNetwork struct{}

func (noNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network disabled in tests")
}

// newFilterTestServer wires a real filter engine, client registry and auth
// service (for audits) on a temp database; handlers are called directly.
func newFilterTestServer(t *testing.T) (*Server, *filter.Engine) {
	t.Helper()
	s, eng, _ := newFilterTestServerDB(t)
	return s, eng
}

// newFilterTestServerDB is newFilterTestServer that also returns the database.
func newFilterTestServerDB(t *testing.T) (*Server, *filter.Engine, *db.DB) {
	t.Helper()
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "picache.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	set, err := settings.Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := clients.New(ctx, d, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := filter.New(ctx, d, set, &http.Client{Transport: noNetwork{}}, filepath.Join(dir, "lists"), log)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	as, err := auth.New(ctx, d, set, box, filepath.Join(dir, "setup-token"), log)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{d: Deps{Settings: set, Auth: as, Filter: eng, Clients: reg, Log: log}, log: log}, eng, d
}

// callFilter runs h like s.route does after authentication (admin
// principal in the context). id, if non-empty, is the {id} path value.
func callFilter(s *Server, h handlerFunc, method, target, id, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.10:40000"
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if id != "" {
		r.SetPathValue("id", id)
	}
	p := &auth.Principal{UserID: 1, Username: "admin", Scope: auth.ScopeAdmin}
	r = r.WithContext(context.WithValue(r.Context(), principalKey, p))
	w := httptest.NewRecorder()
	if err := h(w, r); err != nil {
		writeError(w, r, s.log, err)
	}
	return w
}

func filterDecode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
}

func wantStatus(t *testing.T, what string, w *httptest.ResponseRecorder, status int, field string) {
	t.Helper()
	if w.Code != status {
		t.Errorf("%s: status %d, want %d: %s", what, w.Code, status, w.Body)
		return
	}
	if field != "" && !strings.Contains(w.Body.String(), `"field":"`+field+`"`) {
		t.Errorf("%s: want field %q: %s", what, field, w.Body)
	}
}

func TestFilterRoutesReadOnly(t *testing.T) {
	s, _ := newFilterTestServer(t)
	w := callFilter(s, s.filterLists, "GET", "/api/v1/filter/lists", "", "")
	wantStatus(t, "lists", w, http.StatusOK, "")
	var lists []filter.List
	filterDecode(t, w, &lists)
	if len(lists) != 1 || lists[0].Status != "pending" || !slices.Equal(lists[0].GroupIDs, []int64{1}) {
		t.Errorf("default list: %+v", lists)
	}

	w = callFilter(s, s.filterCatalog, "GET", "/api/v1/filter/catalog", "", "")
	wantStatus(t, "catalog", w, http.StatusOK, "")
	var cat []filter.CatalogEntry
	filterDecode(t, w, &cat)
	if len(cat) == 0 || cat[0].URL != lists[0].URL {
		t.Errorf("catalog: %+v", cat)
	}

	w = callFilter(s, s.filterStats, "GET", "/api/v1/filter/stats", "", "")
	wantStatus(t, "stats", w, http.StatusOK, "")
	for _, member := range []string{`"lists":`, `"entries":`, `"failedLists":`, `"staleLists":`, `"memoryBytes":`} {
		if !strings.Contains(w.Body.String(), member) {
			t.Errorf("stats lack %s: %s", member, w.Body)
		}
	}
}

func TestFilterRoutesListCRUD(t *testing.T) {
	s, _ := newFilterTestServer(t)
	for _, c := range []struct {
		name, body, field string
	}{
		{"plain http to a public host", `{"url":"http://lists.example/a.txt","enabled":true}`, "url"},
		{"unknown member", `{"url":"https://lists.example/a.txt","bogus":1}`, "body"},
		{"bad kind", `{"url":"https://lists.example/a.txt","kind":"deny"}`, "kind"},
		{"unknown group", `{"url":"https://lists.example/a.txt","groupIds":[77]}`, "groupIds"},
	} {
		w := callFilter(s, s.filterCreateList, "POST", "/api/v1/filter/lists", "", c.body)
		wantStatus(t, c.name, w, http.StatusBadRequest, c.field)
	}

	w := callFilter(s, s.filterCreateList, "POST", "/api/v1/filter/lists", "",
		`{"name":"Mine","url":"https://lists.example/a.txt?key=secret","enabled":true,"groupIds":[1]}`)
	wantStatus(t, "create", w, http.StatusCreated, "")
	var l filter.List
	filterDecode(t, w, &l)
	if l.ID == 0 || l.Name != "Mine" || l.Status != "pending" {
		t.Fatalf("created %+v", l)
	}
	id := strconv.FormatInt(l.ID, 10)
	w = callFilter(s, s.filterCreateList, "POST", "/api/v1/filter/lists", "", `{"url":"https://lists.example/a.txt?key=secret"}`)
	wantStatus(t, "duplicate", w, http.StatusConflict, "")

	w = callFilter(s, s.filterUpdateList, "PUT", "/api/v1/filter/lists/"+id, id,
		`{"name":"Renamed","url":"https://lists.example/a.txt?key=secret","enabled":true,"kind":"allow"}`)
	wantStatus(t, "update", w, http.StatusOK, "")
	filterDecode(t, w, &l)
	if l.Name != "Renamed" || l.Kind != "allow" || !slices.Equal(l.GroupIDs, []int64{1}) {
		t.Errorf("updated %+v", l)
	}
	w = callFilter(s, s.filterUpdateList, "PUT", "/api/v1/filter/lists/abc", "abc", `{"url":"https://lists.example/b.txt"}`)
	wantStatus(t, "bad id", w, http.StatusBadRequest, "id")

	// The download fails (no network): the list reports it, the request succeeds.
	w = callFilter(s, s.filterRefreshList, "POST", "/api/v1/filter/lists/"+id+"/refresh", id, "")
	wantStatus(t, "refresh", w, http.StatusOK, "")
	filterDecode(t, w, &l)
	if l.Status != "failed-empty" || !strings.Contains(l.LastError, "network disabled") || strings.Contains(l.LastError, "secret") {
		t.Errorf("refresh result %+v", l)
	}

	w = callFilter(s, s.filterRefreshAll, "POST", "/api/v1/filter/lists/refresh", "", "")
	wantStatus(t, "refresh all", w, http.StatusAccepted, "")
	if !strings.Contains(w.Body.String(), `"started":true`) {
		t.Errorf("refresh all body %s", w.Body)
	}

	w = callFilter(s, s.filterDeleteList, "DELETE", "/api/v1/filter/lists/"+id, id, "")
	wantStatus(t, "delete", w, http.StatusNoContent, "")
	w = callFilter(s, s.filterDeleteList, "DELETE", "/api/v1/filter/lists/"+id, id, "")
	wantStatus(t, "delete again", w, http.StatusNotFound, "")
}

func TestFilterRoutesRules(t *testing.T) {
	s, _ := newFilterTestServer(t)
	w := callFilter(s, s.filterCreateRule, "POST", "/api/v1/filter/rules", "", `{"action":"block","type":"regex","pattern":"(","enabled":true}`)
	wantStatus(t, "bad regex", w, http.StatusBadRequest, "pattern")

	w = callFilter(s, s.filterCreateRule, "POST", "/api/v1/filter/rules", "", `{"action":"block","type":"subtree","pattern":"*.Ads.Example","enabled":true}`)
	wantStatus(t, "create", w, http.StatusCreated, "")
	var rule filter.Rule
	filterDecode(t, w, &rule)
	if rule.Pattern != "ads.example" || !slices.Equal(rule.GroupIDs, []int64{1}) {
		t.Fatalf("created %+v", rule)
	}
	id := strconv.FormatInt(rule.ID, 10)

	w = callFilter(s, s.filterRules, "GET", "/api/v1/filter/rules?action=block&search=ads", "", "")
	wantStatus(t, "query", w, http.StatusOK, "")
	var rules []filter.Rule
	filterDecode(t, w, &rules)
	if len(rules) != 1 {
		t.Errorf("query: %+v", rules)
	}
	w = callFilter(s, s.filterRules, "GET", "/api/v1/filter/rules?action=allow", "", "")
	filterDecode(t, w, &rules)
	if w.Code != http.StatusOK || rules == nil || len(rules) != 0 {
		t.Errorf("empty result must be []: %d %s", w.Code, w.Body)
	}
	w = callFilter(s, s.filterRules, "GET", "/api/v1/filter/rules?type=glob", "", "")
	wantStatus(t, "bad type", w, http.StatusBadRequest, "type")

	w = callFilter(s, s.filterUpdateRule, "PUT", "/api/v1/filter/rules/"+id, id, `{"action":"allow","type":"exact","pattern":"ads.example","enabled":true}`)
	wantStatus(t, "update", w, http.StatusOK, "")
	w = callFilter(s, s.filterDeleteRule, "DELETE", "/api/v1/filter/rules/"+id, id, "")
	wantStatus(t, "delete", w, http.StatusNoContent, "")
	w = callFilter(s, s.filterUpdateRule, "PUT", "/api/v1/filter/rules/"+id, id, `{"action":"allow","type":"exact","pattern":"ads.example"}`)
	wantStatus(t, "update deleted", w, http.StatusNotFound, "")
}

func TestFilterRoutesExplain(t *testing.T) {
	s, eng := newFilterTestServer(t)
	if _, err := eng.CreateRule(context.Background(), filter.RuleInput{Action: "block", Type: "subtree", Pattern: "example.com", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	w := callFilter(s, s.filterExplain, "POST", "/api/v1/filter/explain", "", `{"domain":"Ads.Example.com."}`)
	wantStatus(t, "explain", w, http.StatusOK, "")
	var got explainResponse
	filterDecode(t, w, &got)
	caller := s.d.Clients.Identify(netip.MustParseAddr("192.0.2.10")).GroupIDs
	if got.Domain != "ads.example.com" || !slices.Equal(got.GroupIDs, caller) {
		t.Errorf("domain/groups: %+v (caller groups %v)", got, caller)
	}
	want := eng.Check("ads.example.com", 1, caller)
	if got.Decision.Action != want.Action.String() || got.Decision.Name != want.Name || got.Decision.Source != want.Source {
		t.Errorf("decision %+v, Check says %+v", got.Decision, want)
	}
	if slices.Contains(caller, 1) && (len(got.Matches) != 1 || !got.Matches[0].Decisive || got.Decision.Action != "block") {
		t.Errorf("default-group caller: %+v", got)
	}

	w = callFilter(s, s.filterExplain, "POST", "/api/v1/filter/explain", "", `{"domain":"nothing.test","clientIp":"192.168.1.20"}`)
	wantStatus(t, "explain other client", w, http.StatusOK, "")
	if !strings.Contains(w.Body.String(), `"matches":[]`) || !strings.Contains(w.Body.String(), `"action":"none"`) {
		t.Errorf("no-match body: %s", w.Body)
	}

	for _, c := range []struct{ body, field string }{
		{`{"domain":""}`, "domain"},
		{`{"domain":"not a domain"}`, "domain"},
		{`{"domain":"example.com","clientIp":"999.1.1.1"}`, "clientIp"},
	} {
		w = callFilter(s, s.filterExplain, "POST", "/api/v1/filter/explain", "", c.body)
		wantStatus(t, c.body, w, http.StatusBadRequest, c.field)
	}
}
