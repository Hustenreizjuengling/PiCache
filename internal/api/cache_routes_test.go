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
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
	cachestore "github.com/hustenreizjuengling/picache/internal/lancache/store"
	"github.com/hustenreizjuengling/picache/internal/listing"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// cacheRuntime is the part of Runtime the cache routes use.
type cacheRuntime struct {
	Runtime // other methods are not used by the cache routes

	mu     sync.Mutex
	store  *cachestore.Store
	verify VerifyState
}

func (r *cacheRuntime) ActiveStore() *cachestore.Store {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.store
}

func (r *cacheRuntime) StoreState() StoreState {
	st := r.ActiveStore()
	if st == nil {
		return StoreState{TargetID: "local", PassThrough: true, Reason: "offline"}
	}
	u := st.Usage()
	return StoreState{TargetID: "local", StoreID: st.ID(), Online: true, Usage: &u, SliceSize: st.SliceSize()}
}

func (r *cacheRuntime) EvictNow(ctx context.Context) (cachestore.EvictResult, error) {
	st := r.ActiveStore()
	if st == nil {
		return cachestore.EvictResult{}, apperr.Unavailable("no cache store is online")
	}
	return st.Evict(ctx, cachestore.Policy{MaxBytes: 1})
}

func (r *cacheRuntime) StartVerify(repair bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.verify = VerifyState{Running: true, Repair: repair, StartedAt: time.Now().UTC()}
	return nil
}

func (r *cacheRuntime) VerifyState() VerifyState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.verify
}

type cacheEnv struct {
	t       *testing.T
	srv     *Server
	rt      *cacheRuntime
	session string
}

const cacheTestHost = "192.168.1.2:8080"

func newCacheEnv(t *testing.T) *cacheEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "picache.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	set, err := settings.Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(ctx, d, set, box, filepath.Join(dir, "setup-token"), log)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DataDir: dir, CacheDir: filepath.Join(dir, "cache"), MountRoot: filepath.Join(dir, "mnt"),
		WebListen: []string{":8080"}}
	e := &cacheEnv{t: t, rt: &cacheRuntime{}}
	e.srv = New(Deps{Config: cfg, Settings: set, Auth: a, Runtime: e.rt, Log: log})
	const password = "cache route test password"
	if err := a.Provision(ctx, "admin", password); err != nil {
		t.Fatal(err)
	}
	w := e.do("POST", "/api/v1/auth/login", `{"username":"admin","password":"`+password+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SessionCookie {
			e.session = c.Value
		}
	}
	if e.session == "" {
		t.Fatal("no session cookie")
	}
	return e
}

// openStore creates a cache store with a few objects and makes it active.
func (e *cacheEnv) openStore() *cachestore.Store {
	t := e.t
	root := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	const id = "00112233445566778899aabbccddeeff"
	if _, err := cachestore.InitRoot(root, id, 256<<10); err != nil {
		t.Fatal(err)
	}
	st, err := cachestore.Open(context.Background(), cachestore.Options{Root: root, StoreID: id,
		IndexPath: filepath.Join(t.TempDir(), id+".db"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, o := range []struct {
		service, path, group string
		size                 int
	}{
		{"steam", "/depot/1/a", "steam:a", 3000},
		{"steam", "/depot/1/b", "steam:a", 1000},
		{"steam", "/depot/2/c", "steam:b", 500},
		{"epicgames", "/Builds/x", "epic:x", 700},
	} {
		oid := cachestore.ObjectID(o.service, o.path)
		gen, err := st.SetMeta(context.Background(), oid, cachestore.Meta{Service: o.service, Host: "cdn.example.com",
			Path: o.path, GroupKey: o.group, Total: int64(o.size)})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.WriteSlice(context.Background(), oid, gen, 0, make([]byte, o.size)); err != nil {
			t.Fatal(err)
		}
	}
	// Index writes are batched; an eviction pass without limits flushes them.
	if _, err := st.Evict(context.Background(), cachestore.Policy{}); err != nil {
		t.Fatal(err)
	}
	e.rt.mu.Lock()
	e.rt.store = st
	e.rt.mu.Unlock()
	return st
}

func (e *cacheEnv) do(method, target, body string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	r.Host = cacheTestHost
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if e.session != "" {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: e.session})
	}
	w := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(w, r)
	return w
}

func (e *cacheEnv) expect(method, target, body string, status int, out any) {
	e.t.Helper()
	w := e.do(method, target, body)
	if w.Code != status {
		e.t.Fatalf("%s %s: status %d, want %d: %s", method, target, w.Code, status, w.Body)
	}
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			e.t.Fatalf("%s %s: decode %s: %v", method, target, w.Body, err)
		}
	}
}

