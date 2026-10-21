package prefill

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/prefill/epic"
	"github.com/hustenreizjuengling/picache/internal/prefill/epic/epictest"
	"github.com/hustenreizjuengling/picache/internal/prefill/prefillapi"
)

type fakeEpic struct{ fs *epictest.Server }

func (f fakeEpic) options(used func(host, protocol, purpose string), log *slog.Logger) epic.Options {
	u := f.fs.URL()
	return epic.Options{AccountURL: u, LauncherURL: u, CatalogURL: u, TicketURL: u, DeliveryURL: u,
		PollInterval: 20 * time.Millisecond, Used: used, Log: log}
}

// clock is a test clock that can be moved ahead.
type clock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off)
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.off += d
	c.mu.Unlock()
}

func epicChunks(seed uint32, n, size int) []epictest.Chunk {
	cs := make([]epictest.Chunk, n)
	for i := range cs {
		v := seed + uint32(i)
		cs[i] = epictest.Chunk{GUID: [4]uint32{v, v + 1, v + 2, v + 3}, Hash: uint64(v) * 0x9E3779B97F4A7C15, Group: uint8(v % 100), Size: size}
	}
	return cs
}

func TestDaemonEpicFlow(t *testing.T) {
	efs := epictest.New([]epictest.App{
		{AppName: "Alpha", Namespace: "ns1", CatalogItemID: "item1", Title: "Alpha Game", BuildVersion: "1.0", FeatureLevel: 18,
			Chunks: epicChunks(10, 4, 1000)},
		{AppName: "AlphaDLC", Namespace: "ns1", CatalogItemID: "item2", Title: "Alpha DLC", BuildVersion: "1.0", DLC: "item1"},
		{AppName: "Plug", Namespace: "ns2", CatalogItemID: "item3", Title: "A plugin", BuildVersion: "1", Category: "plugins"},
		{AppName: "Beta", Namespace: "ns2", CatalogItemID: "item4", Title: "beta", BuildVersion: "2.0", FeatureLevel: 22,
			Chunks: epicChunks(50, 3, 300), Signed: true},
	})
	t.Cleanup(efs.Close)
	clk := &clock{}
	e := newEnvWith(t, func(c *Config) {
		c.CacheURL = "http://" + efs.Addr()
		c.epic = fakeEpic{efs}
		c.now = clk.now
	})
	st := e.status()
	if st.Epic.Account != nil || st.Epic.Login != nil {
		t.Fatalf("initial status %+v", st.Epic)
	}
	e.call("GET", "/v1/epic/apps", nil, nil, http.StatusConflict)
	e.call("POST", "/v1/epic/logout", nil, nil, http.StatusNotFound)

	// Device-code sign-in: the code waits, then is confirmed at Epic.
	efs.ConfirmAfter(3)
	var l prefillapi.EpicLogin
	e.call("POST", "/v1/epic/login", nil, &l, 200)
	if l.State != "waiting" || l.UserCode != "ABCD1234" || l.VerificationURL != "https://www.epicgames.com/activate?userCode=ABCD1234" || l.Expires == nil {
		t.Fatalf("login %+v", l)
	}
	e.waitFor("the Epic sign-in", func() bool { return e.status().Epic.Account != nil })
	st = e.status()
	a := st.Epic.Account
	if a.Name != epictest.DisplayName || a.AccountID != epictest.AccountID || a.Expires == nil || st.Epic.Login.State != "done" || st.Epic.Login.UserCode != "" {
		t.Fatalf("account %+v login %+v", a, st.Epic.Login)
	}
	if k := efs.Killed(); len(k) != 1 || !strings.HasPrefix(k[0], "at-98f7") {
		t.Errorf("the sign-in session was not ended: %v", k)
	}
	fi, err := os.Stat(filepath.Join(e.dir, "state", fileEpic))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("epic.json %v %v", fi, err)
	}
	r, _ := e.c.Do(context.Background(), "GET", "/v1/status", nil)
	if strings.Contains(string(r.Body), "rt-") || strings.Contains(string(r.Body), "at-") {
		t.Fatalf("the status contains a token: %s", r.Body)
	}
	e.call("POST", "/v1/epic/login", nil, nil, http.StatusConflict)

	// The games, without DLC and plugins.
	var apps prefillapi.EpicApps
	e.waitFor("the Epic games", func() bool {
		e.call("GET", "/v1/epic/apps", nil, &apps, 200)
		return len(apps.Apps) > 0
	})
	if len(apps.Apps) != 2 || apps.Apps[0].ID != "Alpha" || apps.Apps[0].Name != "Alpha Game" || apps.Apps[1].ID != "Beta" ||
		apps.Apps[0].Version != "1.0" || apps.Error != "" {
		t.Fatalf("apps %+v", apps)
	}
	var sel prefillapi.EpicSelection
	e.call("PUT", "/v1/epic/selection", prefillapi.EpicSelection{Apps: []string{"Beta", "Alpha", "Beta"}}, &sel, 200)
	if !slices.Equal(sel.Apps, []string{"Alpha", "Beta"}) {
		t.Fatalf("selection %v", sel.Apps)
	}

	// The first job: manifests and chunks through the cache; the signed
	// build with signed addresses.
	j := e.runJob(false)
	if j.State != prefillapi.JobDone || len(j.Apps) != 2 || j.Apps[0].State != prefillapi.AppDone || j.Apps[1].State != prefillapi.AppDone {
		t.Fatalf("job %+v", j)
	}
	if j.Total < 4900 || j.Bytes < j.Total || j.FromCache != 0 {
		t.Errorf("job bytes %d of %d, from cache %d", j.Bytes, j.Total, j.FromCache)
	}
	reqs := efs.Requests()
	var want []string
	for _, c := range epicChunks(10, 4, 1000) {
		want = append(want, "egs-cloudfront-chunks.epicgamescdn.com/Builds/Org/o-test/item1/default/"+epictest.ChunkPath(c, 18))
	}
	for _, c := range epicChunks(50, 3, 300) {
		want = append(want, "egdownload.fastly-edge.com/Builds/Org/o-test/item4/default/"+epictest.ChunkPath(c, 22))
	}
	for _, w := range append(want, "egs-cloudfront-chunks.epicgamescdn.com/Builds/Org/o-test/item1/default/Alpha.manifest") {
		if !slices.Contains(reqs, w) {
			t.Errorf("not requested: %s\nrequests %v", w, reqs)
		}
	}
	e.call("GET", "/v1/epic/apps", nil, &apps, 200)
	if apps.Apps[0].Prefilled == nil || !apps.Apps[0].Current || !apps.Apps[1].Current {
		t.Errorf("after the job %+v", apps.Apps)
	}

	// The second job finds both current (by the manifest hash).
	efs.ResetRequests()
	j = e.runJob(false)
	if j.State != prefillapi.JobDone || j.Apps[0].State != prefillapi.AppCurrent || j.Apps[1].State != prefillapi.AppCurrent {
		t.Fatalf("second job %+v", j)
	}
	if n := len(efs.Requests()); n != 0 {
		t.Errorf("second job made %d CDN requests", n)
	}

	// A new build of Alpha is downloaded; Beta stays current.
	efs.SetBuild("Alpha", "1.1", epicChunks(200, 2, 700))
	j = e.runJob(false)
	if j.State != prefillapi.JobDone || j.Apps[0].State != prefillapi.AppDone || j.Apps[1].State != prefillapi.AppCurrent {
		t.Fatalf("new build job %+v", j)
	}
	e.call("GET", "/v1/epic/apps", nil, &apps, 200)
	if apps.Apps[0].Version != "1.1" || !apps.Apps[0].Current {
		t.Errorf("after the new build %+v", apps.Apps[0])
	}

	// A day later the session is renewed (the refresh token rotates and is
	// stored).
	before := efs.Refreshes()
	var acct epicAccount
	if err := e.loadState(fileEpic, &acct); err != nil {
		t.Fatal(err)
	}
	clk.advance(25 * time.Hour)
	e.call("GET", "/v1/epic/apps?refresh=true", nil, &apps, 200)
	var renewed epicAccount
	if err := e.loadState(fileEpic, &renewed); err != nil {
		t.Fatal(err)
	}
	if efs.Refreshes() != before+1 || renewed.RefreshToken == acct.RefreshToken || apps.Error != "" {
		t.Errorf("refreshes %d → %d, token %s → %s, %s", before, efs.Refreshes(), acct.RefreshToken, renewed.RefreshToken, apps.Error)
	}

	// Signing out ends the launcher session at Epic and deletes it.
	e.call("POST", "/v1/epic/logout", nil, nil, http.StatusNoContent)
	if k := efs.Killed(); len(k) != 2 || !strings.HasPrefix(k[1], "at-34a0") {
		t.Errorf("killed %v", k)
	}
	if e.status().Epic.Account != nil {
		t.Error("still signed in")
	}
	if _, err := os.Stat(filepath.Join(e.dir, "state", fileEpic)); !os.IsNotExist(err) {
		t.Errorf("epic.json after the sign-out: %v", err)
	}
	e.call("POST", "/v1/epic/logout", nil, nil, http.StatusNotFound)
}

