package prefill

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/prefill/prefillapi"
	"github.com/hustenreizjuengling/picache/internal/prefill/steam"
	"github.com/hustenreizjuengling/picache/internal/prefill/steam/steamtest"
)

type fakeSteam struct{ fs *steamtest.Server }

func (f fakeSteam) options(used func(host, protocol, purpose string), log *slog.Logger) steam.Options {
	return steam.Options{DirectoryURL: f.fs.DirectoryURL(), Plain: true, Used: used, Log: log}
}

func testApps() map[uint32]steamtest.App {
	return map[uint32]steamtest.App{
		100: {Name: "Alpha", Type: "Game", Depots: []steamtest.Depot{
			{ID: 101, Manifest: 1101, OS: "windows", Chunks: 6, ChunkSize: 1000},
			{ID: 102, Manifest: 1102, OS: "linux", Chunks: 2, ChunkSize: 1000},
		}},
		200: {Name: "Beta", Type: "Game", Depots: []steamtest.Depot{{ID: 201, Manifest: 1201, Chunks: 3, ChunkSize: 300}}},
	}
}

type env struct {
	t      *testing.T
	fs     *steamtest.Server
	c      *prefillapi.Client
	dir    string
	cancel context.CancelFunc
	done   chan error
}

func newEnv(t *testing.T) *env { return newEnvWith(t, nil) }

