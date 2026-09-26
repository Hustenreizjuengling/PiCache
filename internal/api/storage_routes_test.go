package api

import (
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
	cachestore "github.com/hustenreizjuengling/picache/internal/dlcache/store"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/storage"
)

// storageTestRuntime is the part of Runtime the storage routes use;
// ActivateStore behaves like the app (it requires StoreRoot to succeed).
type storageTestRuntime struct {
	st        *storage.Manager
	activated []string
}

func (f *storageTestRuntime) StartedAt() time.Time           { return time.Time{} }
func (f *storageTestRuntime) InstanceID() string             { return "test" }
func (f *storageTestRuntime) Config() *config.Config         { return nil }
func (f *storageTestRuntime) Listeners() ListenerInfo        { return ListenerInfo{} }
func (f *storageTestRuntime) MasterKeySource() string        { return "memory" }
func (f *storageTestRuntime) ActiveStore() *cachestore.Store { return nil }
func (f *storageTestRuntime) StoreState() StoreState {
	if n := len(f.activated); n > 0 {
		return StoreState{TargetID: f.activated[n-1], Online: true}
	}
	return StoreState{TargetID: storage.LocalTargetID}
}
func (f *storageTestRuntime) ActivateStore(_ context.Context, id string) error {
	if _, _, err := f.st.StoreRoot(id); err != nil {
		return err
	}
	f.activated = append(f.activated, id)
	return nil
}
func (f *storageTestRuntime) EvictNow(context.Context) (cachestore.EvictResult, error) {
	return cachestore.EvictResult{}, nil
}
func (f *storageTestRuntime) StartVerify(bool) error                        { return nil }
func (f *storageTestRuntime) VerifyState() VerifyState                      { return VerifyState{} }
func (f *storageTestRuntime) Backup(context.Context, io.Writer, bool) error { return nil }
func (f *storageTestRuntime) StageRestore(context.Context, io.Reader) (*settings.All, error) {
	return nil, nil
}
func (f *storageTestRuntime) Restart()                      {}
func (f *storageTestRuntime) Health(context.Context) Health { return Health{OK: true} }

type storageTestEnv struct {
	srv  *Server
	st   *storage.Manager
	rt   *storageTestRuntime
	auth *auth.Service
	cfg  *config.Config
	db   *db.DB
}

func newStorageTestEnv(t *testing.T) *storageTestEnv {
	t.Helper()
	ctx := context.Background()
	// Resolved: Windows runners have 8.3 short names (RUNNER~1) in the temp
	// path, which the storage path validation refuses.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DataDir: filepath.Join(dir, "data"), CacheDir: filepath.Join(dir, "cache"), MountRoot: filepath.Join(dir, "mnt"),
		DestructiveAPI: true}
	for _, p := range []string{cfg.DataDir, cfg.CacheDir, cfg.MountRoot, filepath.Join(cfg.DataDir, "storage-requests")} {
		if err := os.MkdirAll(p, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	d, err := db.Open(filepath.Join(cfg.DataDir, "picache.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	log := slog.New(slog.DiscardHandler)
	set := openOpenSettings(t, d, log)
	box, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	st, err := storage.New(ctx, d, box, cfg, func() int64 { return 1 << 20 }, log)
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(ctx, d, set, box, filepath.Join(dir, "setup-token"), log)
	if err != nil {
		t.Fatal(err)
	}
	rt := &storageTestRuntime{st: st}
	srv := &Server{d: Deps{Config: cfg, Settings: set, Auth: a, Storage: st, Runtime: rt, Log: log}, log: log, mux: http.NewServeMux()}
	return &storageTestEnv{srv: srv, st: st, rt: rt, auth: a, cfg: cfg, db: d}
}

// call invokes a storage handler directly as an admin (authentication and
// routing are covered by the middleware tests and TestStorageRoutesRegistered).
func (e *storageTestEnv) call(h handlerFunc, method, id, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/v1/storage/targets", strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if id != "" {
		r.SetPathValue("id", id)
	}
	r = r.WithContext(context.WithValue(r.Context(), principalKey,
		&auth.Principal{UserID: 1, Username: "admin", SessionID: "s", Scope: auth.ScopeAdmin}))
	w := httptest.NewRecorder()
	if err := h(w, r); err != nil {
		writeError(w, r, e.srv.log, err)
	}
	return w
}

func storageDecode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
	return v
}

func storageWant(t *testing.T, what string, w *httptest.ResponseRecorder, status int, field string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("%s: status %d, want %d: %s", what, w.Code, status, w.Body)
	}
	if field != "" {
		var e errorBody
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Error.Field != field {
			t.Fatalf("%s: want field %q: %s", what, field, w.Body)
		}
	}
}

