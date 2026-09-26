package api

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
	cachestore "github.com/hustenreizjuengling/picache/internal/dlcache/store"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/update"
)

// coreRuntime is a fake Runtime for the auth/system/settings route tests.
type coreRuntime struct {
	mu         sync.Mutex
	backup     []byte
	backupErr  error
	restored   []byte
	restoreErr error
	staged     *settings.All // what StageRestore returns as the staged settings
	restarted  bool
	tlsAddr    string // bound web-tls listener ("" = none)
}

func (f *coreRuntime) StartedAt() time.Time    { return time.Now().Add(-time.Hour) }
func (f *coreRuntime) InstanceID() string      { return "0123456789abcdef" }
func (f *coreRuntime) Config() *config.Config  { return nil }
func (f *coreRuntime) MasterKeySource() string { return "memory" }
func (f *coreRuntime) Listeners() ListenerInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	bound := map[string][]string{"web": {"127.0.0.1:8080"}}
	if f.tlsAddr != "" {
		bound["web-tls"] = []string{f.tlsAddr}
	}
	return ListenerInfo{Bound: bound}
}
func (f *coreRuntime) StoreState() StoreState                      { return StoreState{TargetID: "local"} }
func (f *coreRuntime) ActiveStore() *cachestore.Store              { return nil }
func (f *coreRuntime) ActivateStore(context.Context, string) error { return nil }
func (f *coreRuntime) EvictNow(context.Context) (cachestore.EvictResult, error) {
	return cachestore.EvictResult{}, nil
}
func (f *coreRuntime) StartVerify(bool) error   { return nil }
func (f *coreRuntime) VerifyState() VerifyState { return VerifyState{} }
func (f *coreRuntime) Backup(_ context.Context, w io.Writer, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.backupErr != nil {
		return f.backupErr
	}
	_, err := w.Write(f.backup)
	return err
}
func (f *coreRuntime) StageRestore(_ context.Context, r io.Reader) (*settings.All, error) {
	b, err := io.ReadAll(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	f.restored = b
	if f.restoreErr != nil {
		return nil, f.restoreErr
	}
	return f.staged, nil
}
func (f *coreRuntime) Restart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarted = true
}
func (f *coreRuntime) Health(context.Context) Health {
	return Health{OK: true, Checks: []HealthCheck{{Name: "dns", Status: "ok"}, {Name: "cache", Status: "warn"}}}
}

// coreEnv is a Server with a real auth.Service and settings store on a temp DB.
type coreEnv struct {
	srv       *Server
	auth      *auth.Service
	set       *settings.Store
	rt        *coreRuntime
	upd       *coreUpdater
	setupFile string
	db        *db.DB
}

// authDB returns the writer pool of the test database (to change rows
// behind the service's back).
func (e *coreEnv) authDB(t *testing.T) *sql.DB {
	t.Helper()
	return e.db.W
}

const (
	coreHost     = "192.168.1.2:8080"
	corePassword = "correct horse battery"
)

func newCoreEnv(t *testing.T) *coreEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "picache.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	set := openOpenSettings(t, d, log)
	box, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	setupFile := filepath.Join(dir, "setup-token")
	a, err := auth.New(ctx, d, set, box, setupFile, log)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DataDir: dir, CacheDir: filepath.Join(dir, "cache"), MountRoot: filepath.Join(dir, "mnt"),
		WebListen: []string{":8080"}, WebTLSListen: []string{":8443"}, WebHosts: []string{"picache.example"},
		DestructiveAPI: true,
	}
	rt, upd := &coreRuntime{}, &coreUpdater{}
	srv := New(Deps{Config: cfg, Settings: set, Auth: a, Runtime: rt, Updates: upd, Log: log})
	return &coreEnv{srv: srv, auth: a, set: set, rt: rt, upd: upd, setupFile: setupFile, db: d}
}

