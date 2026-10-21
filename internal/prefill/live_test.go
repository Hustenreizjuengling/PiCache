//go:build livesteam

package prefill

// Manual live test of the download path: an anonymous Steam session (app
// 1007, depot 1004, about 24 MB) and every chunk read through a running
// PiCache whose download cache listens at PREFILL_LIVE_CACHE (for example
// a test instance on http://127.0.0.1:18080). The second pass must come
// from the cache.
//
//	PREFILL_LIVE_CACHE=http://127.0.0.1:18080 go test -tags livesteam -run TestLiveCache -v ./internal/prefill/

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/prefill/prefillapi"
	"github.com/hustenreizjuengling/picache/internal/prefill/steam"
)

func TestLiveCache(t *testing.T) {
	raw := os.Getenv("PREFILL_LIVE_CACHE")
	if raw == "" {
		t.Skip("PREFILL_LIVE_CACHE is not set")
	}
	base, err := parseCacheURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	d := &Daemon{log: log, now: time.Now, hosts: map[string]*prefillapi.Host{}, cache: newCacheClient(base, 8)}
	id, err := d.cache.heartbeat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cache %s answers as %s", base, id)

	c, err := steam.Connect(ctx, steam.Options{Log: log})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := c.SignIn(ctx, steam.LogOn{LoginID: 0x50435446, MachineID: "live"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.LogOff()
	games, err := sess.Games(ctx, []uint32{1007}, steam.Filter{OS: []string{"windows"}, Languages: []string{"english"}})
	if err != nil || len(games) != 1 {
		t.Fatalf("games %+v %v", games, err)
	}
	servers, err := sess.ContentServers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hosts := serverHosts(servers)
	t.Logf("content servers %v", hosts)
	s := &steamService{d: d}
	for pass := 1; pass <= 2; pass++ {
		r := &jobRun{conns: 8, job: prefillapi.Job{Apps: []prefillapi.JobApp{{Service: "steam", ID: "1007"}}}}
		start := time.Now()
		if err := s.prefillGame(ctx, r, sess, hosts, games[0]); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		el := time.Since(start)
		t.Logf("pass %d: %d bytes in %s, %d from the cache", pass, r.job.Bytes, el.Round(time.Millisecond), r.job.FromCache)
		if pass == 2 && r.job.FromCache < games[0].Bytes() {
			t.Errorf("pass 2 read only %d of %d bytes from the cache", r.job.FromCache, games[0].Bytes())
		}
	}
}
