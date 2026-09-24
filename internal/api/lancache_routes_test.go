package api

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/lancache/services"
	"github.com/hustenreizjuengling/picache/internal/lancache/sni"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// lcSource serves a cache-domains source from memory (no network).
type lcSource struct {
	files map[string]string
	down  atomic.Bool
}

func (l *lcSource) RoundTrip(req *http.Request) (*http.Response, error) {
	if l.down.Load() {
		return nil, io.ErrUnexpectedEOF
	}
	body, ok := l.files[req.URL.Path]
	status := http.StatusOK
	if !ok {
		status, body = http.StatusNotFound, ""
	}
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

type lanCacheEnv struct {
	srv *Server
	reg *services.Registry
	src *lcSource
}

func newLanCacheEnv(t *testing.T) *lanCacheEnv {
	t.Helper()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "picache.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	st, err := settings.Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(ctx, func(a *settings.All) error {
		a.LanCache.DomainsSource = "https://cdn.test/cd/"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var many strings.Builder
	for i := range 60 {
		fmt.Fprintf(&many, "host%d.bigcdn.example.com\n", i)
	}
	src := &lcSource{files: map[string]string{
		"/cd/cache_domains.json": `{"cache_domains":[
			{"name":"steam","description":"Steam","domain_files":["steam.txt"]},
			{"name":"bigcdn","description":"Many hosts","domain_files":["big.txt"]}]}`,
		"/cd/steam.txt": "lancache.steamcontent.com\n",
		"/cd/big.txt":   many.String(),
	}}
	reg, err := services.New(ctx, d, st, &http.Client{Transport: src}, filepath.Join(dir, "cache-domains"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	as, err := auth.New(ctx, d, st, box, filepath.Join(dir, "setup-token"), log)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{d: Deps{Settings: st, Services: reg, SNI: sni.New(sni.Deps{Settings: st, Log: log}), Auth: as, Log: log}, log: log}
	return &lanCacheEnv{srv: s, reg: reg, src: src}
}

// call runs h like s.route does after authentication (admin principal).
func (e *lanCacheEnv) call(h handlerFunc, method, id, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/v1/lancache/x", strings.NewReader(body))
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
		writeError(w, r, e.srv.log, err)
	}
	return w
}

func lcDecode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", w.Body, err)
	}
	return v
}

func lcWantError(t *testing.T, w *httptest.ResponseRecorder, status int, field, msg string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body)
	}
	b := lcDecode[errorBody](t, w)
	if b.Error.Field != field || !strings.Contains(b.Error.Message, msg) {
		t.Fatalf("error = %+v, want field %q containing %q", b.Error, field, msg)
	}
}