func TestStorageRoutesRegistered(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	s.registerStorageRoutes()
	id := "0123456789abcdef0123456789abcdef"
	for _, rc := range []struct{ method, path, pattern string }{
		{"GET", "/api/v1/storage/capabilities", "GET /api/v1/storage/capabilities"},
		{"GET", "/api/v1/storage/targets", "GET /api/v1/storage/targets"},
		{"POST", "/api/v1/storage/targets", "POST /api/v1/storage/targets"},
		{"GET", "/api/v1/storage/targets/local", "GET /api/v1/storage/targets/{id}"},
		{"PUT", "/api/v1/storage/targets/" + id, "PUT /api/v1/storage/targets/{id}"},
		{"DELETE", "/api/v1/storage/targets/" + id, "DELETE /api/v1/storage/targets/{id}"},
		{"POST", "/api/v1/storage/targets/" + id + "/test", "POST /api/v1/storage/targets/{id}/test"},
		{"POST", "/api/v1/storage/targets/" + id + "/apply", "POST /api/v1/storage/targets/{id}/apply"},
		{"POST", "/api/v1/storage/targets/" + id + "/init", "POST /api/v1/storage/targets/{id}/init"},
		{"POST", "/api/v1/storage/targets/" + id + "/activate", "POST /api/v1/storage/targets/{id}/activate"},
		{"GET", "/api/v1/storage/targets/" + id + "/snippets", "GET /api/v1/storage/targets/{id}/snippets"},
		{"POST", "/api/v1/storage/targets/local/benchmark", "POST /api/v1/storage/targets/{id}/benchmark"},
		{"GET", "/api/v1/storage/benchmark", "GET /api/v1/storage/benchmark"},
		{"DELETE", "/api/v1/storage/benchmark", "DELETE /api/v1/storage/benchmark"},
	} {
		if _, p := s.mux.Handler(httptest.NewRequest(rc.method, rc.path, nil)); p != rc.pattern {
			t.Errorf("%s %s → %q, want %q", rc.method, rc.path, p, rc.pattern)
		}
	}
}

