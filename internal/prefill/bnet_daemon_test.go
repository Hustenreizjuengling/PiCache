package prefill

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/prefill/battlenet"
	"github.com/hustenreizjuengling/picache/internal/prefill/battlenet/bnettest"
	"github.com/hustenreizjuengling/picache/internal/prefill/prefillapi"
)

type fakeBnet struct{ fs *bnettest.Server }

func (f fakeBnet) options(used func(host, protocol, purpose string), log *slog.Logger) battlenet.Options {
	return battlenet.Options{ChannelsURL: f.fs.URL(), RibbitURL: f.fs.RibbitURL,
		PatchURL: func(string) string { return f.fs.URL() + "/none" }, SliceSize: 64, Used: used, Log: log}
}

func bnetFiles() []bnettest.File {
	all := []string{"Windows", "OSX", "x86_64", "x86_32", "enUS", "deDE", "EU", "US", "speech", "text"}
	return []bnettest.File{
		{Name: "common", Size: 300, Tags: all},
		{Name: "de", Size: 200, Tags: []string{"Windows", "x86_64", "deDE", "EU", "US", "speech", "text"}},
		{Name: "en", Size: 200, Tags: []string{"Windows", "x86_64", "enUS", "EU", "US", "speech", "text"}},
		{Name: "loose", Size: 100, Tags: all, Loose: true},
	}
}

func bnetRequests(fs *bnettest.Server, product string) int {
	n := 0
	for _, r := range fs.Requests() {
		if strings.HasPrefix(r.Path, "/"+bnettest.Path(product)+"/data/") {
			n++
		}
	}
	return n
}

func TestDaemonBattleNetFlow(t *testing.T) {
	fs := bnettest.New(
		bnettest.Product{Code: "wow", Version: "12.0.0.1", Files: bnetFiles()},
		bnettest.Product{Code: "s1", Version: "1.0", Files: bnetFiles(), EncryptedBuild: true},
	)
	t.Cleanup(fs.Close)
	e := newEnvWith(t, func(c *Config) {
		c.CacheURL = "http://" + fs.Addr()
		c.bnet = fakeBnet{fs}
	})
	e.call("POST", "/v1/jobs", nil, nil, http.StatusConflict)
	var ps prefillapi.BattleNetProducts
	e.call("GET", "/v1/battlenet/products", nil, &ps, 200)
	byCode := func(code string) prefillapi.BattleNetProduct {
		i := slices.IndexFunc(ps.Products, func(p prefillapi.BattleNetProduct) bool { return p.Code == code })
		if i < 0 {
			t.Fatalf("no %s in %+v", code, ps)
		}
		return ps.Products[i]
	}
	if len(ps.Products) != len(battlenet.Products) || ps.Error != "" || ps.Updated == nil {
		t.Fatalf("products %+v", ps)
	}
	if w := byCode("wow"); w.Name != "World of Warcraft" || w.Version != "12.0.0.1" || w.Error != "" || w.Prefilled != nil {
		t.Fatalf("wow %+v", w)
	}
	if p := byCode("pro"); p.Version != "" || p.Error != "not offered in this region" {
		t.Fatalf("pro %+v", p)
	}

	set := e.status().Settings
	set.BattleNet.Locale = "deDE"
	e.call("PUT", "/v1/settings", set, nil, 200)
	var sel prefillapi.BattleNetSelection
	e.call("PUT", "/v1/battlenet/selection", prefillapi.BattleNetSelection{Products: []string{"wow", "s1"}}, &sel, 200)
	if !slices.Equal(sel.Products, []string{"s1", "wow"}) {
		t.Fatalf("selection %v", sel)
	}

	// The first job downloads wow; s1's build config is encrypted.
	fs.ResetRequests()
	j := e.runJob(false)
	if j.State != prefillapi.JobFailed || len(j.Apps) != 2 || j.Apps[1].ID != "wow" || j.Apps[1].State != prefillapi.AppDone ||
		j.Apps[0].State != prefillapi.AppFailed || !strings.Contains(j.Apps[0].Error, "encrypted") {
		t.Fatalf("job %+v", j)
	}
	if j.Apps[1].Bytes < 600 || j.Apps[1].Total == 0 || j.FromCache != 0 {
		t.Errorf("wow bytes %d of %d, from cache %d", j.Apps[1].Bytes, j.Apps[1].Total, j.FromCache)
	}
	for _, r := range fs.Requests() {
		if r.Host != "level3.blizzard.com" {
			t.Errorf("request %+v", r)
		}
	}
	if !slices.Contains(j.Hosts, "http://level3.blizzard.com") {
		t.Errorf("hosts %v", j.Hosts)
	}
	e.call("GET", "/v1/battlenet/products", nil, &ps, 200)
	if w := byCode("wow"); w.Prefilled == nil || !w.Current {
		t.Fatalf("after the job %+v", w)
	}

	// The second job finds wow current.
	fs.ResetRequests()
	j = e.runJob(false)
	if j.Apps[1].State != prefillapi.AppCurrent || bnetRequests(fs, "wow") != 0 {
		t.Fatalf("second job %+v, %d requests", j, bnetRequests(fs, "wow"))
	}

	// Another language is another download.
	set.BattleNet.Locale = "enUS"
	e.call("PUT", "/v1/settings", set, nil, 200)
	e.call("GET", "/v1/battlenet/products", nil, &ps, 200)
	if byCode("wow").Current {
		t.Error("current in another language")
	}
	e.call("PUT", "/v1/battlenet/selection", prefillapi.BattleNetSelection{Products: []string{"wow"}}, nil, 200)
	j = e.runJob(false)
	if j.State != prefillapi.JobDone || j.Apps[0].State != prefillapi.AppDone || j.FromCache == 0 {
		t.Fatalf("enUS job %+v", j)
	}

	// A new build is downloaded again; the version list follows.
	fs.SetProduct(bnettest.Product{Code: "wow", Version: "12.0.0.2", Files: bnetFiles()})
	fs.ResetRequests()
	j = e.runJob(false)
	if j.Apps[0].State != prefillapi.AppDone || bnetRequests(fs, "wow") == 0 {
		t.Fatalf("new build job %+v", j)
	}
	e.call("GET", "/v1/battlenet/products", nil, &ps, 200)
	if w := byCode("wow"); w.Version != "12.0.0.2" || !w.Current {
		t.Fatalf("after the new build %+v", w)
	}

	// The version service via Ribbit when the channels fail.
	fs.DisableChannels(true)
	e.call("GET", "/v1/battlenet/products?refresh=true", nil, &ps, 200)
	if byCode("wow").Version != "12.0.0.2" || fs.RibbitCalls() == 0 {
		t.Fatalf("ribbit %+v", byCode("wow"))
	}
}
