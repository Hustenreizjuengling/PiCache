package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/dns/parental"
)

func TestParentalAndNetworkRoutesRegistered(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	s.registerParentalRoutes()
	s.registerNetworkRoutes()
	for _, rc := range []struct{ method, path, pattern string }{
		{"GET", "/api/v1/parental/services", "GET /api/v1/parental/services"},
		{"GET", "/api/v1/parental/groups", "GET /api/v1/parental/groups"},
		{"GET", "/api/v1/parental/groups/3", "GET /api/v1/parental/groups/{id}"},
		{"PUT", "/api/v1/parental/groups/3", "PUT /api/v1/parental/groups/{id}"},
		{"PUT", "/api/v1/parental/groups/3/override", "PUT /api/v1/parental/groups/{id}/override"},
		{"DELETE", "/api/v1/parental/groups/3/override", "DELETE /api/v1/parental/groups/{id}/override"},
		{"GET", "/api/v1/network/check", "GET /api/v1/network/check"},
		{"POST", "/api/v1/network/scan", "POST /api/v1/network/scan"},
	} {
		if _, p := s.mux.Handler(httptest.NewRequest(rc.method, rc.path, nil)); p != rc.pattern {
			t.Errorf("%s %s → %q, want %q", rc.method, rc.path, p, rc.pattern)
		}
	}
}

