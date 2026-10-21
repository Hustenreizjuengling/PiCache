package api

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/prefill/prefillapi"
)

// fakePrefill answers like the prefill service.
type fakePrefill struct {
	installed bool
	down      bool
	calls     []string
	answers   map[string]prefillapi.Response
}

func (f *fakePrefill) Installed() bool { return f.installed }
func (f *fakePrefill) Socket() string  { return "/run/picache-prefill/prefill.sock" }

func (f *fakePrefill) Do(_ context.Context, method, path string, body any) (prefillapi.Response, error) {
	b, _ := json.Marshal(body)
	f.calls = append(f.calls, method+" "+path+" "+string(b))
	if f.down {
		return prefillapi.Response{}, prefillapi.ErrNotRunning
	}
	if r, ok := f.answers[method+" "+path]; ok {
		return r, nil
	}
	return prefillapi.Response{Status: http.StatusNoContent}, nil
}

func newPrefillEnv(t *testing.T, f *fakePrefill) (*coreEnv, string) {
	t.Helper()
	e := newCoreEnv(t)
	d := e.srv.d
	d.Prefill = f
	e.srv = New(d)
	return e, e.provisionAndLogin(t)
}

func TestPrefillRoutes(t *testing.T) {
	status, _ := json.Marshal(prefillapi.Status{Version: "1.4.0", Steam: prefillapi.Steam{Account: &prefillapi.SteamAccount{Name: "gamer"}}})
	f := &fakePrefill{installed: true, answers: map[string]prefillapi.Response{
		"GET /v1/status":          {Status: 200, Body: status},
		"POST /v1/steam/login":    {Status: 409, Body: []byte(`{"error":{"code":"conflict","message":"already signed in to Steam as gamer: sign out first"}}`)},
		"PUT /v1/settings":        {Status: 400, Body: []byte(`{"error":{"code":"invalid","message":"connections: 1–32"}}`)},
		"POST /v1/jobs":           {Status: 202, Body: []byte(`{"id":3,"state":"running","apps":[],"hosts":[]}`)},
		"PUT /v1/steam/selection": {Status: 200, Body: []byte(`{"apps":[1,2]}`)},
		"DELETE /v1/jobs/current": {Status: 404, Body: []byte(`{"error":{"code":"not_found","message":"no job is running"}}`)},
		"GET /v1/steam/apps":      {Status: 200, Body: []byte(`{"apps":[],"updated":null,"error":""}`)},
		"GET /v1/history":         {Status: 200, Body: []byte(`{"jobs":[]}`)},
		"POST /v1/steam/logout":   {Status: 204},
		"GET /v1/settings":        {Status: 500, Body: []byte(`not json`)},
		"DELETE /v1/steam/login":  {Status: 204},
	}}
	e, session := newPrefillEnv(t, f)

	w := e.do("GET", "/api/v1/prefill", "", session)
	var st PrefillState
	coreDecode(t, w, &st)
	if w.Code != 200 || !st.Installed || !st.Running || st.Status == nil || st.Status.Steam.Account.Name != "gamer" {
		t.Fatalf("state %d %+v", w.Code, st)
	}
	coreWantError(t, e.do("POST", "/api/v1/prefill/steam/login", "", session), http.StatusConflict, "conflict", "")
	coreWantError(t, e.do("PUT", "/api/v1/prefill/settings", `{"schedule":{"enabled":false,"days":[],"start":"03:00","end":"07:00"},"maxMbps":0,"connections":99,"steam":{"os":["windows"],"languages":["english"],"apps":[]}}`, session),
		http.StatusBadRequest, "invalid", "")
	// PiCache refuses unknown members before the service sees them.
	n := len(f.calls)
	coreWantError(t, e.do("PUT", "/api/v1/prefill/steam/selection", `{"apps":[1],"token":"x"}`, session), http.StatusBadRequest, "invalid", "body")
	if len(f.calls) != n {
		t.Error("an invalid body reached the service")
	}
	w = e.do("PUT", "/api/v1/prefill/steam/selection", `{"apps":[2,1]}`, session)
	if w.Code != 200 || !strings.Contains(f.calls[len(f.calls)-1], `{"apps":[2,1]}`) {
		t.Fatalf("selection %d %s %v", w.Code, w.Body, f.calls)
	}
	w = e.do("POST", "/api/v1/prefill/jobs", `{"force":true}`, session)
	if w.Code != http.StatusAccepted || !strings.Contains(f.calls[len(f.calls)-1], `{"force":true}`) {
		t.Fatalf("job %d %s", w.Code, w.Body)
	}
	coreWantError(t, e.do("DELETE", "/api/v1/prefill/jobs/current", "", session), http.StatusNotFound, "not_found", "")
	if w := e.do("POST", "/api/v1/prefill/steam/logout", "", session); w.Code != http.StatusNoContent {
		t.Fatalf("logout %d", w.Code)
	}
	coreWantError(t, e.do("GET", "/api/v1/prefill/settings", "", session), http.StatusServiceUnavailable, "unavailable", "")
	actions := e.auditActions(t)
	for _, a := range []string{"prefill.steam.selection", "prefill.job.start", "prefill.steam.logout"} {
		if !slices.Contains(actions, a) {
			t.Errorf("audit lacks %s: %v", a, actions)
		}
	}

	// A read token may not use the prefill endpoints.
	read := e.createToken(t, session, "read")
	if w := e.do("GET", "/api/v1/prefill", "", read); w.Code != http.StatusForbidden {
		t.Errorf("read token: %d", w.Code)
	}

	// The service stopped: the state says so, the other endpoints answer 503.
	f.down = true
	w = e.do("GET", "/api/v1/prefill", "", session)
	coreDecode(t, w, &st)
	if w.Code != 200 || st.Running || !strings.Contains(st.Error, "sudo picache prefill enable") {
		t.Fatalf("down state %+v", st)
	}
	coreWantError(t, e.do("GET", "/api/v1/prefill/history", "", session), http.StatusServiceUnavailable, "unavailable", "")
	// Not installed: no call at all.
	f.installed, f.down = false, false
	n = len(f.calls)
	w = e.do("GET", "/api/v1/prefill", "", session)
	coreDecode(t, w, &st)
	if st.Installed || st.Running || len(f.calls) != n {
		t.Fatalf("not installed %+v", st)
	}
}
