package api

import (
	"context"
	"crypto/tls"
	"encoding/json/v2"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/auth"
)

// fakeTLS is a WebTLS that records calls.
type fakeTLS struct {
	mu        sync.Mutex
	st        TLSStatus
	checkErr  error
	uploads   int
	deleted   bool
	newCAs    int
	caPEM     []byte
	lastCheck string
}

func (f *fakeTLS) Status() TLSStatus { f.mu.Lock(); defer f.mu.Unlock(); return f.st }
func (f *fakeTLS) CheckUpload(c, k string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastCheck = c + k
	return f.checkErr
}
func (f *fakeTLS) Upload(c, k string) (CertInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads++
	f.st.Source, f.st.UploadStored = "uploaded", true
	return CertInfo{Subject: "CN=picache.example", Issuer: "CN=CA", SANs: []string{"picache.example"},
		NotAfter: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), FingerprintSHA256: "AA:BB"}, nil
}
func (f *fakeTLS) DeleteUpload() (CertInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.st.UploadStored {
		return CertInfo{}, ErrNoUploadedCertificate
	}
	f.deleted, f.st.UploadStored, f.st.Source = true, false, "local-ca"
	return CertInfo{Subject: "CN=picache.example", FingerprintSHA256: "AA:BB"}, nil
}
func (f *fakeTLS) NewLocalCA() (LocalCAInfo, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newCAs++
	return LocalCAInfo{Subject: "CN=PiCache local CA", PermittedNames: []string{"picache"}, PermittedAddresses: []string{"127.0.0.1"}}, f.newCAs > 1, nil
}
func (f *fakeTLS) CACert() ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.caPEM, f.caPEM != nil
}

func newTLSRoutesEnv(t *testing.T) (*coreEnv, *fakeTLS, string) {
	t.Helper()
	e := newCoreEnv(t)
	f := &fakeTLS{st: TLSStatus{Listener: true, Source: "local-ca", HostsCovered: []string{"picache"}, HostsNotCovered: []string{}}}
	e.srv.d.TLS = f
	e.rt.tlsAddr = "0.0.0.0:8443"
	return e, f, e.provisionAndLogin(t)
}

func uploadBody(t *testing.T, cert, key, pw string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"certPem": cert, "keyPem": key, "currentPassword": pw})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// GET /system/tls says whether this request may upload: not without a
// listener, not with PICACHE_WEB_TLS_CERT, not over plain HTTP (except from
// this machine).
func TestTLSStatusUploadPermission(t *testing.T) {
	e, f, session := newTLSRoutesEnv(t)
	get := func(opts ...reqOpt) TLSStatus {
		var st TLSStatus
		coreDecode(t, e.req("GET", "/api/v1/system/tls", "", session, opts...), &st)
		return st
	}
	if st := get(); st.Upload.Allowed || st.Upload.Reason != "plain-http" || st.Source != "local-ca" {
		t.Fatalf("plain HTTP: %+v", st)
	}
	if st := get(overTLS(tls.VersionTLS13)); !st.Upload.Allowed || st.Upload.Reason != "" {
		t.Fatalf("HTTPS: %+v", st.Upload)
	}
	if st := get(from("127.0.0.1:1")); !st.Upload.Allowed {
		t.Fatalf("loopback: %+v", st.Upload)
	}
	f.st.EnvOverride = true
	if st := get(overTLS(tls.VersionTLS13)); st.Upload.Allowed || st.Upload.Reason != "env-override" {
		t.Fatalf("env override: %+v", st.Upload)
	}
	f.st = TLSStatus{Source: "none"}
	if st := get(overTLS(tls.VersionTLS13)); st.Upload.Allowed || st.Upload.Reason != "no-listener" || st.HostsCovered == nil || st.HostsNotCovered == nil {
		t.Fatalf("no listener: %+v", st)
	}
	e.srv.d.TLS = nil
	w := e.req("GET", "/api/v1/system/tls", "", session)
	if !strings.Contains(w.Body.String(), `"source":"none"`) || !strings.Contains(w.Body.String(), `"hostsCovered":[]`) {
		t.Fatalf("without a manager: %s", w.Body)
	}
}