func (e *cacheEnv) expectError(method, target, body string, status int, code string) {
	e.t.Helper()
	var out errorBody
	e.expect(method, target, body, status, &out)
	if out.Error.Code != code {
		e.t.Fatalf("%s %s: error code %q, want %q", method, target, out.Error.Code, code)
	}
}

func TestCacheRoutesWithoutStore(t *testing.T) {
	e := newCacheEnv(t)
	var st StoreState
	e.expect("GET", "/api/v1/cache/state", "", http.StatusOK, &st)
	if st.Online || !st.PassThrough {
		t.Fatalf("state %+v", st)
	}
	for _, r := range []struct{ method, path, body string }{
		{"GET", "/api/v1/cache/services", ""},
		{"GET", "/api/v1/cache/groups", ""},
		{"GET", "/api/v1/cache/groups/detail?service=steam&key=steam:a", ""},
		{"GET", "/api/v1/cache/objects", ""},
		{"DELETE", "/api/v1/cache/objects/00112233445566778899aabbccddeeff", ""},
		{"POST", "/api/v1/cache/objects/00112233445566778899aabbccddeeff/pin", `{"pinned":true}`},
		{"POST", "/api/v1/cache/groups/delete", `{"service":"steam","key":"steam:a"}`},
		{"POST", "/api/v1/cache/groups/pin", `{"service":"steam","key":"steam:a","pinned":true}`},
		{"POST", "/api/v1/cache/services/steam/purge", ""},
		{"POST", "/api/v1/cache/evict", ""},
		{"POST", "/api/v1/cache/verify", `{"repair":false}`},
		{"GET", "/api/v1/cache/verify", ""},
	} {
		e.expectError(r.method, r.path, r.body, http.StatusServiceUnavailable, "unavailable")
	}
	e.session = ""
	e.expectError("GET", "/api/v1/cache/groups", "", http.StatusUnauthorized, "unauthorized")
}

func TestCacheLibraryRoutes(t *testing.T) {
	e := newCacheEnv(t)
	st := e.openStore()

	var svc []cachestore.ServiceUsage
	e.expect("GET", "/api/v1/cache/services", "", http.StatusOK, &svc)
	if len(svc) != 2 || svc[0].Service != "steam" || svc[0].Objects != 3 {
		t.Fatalf("services %+v", svc)
	}

	var groups listing.Page[GroupView]
	e.expect("GET", "/api/v1/cache/groups?sort=bytes&desc=true&limit=2", "", http.StatusOK, &groups)
	if groups.Total != 3 || len(groups.Items) != 2 || groups.Items[0].GroupKey != "steam:a" ||
		groups.Items[0].CachedBytes != 4000 || groups.Items[0].Label != "steam:a" || groups.Items[0].Clients != 0 ||
		groups.Items[0].ExpiresAt.IsZero() {
		t.Fatalf("groups %+v", groups)
	}
	// GroupView inlines the store aggregate.
	w := e.do("GET", "/api/v1/cache/groups?service=epicgames", "")
	for _, member := range []string{`"groupKey":"epic:x"`, `"cachedBytes":700`, `"label":"epic:x"`, `"clients":0`} {
		if !strings.Contains(w.Body.String(), member) {
			t.Fatalf("groups JSON %s lacks %s", w.Body, member)
		}
	}
	e.expect("GET", "/api/v1/cache/groups?search=STEAM:B", "", http.StatusOK, &groups)
	if groups.Total != 1 || groups.Items[0].GroupKey != "steam:b" {
		t.Fatalf("search %+v", groups)
	}
	e.expectError("GET", "/api/v1/cache/groups?sort=size", "", http.StatusBadRequest, "invalid")
	e.expectError("GET", "/api/v1/cache/groups?limit=x", "", http.StatusBadRequest, "invalid")

	var detail struct {
		Group   GroupView                       `json:"group"`
		Clients []map[string]any                `json:"clients"`
		Objects listing.Page[cachestore.Object] `json:"objects"`
	}
	e.expect("GET", "/api/v1/cache/groups/detail?service=steam&key=steam:a&sort=size&desc=true", "", http.StatusOK, &detail)
	if detail.Group.Objects != 2 || detail.Clients == nil || len(detail.Clients) != 0 || detail.Objects.Total != 2 ||
		detail.Objects.Items[0].Path != "/depot/1/a" {
		t.Fatalf("detail %+v", detail)
	}
	e.expectError("GET", "/api/v1/cache/groups/detail?service=steam", "", http.StatusBadRequest, "invalid")
	e.expectError("GET", "/api/v1/cache/groups/detail?service=steam&key=steam:zzz", "", http.StatusNotFound, "not_found")

	var objs listing.Page[cachestore.Object]
	e.expect("GET", "/api/v1/cache/objects?group=steam:a&sort=path", "", http.StatusOK, &objs)
	if objs.Total != 2 || objs.Items[0].Path != "/depot/1/a" || objs.Items[1].Path != "/depot/1/b" {
		t.Fatalf("objects %+v", objs)
	}
	e.expect("GET", "/api/v1/cache/objects?search=builds", "", http.StatusOK, &objs)
	if objs.Total != 1 || objs.Items[0].Service != "epicgames" {
		t.Fatalf("object search %+v", objs)
	}

	// A store closed underneath a request (store switch) reads as offline.
	st.Close()
	e.expectError("GET", "/api/v1/cache/groups", "", http.StatusServiceUnavailable, "unavailable")
}

