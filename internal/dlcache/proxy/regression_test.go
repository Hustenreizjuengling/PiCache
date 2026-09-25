package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	cachestore "github.com/hustenreizjuengling/picache/internal/dlcache/store"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// TestDownloadCacheDisabled: with the download cache off (the default) the
// cache proxy answers only the heartbeat; nothing is fetched or stored.
func TestDownloadCacheDisabled(t *testing.T) {
	h := newHarness(t)
	h.origin.set("/f", &originObj{data: testData(3000)})
	h.updateSettings(func(a *settings.All) { a.DownloadCache.Enabled = false })
	for _, tc := range []struct{ method, host, uri string }{
		{"GET", testHost, "/f"},
		{"HEAD", testHost, "/f"},
		{"POST", testHost, "/f"},
		{"GET", "attacker.example", "/depot/7/chunk/x"},
	} {
		req := hdr("User-Agent", "Valve/Steam HTTP Client 1.0")
		if resp, _ := h.get(tc.method, tc.host, tc.uri, req); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s%s: status %d, want 403", tc.method, tc.host, tc.uri, resp.StatusCode)
		}
	}
	if n := len(h.origin.requests()); n != 0 {
		t.Fatalf("%d upstream requests while the download cache is disabled", n)
	}
	if h.store.object(testService, "/f") != nil || h.store.object("steam", "/depot/7/chunk/x") != nil {
		t.Fatal("content stored while the download cache is disabled")
	}
	if resp, _ := h.get("GET", "10.0.0.5", heartbeatPath, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("heartbeat: %d", resp.StatusCode)
	}
	if st := h.s.Stats(); st.Refused != 4 || st.Requests != 0 {
		t.Fatalf("stats %+v", st)
	}
	h.updateSettings(func(a *settings.All) { a.DownloadCache.Enabled = true })
	if resp, body := h.get("GET", testHost, "/f", nil); resp.StatusCode != http.StatusOK || len(body) != 3000 {
		t.Fatalf("enabled again: %d", resp.StatusCode)
	}
}

// TestStoreClosedDuringRequest: a store that closes while a request reads
// from it (Close cancels the read, then every call reports ErrClosed) must
// not crash the handler; the request continues without the store.
func TestStoreClosedDuringRequest(t *testing.T) {
	h := newHarness(t)
	data := testData(3 * testSlice)
	h.origin.set("/c", &originObj{data: data})
	if _, body := h.get("GET", testHost, "/c", nil); !bytes.Equal(body, data) {
		t.Fatal("warm-up")
	}
	h.waitSlices(testService, "/c", 3)
	var once sync.Once
	h.store.setReadHook(func(string, int64) error {
		err := error(nil)
		once.Do(func() {
			h.store.close()        // later calls: ErrClosed
			err = context.Canceled // the read in flight when Close started
		})
		return err
	})
	if status, body, err := getOnce(h, testHost, "/c"); err != nil || status != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("status %d, %d of %d bytes, %v", status, len(body), len(data), err)
	}
	// The same through a store that is no longer the active one: its
	// errors end its use at once.
	h2 := newHarness(t)
	h2.origin.set("/c", &originObj{data: data})
	h2.get("GET", testHost, "/c", nil)
	h2.waitSlices(testService, "/c", 3)
	h2.store.setReadHook(func(string, int64) error {
		h2.switched.Store(true) // Deps.Store returns nil from now on
		return context.Canceled
	})
	if status, body, err := getOnce(h2, testHost, "/c"); err != nil || status != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("switched store: status %d, %d bytes, %v", status, len(body), err)
	}
}

