package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

func TestMidStreamFailureFallsBackToDirect(t *testing.T) {
	h := newHarness(t)
	data := testData(4 * testSlice)
	// Every fill of slice 2 breaks after half of it.
	h.origin.set("/f", &originObj{data: data, failOn: func(rng string) bool { return rng == "bytes=2048-3071" }})

	resp, body := h.get("GET", testHost, "/f", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("status %d, %d bytes", resp.StatusCode, len(body))
	}
	got := h.origin.ranges("/f")
	if n := countOf(got, "bytes=2048-3071"); n < 2 {
		t.Fatalf("slice 2 tried %d times, want a retry: %v", n, got)
	}
	// After the retry the rest of the range comes directly.
	if !slices.Contains(got, "bytes=2560-4095") {
		t.Fatalf("no direct fetch of the remaining bytes: %v", got)
	}
	ev := h.events(1)[0]
	if ev.Status != http.StatusOK || ev.BytesSent != int64(len(data)) || ev.CacheStatus == statusError {
		t.Fatalf("event %+v", ev)
	}
	if o := h.store.object(testService, "/f"); o == nil || o.slices[2] != nil {
		t.Fatal("the broken slice must not be stored")
	}
}

func TestMidStreamFailureAbortsWhenDirectFails(t *testing.T) {
	h := newHarness(t)
	data := testData(4 * testSlice)
	h.origin.set("/f", &originObj{data: data, failOn: func(rng string) bool { return strings.HasPrefix(rng, "bytes=2") }})

	resp := h.do("GET", testHost, "/f", hdr("Range", "bytes=0-3000"))
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || err == nil || len(body) >= 3001 {
		t.Fatalf("status %d, err %v, %d bytes: want an aborted response", resp.StatusCode, err, len(body))
	}
	if !bytes.Equal(body, data[:len(body)]) {
		t.Fatal("wrong bytes before the abort")
	}
	if !slices.Contains(h.origin.ranges("/f"), "bytes=2560-3000") {
		t.Fatalf("no direct attempt: %v", h.origin.ranges("/f"))
	}
	if ev := h.events(1)[0]; ev.CacheStatus != statusError {
		t.Fatalf("event %+v", ev)
	}
}