// A refresh token Epic no longer accepts marks the account; a sign-in
// can be cancelled.
func TestDaemonEpicRejected(t *testing.T) {
	efs := epictest.New([]epictest.App{{AppName: "Alpha", Namespace: "ns1", CatalogItemID: "item1", Title: "Alpha", BuildVersion: "1", Chunks: epicChunks(1, 1, 10)}})
	t.Cleanup(efs.Close)
	clk := &clock{}
	e := newEnvWith(t, func(c *Config) {
		c.CacheURL = "http://" + efs.Addr()
		c.epic = fakeEpic{efs}
		c.now = clk.now
	})
	var l prefillapi.EpicLogin
	e.call("POST", "/v1/epic/login", nil, &l, 200)
	e.call("DELETE", "/v1/epic/login", nil, nil, http.StatusNoContent)
	if st := e.status().Epic.Login; st == nil || st.State != "cancelled" {
		t.Fatalf("after cancel %+v", st)
	}
	efs.ConfirmAfter(1)
	e.call("POST", "/v1/epic/login", nil, &l, 200)
	e.waitFor("the Epic sign-in", func() bool { return e.status().Epic.Account != nil })
	var acct epicAccount
	if err := e.loadState(fileEpic, &acct); err != nil {
		t.Fatal(err)
	}
	efs.Revoke(acct.RefreshToken)
	clk.advance(25 * time.Hour)
	var apps prefillapi.EpicApps
	e.call("GET", "/v1/epic/apps?refresh=true", nil, &apps, 200)
	if apps.Error == "" {
		t.Error("no error with a revoked refresh token")
	}
	if a := e.status().Epic.Account; a == nil || !strings.Contains(a.Error, "sign in again") {
		t.Fatalf("account %+v", a)
	}
	// A new sign-in replaces the rejected one.
	efs.ConfirmAfter(1)
	e.call("POST", "/v1/epic/login", nil, &l, 200)
	e.waitFor("the new sign-in", func() bool { a := e.status().Epic.Account; return a != nil && a.Error == "" })
}