// getOnce performs a GET on a fresh connection: a response the proxy
// aborts is not retried by the client (as it may be on a reused one).
func getOnce(h *harness, host, uri string) (int, []byte, error) {
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true, DisableCompression: true}, Timeout: 20 * time.Second}
	req, err := http.NewRequest("GET", h.proxy.URL+uri, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Host = host
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

// TestObjectInUseDuringRequest: the object is marked in use (never
// evicted) while it is served and released afterwards.
func TestObjectInUseDuringRequest(t *testing.T) {
	h := newHarness(t)
	data := testData(2 * testSlice)
	h.origin.set("/u", &originObj{data: data})
	h.get("GET", testHost, "/u", nil)
	h.waitSlices(testService, "/u", 2)
	id := cachestore.ObjectID(testService, "/u")
	inUse := make(chan int, 4)
	h.store.setReadHook(func(rid string, _ int64) error {
		if rid == id {
			h.store.mu.Lock()
			inUse <- h.store.inUse[id]
			h.store.mu.Unlock()
		}
		return nil
	})
	if _, body := h.get("GET", testHost, "/u", nil); !bytes.Equal(body, data) {
		t.Fatal("wrong body")
	}
	if n := <-inUse; n != 1 {
		t.Fatalf("in use %d times while served", n)
	}
	eventually(t, func() bool {
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		return h.store.inUse[id] == 0 && h.store.uses[id] == 2
	})
}

// TestHitsCountCachedBytesOnly: a MISS download records an access but no
// hit; a HIT records the bytes served from the cache.
func TestHitsCountCachedBytesOnly(t *testing.T) {
	h := newHarness(t)
	data := testData(2*testSlice + 100)
	h.origin.set("/h", &originObj{data: data})
	id := cachestore.ObjectID(testService, "/h")
	if resp, _ := h.get("GET", testHost, "/h", nil); resp.Header.Get(cacheStatusHeader) != statusMiss {
		t.Fatal("first download must be a miss")
	}
	h.events(1)
	h.waitSlices(testService, "/h", 3)
	h.store.mu.Lock()
	hits, served, access := h.store.hits[id], h.store.touched[id], h.store.access[id]
	h.store.mu.Unlock()
	if hits != 0 || served != 0 || access != 1 {
		t.Fatalf("after a miss: hits=%d served=%d accesses=%d", hits, served, access)
	}
	if resp, _ := h.get("GET", testHost, "/h", nil); resp.Header.Get(cacheStatusHeader) != statusHit {
		t.Fatal("second download must be a hit")
	}
	h.events(2)
	h.store.mu.Lock()
	hits, served = h.store.hits[id], h.store.touched[id]
	h.store.mu.Unlock()
	if hits != 1 || served != int64(len(data)) {
		t.Fatalf("after a hit: hits=%d served=%d", hits, served)
	}
}

// TestCacheKeyKeepsReservedEncodings: "/a%2Bb" and "/a+b" are different
// upstream resources and must not share a cache entry.
func TestCacheKeyKeepsReservedEncodings(t *testing.T) {
	h := newHarness(t)
	enc, plain := bytes.Repeat([]byte("E"), 1500), bytes.Repeat([]byte("P"), 1500)
	h.origin.set("/game/a%2Bb.bin", &originObj{data: enc})
	h.origin.set("/game/a+b.bin", &originObj{data: plain})
	if resp, body := h.get("GET", testHost, "/game/a%2Bb.bin", nil); resp.StatusCode != http.StatusOK || !bytes.Equal(body, enc) {
		t.Fatalf("encoded: %d %q", resp.StatusCode, body[:min(len(body), 4)])
	}
	h.waitWrites(2)
	resp, body := h.get("GET", testHost, "/game/a+b.bin", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, plain) {
		t.Fatalf("plain form served %q (status %q)", body[:min(len(body), 4)], resp.Header.Get(cacheStatusHeader))
	}
	// Equivalent forms (hex case, encoded non-reserved characters) share it.
	h.waitWrites(4)
	n := len(h.origin.requests())
	if resp, body := h.get("GET", testHost, "/game/a%2bb.bin", nil); !bytes.Equal(body, enc) || resp.Header.Get(cacheStatusHeader) != statusHit {
		t.Fatalf("lower-case hex: %q", resp.Header.Get(cacheStatusHeader))
	}
	if len(h.origin.requests()) != n {
		t.Fatal("an equivalent form went upstream")
	}
}

func TestCacheKeyPath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/depot/1/chunk/ab", "/depot/1/chunk/ab"},
		{"/a%20b", "/a b"},
		{"/%C3%A4", "/ä"},
		{"/a%2Bb", "/a%2Bb"},
		{"/a%2bb", "/a%2Bb"},
		{"/a+b", "/a+b"},
		{"/a%3Bb;c", "/a%3Bb;c"},
		{"/a%40b%3A", "/a%40b%3A"},
		{"/a%5Bb%5D", "/a%5Bb%5D"},
		{"/[x]", "/[x]"},
		{"/100%25", "/100%25"},
		{"/q%3Fx%23", "/q?x#"},
		{"/file(1).bin", "/file(1).bin"},
		{"/file%281%29.bin", "/file%281%29.bin"},
	} {
		if got := cacheKeyPath(tc.in); got != tc.want {
			t.Errorf("cacheKeyPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// gatedClients sends n concurrent requests for path and returns their
// bodies. The origin's gate is opened once every request reached the
// proxy.
func gatedClients(t *testing.T, h *harness, host, path string, n int, gate chan struct{}) [][]byte {
	t.Helper()
	bodies := make([][]byte, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			req, _ := http.NewRequest("GET", h.proxy.URL+path, nil)
			req.Host = host
			resp, err := h.client.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			bodies[i], _ = io.ReadAll(resp.Body)
		})
	}
	eventually(t, func() bool { return h.s.Stats().Requests >= int64(n) })
	time.Sleep(100 * time.Millisecond) // let them all reach the fill
	close(gate)
	wg.Wait()
	return bodies
}

// TestCollapsingWithoutRanges: concurrent clients of an object whose
// upstream ignores Range share one upstream download, whether the host is
// not yet known for it or already marked no-slice.
func TestCollapsingWithoutRanges(t *testing.T) {
	const clients = 5
	for _, marked := range []bool{false, true} {
		name := "unmarked"
		if marked {
			name = "marked"
		}
		t.Run(name, func(t *testing.T) {
			// Collapsing needs every client to reach the first fetch before the
			// leader's answer arrives. On a busy CI runner a client is sometimes
			// too late and fetches on its own (correct, but not collapsed; see
			// the issue "proxy: concurrent first requests do not always
			// collapse"), so try up to three times; one run must collapse.
			var h *harness
			var host string
			var data []byte
			for attempt := 1; ; attempt++ {
				h = newHarness(t)
				host = testHost
				if marked {
					host = "noslice.example"
					for i := range noSliceThreshold {
						h.s.noslice.failure(host, cachestore.ObjectID(testService, "/o"+string(rune('a'+i))), time.Now())
					}
					if use, _ := h.s.noslice.useSlicing(host, time.Now()); use {
						t.Fatal("host not marked")
					}
				}
				data = testData(8 * testSlice)
				gate := make(chan struct{})
				h.origin.set("/whole", &originObj{data: data, noRange: true, gate: gate})
				for i, body := range gatedClients(t, h, host, "/whole", clients, gate) {
					if !bytes.Equal(body, data) {
						t.Fatalf("marked=%v client %d: %d of %d bytes", marked, i, len(body), len(data))
					}
				}
				got := len(h.origin.ranges("/whole"))
				if got == 1 {
					break
				}
				if attempt == 3 && !marked {
					// Known race for hosts not yet marked no-slice, reproducible
					// on CI runners: https://github.com/Hustenreizjuengling/PiCache/issues/3
					t.Skipf("unmarked host: %d upstream downloads for %d clients (issue #3)", got, clients)
				}
				if attempt == 3 {
					t.Fatalf("marked=%v: %d upstream downloads for %d clients", marked, got, clients)
				}
				t.Logf("marked=%v attempt %d: %d upstream downloads for %d clients; retrying", marked, attempt, got, clients)
			}
			h.waitSlices(testService, "/whole", 8)
			evs := h.events(clients)
			var hit, wan int64
			for _, ev := range evs {
				hit += ev.BytesHit
				wan += ev.BytesWAN
			}
			if wan != int64(len(data)) || hit != int64((clients-1)*len(data)) {
				t.Fatalf("marked=%v: hit %d wan %d", marked, hit, wan)
			}
			n := len(h.origin.requests())
			if resp, body := h.get("GET", host, "/whole", nil); !bytes.Equal(body, data) || resp.Header.Get(cacheStatusHeader) != statusHit ||
				len(h.origin.requests()) != n {
				t.Fatalf("marked=%v: afterwards %q", marked, resp.Header.Get(cacheStatusHeader))
			}
		})
	}
}

// TestFollowerFallsBackWhenLeaderStalls: a follower does not wait forever
// for a leader whose upstream stalls; it fetches the object itself.
func TestFollowerFallsBackWhenLeaderStalls(t *testing.T) {
	h := newHarness(t)
	h.s.tm.stall, h.s.tm.idleRead = 300*time.Millisecond, 3*time.Second
	data := testData(4 * testSlice)
	h.origin.set("/s", &originObj{data: data, noRange: true, stallOn: func(string) bool { return true }})
	leader := make(chan struct{})
	go func() {
		defer close(leader)
		resp := h.do("GET", testHost, "/s", nil)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	eventually(t, func() bool {
		return h.s.leaders.get(objKey{store: h.store.ID(), id: cachestore.ObjectID(testService, "/s")}) != nil
	})
	h.origin.set("/s", &originObj{data: data, noRange: true})
	start := time.Now()
	resp, body := h.get("GET", testHost, "/s", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("follower: %d, %d bytes", resp.StatusCode, len(body))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the follower waited %v for a stalled leader", d)
	}
	<-leader
}

// TestCacheStatusHeaderCountsFills: bytes a request takes from another
// request's fill count as hits, and its header says so too.
func TestCacheStatusHeaderCountsFills(t *testing.T) {
	h := newHarness(t)
	h.store.writeDelay = 700 * time.Millisecond // slices wait for the store after the download
	data := testData(4 * testSlice)
	h.origin.set("/p", &originObj{data: data})
	if resp, _ := h.get("GET", testHost, "/p", nil); resp.Header.Get(cacheStatusHeader) != statusMiss {
		t.Fatal("first download must be a miss")
	}
	upstream := len(h.origin.requests())
	resp, body := h.get("GET", testHost, "/p", nil) // the slices are still being stored
	if !bytes.Equal(body, data) || len(h.origin.requests()) != upstream {
		t.Fatal("second download went upstream")
	}
	evs := h.events(2) // the first request's event comes last: its fills store the slices
	ev := evs[0]
	if evs[1].Time.After(ev.Time) {
		ev = evs[1]
	}
	if ev.CacheStatus != statusHit || resp.Header.Get(cacheStatusHeader) != ev.CacheStatus {
		t.Fatalf("header %q, event %q (hit %d of %d)", resp.Header.Get(cacheStatusHeader), ev.CacheStatus, ev.BytesHit, ev.BytesSent)
	}
}

// TestNoSliceProbeNotUsedByHits: the daily range probe of a marked host is
// kept for a request that actually goes upstream.
func TestNoSliceProbeNotUsedByHits(t *testing.T) {
	h := newHarness(t)
	const host = "noslice.example"
	data := testData(3 * testSlice)
	h.origin.set("/cached", &originObj{data: data})
	h.get("GET", host, "/cached", nil)
	h.waitSlices(testService, "/cached", 3)
	for i := range noSliceThreshold {
		h.s.noslice.failure(host, cachestore.ObjectID(testService, "/o"+string(rune('a'+i))), time.Now())
	}
	h.s.noslice.mu.Lock()
	h.s.noslice.hosts[host].lastProbe = time.Now().Add(-25 * time.Hour)
	h.s.noslice.mu.Unlock()
	if resp, _ := h.get("GET", host, "/cached", nil); resp.Header.Get(cacheStatusHeader) != statusHit {
		t.Fatal("expected a hit")
	}
	h.events(2)
	h.origin.set("/new", &originObj{data: data})
	if _, body := h.get("GET", host, "/new", nil); !bytes.Equal(body, data) {
		t.Fatal("wrong body")
	}
	if rs := h.origin.ranges("/new"); len(rs) == 0 || !strings.HasPrefix(rs[0], "bytes=") {
		t.Fatalf("the miss after a hit was fetched without the probe: ranges %q", rs)
	}
	if hosts, _ := h.s.NoSliceHosts(context.Background()); len(hosts) != 0 {
		t.Fatalf("a valid 206 of the probe must clear the host: %+v", hosts)
	}
}