// newEnvWith starts the service; mod changes its configuration.
func newEnvWith(t *testing.T, mod func(*Config)) *env {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the prefill service needs Linux (SO_PEERCRED)")
	}
	fs := steamtest.New(steamtest.Account{Name: "gamer", SteamID: 76561198000000001,
		Packages: map[uint32][]uint32{10: {100, 200}}}, testApps())
	t.Cleanup(fs.Close)
	dir := t.TempDir()
	sock := filepath.Join(dir, "p.sock")
	cfg := Config{
		StateDir: filepath.Join(dir, "state"), Socket: sock, CacheURL: "http://" + fs.Addr(),
		DeviceName: "PiCache Prefill (test)", Version: "test",
		Log:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		steam: fakeSteam{fs},
	}
	if mod != nil {
		mod(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &env{t: t, fs: fs, c: prefillapi.NewClient(sock), dir: dir, cancel: cancel, done: make(chan error, 1)}
	go func() { e.done <- Run(ctx, cfg) }()
	t.Cleanup(e.stop)
	e.waitFor("the socket", func() bool { return e.c.Installed() })
	return e
}

func (e *env) stop() {
	if e.cancel == nil {
		return
	}
	e.cancel()
	e.cancel = nil
	select {
	case err := <-e.done:
		if err != nil {
			e.t.Errorf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		e.t.Error("Run did not stop")
	}
}

func (e *env) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// call sends a request and decodes a 2xx answer into out.
func (e *env) call(method, path string, body, out any, want int) {
	e.t.Helper()
	r, err := e.c.Do(context.Background(), method, path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if r.Status != want {
		e.t.Fatalf("%s %s: HTTP %d %s, want %d", method, path, r.Status, r.Body, want)
	}
	if out != nil {
		if err := json.Unmarshal(r.Body, out); err != nil {
			e.t.Fatalf("%s %s: %v", method, path, err)
		}
	}
}

// loadState reads a state file of the service.
func (e *env) loadState(name string, v any) error {
	b, err := os.ReadFile(filepath.Join(e.dir, "state", name))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (e *env) status() prefillapi.Status {
	var st prefillapi.Status
	e.call("GET", "/v1/status", nil, &st, 200)
	return st
}

func (e *env) runJob(force bool) prefillapi.Job {
	e.t.Helper()
	var j prefillapi.Job
	e.call("POST", "/v1/jobs", prefillapi.StartJob{Force: force}, &j, http.StatusAccepted)
	e.waitFor("the job", func() bool { return e.status().Job == nil })
	var h prefillapi.History
	e.call("GET", "/v1/history", nil, &h, 200)
	if len(h.Jobs) == 0 || h.Jobs[0].ID != j.ID {
		e.t.Fatalf("history %+v", h)
	}
	return h.Jobs[0]
}

func TestDaemonSteamFlow(t *testing.T) {
	e := newEnv(t)
	st := e.status()
	if st.Steam.Account != nil || st.Settings.Connections != 8 || !slices.Equal(st.Settings.Steam.OS, []string{"windows"}) {
		t.Fatalf("initial status %+v", st)
	}
	// Without a sign-in there are no games and nothing to prefill.
	e.call("GET", "/v1/steam/apps", nil, nil, http.StatusConflict)
	e.call("POST", "/v1/jobs", nil, nil, http.StatusConflict)

	// QR sign-in: the code waits, is scanned, then confirmed.
	e.fs.ConfirmAfter(3)
	var l prefillapi.SteamLogin
	e.call("POST", "/v1/steam/login", nil, &l, 200)
	if l.State != "waiting" || l.ChallengeURL != "https://s.team/q/1/1001" {
		t.Fatalf("login %+v", l)
	}
	// Asking again returns the pending sign-in.
	e.call("POST", "/v1/steam/login", nil, &l, 200)
	e.waitFor("the sign-in", func() bool { return e.status().Steam.Account != nil })
	st = e.status()
	a := st.Steam.Account
	if a.Name != "gamer" || a.SteamID != "76561198000000001" || a.DeviceName != "PiCache Prefill (test)" || a.Expires == nil || st.Steam.Login.State != "done" {
		t.Fatalf("account %+v login %+v", a, st.Steam.Login)
	}
	// The token is stored 0600 and never in the status.
	fi, err := os.Stat(filepath.Join(e.dir, "state", fileSteam))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("steam.json %v %v", fi, err)
	}
	r, _ := e.c.Do(context.Background(), "GET", "/v1/status", nil)
	if strings.Contains(string(r.Body), e.fs.Token()) {
		t.Fatal("the status contains the token")
	}
	e.call("POST", "/v1/steam/login", nil, nil, http.StatusConflict)

	var apps prefillapi.SteamApps
	e.call("GET", "/v1/steam/apps", nil, &apps, 200)
	if len(apps.Apps) != 2 || apps.Apps[0].Name != "Alpha" || apps.Apps[0].Bytes != 6000 || apps.Apps[1].Bytes != 900 || apps.Error != "" {
		t.Fatalf("apps %+v", apps)
	}
	var sel prefillapi.Selection
	e.call("PUT", "/v1/steam/selection", prefillapi.Selection{Apps: []uint32{200, 100, 100}}, &sel, 200)
	if !slices.Equal(sel.Apps, []uint32{100, 200}) {
		t.Fatalf("selection %v", sel.Apps)
	}

	// The first job downloads both games through the cache.
	j := e.runJob(false)
	if j.State != prefillapi.JobDone || len(j.Apps) != 2 || j.Apps[0].State != prefillapi.AppDone || j.Apps[1].State != prefillapi.AppDone {
		t.Fatalf("job %+v", j)
	}
	if j.Total != 6900 || j.Bytes < 6900 || j.FromCache != 0 {
		t.Errorf("job bytes %d of %d, from cache %d", j.Bytes, j.Total, j.FromCache)
	}
	reqs := e.fs.Requests()
	want := append(steamtest.ChunkPaths(testApps()[100].Depots[0]), steamtest.ChunkPaths(testApps()[200].Depots[0])...)
	var chunks []string
	for _, rq := range reqs {
		if rq.UserAgent != steam.UserAgent || rq.Host != "cache1.example" || !rq.Prefill {
			t.Errorf("request %+v", rq)
		}
		if strings.Contains(rq.Path, "/chunk/") {
			chunks = append(chunks, rq.Path)
		}
	}
	slices.Sort(chunks)
	slices.Sort(want)
	if !slices.Equal(chunks, want) {
		t.Errorf("chunks %v\nwant %v", chunks, want)
	}
	if !slices.ContainsFunc(j.Hosts, func(h string) bool { return h == "http://cache1.example" }) {
		t.Errorf("hosts %v", j.Hosts)
	}
	e.call("GET", "/v1/steam/apps", nil, &apps, 200)
	if apps.Apps[0].Prefilled == nil || !apps.Apps[0].Current {
		t.Errorf("after the job %+v", apps.Apps[0])
	}

	// The second job finds both current.
	e.fs.ResetRequests()
	j = e.runJob(false)
	if j.State != prefillapi.JobDone || j.Apps[0].State != prefillapi.AppCurrent || j.Apps[1].State != prefillapi.AppCurrent {
		t.Fatalf("second job %+v", j)
	}
	if n := len(e.fs.Requests()); n != 0 {
		t.Errorf("second job made %d requests", n)
	}

	// A forced job: the first server refuses depot 201, the next serves it;
	// the cache now has every chunk.
	e.fs.Forbid("cache1.example", 201)
	e.fs.ResetRequests()
	j = e.runJob(true)
	if j.State != prefillapi.JobDone || j.FromCache < 6900 {
		t.Fatalf("forced job %+v", j)
	}
	served := false
	for _, rq := range e.fs.Requests() {
		if rq.Host == "cache2.example" && strings.HasPrefix(rq.Path, "/depot/201/chunk/") {
			served = true
		}
	}
	if !served {
		t.Error("depot 201 was not fetched from the second server")
	}

	// Signing out revokes the token at Steam and deletes it.
	e.call("POST", "/v1/steam/logout", nil, nil, http.StatusNoContent)
	if r := e.fs.Revoked(); len(r) != 1 || r[0] != e.fs.Token() {
		t.Errorf("revoked %v", r)
	}
	if e.status().Steam.Account != nil {
		t.Error("still signed in")
	}
	if _, err := os.Stat(filepath.Join(e.dir, "state", fileSteam)); !os.IsNotExist(err) {
		t.Errorf("steam.json after the sign-out: %v", err)
	}
	e.call("POST", "/v1/steam/logout", nil, nil, http.StatusNotFound)
}

func TestDaemonRejectedTokenAndRestart(t *testing.T) {
	e := newEnv(t)
	e.fs.ConfirmAfter(1)
	e.call("POST", "/v1/steam/login", nil, nil, 200)
	e.waitFor("the sign-in", func() bool { return e.status().Steam.Account != nil })
	e.call("PUT", "/v1/steam/selection", prefillapi.Selection{Apps: []uint32{200}}, nil, 200)

	// Steam revokes the token: the job fails and the account says so.
	e.fs.RejectToken(true)
	j := e.runJob(false)
	if j.State != prefillapi.JobFailed || !strings.Contains(j.Error, "revoked") {
		t.Fatalf("job %+v", j)
	}
	a := e.status().Steam.Account
	if a == nil || !strings.Contains(a.Error, "sign in again") {
		t.Fatalf("account %+v", a)
	}
	// A new sign-in is allowed now.
	e.fs.RejectToken(false)
	e.fs.ConfirmAfter(1)
	e.call("POST", "/v1/steam/login", nil, nil, 200)
	e.waitFor("the new sign-in", func() bool {
		a := e.status().Steam.Account
		return a != nil && a.Error == ""
	})
	// The LoginID and machine ID stay across sign-ins.
	logons := e.fs.Logons()
	if len(logons) < 2 || logons[0].LoginID == 0 {
		t.Fatalf("logons %+v", logons)
	}

	// The state survives a restart.
	e.stop()
	var acct steamAccount
	st, err := openStore(filepath.Join(e.dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.load(fileSteam, &acct); err != nil || acct.Name != "gamer" || acct.LoginID != logons[0].LoginID {
		t.Fatalf("stored account %+v %v", acct, err)
	}
	st.close()
}

func TestSettingsValidation(t *testing.T) {
	ok := defaultSettings()
	if _, err := validateSettings(ok); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(*prefillapi.Settings){
		"start":       func(s *prefillapi.Settings) { s.Schedule.Start = "3:00" },
		"end":         func(s *prefillapi.Settings) { s.Schedule.End = "24:00" },
		"day":         func(s *prefillapi.Settings) { s.Schedule.Days = []int{7} },
		"no days":     func(s *prefillapi.Settings) { s.Schedule.Enabled, s.Schedule.Days = true, nil },
		"connections": func(s *prefillapi.Settings) { s.Connections = 33 },
		"mbps":        func(s *prefillapi.Settings) { s.MaxMbps = -1 },
		"os":          func(s *prefillapi.Settings) { s.Steam.OS = []string{"amiga"} },
		"no os":       func(s *prefillapi.Settings) { s.Steam.OS = nil },
		"language":    func(s *prefillapi.Settings) { s.Steam.Languages = []string{"klingon"} },
	} {
		s := cloneSettings(ok)
		f(&s)
		if _, err := validateSettings(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	s := cloneSettings(ok)
	s.Steam.Languages = []string{"german", "english", "german"}
	s, err := validateSettings(s)
	if err != nil || !slices.Equal(s.Steam.Languages, []string{"english", "german"}) {
		t.Errorf("normalised %v %v", s.Steam.Languages, err)
	}
}

func TestWindow(t *testing.T) {
	loc := time.FixedZone("test", 3600)
	at := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, loc) } // 2026-10-19 is a Monday
	s := prefillapi.Schedule{Enabled: true, Days: []int{1, 3}, Start: "23:00", End: "02:00"}
	for _, c := range []struct {
		now        time.Time
		start, end time.Time
	}{
		{at(19, 12, 0), at(19, 23, 0), at(20, 2, 0)},  // Monday noon: tonight
		{at(19, 23, 30), at(19, 23, 0), at(20, 2, 0)}, // inside
		{at(20, 1, 59), at(19, 23, 0), at(20, 2, 0)},  // inside, after midnight
		{at(20, 2, 0), at(21, 23, 0), at(22, 2, 0)},   // ended: Wednesday
		{at(22, 3, 0), at(26, 23, 0), at(27, 2, 0)},   // next Monday
	} {
		st, en, ok := window(s, c.now)
		if !ok || !st.Equal(c.start) || !en.Equal(c.end) {
			t.Errorf("%s: %s–%s %v, want %s–%s", c.now, st, en, ok, c.start, c.end)
		}
	}
	if _, _, ok := window(prefillapi.Schedule{Days: []int{1}, Start: "01:00", End: "02:00"}, at(19, 0, 0)); ok {
		t.Error("a disabled schedule has a window")
	}
	full := prefillapi.Schedule{Enabled: true, Days: []int{0, 1, 2, 3, 4, 5, 6}, Start: "00:00", End: "00:00"}
	if st, en, ok := window(full, at(19, 5, 0)); !ok || !st.Equal(at(19, 0, 0)) || !en.Equal(at(20, 0, 0)) {
		t.Errorf("24 h window %s–%s", st, en)
	}
}

func TestPeerAllowed(t *testing.T) {
	d := &Daemon{cfg: Config{ClientUIDs: []int{4242}}}
	cases := map[int]bool{0: true, 4242: true, 4243: os.Getuid() == 4243, -1: false}
	if u := os.Getuid(); u >= 0 { // -1 outside Unix
		cases[u] = true
	}
	for uid, want := range cases {
		if got := d.peerAllowed(context.WithValue(context.Background(), ctxKeyPeer{}, uid)); got != want {
			t.Errorf("uid %d: %v", uid, got)
		}
	}
	if d.peerAllowed(context.Background()) {
		t.Error("no peer allowed")
	}
}

// systemd's StateDirectory= with DynamicUser=yes is a symbolic link.
func TestStoreSymlinkedDir(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "private", "picache-prefill")
	if err := os.MkdirAll(priv, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "picache-prefill")
	if err := os.Symlink("private/picache-prefill", link); err != nil {
		t.Skip("no symbolic links:", err)
	}
	st, err := openStore(link)
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	if err := st.save(fileSettings, defaultSettings()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(priv, fileSettings)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(filepath.Join(dir, "file")); err == nil {
		t.Error("a file was accepted as the state directory")
	}
}

func TestCacheURL(t *testing.T) {
	for raw, want := range map[string]string{
		"http://127.0.0.1":      "http://127.0.0.1:80",
		"http://10.0.0.2:8080/": "http://10.0.0.2:8080",
		"http://[::1]":          "http://[::1]:80",
	} {
		u, err := parseCacheURL(raw)
		if err != nil || u.String() != want {
			t.Errorf("%s → %v %v", raw, u, err)
		}
	}
	for _, bad := range []string{"https://x", "http://x/path", "http://u:p@x", "http://x?a=1", "x:80"} {
		if _, err := parseCacheURL(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
