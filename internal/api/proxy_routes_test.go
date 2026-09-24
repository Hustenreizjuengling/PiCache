package api

import (
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/lancache/proxy"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

type proxyEnv struct {
	srv *Server
	db  *db.DB
}

// newProxyEnv returns a Server with a real proxy (no store: pass-through)
// whose no-slice table holds one marked host, "slow.example".
func newProxyEnv(t *testing.T) *proxyEnv {
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
	box, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	as, err := auth.New(ctx, d, st, box, filepath.Join(dir, "setup-token"), log)
	if err != nil {
		t.Fatal(err)
	}
	// The first proxy migrates its table; the second one loads the row.
	if _, err := proxy.New(ctx, proxy.Deps{DB: d, Settings: st, Log: log}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := d.W.ExecContext(ctx, `INSERT INTO proxy_noslice_hosts (host, failures, objects, window_start, marked, since)
		VALUES ('slow.example', 3, '', ?, 1, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	px, err := proxy.New(ctx, proxy.Deps{DB: d, Settings: st, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{d: Deps{Settings: st, Auth: as, Proxy: px, Log: log}, log: log}
	return &proxyEnv{srv: s, db: d}
}

// call runs h like s.route does after authentication (admin principal).
func (e *proxyEnv) call(h handlerFunc, method, host string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/v1/cache/x", nil)
	if host != "" {
		r.SetPathValue("host", host)
	}
	p := &auth.Principal{UserID: 1, Username: "admin", Scope: auth.ScopeAdmin}
	r = r.WithContext(context.WithValue(r.Context(), principalKey, p))
	w := httptest.NewRecorder()
	if err := h(w, r); err != nil {
		writeError(w, r, e.srv.log, err)
	}
	return w
}

func proxyDecode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", w.Body, err)
	}
	return v
}

func TestProxyRoutesLive(t *testing.T) {
	e := newProxyEnv(t)
	for _, h := range []handlerFunc{e.srv.handleProxyLive, e.srv.handleProxyActive} {
		// Empty lists are arrays, never null.
		if w := e.call(h, "GET", ""); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
			t.Fatalf("live: %d %s", w.Code, w.Body)
		}
	}
	st := proxyDecode[proxy.Stats](t, e.call(e.srv.handleProxyStats, "GET", ""))
	if !st.PassThrough || st.Requests != 0 {
		t.Fatalf("stats %+v", st)
	}
	hosts := proxyDecode[[]proxy.NoSliceHost](t, e.call(e.srv.handleNoSliceList, "GET", ""))
	if len(hosts) != 1 || hosts[0].Host != "slow.example" || !hosts[0].Marked || hosts[0].Since.IsZero() {
		t.Fatalf("no-slice hosts %+v", hosts)
	}
}

func TestProxyRoutesNoSliceReset(t *testing.T) {
	e := newProxyEnv(t)
	for _, tc := range []struct {
		host   string
		status int
	}{
		{"10.0.0.1", http.StatusBadRequest},
		{"bad host", http.StatusBadRequest},
		{"unknown.example", http.StatusNotFound},
	} {
		w := e.call(e.srv.handleNoSliceReset, "DELETE", tc.host)
		if w.Code != tc.status {
			t.Fatalf("%q: %d %s", tc.host, w.Code, w.Body)
		}
		if tc.status == http.StatusBadRequest {
			var b errorBody
			if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || b.Error.Field != "host" {
				t.Fatalf("%q: error body %s", tc.host, w.Body)
			}
		}
	}
	if w := e.call(e.srv.handleNoSliceReset, "DELETE", "Slow.Example."); w.Code != http.StatusNoContent {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	if hosts := proxyDecode[[]proxy.NoSliceHost](t, e.call(e.srv.handleNoSliceList, "GET", "")); len(hosts) != 0 {
		t.Fatalf("still listed: %+v", hosts)
	}
	var n int
	if err := e.db.R.QueryRow(`SELECT COUNT(*) FROM proxy_noslice_hosts`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("reset not persisted: %d %v", n, err)
	}
	entries, _, err := e.srv.d.Auth.AuditLog(context.Background(), auth.AuditQuery{Limit: 10})
	if err != nil || len(entries) != 1 || entries[0].Action != "cache.noslice.reset" || entries[0].Target != "slow.example" {
		t.Fatalf("audit %+v %v", entries, err)
	}
}

func TestProxyRoutesWithoutProxy(t *testing.T) {
	e := newProxyEnv(t)
	e.srv.d.Proxy = nil
	for _, h := range []handlerFunc{e.srv.handleProxyLive, e.srv.handleProxyActive, e.srv.handleProxyStats, e.srv.handleNoSliceList, e.srv.handleNoSliceReset} {
		if w := e.call(h, "GET", "slow.example"); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d", w.Code)
		}
	}
}

func TestProxyRoutesRegistered(t *testing.T) {
	e := newProxyEnv(t)
	s := &Server{d: e.srv.d, log: e.srv.log, mux: http.NewServeMux()}
	s.registerProxyRoutes()
	for _, pattern := range []string{
		"GET /api/v1/cache/live", "GET /api/v1/cache/active", "GET /api/v1/cache/proxy/stats",
		"GET /api/v1/cache/noslice", "DELETE /api/v1/cache/noslice/cdn.example.com",
	} {
		method, path, _ := strings.Cut(pattern, " ")
		if _, matched := s.mux.Handler(httptest.NewRequest(method, path, nil)); matched == "" {
			t.Errorf("%s not registered", pattern)
		}
	}
	// Without credentials every proxy route is refused.
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/cache/live", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", w.Code)
	}
}