func TestStalledFillGoesDirect(t *testing.T) {
	h := newHarness(t)
	h.s.tm.stall = 300 * time.Millisecond
	data := testData(3 * testSlice)
	h.origin.set("/s", &originObj{data: data, stallOn: func(rng string) bool { return rng == "bytes=1024-2047" }})

	start := time.Now()
	resp, body := h.get("GET", testHost, "/s", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("status %d, %d bytes", resp.StatusCode, len(body))
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
	if got := h.origin.ranges("/s"); !slices.Contains(got, "bytes=1536-3071") {
		t.Fatalf("no direct fetch after the stall: %v", got)
	}
}

func TestRecordedObjectShrunk(t *testing.T) {
	h := newHarness(t)
	oldData, newData := testData(5000), bytes.Repeat([]byte("x"), 2000)
	for _, p := range []string{"/a", "/b"} {
		h.origin.set(p, &originObj{data: oldData})
		h.get("GET", testHost, p, nil)
		h.waitSlices(testService, p, 5)
		h.origin.set(p, &originObj{data: newData})
		h.store.dropSlice(testService, p, 3)
	}

	// Headers were sent with the old size: 416 for slice 3 → the object is
	// invalidated and the response aborted.
	resp := h.do("GET", testHost, "/a", nil)
	_, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil || resp.ContentLength != 5000 {
		t.Fatalf("want an aborted 5000-byte response: err %v, length %d", err, resp.ContentLength)
	}
	eventually(t, func() bool { return h.store.object(testService, "/a") == nil })
	if _, body := h.get("GET", testHost, "/a", nil); !bytes.Equal(body, newData) {
		t.Fatal("new content not served after the invalidation")
	}

	// Nothing sent yet: re-planned with the new size, the range is now
	// beyond the end.
	resp, _ = h.get("GET", testHost, "/b", hdr("Range", "bytes=3500-3600"))
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable || resp.Header.Get("Content-Range") != "bytes */2000" {
		t.Fatalf("status %d %q", resp.StatusCode, resp.Header.Get("Content-Range"))
	}
}

func TestRecordedObjectUpstreamStatusRelayed(t *testing.T) {
	h := newHarness(t)
	h.origin.set("/k", &originObj{data: testData(3000)})
	h.get("GET", testHost, "/k", nil)
	h.waitSlices(testService, "/k", 3)
	h.store.dropSlice(testService, "/k", 0)
	h.origin.set("/k", &originObj{status: http.StatusForbidden})

	resp, body := h.get("GET", testHost, "/k", nil)
	if resp.StatusCode != http.StatusForbidden || string(body) != "origin says 403" || resp.Header.Get(processedByHeader) != "" {
		t.Fatalf("status %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if o := h.store.object(testService, "/k"); o == nil || len(o.slices) != 2 {
		t.Fatal("the cached slices must stay")
	}
}

func TestSSRFGuardOnHostNames(t *testing.T) {
	h := newHarness(t)
	h.cls.mu.Lock()
	h.cls.hosts["private.example"] = true
	h.cls.mu.Unlock()
	data := testData(100)
	h.origin.set("/data", &originObj{data: data})
	h.origin.set("/to-private", &originObj{location: "http://private.example/data"})
	h.origin.set("/to-linklocal", &originObj{location: "http://linklocal.example/data"})

	for _, tc := range []struct{ host, path string }{
		{testHost, "/to-private"}, {testHost, "/to-linklocal"}, {"private.example", "/data"},
	} {
		if resp, _ := h.get("GET", tc.host, tc.path, nil); resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("%s%s: status %d, want 502", tc.host, tc.path, resp.StatusCode)
		}
	}
	if n := len(h.origin.ranges("/data")); n != 0 {
		t.Fatalf("a refused destination was contacted %d times", n)
	}
	h.updateSettings(func(a *settings.All) { a.LanCache.AllowPrivateUpstreams = true })
	for _, tc := range []struct {
		host, path string
		want       int
	}{
		{testHost, "/to-private", http.StatusOK}, {"private.example", "/data", http.StatusOK},
		{testHost, "/to-linklocal", http.StatusBadGateway}, // link-local never
	} {
		if resp, _ := h.get("GET", tc.host, tc.path, nil); resp.StatusCode != tc.want {
			t.Fatalf("private allowed, %s%s: status %d, want %d", tc.host, tc.path, resp.StatusCode, tc.want)
		}
	}
}

func TestFillContinuesAfterClientLeft(t *testing.T) {
	h := newHarness(t)
	gate := make(chan struct{})
	data := testData(2 * testSlice)
	h.origin.set("/g", &originObj{data: data, gate: gate})

	resp := h.do("GET", testHost, "/g", nil)
	buf := make([]byte, 256)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() // not fully read: the connection is closed
	eventually(t, func() bool { return len(h.s.Active()) == 0 })
	close(gate)

	h.waitSlices(testService, "/g", 1)
	if ev := h.events(1)[0]; ev.BytesStored != testSlice || ev.BytesWAN != testSlice {
		t.Fatalf("event %+v", ev)
	}
	time.Sleep(100 * time.Millisecond)
	if got := h.origin.ranges("/g"); len(got) != 1 {
		t.Fatalf("new slices were started for a gone client: %v", got)
	}
}

func TestHEADOfRecordedObjectStaysLocal(t *testing.T) {
	h := newHarness(t)
	data := testData(2500)
	h.origin.set("/o", &originObj{data: data})
	h.get("GET", testHost, "/o", nil)
	h.waitSlices(testService, "/o", 3)
	n := len(h.origin.requests())

	resp, body := h.get("HEAD", testHost, "/o", hdr("Range", "bytes=-10"))
	if resp.StatusCode != http.StatusPartialContent || resp.ContentLength != 10 || len(body) != 0 ||
		resp.Header.Get("Content-Range") != "bytes 2490-2499/2500" || resp.Header.Get(cacheStatusHeader) != statusHit {
		t.Fatalf("status %d, length %d, %v", resp.StatusCode, resp.ContentLength, resp.Header)
	}
	if len(h.origin.requests()) != n {
		t.Fatal("HEAD of a recorded object went upstream")
	}
}

func TestHTTPSRedirectVerifiesCertificate(t *testing.T) {
	h := newHarness(t, withUntrustedTLSOrigin())
	h.tls.set("/data", &originObj{data: testData(100)})
	h.origin.set("/r", &originObj{location: "https://secure.example/data"})
	if resp, _ := h.get("GET", testHost, "/r", nil); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 for an unverifiable certificate", resp.StatusCode)
	}
	if n := len(h.tls.requests()); n != 0 {
		t.Fatalf("%d requests reached the untrusted origin", n)
	}
}

func TestACLRefusesOtherNetworks(t *testing.T) {
	h := newHarness(t)
	h.origin.set("/f", &originObj{data: testData(10)})
	for _, remote := range []string{"203.0.113.5:4000", "[2001:db8::1]:4000", "garbage"} {
		r := httptest.NewRequest("GET", "/lancache-heartbeat", nil)
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		h.s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || w.Header().Get(processedByHeader) != "" {
			t.Fatalf("%s: status %d", remote, w.Code)
		}
	}
	if st := h.s.Stats(); st.Refused != 3 || st.Requests != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func countOf(ss []string, s string) int {
	n := 0
	for _, x := range ss {
		if x == s {
			n++
		}
	}
	return n
}
