package services

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

const indexPath = "/cache-domains/cache_domains.json"

func TestStartSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cdn := newFakeCDN(standardSource())
		e := newEnv(t, cdn)
		defer e.db.Close()
		r := e.registry(t)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			r.Start(ctx)
			close(done)
		}()

		// No snapshot: fetched right away.
		synctest.Wait()
		if n := cdn.count(indexPath); n != 1 || !r.Status().Ready {
			t.Fatalf("initial fetches = %d, ready = %v", n, r.Status().Ready)
		}
		// Next refresh after 24 h ± 10 %.
		time.Sleep(21 * time.Hour)
		synctest.Wait()
		if n := cdn.count(indexPath); n != 1 {
			t.Fatalf("refetched too early: %d", n)
		}
		time.Sleep(6 * time.Hour)
		synctest.Wait()
		if n := cdn.count(indexPath); n != 2 {
			t.Fatalf("periodic refresh missing: %d", n)
		}

		// A new source URL is fetched immediately; failures are retried
		// with backoff while the old snapshot stays active.
		if _, err := e.set.Update(ctx, func(a *settings.All) error {
			a.DownloadCache.DomainsSource = "https://mirror.test/cd/"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		const mirror = "/cd/cache_domains.json"
		if n := cdn.count(mirror); n != 1 {
			t.Fatalf("source change fetches = %d", n)
		}
		if st := r.Status(); !st.Ready || st.Error == "" {
			t.Fatalf("status after failed source change = %+v", st)
		}
		time.Sleep(time.Minute + time.Second)
		synctest.Wait()
		if n := cdn.count(mirror); n != 2 {
			t.Fatalf("first retry missing: %d", n)
		}
		time.Sleep(time.Minute + time.Second) // backoff doubled: 2 min
		synctest.Wait()
		if n := cdn.count(mirror); n != 2 {
			t.Fatalf("retried before backoff: %d", n)
		}
		cdn.set(mirror, cdn.files[indexPath])
		for _, f := range []string{"steam.txt", "blizzard.txt", "epicgames.txt", "epic-extra.txt", "origin.txt", "windowsupdates.txt", "test.txt"} {
			cdn.set("/cd/"+f, cdn.files["/cache-domains/"+f])
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if st := r.Status(); cdn.count(mirror) != 3 || st.Error != "" || st.Source != "https://mirror.test/cd/" {
			t.Fatalf("recovery: fetches %d, status %+v", cdn.count(mirror), st)
		}

		// Manual only: no further scheduled fetches.
		if _, err := e.set.Update(ctx, func(a *settings.All) error { a.DownloadCache.UpdateIntervalHours = 0; return nil }); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * 24 * time.Hour)
		synctest.Wait()
		if n := cdn.count(mirror); n != 3 {
			t.Fatalf("fetched in manual mode: %d", n)
		}

		cancel()
		<-done
	})
}

func TestNextFetch(t *testing.T) {
	r, e := loadedRegistry(t)
	now := time.Now()
	if d, ok := r.nextFetch(now); !ok || d < 21*time.Hour || d > 27*time.Hour {
		t.Fatalf("fresh snapshot: next fetch in %v (%v)", d, ok)
	}
	// The source was changed while PiCache was not running: the snapshot
	// of the old source is used but refetched right away.
	if _, err := e.set.Update(context.Background(), func(a *settings.All) error {
		a.DownloadCache.DomainsSource = "https://mirror.test/cd/"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r2 := e.registry(t)
	if !r2.Status().Ready {
		t.Fatal("old snapshot not used")
	}
	if d, ok := r2.nextFetch(now); !ok || d != 0 {
		t.Fatalf("source changed: next fetch in %v (%v)", d, ok)
	}
}

func TestRetryDelay(t *testing.T) {
	for n, want := range map[int]time.Duration{1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute, 7: time.Hour, 50: time.Hour} {
		if got := retryDelay(n); got != want {
			t.Errorf("retryDelay(%d) = %v, want %v", n, got, want)
		}
	}
}