// openOpenSettings opens the settings with the web UI open to every
// address (the state of an upgraded installation): httptest requests come
// from 192.0.2.1, which a new installation's web ACL refuses. The web
// access tests switch the restriction on themselves.
func openOpenSettings(t *testing.T, d *db.DB, log *slog.Logger) *settings.Store {
	t.Helper()
	set, err := settings.Open(context.Background(), d, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Update(context.Background(), func(a *settings.All) error { a.Web.RestrictToNetworks = false; return nil }); err != nil {
		t.Fatal(err)
	}
	return set
}

// coreRequest builds a request to the test host; a non-empty body is sent as JSON.
func coreRequest(method, target, body string) *http.Request {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	r.Host = coreHost
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func (e *coreEnv) serve(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(w, r)
	return w
}

// do sends a request with an optional session cookie or Bearer credential
// ("pc_…" or a session token prefixed with "Bearer ").
func (e *coreEnv) do(method, target, body, cred string) *httptest.ResponseRecorder {
	r := coreRequest(method, target, body)
	switch {
	case strings.HasPrefix(cred, "pc_"):
		r.Header.Set("Authorization", "Bearer "+cred)
	case strings.HasPrefix(cred, "Bearer "):
		r.Header.Set("Authorization", cred)
	case cred != "":
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: cred})
	}
	return e.serve(r)
}

// provisionAndLogin creates the admin and returns a session cookie value.
func (e *coreEnv) provisionAndLogin(t *testing.T) string {
	t.Helper()
	if err := e.auth.Provision(context.Background(), "admin", corePassword); err != nil {
		t.Fatal(err)
	}
	w := e.do("POST", "/api/v1/auth/login", `{"username":"admin","password":"`+corePassword+`"}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	return coreSessionCookie(t, w).Value
}

// createToken creates an API token with the given scope via the API.
func (e *coreEnv) createToken(t *testing.T, session, scope string) string {
	t.Helper()
	w := e.do("POST", "/api/v1/tokens", `{"name":"test","scope":"`+scope+`","currentPassword":"`+corePassword+`"}`, session)
	if w.Code != http.StatusCreated {
		t.Fatalf("create token: %d %s", w.Code, w.Body)
	}
	var out struct {
		Token string `json:"token"`
	}
	coreDecode(t, w, &out)
	return out.Token
}

// coreSessionCookie returns the session cookie a response sets (either name).
func coreSessionCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SessionCookie || c.Name == auth.SecureSessionCookie {
			return c
		}
	}
	t.Fatalf("no session cookie in %v", w.Header())
	return nil
}

func coreDecode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", w.Body, err)
	}
}

// coreWantError asserts an API error response.
func coreWantError(t *testing.T, w *httptest.ResponseRecorder, status int, code, field string) {
	t.Helper()
	var body errorBody
	if w.Code != status {
		t.Fatalf("status %d, want %d (body %s)", w.Code, status, w.Body)
	}
	coreDecode(t, w, &body)
	if body.Error.Code != code || body.Error.Field != field {
		t.Fatalf("error %+v, want code %q field %q", body.Error, code, field)
	}
}

func (e *coreEnv) auditActions(t *testing.T) []string {
	t.Helper()
	entries, _, err := e.auth.AuditLog(context.Background(), auth.AuditQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, en := range entries {
		out = append(out, en.Action)
	}
	return out
}

func coreReadSetupToken(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

var errCoreBackup = errors.New("disk full")

// coreUpdater is a fake Updater: it returns overview and records checks
// and queued versions (queueErr: what QueueUpdate answers).
type coreUpdater struct {
	mu       sync.Mutex
	overview update.Overview
	checks   int
	queued   []string // "version by user"
	queueErr error
}

func (f *coreUpdater) UpdateOverview(context.Context) update.Overview {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.overview
}

func (f *coreUpdater) CheckUpdate(context.Context) update.Overview {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	return f.overview
}

func (f *coreUpdater) QueueUpdate(_ context.Context, version, by string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.queueErr != nil {
		return f.queueErr
	}
	f.queued = append(f.queued, version+" by "+by)
	return nil
}