func (e *lanCacheEnv) audited(t *testing.T) []string {
	t.Helper()
	entries, _, err := e.srv.d.Auth.AuditLog(context.Background(), auth.AuditQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, a := range entries {
		out = append(out, a.Action+" "+a.Target)
	}
	return out
}

func TestLanCacheRoutesServiceList(t *testing.T) {
	e := newLanCacheEnv(t)
	w := e.call(e.srv.lanCacheServices, "GET", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	list := lcDecode[[]services.Service](t, w)
	i := slices.IndexFunc(list, func(s services.Service) bool { return s.ID == "bigcdn" })
	if i < 0 || len(list[i].Domains) != lanCacheListDomains || list[i].DomainCount != 60 || list[i].ExtraDomains == nil {
		t.Fatalf("list entry = %+v", list[i])
	}
	full := lcDecode[services.Service](t, e.call(e.srv.lanCacheService, "GET", "bigcdn", ""))
	if len(full.Domains) != 60 {
		t.Fatalf("detail domains = %d", len(full.Domains))
	}
	// The trimmed list must not have modified the registry's data.
	if sv, _ := e.reg.Service(context.Background(), "bigcdn"); len(sv.Domains) != 60 {
		t.Fatal("list view trimmed the shared domains")
	}
	lcWantError(t, e.call(e.srv.lanCacheService, "GET", "nope", ""), http.StatusNotFound, "", "not found")
	lcWantError(t, e.call(e.srv.lanCacheService, "GET", "../etc", ""), http.StatusBadRequest, "id", "invalid")
}

func TestLanCacheRoutesEnableAndDomains(t *testing.T) {
	e := newLanCacheEnv(t)
	w := e.call(e.srv.lanCacheSetEnabled, "PUT", "bigcdn", `{"enabled":false}`)
	if w.Code != http.StatusOK || lcDecode[services.Service](t, w).Enabled {
		t.Fatalf("disable: %d %s", w.Code, w.Body)
	}
	if _, ok := e.reg.MatchDNS("host1.bigcdn.example.com"); ok {
		t.Fatal("disabled service still matched")
	}
	lcWantError(t, e.call(e.srv.lanCacheSetEnabled, "PUT", "bigcdn", `{}`), http.StatusBadRequest, "enabled", "required")
	lcWantError(t, e.call(e.srv.lanCacheSetEnabled, "PUT", "bigcdn", `{"enabled":true,"x":1}`), http.StatusBadRequest, "body", "")
	lcWantError(t, e.call(e.srv.lanCacheSetEnabled, "PUT", "nope", `{"enabled":true}`), http.StatusNotFound, "", "")

	w = e.call(e.srv.lanCacheSetDomains, "PUT", "steam", `{"extraDomains":["Cache1.Example-CDN.net"]}`)
	if sv := lcDecode[services.Service](t, w); w.Code != http.StatusOK || !slices.Equal(sv.ExtraDomains, []string{"cache1.example-cdn.net"}) {
		t.Fatalf("domains: %d %s", w.Code, w.Body)
	}
	lcWantError(t, e.call(e.srv.lanCacheSetDomains, "PUT", "steam", `{"extraDomains":["ok.example.org","*.co.uk"]}`),
		http.StatusBadRequest, "extraDomains[1]", `"*.co.uk"`)

	got := e.audited(t)
	for _, want := range []string{"lancache.service.enable bigcdn", "lancache.service.domains steam"} {
		if !slices.Contains(got, want) {
			t.Fatalf("audit %q missing in %q", want, got)
		}
	}
}

func TestLanCacheRoutesCustomServices(t *testing.T) {
	e := newLanCacheEnv(t)
	w := e.call(e.srv.lanCacheCreate, "POST", "", `{"name":"LAN Mirror","description":"d","domains":["mirror.example.org"]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	sv := lcDecode[services.Service](t, w)
	if sv.ID != "custom-lan-mirror" || !sv.Custom {
		t.Fatalf("created = %+v", sv)
	}
	lcWantError(t, e.call(e.srv.lanCacheCreate, "POST", "", `{"name":"x","domains":["*"]}`), http.StatusBadRequest, "domains[0]", "")

	w = e.call(e.srv.lanCacheUpdate, "PUT", sv.ID, `{"name":"Renamed","description":"","domains":["m2.example.org"]}`)
	if up := lcDecode[services.Service](t, w); w.Code != http.StatusOK || up.Name != "Renamed" {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	lcWantError(t, e.call(e.srv.lanCacheUpdate, "PUT", "steam", `{"name":"x","domains":["a.example.org"]}`), http.StatusForbidden, "", "custom")
	lcWantError(t, e.call(e.srv.lanCacheDelete, "DELETE", "steam", ""), http.StatusForbidden, "", "custom")
	if w := e.call(e.srv.lanCacheDelete, "DELETE", sv.ID, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	lcWantError(t, e.call(e.srv.lanCacheDelete, "DELETE", sv.ID, ""), http.StatusNotFound, "", "")
	got := e.audited(t)
	for _, want := range []string{"lancache.service.create custom-lan-mirror", "lancache.service.update custom-lan-mirror", "lancache.service.delete custom-lan-mirror"} {
		if !slices.Contains(got, want) {
			t.Fatalf("audit %q missing in %q", want, got)
		}
	}
}

func TestLanCacheRoutesSourceAndLabels(t *testing.T) {
	e := newLanCacheEnv(t)
	st := lcDecode[services.SourceStatus](t, e.call(e.srv.lanCacheSource, "GET", "", ""))
	if !st.Ready || st.ServiceCount != 2 || st.Source != "https://cdn.test/cd/" {
		t.Fatalf("source = %+v", st)
	}
	w := e.call(e.srv.lanCacheRefresh, "POST", "", "")
	if w.Code != http.StatusOK || !lcDecode[services.SourceStatus](t, w).Ready {
		t.Fatalf("refresh: %d %s", w.Code, w.Body)
	}
	e.src.down.Store(true)
	w = e.call(e.srv.lanCacheRefresh, "POST", "", "")
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "cache-domains update failed") {
		t.Fatalf("failed refresh: %d %s", w.Code, w.Body)
	}

	if w := e.call(e.srv.lanCacheSetLabel, "PUT", "", `{"groupKey":"steam:depot:228990","label":"Redistributables"}`); w.Code != http.StatusNoContent {
		t.Fatalf("label: %d %s", w.Code, w.Body)
	}
	if got := e.reg.Label("steam:depot:228990"); got != "Redistributables" {
		t.Fatalf("label = %q", got)
	}
	lcWantError(t, e.call(e.srv.lanCacheSetLabel, "PUT", "", `{"groupKey":"nocolon","label":"x"}`), http.StatusBadRequest, "groupKey", "")

	sn := lcDecode[sni.Stats](t, e.call(e.srv.lanCacheSNI, "GET", "", ""))
	if sn.Listening || sn.Total != 0 {
		t.Fatalf("sni stats = %+v", sn)
	}
	if got := e.audited(t); !slices.Contains(got, "lancache.label.set steam:depot:228990") || !slices.Contains(got, "lancache.source.refresh ") {
		t.Fatalf("audit = %q", got)
	}
}

func TestLanCacheRoutesRegistered(t *testing.T) {
	e := newLanCacheEnv(t)
	s := &Server{d: e.srv.d, log: e.srv.log, mux: http.NewServeMux()}
	s.registerLanCacheRoutes()
	for _, pattern := range []string{
		"GET /api/v1/lancache/services", "GET /api/v1/lancache/services/steam", "PUT /api/v1/lancache/services/steam/enabled",
		"PUT /api/v1/lancache/services/steam/domains", "POST /api/v1/lancache/services", "PUT /api/v1/lancache/services/x",
		"DELETE /api/v1/lancache/services/x", "GET /api/v1/lancache/source", "POST /api/v1/lancache/source/refresh",
		"PUT /api/v1/lancache/labels", "GET /api/v1/lancache/sni",
	} {
		method, path, _ := strings.Cut(pattern, " ")
		if _, matched := s.mux.Handler(httptest.NewRequest(method, path, nil)); matched == "" {
			t.Errorf("%s not registered", pattern)
		}
	}
}