// newParentalEnv is the storage test environment with a clients registry
// and the parental engine behind the real middleware, an admin session and
// a read token.
func newParentalEnv(t *testing.T) (ce *coreEnv, e *storageTestEnv, reg *clients.Registry, session, readTok string) {
	t.Helper()
	e = newStorageTestEnv(t)
	ctx := context.Background()
	reg, err := clients.New(ctx, e.db, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	p, err := parental.New(ctx, e.db, reg, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.d.Clients, e.srv.d.Parental = reg, p
	e.srv.d.Config.WebListen = []string{":8080"}
	ce = &coreEnv{srv: New(e.srv.d), auth: e.auth, set: e.srv.d.Settings}
	session = ce.provisionAndLogin(t)
	readTok = ce.createToken(t, session, "read")
	return ce, e, reg, session, readTok
}

func TestParentalRoutes(t *testing.T) {
	ce, e, reg, session, readTok := newParentalEnv(t)
	kids, err := reg.CreateGroup(context.Background(), clients.GroupInput{Name: "Kids", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/parental/groups/2"
	if kids.ID != 2 {
		t.Fatalf("group id %d", kids.ID)
	}

	// Read tokens see the catalogue and the controls, nothing else.
	var svcs []parental.Service
	w := ce.do("GET", "/api/v1/parental/services", "", readTok)
	coreDecode(t, w, &svcs)
	if w.Code != http.StatusOK || len(svcs) < 20 || svcs[0].ID == "" || len(svcs[0].Domains) == 0 {
		t.Fatalf("services %d %s", w.Code, w.Body)
	}
	var list []parental.GroupControls
	w = ce.do("GET", "/api/v1/parental/groups", "", readTok)
	coreDecode(t, w, &list)
	if w.Code != http.StatusOK || len(list) != 2 || list[0].GroupID != 1 || list[1].GroupName != "Kids" {
		t.Fatalf("groups %d %s", w.Code, w.Body)
	}
	for _, name := range []string{`"groupId":1`, `"groupName":"Default"`, `"groupEnabled":true`, `"clientCount":0`,
		`"blockedServices":[]`, `"schedules":[]`, `"state":{"blockAll":false,"blockedServices":[],"lifted":false,"timeZone":"`, `"utcOffsetMinutes":`} {
		if !strings.Contains(w.Body.String(), name) {
			t.Errorf("list lacks %s: %s", name, w.Body)
		}
	}
	body := `{"blockedServices":["tiktok","youtube"],"schedules":[{"name":"Bedtime","enabled":true,"days":[0,1,2,3,4],` +
		`"start":"21:00","end":"07:00","block":"all","services":[]}]}`
	for _, rc := range []struct{ method, path, body string }{
		{"PUT", path, body},
		{"PUT", path + "/override", `{"mode":"block","minutes":30}`},
		{"DELETE", path + "/override", ""},
	} {
		coreWantError(t, ce.do(rc.method, rc.path, rc.body, readTok), http.StatusForbidden, "forbidden", "")
	}
	coreWantError(t, ce.do("GET", "/api/v1/parental/groups", "", ""), http.StatusUnauthorized, "unauthorized", "")

	// Update.
	var gc parental.GroupControls
	w = ce.do("PUT", path, body, session)
	coreDecode(t, w, &gc)
	if w.Code != http.StatusOK || len(gc.Schedules) != 1 || !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(gc.Schedules[0].ID) ||
		len(gc.BlockedServices) != 2 || gc.UpdatedAt.IsZero() || gc.GroupName != "Kids" {
		t.Fatalf("update %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"schedules":[{"id":"`+gc.Schedules[0].ID+`","name":"Bedtime","enabled":true,"days":[0,1,2,3,4],"start":"21:00","end":"07:00","block":"all","services":[]}]`) {
		t.Errorf("schedule JSON %s", w.Body)
	}
	for _, tc := range []struct{ body, field string }{
		{`{"blockedServices":["myspace"],"schedules":[]}`, "blockedServices"},
		{`{"blockedServices":[],"schedules":[{"name":"x","enabled":true,"days":[],"start":"21:00","end":"07:00","block":"all"}]}`, "schedules[0].days"},
		{`{"blockedServices":[],"schedules":[{"name":"x","enabled":true,"days":[1],"start":"9","end":"07:00","block":"all"}]}`, "schedules[0].start"},
		{`{"blockedServices":[],"schedules":[{"name":"x","enabled":true,"days":[1],"start":"07:00","end":"07:00","block":"all"}]}`, "schedules[0].end"},
		{`{"blockedServices":[],"schedules":[{"name":"x","enabled":true,"days":[1],"start":"07:00","end":"08:00","block":"services","services":[]}]}`, "schedules[0].services"},
		{`{"blockedServices":[],"schedules":[],"state":{}}`, "body"},
	} {
		coreWantError(t, ce.do("PUT", path, tc.body, session), http.StatusBadRequest, "invalid", tc.field)
	}
	coreWantError(t, ce.do("PUT", "/api/v1/parental/groups/999", body, session), http.StatusNotFound, "not_found", "")
	coreWantError(t, ce.do("GET", "/api/v1/parental/groups/999", "", readTok), http.StatusNotFound, "not_found", "")
	coreWantError(t, ce.do("GET", "/api/v1/parental/groups/abc", "", readTok), http.StatusBadRequest, "invalid", "id")

	// Override.
	w = ce.do("PUT", path+"/override", `{"mode":"block","minutes":30}`, session)
	coreDecode(t, w, &gc)
	if w.Code != http.StatusOK || gc.Override == nil || gc.Override.Mode != "block" || time.Until(gc.Override.Until) < 29*time.Minute ||
		!gc.State.BlockAll || gc.State.Reason != "override" {
		t.Fatalf("override %d %s", w.Code, w.Body)
	}
	until := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	w = ce.do("PUT", path+"/override", `{"mode":"allow","until":"`+until+`"}`, session)
	coreDecode(t, w, &gc)
	if w.Code != http.StatusOK || gc.Override.Mode != "allow" || gc.Override.Until.Format(time.RFC3339) != until || !gc.State.Lifted {
		t.Fatalf("allow %d %s", w.Code, w.Body)
	}
	for _, tc := range []struct{ body, field string }{
		{`{"mode":"pause","minutes":5}`, "override.mode"},
		{`{"mode":"block"}`, "minutes"},
		{`{"mode":"block","minutes":10081}`, "minutes"},
		{`{"mode":"block","minutes":5,"until":"` + until + `"}`, "minutes"},
		{`{"mode":"block","until":"2000-01-01T00:00:00Z"}`, "override.until"},
		{`{"mode":"block","until":"` + time.Now().Add(8*24*time.Hour).UTC().Format(time.RFC3339) + `"}`, "override.until"},
	} {
		coreWantError(t, ce.do("PUT", path+"/override", tc.body, session), http.StatusBadRequest, "invalid", tc.field)
	}
	coreWantError(t, ce.do("PUT", "/api/v1/parental/groups/999/override", `{"mode":"block","minutes":5}`, session),
		http.StatusNotFound, "not_found", "")
	w = ce.do("DELETE", path+"/override", "", session)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"override"`) {
		t.Fatalf("clear %d %s", w.Code, w.Body)
	}

	entries, _, err := e.auth.AuditLog(context.Background(), auth.AuditQuery{Search: "parental.", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, a := range entries {
		actions = append(actions, a.Action)
		if a.Target != "2" {
			t.Errorf("audit target %+v", a)
		}
	}
	if got := strings.Join(actions, ","); got != "parental.override_clear,parental.override,parental.override,parental.update" {
		t.Fatalf("audit actions %s", got)
	}
	if !strings.Contains(entries[3].Details, `"blockedServices":["tiktok","youtube"]`) || !strings.Contains(entries[1].Details, `"mode":"allow"`) {
		t.Errorf("audit details %q / %q", entries[3].Details, entries[1].Details)
	}
}

// Without the engine the group endpoints answer 503; the catalogue stays.
func TestParentalRoutesUnavailable(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	coreWantError(t, e.do("GET", "/api/v1/parental/groups", "", session), http.StatusServiceUnavailable, "unavailable", "")
	coreWantError(t, e.do("PUT", "/api/v1/parental/groups/1/override", `{"mode":"block","minutes":5}`, session),
		http.StatusServiceUnavailable, "unavailable", "")
	if w := e.do("GET", "/api/v1/parental/services", "", session); w.Code != http.StatusOK {
		t.Fatalf("services: %d", w.Code)
	}
	coreWantError(t, e.do("GET", "/api/v1/network/check", "", session), http.StatusServiceUnavailable, "unavailable", "")
}