// PUT /system/tls checks its rules in order and audits without PEM.
func TestTLSUploadRoute(t *testing.T) {
	e, f, session := newTLSRoutesEnv(t)
	https := overTLS(tls.VersionTLS13)
	good := uploadBody(t, "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n", "-----BEGIN PRIVATE KEY-----\ny\n-----END PRIVATE KEY-----\n", corePassword)
	f.st.Listener = false
	coreWantError(t, e.req("PUT", "/api/v1/system/tls", good, session, https), http.StatusConflict, "conflict", "")
	f.st.Listener, f.st.EnvOverride = true, true
	w := e.req("PUT", "/api/v1/system/tls", good, session, https)
	coreWantError(t, w, http.StatusConflict, "conflict", "")
	if !strings.Contains(w.Body.String(), "PICACHE_WEB_TLS_CERT is set") {
		t.Fatalf("env override: %s", w.Body)
	}
	f.st.EnvOverride = false
	w = e.req("PUT", "/api/v1/system/tls", good, session)
	coreWantError(t, w, http.StatusBadRequest, "invalid", "keyPem")
	if !strings.Contains(w.Body.String(), "send the private key over HTTPS (port 8443)") {
		t.Fatalf("plain HTTP: %s", w.Body)
	}
	coreWantError(t, e.req("PUT", "/api/v1/system/tls", uploadBody(t, strings.Repeat("a", 130<<10), "", corePassword), session, https),
		http.StatusBadRequest, "invalid", "body")
	w = e.req("PUT", "/api/v1/system/tls", uploadBody(t, strings.Repeat("a", 40<<10), strings.Repeat("b", 30<<10), corePassword), session, https)
	coreWantError(t, w, http.StatusBadRequest, "invalid", "certPem")
	if !strings.Contains(w.Body.String(), "certificate and key together must be at most 64 KiB") {
		t.Fatalf("64 KiB: %s", w.Body)
	}
	f.checkErr = apperr.Invalid("certPem", "certificate 2: cannot be read")
	coreWantError(t, e.req("PUT", "/api/v1/system/tls", good, session, https), http.StatusBadRequest, "invalid", "certPem")
	f.checkErr = nil
	coreWantError(t, e.req("PUT", "/api/v1/system/tls", uploadBody(t, "c", "k", "wrong password"), session, https),
		http.StatusBadRequest, "invalid", "currentPassword")
	if f.uploads != 0 {
		t.Fatal("nothing may be stored before every check passed")
	}
	// From this machine over plain HTTP.
	w = e.req("PUT", "/api/v1/system/tls", good, session, from("127.0.0.1:5"))
	if w.Code != http.StatusOK || f.uploads != 1 || !strings.Contains(w.Body.String(), `"source":"uploaded"`) {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	for _, en := range e.auditEntries(t) {
		if strings.Contains(en.Details, "PRIVATE KEY") || strings.Contains(en.Details, "BEGIN") {
			t.Fatalf("audit %s contains PEM: %s", en.Action, en.Details)
		}
		if en.Action == "system.tls.upload" && (!strings.Contains(en.Details, `"source":"uploaded"`) || !strings.Contains(en.Details, `"fingerprintSha256":"AA:BB"`)) {
			t.Fatalf("upload audit %s", en.Details)
		}
	}
	// An admin API token can never change the certificate.
	tok := e.createToken(t, session, "admin")
	coreWantError(t, e.req("PUT", "/api/v1/system/tls", good, tok, https), http.StatusForbidden, "forbidden", "")
}

func TestTLSDeleteLocalCAAndCACert(t *testing.T) {
	e, f, session := newTLSRoutesEnv(t)
	coreWantError(t, e.req("DELETE", "/api/v1/system/tls", "", session), http.StatusNotFound, "not_found", "")
	f.st.UploadStored = true
	if w := e.req("DELETE", "/api/v1/system/tls", "", session); w.Code != http.StatusOK || !f.deleted {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	coreWantError(t, e.req("POST", "/api/v1/system/tls/local-ca", `{"currentPassword":"wrong one"}`, session),
		http.StatusBadRequest, "invalid", "currentPassword")
	if w := e.req("POST", "/api/v1/system/tls/local-ca", `{"currentPassword":"`+corePassword+`"}`, session); w.Code != http.StatusOK || f.newCAs != 1 {
		t.Fatalf("local CA: %d %s", w.Code, w.Body)
	}
	f.st.Listener = false
	coreWantError(t, e.req("POST", "/api/v1/system/tls/local-ca", `{"currentPassword":"`+corePassword+`"}`, session),
		http.StatusConflict, "conflict", "")
	actions := strings.Join(e.auditActions(t), ",")
	if !strings.Contains(actions, "system.tls.delete") || !strings.Contains(actions, "system.tls.local_ca") {
		t.Fatalf("audit %s", actions)
	}
	coreWantError(t, e.req("GET", "/api/v1/system/tls/ca.crt", "", ""), http.StatusNotFound, "not_found", "")
	f.caPEM = []byte("-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----\n")
	w := e.req("GET", "/api/v1/system/tls/ca.crt", "", "")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/x-x509-ca-cert" ||
		w.Header().Get("Content-Disposition") != `attachment; filename="picache-ca.crt"` || w.Body.String() != string(f.caPEM) {
		t.Fatalf("ca.crt %d %v %q", w.Code, w.Header(), w.Body)
	}
}

func (e *coreEnv) auditEntries(t *testing.T) []auth.AuditEntry {
	t.Helper()
	entries, _, err := e.auth.AuditLog(context.Background(), auth.AuditQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