func TestStorageRoutesCRUD(t *testing.T) {
	e := newStorageTestEnv(t)
	const pw = "N4s-Passw0rd,secret"

	w := e.call(e.srv.storageTargets, "GET", "", "")
	storageWant(t, "list", w, http.StatusOK, "")
	list := storageDecode[[]storage.TargetWithStatus](t, w)
	if len(list) != 1 || list[0].ID != storage.LocalTargetID || !list[0].Active {
		t.Fatalf("list %+v", list)
	}

	body := `{"name":"NAS","kind":"smb","mode":"external","path":"` + jsonPath(filepath.Join(e.cfg.MountRoot, "nas")) +
		`","server":"192.168.1.10","share":"picache","username":"picache","password":"` + pw + `","smbVersion":"3.1.1"}`
	w = e.call(e.srv.storageCreate, "POST", "", body)
	storageWant(t, "create", w, http.StatusCreated, "")
	if strings.Contains(w.Body.String(), pw) {
		t.Fatalf("create returns the password: %s", w.Body)
	}
	created := storageDecode[storage.Target](t, w)
	if !created.HasPassword || created.Kind != storage.KindSMB {
		t.Fatalf("created %+v", created)
	}

	w = e.call(e.srv.storageTarget, "GET", created.ID, "")
	storageWant(t, "get", w, http.StatusOK, "")
	if got := storageDecode[storage.TargetWithStatus](t, w); got.ID != created.ID || got.Active || strings.Contains(w.Body.String(), pw) {
		t.Fatalf("get %s", w.Body)
	}

	w = e.call(e.srv.storageSnippets, "GET", created.ID, "")
	storageWant(t, "snippets", w, http.StatusOK, "")
	if strings.Contains(w.Body.String(), pw) || !strings.Contains(w.Body.String(), "<your NAS password>") {
		t.Fatalf("snippets %s", w.Body)
	}

	storageWant(t, "host name", e.call(e.srv.storageCreate, "POST", "", strings.Replace(body, "192.168.1.10", "nas.lan", 1)),
		http.StatusBadRequest, "server")
	storageWant(t, "unknown member", e.call(e.srv.storageCreate, "POST", "", `{"name":"x","kind":"local","uid":0}`),
		http.StatusBadRequest, "body")
	storageWant(t, "bad id", e.call(e.srv.storageTarget, "GET", "../../etc", ""), http.StatusBadRequest, "id")
	storageWant(t, "unknown id", e.call(e.srv.storageTarget, "GET", "ffffffffffffffffffffffffffffffff", ""), http.StatusNotFound, "")

	w = e.call(e.srv.storageUpdate, "PUT", created.ID, strings.Replace(body, `"name":"NAS"`, `"name":"Renamed"`, 1))
	storageWant(t, "update", w, http.StatusOK, "")
	if got := storageDecode[storage.Target](t, w); got.Name != "Renamed" || !got.HasPassword {
		t.Fatalf("update %+v", got)
	}

	storageWant(t, "delete local", e.call(e.srv.storageDelete, "DELETE", storage.LocalTargetID, ""), http.StatusForbidden, "")
	storageWant(t, "delete", e.call(e.srv.storageDelete, "DELETE", created.ID, ""), http.StatusNoContent, "")
	storageWant(t, "deleted", e.call(e.srv.storageTarget, "GET", created.ID, ""), http.StatusNotFound, "")

	entries, _, err := e.auth.AuditLog(context.Background(), auth.AuditQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, a := range entries {
		actions = append(actions, a.Action)
		if strings.Contains(a.Details, pw) {
			t.Fatalf("audit leaks the password: %+v", a)
		}
	}
	for _, want := range []string{"storage.create", "storage.update", "storage.delete"} {
		if !strings.Contains(strings.Join(actions, ","), want) {
			t.Errorf("audit %v lacks %s", actions, want)
		}
	}
}

func TestStorageRoutesInitActivateTest(t *testing.T) {
	e := newStorageTestEnv(t)
	dir := filepath.Join(e.cfg.MountRoot, "disk")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	w := e.call(e.srv.storageCreate, "POST", "", `{"name":"Disk","kind":"local","path":"`+jsonPath(dir)+`"}`)
	storageWant(t, "create", w, http.StatusCreated, "")
	id := storageDecode[storage.Target](t, w).ID

	w = e.call(e.srv.storageTest, "POST", id, "")
	storageWant(t, "test", w, http.StatusOK, "")
	// The UI decides "set up" vs "offline" from explicit fields.
	if body := w.Body.String(); !strings.Contains(body, `"initialised":false`) || !strings.Contains(body, `"writable":true`) {
		t.Fatalf("status fields before init: %s", body)
	}
	if res := storageDecode[storage.TestResult](t, w); !res.OK || len(res.Steps) == 0 || res.Status.Online {
		t.Fatalf("test %+v", res)
	}
	storageWant(t, "test unknown", e.call(e.srv.storageTest, "POST", "ffffffffffffffffffffffffffffffff", ""), http.StatusNotFound, "")

	storageWant(t, "activate before init", e.call(e.srv.storageActivate, "POST", id, ""), http.StatusConflict, "")
	storageWant(t, "init without body", e.call(e.srv.storageInit, "POST", id, ""), http.StatusBadRequest, "body")
	storageWant(t, "adopt nothing", e.call(e.srv.storageInit, "POST", id, `{"adopt":true}`), http.StatusConflict, "")

	w = e.call(e.srv.storageInit, "POST", id, `{"adopt":false}`)
	storageWant(t, "init", w, http.StatusOK, "")
	if res := storageDecode[storage.InitResult](t, w); len(res.StoreID) != 32 || res.Adopted {
		t.Fatalf("init %+v", res)
	}
	w = e.call(e.srv.storageTarget, "GET", id, "")
	storageWant(t, "get after init", w, http.StatusOK, "")
	if body := w.Body.String(); !strings.Contains(body, `"initialised":true`) || !strings.Contains(body, `"writable":true`) {
		t.Fatalf("status fields after init: %s", body)
	}

	w = e.call(e.srv.storageActivate, "POST", id, "")
	storageWant(t, "activate", w, http.StatusOK, "")
	if st := storageDecode[StoreState](t, w); st.TargetID != id || len(e.rt.activated) != 1 {
		t.Fatalf("activate %+v %v", st, e.rt.activated)
	}
	storageWant(t, "activate unknown", e.call(e.srv.storageActivate, "POST", "ffffffffffffffffffffffffffffffff", ""), http.StatusNotFound, "")
}

func TestStorageRoutesApplyAndCapabilities(t *testing.T) {
	e := newStorageTestEnv(t)
	w := e.call(e.srv.storageCapabilities, "GET", "", "")
	storageWant(t, "capabilities", w, http.StatusOK, "")
	if c := storageDecode[storage.Capabilities](t, w); c.MountRoot != e.cfg.MountRoot || c.Filesystems == nil {
		t.Fatalf("capabilities %+v", c)
	}

	w = e.call(e.srv.storageCreate, "POST", "", `{"name":"NFS","kind":"nfs","mode":"host-apply","server":"192.168.1.20","export":"/volume1/cache"}`)
	storageWant(t, "create", w, http.StatusCreated, "")
	nfs := storageDecode[storage.Target](t, w)
	if nfs.Path != filepath.Join(e.cfg.MountRoot, nfs.ID) {
		t.Fatalf("host-apply path %q", nfs.Path)
	}
	want := http.StatusServiceUnavailable // the root helper is not installed on test machines
	if _, err := os.Stat(storage.HostApplyFlagFile); err == nil {
		want = http.StatusOK
	}
	storageWant(t, "apply", e.call(e.srv.storageApply, "POST", nfs.ID, ""), want, "")
	storageWant(t, "apply local", e.call(e.srv.storageApply, "POST", storage.LocalTargetID, ""), http.StatusConflict, "")
}

// TestStorageRoutesBenchmark runs a speed test of the built-in store through
// the real middleware: read tokens see results, only admins start and
// cancel; the start is audited.
func TestStorageRoutesBenchmark(t *testing.T) {
	e := newStorageTestEnv(t)
	e.srv.d.Config.WebListen = []string{":8080"}
	ce := &coreEnv{srv: New(e.srv.d), auth: e.auth}
	session := ce.provisionAndLogin(t)
	readTok := ce.createToken(t, session, "read")
	const start = "/api/v1/storage/targets/local/benchmark"

	w := ce.do("GET", "/api/v1/storage/benchmark", "", readTok)
	if w.Code != http.StatusOK || w.Body.String() != `{"last":{}}` {
		t.Fatalf("empty overview: %d %s", w.Code, w.Body)
	}
	coreWantError(t, ce.do("POST", start, `{"sizeMiB":64}`, readTok), http.StatusForbidden, "forbidden", "")
	coreWantError(t, ce.do("DELETE", "/api/v1/storage/benchmark", "", readTok), http.StatusForbidden, "forbidden", "")
	coreWantError(t, ce.do("GET", "/api/v1/storage/benchmark", "", ""), http.StatusUnauthorized, "unauthorized", "")

	coreWantError(t, ce.do("POST", start, `{"sizeMiB":100}`, session), http.StatusBadRequest, "invalid", "sizeMiB")
	coreWantError(t, ce.do("POST", start, `{"size":64}`, session), http.StatusBadRequest, "invalid", "body")
	coreWantError(t, ce.do("POST", "/api/v1/storage/targets/ffffffffffffffffffffffffffffffff/benchmark", "", session),
		http.StatusNotFound, "not_found", "")
	// Without a body the default size is used; a missing directory is not available.
	missing := e.call(e.srv.storageCreate, "POST", "", `{"name":"Gone","kind":"local","path":"`+jsonPath(filepath.Join(e.cfg.MountRoot, "gone"))+`"}`)
	gone := storageDecode[storage.Target](t, missing).ID
	w = ce.do("POST", "/api/v1/storage/targets/"+gone+"/benchmark", "", session)
	coreWantError(t, w, http.StatusConflict, "conflict", "")
	if !strings.Contains(w.Body.String(), "the storage target is not available: ") {
		t.Fatalf("unavailable: %s", w.Body)
	}

	w = ce.do("POST", start, `{"sizeMiB":64}`, session)
	if w.Code == http.StatusBadRequest && strings.Contains(w.Body.String(), "not enough free space") {
		t.Skipf("the temp directory lacks the free space of a speed test: %s", w.Body)
	}
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	var run storage.BenchmarkRun
	coreDecode(t, w, &run)
	if run.State != "running" || run.TargetID != storage.LocalTargetID || run.SizeMiB != 64 || run.Phase != "prepare" {
		t.Fatalf("started %+v", run)
	}
	// Cancel (the run may also have finished already); DELETE waits for the end.
	if w := ce.do("DELETE", "/api/v1/storage/benchmark", "", session); w.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	var o storage.BenchmarkOverview
	coreDecode(t, ce.do("GET", "/api/v1/storage/benchmark", "", readTok), &o)
	if o.Run == nil || (o.Run.State != "cancelled" && o.Run.State != "done") || o.Run.FinishedAt.IsZero() {
		t.Fatalf("after cancel: %+v", o.Run)
	}
	if ents, _ := os.ReadDir(filepath.Join(e.cfg.CacheDir, "tmp")); len(ents) != 0 {
		t.Fatalf("test files left: %v", ents)
	}
	if w := ce.do("DELETE", "/api/v1/storage/benchmark", "", session); w.Code != http.StatusNoContent {
		t.Fatalf("cancel without a run: %d", w.Code)
	}

	entries, _, err := e.auth.AuditLog(context.Background(), auth.AuditQuery{Search: "storage.benchmark"})
	if err != nil {
		t.Fatal(err)
	}
	var starts int
	for _, a := range entries {
		if a.Action == "storage.benchmark" {
			starts++
			if a.Target != storage.LocalTargetID || a.Details != `{"sizeMiB":64}` {
				t.Fatalf("audit entry %+v", a)
			}
		}
	}
	if starts != 1 { // refused starts are not audited
		t.Fatalf("audit %+v", entries)
	}
}

// jsonPath escapes a file system path for a JSON string literal (Windows).
func jsonPath(p string) string { return strings.ReplaceAll(p, `\`, `\\`) }