func TestCacheAdminRoutes(t *testing.T) {
	e := newCacheEnv(t)
	st := e.openStore()
	ctx := context.Background()
	a := cachestore.ObjectID("steam", "/depot/1/a")

	e.expectError("DELETE", "/api/v1/cache/objects/not-an-id", "", http.StatusBadRequest, "invalid")
	e.expectError("POST", "/api/v1/cache/objects/"+a+"/pin", `{}`, http.StatusBadRequest, "invalid")
	e.expectError("POST", "/api/v1/cache/objects/"+a+"/pin", `{"pinned":true,"x":1}`, http.StatusBadRequest, "invalid")
	e.expect("POST", "/api/v1/cache/objects/"+a+"/pin", `{"pinned":true}`, http.StatusNoContent, nil)
	objs, err := st.Objects(ctx, cachestore.ObjectQuery{Search: "/depot/1/a"})
	if err != nil || len(objs.Items) != 1 || !objs.Items[0].Pinned {
		t.Fatalf("pin: %+v %v", objs, err)
	}

	e.expect("POST", "/api/v1/cache/groups/pin", `{"service":"steam","key":"steam:b","pinned":true}`, http.StatusNoContent, nil)
	g, err := st.Groups(ctx, cachestore.GroupQuery{GroupKey: "steam:b"})
	if err != nil || len(g.Items) != 1 || !g.Items[0].Pinned {
		t.Fatalf("group pin: %+v %v", g, err)
	}
	e.expectError("POST", "/api/v1/cache/groups/pin", `{"service":"steam","key":"steam:b"}`, http.StatusBadRequest, "invalid")

	// Evict with a 1-byte limit: everything unpinned goes, the store is full.
	var res cachestore.EvictResult
	e.expect("POST", "/api/v1/cache/evict", "", http.StatusOK, &res)
	if res.Objects != 2 || !res.Full || res.Reasons["size"] != 2 {
		t.Fatalf("evict %+v", res)
	}

	var freed struct {
		BytesFreed int64 `json:"bytesFreed"`
	}
	e.expect("POST", "/api/v1/cache/groups/delete", `{"service":"steam","key":"steam:b"}`, http.StatusOK, &freed)
	if freed.BytesFreed != 500 {
		t.Fatalf("group delete freed %d", freed.BytesFreed)
	}
	e.expectError("POST", "/api/v1/cache/groups/delete", `{"service":"steam","key":"steam:b","pinned":true}`,
		http.StatusBadRequest, "invalid")
	e.expectError("POST", "/api/v1/cache/services/Not%20Valid/purge", "", http.StatusBadRequest, "invalid")
	e.expect("POST", "/api/v1/cache/services/steam/purge", "", http.StatusOK, &freed)
	if freed.BytesFreed != 3000 {
		t.Fatalf("purge freed %d", freed.BytesFreed)
	}
	e.expectError("DELETE", "/api/v1/cache/objects/"+a, "", http.StatusNotFound, "not_found")

	var vs VerifyState
	e.expect("POST", "/api/v1/cache/verify", `{"repair":true}`, http.StatusAccepted, &vs)
	if !vs.Running || !vs.Repair {
		t.Fatalf("verify %+v", vs)
	}
	e.expect("GET", "/api/v1/cache/verify", "", http.StatusOK, &vs)
	if !vs.Running {
		t.Fatalf("verify state %+v", vs)
	}
	e.expectError("POST", "/api/v1/cache/verify", `{"repair":"yes"}`, http.StatusBadRequest, "invalid")
}
