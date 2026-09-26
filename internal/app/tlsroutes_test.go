package app

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// tlsRuntime is the part of api.Runtime the certificate routes use.
type tlsRuntime struct{ api.Runtime }

func (tlsRuntime) Listeners() api.ListenerInfo {
	return api.ListenerInfo{Bound: map[string][]string{"web": {"0.0.0.0:8080"}, "web-tls": {"0.0.0.0:8443"}}}
}

// The certificate routes with the real manager: an upload, a new local CA
// and a delete leave no key material in the audit log or the log, and the
// CA certificate can be downloaded.
func TestTLSRoutesWithManager(t *testing.T) {
	ctx := context.Background()
	e := newTLSEnv(t, "", "")
	e.m.start()
	d, err := db.Open(filepath.Join(e.dir, "picache.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := e.set.Update(ctx, func(s *settings.All) error { s.Web.RestrictToNetworks = false; return nil }); err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.New(make([]byte, 32))
	svc, err := auth.New(ctx, d, e.set, box, "", newTestLogger(e.log))
	if err != nil {
		t.Fatal(err)
	}
	const pw = "correct horse battery"
	if err := svc.Provision(ctx, "admin", pw); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DataDir: e.dir, WebListen: []string{":8080"}, WebTLSListen: []string{":8443"}, DestructiveAPI: true}
	srv := api.New(api.Deps{Config: cfg, Settings: e.set, Auth: svc, Runtime: tlsRuntime{}, TLS: e.m, Log: newTestLogger(e.log)})
	do := func(method, target, body, cookie string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://192.168.1.10:8443"+target, strings.NewReader(body))
		r.Host = "192.168.1.10:8443"
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: auth.SecureSessionCookie, Value: cookie})
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w
	}
	w := do("POST", "/api/v1/auth/login", `{"username":"admin","password":"`+pw+`"}`, "")
	var session string
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SecureSessionCookie {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	ca := newTestCA(t, "Public CA", nil)
	certPEMb, keyPEMb, _ := ca.leaf(t, leafOpts{dns: []string{"picache.lan"}})
	body, _ := json.Marshal(map[string]string{"certPem": string(certPEMb), "keyPem": string(keyPEMb), "currentPassword": pw})
	if w := do("PUT", "/api/v1/system/tls", string(body), session); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"source":"uploaded"`) {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	if w := do("POST", "/api/v1/system/tls/local-ca", `{"currentPassword":"`+pw+`"}`, session); w.Code != http.StatusOK {
		t.Fatalf("local CA: %d %s", w.Code, w.Body)
	}
	if w := do("DELETE", "/api/v1/system/tls", "", session); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"source":"local-ca"`) {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	w = do("GET", "/api/v1/system/tls/ca.crt", "", "")
	if pemCA, _ := e.m.CACert(); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/x-x509-ca-cert" ||
		w.Header().Get("Content-Disposition") != `attachment; filename="picache-ca.crt"` || w.Body.String() != string(pemCA) {
		t.Fatalf("ca.crt: %d %v", w.Code, w.Header())
	}
	entries, _, err := svc.AuditLog(ctx, auth.AuditQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, en := range entries {
		actions = append(actions, en.Action)
		if strings.Contains(en.Details, "PRIVATE KEY") || strings.Contains(en.Details, "BEGIN") {
			t.Fatalf("audit %s contains PEM: %s", en.Action, en.Details)
		}
	}
	joined := strings.Join(actions, ",")
	for _, a := range []string{"system.tls.upload", "system.tls.local_ca", "system.tls.delete"} {
		if !strings.Contains(joined, a) {
			t.Errorf("audit lacks %s: %v", a, actions)
		}
	}
	if strings.Contains(e.log.String(), "PRIVATE KEY") {
		t.Fatal("a log line contains key material")
	}
}
