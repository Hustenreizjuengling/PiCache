package proxy

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	cachestore "github.com/hustenreizjuengling/picache/internal/dlcache/store"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

func TestHeartbeat(t *testing.T) {
	h := newHarness(t)
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		resp, _ := h.get(method, "10.0.0.5", "/lancache-heartbeat", nil)
		if resp.StatusCode != http.StatusNoContent || resp.Header.Get(processedByHeader) != testInstance {
			t.Fatalf("%s: status %d, processed-by %q", method, resp.StatusCode, resp.Header.Get(processedByHeader))
		}
		if resp.Header.Get("Access-Control-Allow-Origin") != "*" || resp.Header.Get("Access-Control-Expose-Headers") != "*" {
			t.Fatalf("%s: CORS headers missing: %v", method, resp.Header)
		}
		pna := resp.Header.Get("Access-Control-Allow-Private-Network")
		if (method == http.MethodOptions) != (pna == "true") {
			t.Fatalf("%s: Access-Control-Allow-Private-Network = %q", method, pna)
		}
	}
	if len(h.origin.requests()) != 0 || len(h.logs.all()) != 0 {
		t.Fatal("heartbeat must neither reach upstream nor be logged")
	}
}

func TestRequestGuards(t *testing.T) {
	h := newHarness(t)
	h.origin.set("/f", &originObj{data: testData(100)})
	tests := []struct {
		name   string
		method string
		host   string
		path   string
		header http.Header
		want   int
	}{
		{"loop", "GET", testHost, "/f", hdr(processedByHeader, "other, "+testInstance), http.StatusLoopDetected},
		{"ip literal host", "GET", "192.0.2.10", "/f", nil, http.StatusBadRequest},
		{"ipv6 literal host", "GET", "[2001:db8::1]:80", "/f", nil, http.StatusBadRequest},
		{"unknown host", "GET", "evil.example", "/f", nil, http.StatusForbidden},
		{"steam UA, not a depot path", "GET", "cache9.steamcontent.com", "/other", hdr("User-Agent", "Valve/Steam HTTP Client 1.0"), http.StatusForbidden},
		{"steam UA, POST", "POST", "cache9.steamcontent.com", "/depot/1/chunk/aa", hdr("User-Agent", "Valve/Steam HTTP Client 1.0"), http.StatusForbidden},
		{"steam UA, non-canonical", "GET", "cache9.steamcontent.com", "/depot/1/../../admin", hdr("User-Agent", "Valve/Steam HTTP Client 1.0"), http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := h.get(tc.method, tc.host, tc.path, tc.header)
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
	if n := len(h.origin.requests()); n != 0 {
		t.Fatalf("%d requests reached the origin", n)
	}
	st := h.s.Stats()
	if st.Refused < 5 || st.Errors < 1 {
		t.Fatalf("stats %+v", st)
	}
	if len(st.SteamHostsRefused) != 1 || st.SteamHostsRefused[0] != "cache9.steamcontent.com" {
		t.Fatalf("steam refused hosts %v", st.SteamHostsRefused)
	}
}

func TestMissHitPartial(t *testing.T) {
	h := newHarness(t)
	data := testData(5000)
	h.origin.set("/game/file.pak", &originObj{data: data})

	resp, body := h.get("GET", testHost, "/game/file.pak?token=secret", hdr("Cookie", "sid=1", "Accept-Encoding", "gzip"))
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("miss: status %d, %d bytes", resp.StatusCode, len(body))
	}
	if got := resp.Header.Get(cacheStatusHeader); got != statusMiss {
		t.Fatalf("cache status %q", got)
	}
	for _, k := range []string{"Etag", "Set-Cookie", "X-Cache"} {
		if resp.Header.Get(k) != "" {
			t.Fatalf("response carries %s", k)
		}
	}
	if resp.Header.Get("X-Keep") != "kept" || resp.Header.Get("Accept-Ranges") != "bytes" || resp.Header.Get(processedByHeader) != testInstance {
		t.Fatalf("headers %v", resp.Header)
	}
	reqs := h.origin.requests()
	if len(reqs) != 5 {
		t.Fatalf("origin got %d requests, want 5 slices", len(reqs))
	}
	slices.SortFunc(reqs, func(a, b originReq) int { return cmp.Compare(len(a.rng), len(b.rng))*2 + cmp.Compare(a.rng, b.rng) })
	for i, r := range reqs { // read-ahead: any order
		want := "bytes=" + strconv.Itoa(i*testSlice) + "-" + strconv.Itoa(i*testSlice+testSlice-1)
		if r.rng != want || r.uri != "/game/file.pak?token=secret" || r.host != testHost {
			t.Fatalf("request %d: %+v", i, r)
		}
		if r.header.Get("Cookie") != "" || r.header.Get("Accept-Encoding") != "identity" ||
			r.header.Get(processedByHeader) != testInstance || r.header.Get("X-Forwarded-For") != "" {
			t.Fatalf("request %d headers: %v", i, r.header)
		}
	}
	h.waitSlices(testService, "/game/file.pak", 5)
	obj := h.store.object(testService, "/game/file.pak")
	if obj.meta.Total != 5000 || obj.meta.Header.Get("Etag") != "" || obj.meta.Header.Get("Set-Cookie") != "" ||
		obj.meta.Header.Get("Content-Type") != "application/x-test" || obj.meta.GroupKey == "" || obj.meta.Host != testHost {
		t.Fatalf("meta %+v", obj.meta)
	}
	ev := h.events(1)[0]
	if ev.CacheStatus != statusMiss || ev.BytesSent != 5000 || ev.BytesWAN != 5000 || ev.BytesStored != 5000 ||
		ev.BytesHit != 0 || ev.Path != "/game/file.pak" || ev.Status != 200 || ev.ClientName != "tester" ||
		!strings.HasPrefix(ev.Label, "label:") {
		t.Fatalf("miss event %+v", ev)
	}

	resp, body = h.get("GET", testHost, "/game/file.pak", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) || resp.Header.Get(cacheStatusHeader) != statusHit {
		t.Fatalf("hit: status %d, cache %q", resp.StatusCode, resp.Header.Get(cacheStatusHeader))
	}
	if len(h.origin.requests()) != 5 {
		t.Fatal("hit went upstream")
	}
	ev = h.events(2)[1]
	if ev.CacheStatus != statusHit || ev.BytesHit != 5000 || ev.BytesWAN != 0 {
		t.Fatalf("hit event %+v", ev)
	}
	// Cached data goes to the unwrapped ResponseWriter (sendfile).
	for _, w := range h.store.writerTypes() {
		if w != "*http.response" {
			t.Fatalf("WriteRange got %s", w)
		}
	}

	h.store.dropSlice(testService, "/game/file.pak", 2)
	resp, body = h.get("GET", testHost, "/game/file.pak", nil)
	if !bytes.Equal(body, data) || resp.Header.Get(cacheStatusHeader) != statusPartial {
		t.Fatalf("partial: cache %q", resp.Header.Get(cacheStatusHeader))
	}
	if got := h.origin.ranges("/game/file.pak"); len(got) != 6 || got[5] != "bytes=2048-3071" {
		t.Fatalf("partial ranges %v", got)
	}
	ev = h.events(3)[2]
	if ev.CacheStatus != statusPartial || ev.BytesHit != 3976 || ev.BytesWAN != 1024 {
		t.Fatalf("partial event %+v", ev)
	}
	st := h.s.Stats()
	if st.Requests != 3 || st.BytesHit != 8976 || st.BytesWAN != 6024 || st.PassThrough {
		t.Fatalf("stats %+v", st)
	}
}

func TestRanges(t *testing.T) {
	h := newHarness(t)
	data := testData(5000)
	h.origin.set("/r", &originObj{data: data})
	h.origin.set("/s", &originObj{data: data})
	h.origin.set("/m", &originObj{data: data})

	// Single range on an unknown object: fetched from its first slice.
	resp, body := h.get("GET", testHost, "/r", hdr("Range", "bytes=1500-2600"))
	if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Range") != "bytes 1500-2600/5000" ||
		!bytes.Equal(body, data[1500:2601]) {
		t.Fatalf("range: %d %q %d bytes", resp.StatusCode, resp.Header.Get("Content-Range"), len(body))
	}
	if got := h.origin.ranges("/r"); len(got) != 2 || got[0] != "bytes=1024-2047" || got[1] != "bytes=2048-3071" {
		t.Fatalf("origin ranges %v", got)
	}

	// Suffix range: slice 0 first (size unknown), then the last slice.
	resp, body = h.get("GET", testHost, "/s", hdr("Range", "bytes=-100"))
	if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Range") != "bytes 4900-4999/5000" ||
		!bytes.Equal(body, data[4900:]) {
		t.Fatalf("suffix: %d %q", resp.StatusCode, resp.Header.Get("Content-Range"))
	}
	if got := h.origin.ranges("/s"); len(got) != 2 || got[0] != "bytes=0-1023" || got[1] != "bytes=4096-5119" {
		t.Fatalf("suffix origin ranges %v", got)
	}

	// Multi-range: multipart/byteranges with a precomputed length.
	resp, body = h.get("GET", testHost, "/m", hdr("Range", "bytes=0-9, 2000-2009,4990-"))
	mt, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusPartialContent || err != nil || mt != "multipart/byteranges" {
		t.Fatalf("multi: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Fatalf("multipart length %s, body %d", resp.Header.Get("Content-Length"), len(body))
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	want := []struct {
		cr   string
		data []byte
	}{{"bytes 0-9/5000", data[:10]}, {"bytes 2000-2009/5000", data[2000:2010]}, {"bytes 4990-4999/5000", data[4990:]}}
	for i, w := range want {
		p, err := mr.NextPart()
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		got, _ := io.ReadAll(p)
		if p.Header.Get("Content-Range") != w.cr || p.Header.Get("Content-Type") != "application/x-test" || !bytes.Equal(got, w.data) {
			t.Fatalf("part %d: %v %d bytes", i, p.Header, len(got))
		}
	}
	if _, err := mr.NextPart(); err != io.EOF {
		t.Fatalf("extra part: %v", err)
	}

	// Overlapping, descending or too many ranges: the full object.
	many := make([]string, 17)
	for i := range many {
		many[i] = strconv.Itoa(i*100) + "-" + strconv.Itoa(i*100+1)
	}
	for _, rng := range []string{"bytes=0-100,50-60", "bytes=200-300,0-10", "bytes=" + strings.Join(many, ",")} {
		resp, body = h.get("GET", testHost, "/m", hdr("Range", rng))
		if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
			t.Fatalf("%s: status %d", rng, resp.StatusCode)
		}
	}
	// Invalid Range headers are ignored.
	resp, _ = h.get("GET", testHost, "/m", hdr("Range", "bytes=10-5"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("invalid range: status %d", resp.StatusCode)
	}
}

func TestRangeNotSatisfiable(t *testing.T) {
	h := newHarness(t)
	h.origin.set("/u", &originObj{data: testData(5000)})
	h.origin.set("/k", &originObj{data: testData(5000)})
	h.origin.set("/empty", &originObj{data: nil})

	// Unknown object: the first slice (index 8) is beyond the end → size
	// from slice 0 → 416.
	resp, body := h.get("GET", testHost, "/u", hdr("Range", "bytes=9000-9999"))
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable || resp.Header.Get("Content-Range") != "bytes */5000" || len(body) != 0 {
		t.Fatalf("unknown: %d %q", resp.StatusCode, resp.Header.Get("Content-Range"))
	}
	if got := h.origin.ranges("/u"); len(got) != 2 || got[1] != "bytes=0-1023" {
		t.Fatalf("origin ranges %v", got)
	}

	// Known object: answered without upstream.
	h.get("GET", testHost, "/k", nil)
	h.waitSlices(testService, "/k", 5)
	n := len(h.origin.requests())
	resp, _ = h.get("GET", testHost, "/k", hdr("Range", "bytes=5000-"))
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable || resp.Header.Get("Content-Range") != "bytes */5000" {
		t.Fatalf("known: %d", resp.StatusCode)
	}
	if len(h.origin.requests()) != n {
		t.Fatal("416 of a known object went upstream")
	}

	// Empty object: 416 for slice 0 → passed through without Range.
	resp, body = h.get("GET", testHost, "/empty", nil)
	if resp.StatusCode != http.StatusOK || len(body) != 0 || resp.Header.Get(cacheStatusHeader) != statusPass {
		t.Fatalf("empty: %d %q", resp.StatusCode, resp.Header.Get(cacheStatusHeader))
	}
	if got := h.origin.ranges("/empty"); len(got) != 2 || got[0] != "bytes=0-1023" || got[1] != "" {
		t.Fatalf("empty ranges %v", got)
	}
	// Also when the client sent a Range: the retry goes without it.
	resp, _ = h.get("GET", testHost, "/empty", hdr("Range", "bytes=0-9", "If-Range", "Wed, 21 Oct 2015 07:28:00 GMT"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("empty with range: %d", resp.StatusCode)
	}
	if reqs := h.origin.requests(); reqs[len(reqs)-1].rng != "" || reqs[len(reqs)-1].header.Get("If-Range") != "" {
		t.Fatalf("retry after 416 carried the range: %+v", reqs[len(reqs)-1])
	}
}

func TestHeadAndConditionals(t *testing.T) {
	h := newHarness(t)
	data := testData(3000)
	mod := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	h.origin.set("/h", &originObj{data: data, modTime: mod})

	resp, body := h.get("HEAD", testHost, "/h", nil)
	if resp.StatusCode != http.StatusOK || resp.ContentLength != 3000 || len(body) != 0 || resp.Header.Get("Last-Modified") != mod.Format(http.TimeFormat) {
		t.Fatalf("head: %d cl %d", resp.StatusCode, resp.ContentLength)
	}
	h.waitSlices(testService, "/h", 1) // the fill of slice 0 completed and was stored
	if got := h.origin.ranges("/h"); len(got) != 1 || got[0] != "bytes=0-1023" {
		t.Fatalf("head fetched %v", got)
	}
	h.get("GET", testHost, "/h", nil)
	h.waitSlices(testService, "/h", 3)
	upstream := len(h.origin.requests())

	lm := mod.Format(http.TimeFormat)
	other := mod.Add(time.Hour).Format(http.TimeFormat)
	tests := []struct {
		name   string
		header http.Header
		status int
		body   []byte
	}{
		{"if-range date matches", hdr("Range", "bytes=10-19", "If-Range", lm), http.StatusPartialContent, data[10:20]},
		{"if-range date differs", hdr("Range", "bytes=10-19", "If-Range", other), http.StatusOK, data},
		{"if-range etag", hdr("Range", "bytes=10-19", "If-Range", `"v1"`), http.StatusOK, data},
		{"if-modified-since equal", hdr("If-Modified-Since", lm), http.StatusNotModified, nil},
		{"if-modified-since later", hdr("If-Modified-Since", other), http.StatusNotModified, nil},
		{"if-modified-since earlier", hdr("If-Modified-Since", mod.Add(-time.Hour).Format(http.TimeFormat)), http.StatusOK, data},
		{"if-none-match wins", hdr("If-Modified-Since", lm, "If-None-Match", `"x"`), http.StatusOK, data},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := h.get("GET", testHost, "/h", tc.header)
			if resp.StatusCode != tc.status || !bytes.Equal(body, tc.body) {
				t.Fatalf("status %d (%d bytes), want %d", resp.StatusCode, len(body), tc.status)
			}
		})
	}
	if len(h.origin.requests()) != upstream {
		t.Fatal("conditional requests of a cached object went upstream")
	}
}

func TestCollapsingStreams(t *testing.T) {
	h := newHarness(t)
	data := testData(2 * testSlice)
	gate := make(chan struct{})
	h.origin.set("/big", &originObj{data: data, gate: gate})
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()

	const clients = 5
	var wg sync.WaitGroup
	firstHalf := make(chan error, clients)
	results := make(chan error, clients)
	for range clients {
		wg.Go(func() {
			resp := h.do("GET", testHost, "/big", nil)
			defer resp.Body.Close()
			buf := make([]byte, testSlice/2)
			_, err := io.ReadFull(resp.Body, buf)
			if err == nil && !bytes.Equal(buf, data[:testSlice/2]) {
				err = errors.New("wrong first half")
			}
			firstHalf <- err
			rest, err := io.ReadAll(resp.Body)
			if err == nil && !bytes.Equal(append(buf, rest...), data) {
				err = errors.New("wrong body")
			}
			results <- err
		})
	}
	// Every client streams the first half while the upstream is still
	// blocked in the middle of slice 0.
	for range clients {
		select {
		case err := <-firstHalf:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("clients do not stream from the growing fill")
		}
	}
	close(gate)
	wg.Wait()
	for range clients {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	got := h.origin.ranges("/big")
	if len(got) != 2 || got[0] != "bytes=0-1023" || got[1] != "bytes=1024-2047" {
		t.Fatalf("origin requests %v, want one per slice", got)
	}
	h.waitSlices(testService, "/big", 2)
	evs := h.events(clients)
	var wan, hit int64
	for _, ev := range evs {
		wan += ev.BytesWAN
		hit += ev.BytesHit
	}
	if wan != int64(len(data)) || hit != int64((clients-1)*len(data)) {
		t.Fatalf("wan %d hit %d", wan, hit)
	}
}

func TestReadAhead(t *testing.T) {
	h := newHarness(t)
	data := testData(5 * testSlice)
	// Slice 1 is answered only after slice 2 was requested: that only
	// happens if slice 2 is read ahead.
	h.origin.set("/ra", &originObj{data: data, waitRange: "bytes=1024-2047", waitFor: "bytes=2048-3071"})
	start := time.Now()
	resp, body := h.get("GET", testHost, "/ra", nil)
	if resp.StatusCode != 200 || !bytes.Equal(body, data) {
		t.Fatal("wrong body")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("slice 2 was not read ahead")
	}
	got := h.origin.ranges("/ra")
	if len(got) != 5 {
		t.Fatalf("origin requests %v, want one per slice", got)
	}
}

func TestNoRangeSupport(t *testing.T) {
	h := newHarness(t)
	small, large := testData(800), testData(5000)
	h.origin.set("/small", &originObj{data: small, noRange: true})
	h.origin.set("/large", &originObj{data: large, noRange: true})
	h.origin.set("/large2", &originObj{data: large, noRange: true})
	h.origin.set("/chunked", &originObj{data: large, noRange: true, noLength: true})

	// (a) A 200 no larger than a slice is the complete object.
	resp, body := h.get("GET", testHost, "/small", hdr("Range", "bytes=100-199"))
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, small[100:200]) {
		t.Fatalf("small: %d", resp.StatusCode)
	}
	h.waitSlices(testService, "/small", 1)
	if hosts, _ := h.s.NoSliceHosts(context.Background()); len(hosts) != 0 {
		t.Fatalf("(a) counted as range failure: %v", hosts)
	}

	// (b) A 200 larger than a slice: streamed, complete slices stored,
	// one failure counted.
	resp, body = h.get("GET", testHost, "/large", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, large) {
		t.Fatalf("large: %d", resp.StatusCode)
	}
	h.waitSlices(testService, "/large", 5)
	if !h.store.object(testService, "/large").meta.NoSlice {
		t.Fatal("object fetched without range support not marked")
	}
	resp, body = h.get("GET", testHost, "/large2", hdr("Range", "bytes=3000-3099"))
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, large[3000:3100]) {
		t.Fatalf("large range: %d", resp.StatusCode)
	}
	h.waitSlices(testService, "/large2", 4) // slices 0–3 passed by until byte 3099 (slice 3 completed)
	hosts, _ := h.s.NoSliceHosts(context.Background())
	if len(hosts) != 1 || hosts[0].Host != testHost || hosts[0].Failures != 2 || hosts[0].Marked {
		t.Fatalf("no-slice hosts %+v", hosts)
	}

	// (c) A 200 without Content-Length is served, never stored.
	resp, body = h.get("GET", testHost, "/chunked", hdr("Range", "bytes=0-9"))
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, large) || resp.Header.Get(cacheStatusHeader) != statusPass {
		t.Fatalf("chunked: %d %q", resp.StatusCode, resp.Header.Get(cacheStatusHeader))
	}
	if h.store.object(testService, "/chunked") != nil {
		t.Fatal("object without length was stored")
	}
}

func TestNoSliceMarking(t *testing.T) {
	h := newHarness(t)
	const host = "noslice.example"
	data := testData(3000)
	for _, p := range []string{"/1", "/2", "/3", "/4"} {
		h.origin.set(p, &originObj{data: data, noRange: true})
	}
	for _, p := range []string{"/1", "/1", "/2", "/3"} {
		if _, body := h.get("GET", host, p, nil); !bytes.Equal(body, data) {
			t.Fatalf("%s: wrong body", p)
		}
		h.waitSlices(testService, p, 3)
		h.store.dropSlice(testService, p, 0) // force a fetch on the repeat of /1
	}
	hosts, _ := h.s.NoSliceHosts(context.Background())
	if len(hosts) != 1 || !hosts[0].Marked || hosts[0].Failures != 3 {
		t.Fatalf("hosts %+v", hosts)
	}
	// Marked: requests go without Range and are stored.
	resp, body := h.get("GET", host, "/4", hdr("Range", "bytes=1500-1599"))
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, data[1500:1600]) {
		t.Fatalf("marked: %d", resp.StatusCode)
	}
	if got := h.origin.ranges("/4"); len(got) != 1 || got[0] != "" {
		t.Fatalf("marked host got ranges %v", got)
	}
	h.waitSlices(testService, "/4", 2)

	// Persisted and reloaded.
	eventually(t, func() bool {
		var n int
		_ = h.db.R.QueryRow(`SELECT COUNT(*) FROM proxy_noslice_hosts WHERE host = ? AND marked = 1`, host).Scan(&n)
		return n == 1
	})
	tr, err := newNoSliceTracker(context.Background(), h.db, h.s.log)
	if err != nil {
		t.Fatal(err)
	}
	if l := tr.list(); len(l) != 1 || !l[0].Marked || l[0].Failures != 3 {
		t.Fatalf("reloaded %+v", l)
	}

	// A probe with a valid 206 clears the mark.
	h.origin.set("/5", &originObj{data: data})
	h.s.noslice.mu.Lock()
	h.s.noslice.hosts[host].lastProbe = time.Now().Add(-25 * time.Hour)
	h.s.noslice.mu.Unlock()
	h.get("GET", host, "/5", nil)
	if hosts, _ := h.s.NoSliceHosts(context.Background()); len(hosts) != 0 {
		t.Fatalf("valid 206 did not reset: %+v", hosts)
	}

	// Reset via the API method.
	h.s.noslice.failure(host, cachestore.ObjectID(testService, "/x"), time.Now())
	if err := h.s.ResetNoSlice(context.Background(), "NoSlice.Example."); err != nil {
		t.Fatal(err)
	}
	if err := h.s.ResetNoSlice(context.Background(), host); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("reset unknown: %v", err)
	}
	if err := h.s.ResetNoSlice(context.Background(), "10.0.0.1"); apperr.KindOf(err) != apperr.KindInvalid {
		t.Fatalf("reset ip: %v", err)
	}
}

func TestContentEncodingPassThrough(t *testing.T) {
	h := newHarness(t)
	data := testData(3000)
	h.origin.set("/gz", &originObj{data: data, gzip: true})
	resp, body := h.get("GET", testHost, "/gz", hdr("Accept-Encoding", "gzip", "Range", "bytes=0-99"))
	if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Encoding") != "gzip" ||
		!bytes.Equal(body, data[:100]) || resp.Header.Get(cacheStatusHeader) != statusPass {
		t.Fatalf("status %d, %v", resp.StatusCode, resp.Header)
	}
	reqs := h.origin.requests()
	last := reqs[len(reqs)-1]
	if last.rng != "bytes=0-99" || last.header.Get("Accept-Encoding") != "gzip" {
		t.Fatalf("pass-through request %+v", last)
	}
	if h.store.object(testService, "/gz") != nil {
		t.Fatal("encoded response stored")
	}
	if ev := h.events(1)[0]; ev.CacheStatus != statusPass {
		t.Fatalf("event %+v", ev)
	}
}

func TestSizeChange(t *testing.T) {
	h := newHarness(t)
	oldData, newData := testData(5000), bytes.Repeat([]byte("n"), 6000)
	h.origin.set("/v", &originObj{data: oldData})
	h.get("GET", testHost, "/v", nil)
	h.waitSlices(testService, "/v", 5)

	// Before anything was sent: re-planned with the new size.
	h.origin.set("/v", &originObj{data: newData})
	h.store.dropSlice(testService, "/v", 0)
	resp, body := h.get("GET", testHost, "/v", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, newData) {
		t.Fatalf("before commit: %d, %d bytes", resp.StatusCode, len(body))
	}
	eventually(t, func() bool { o := h.store.object(testService, "/v"); return o != nil && o.meta.Total == 6000 })

	// After headers were sent with the old size: the response is aborted.
	h.origin.set("/w", &originObj{data: oldData})
	h.get("GET", testHost, "/w", nil)
	h.waitSlices(testService, "/w", 5)
	h.origin.set("/w", &originObj{data: newData})
	h.store.dropSlice(testService, "/w", 3)
	resp = h.do("GET", testHost, "/w", nil)
	_, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil || resp.ContentLength != 5000 {
		t.Fatalf("expected an aborted 5000-byte response, got err %v, length %d", err, resp.ContentLength)
	}
	evs := h.events(4)
	if ev := evs[3]; ev.CacheStatus != statusError {
		t.Fatalf("aborted event %+v", ev)
	}
	eventually(t, func() bool { o := h.store.object(testService, "/w"); return o != nil && o.meta.Total == 6000 })
}

func TestRedirects(t *testing.T) {
	h := newHarness(t)
	data := testData(1500)
	h.origin.set("/data", &originObj{data: data})
	h.origin.set("/r", &originObj{location: "/data"})
	h.origin.set("/r2", &originObj{location: "http://other.example/data", status: http.StatusTemporaryRedirect})
	h.origin.set("/private", &originObj{location: "http://10.1.2.3/data"})
	h.origin.set("/metadata", &originObj{location: "http://169.254.169.254/latest"})
	h.origin.set("/ssh", &originObj{location: "http://other.example:22/data"})
	h.origin.set("/loop", &originObj{location: "/loop"})

	for _, p := range []string{"/r", "/r2"} {
		resp, body := h.get("GET", testHost, p, hdr("Cookie", "a=b"))
		if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
		h.waitSlices(testService, p, 2) // stored under the original key
	}
	for _, r := range h.origin.requests() {
		if r.uri == "/data" && r.rng == "" {
			t.Fatalf("Range not kept on the redirect: %+v", r)
		}
	}
	for _, p := range []string{"/private", "/metadata", "/ssh", "/loop"} {
		resp, _ := h.get("GET", testHost, p, nil)
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("%s: status %d, want 502", p, resp.StatusCode)
		}
	}
	if got := h.origin.ranges("/loop"); len(got) != 1+maxRedirects {
		t.Fatalf("loop followed %d times", len(got))
	}
	// Private upstreams can be allowed explicitly; link-local never.
	h.updateSettings(func(a *settings.All) { a.DownloadCache.AllowPrivateUpstreams = true })
	if resp, _ := h.get("GET", testHost, "/private", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("allowed private redirect: %d", resp.StatusCode)
	}
	if resp, _ := h.get("GET", testHost, "/metadata", nil); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("link-local redirect: %d", resp.StatusCode)
	}
}

func TestUpstreamErrors(t *testing.T) {
	h := newHarness(t)
	h.origin.set("/forbidden", &originObj{status: http.StatusForbidden})
	h.origin.set("/broken", &originObj{status: http.StatusBadGateway})
	data := testData(700)
	h.alt.set("/only-alt", &originObj{data: data})

	resp, body := h.get("GET", testHost, "/forbidden", hdr("Range", "bytes=0-0"))
	if resp.StatusCode != http.StatusForbidden || string(body) != "origin says 403" || resp.Header.Get("X-Origin") != "yes" ||
		resp.Header.Get(processedByHeader) != "" {
		t.Fatalf("403: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if resp, _ := h.get("GET", testHost, "/broken", nil); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("502 passthrough: %d", resp.StatusCode)
	}
	// 404 from the first address: retried once on the next A record.
	resp, body = h.get("GET", testHost, "/only-alt", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("404 retry: %d", resp.StatusCode)
	}
	if alt := h.alt.requests(); len(alt) != 1 || alt[0].host != testHost {
		t.Fatalf("alt origin requests %+v", alt)
	}
	h.waitSlices(testService, "/only-alt", 1)
	for _, p := range []string{"/forbidden", "/broken"} {
		if h.store.object(testService, p) != nil {
			t.Fatalf("%s stored", p)
		}
	}
}

func TestUpgradeRequiredHTTPS(t *testing.T) {
	h := newHarness(t, withTLSOrigin())
	h.origin.status, h.origin.tlsCheck = http.StatusUpgradeRequired, false
	data := testData(1500)
	h.tls.set("/a", &originObj{data: data})
	h.tls.set("/b", &originObj{data: data})

	resp, body := h.get("GET", "example.com", "/a?x=1", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("426 retry: %d", resp.StatusCode)
	}
	h.waitSlices(testService, "/a", 2)
	for _, r := range h.tls.requests() {
		if !r.tls || r.uri != "/a?x=1" || r.host != "example.com" {
			t.Fatalf("https request %+v", r)
		}
	}
	plain := len(h.origin.requests())
	if _, body := h.get("GET", "example.com", "/b", nil); !bytes.Equal(body, data) {
		t.Fatal("wrong body")
	}
	if len(h.origin.requests()) != plain {
		t.Fatal("https-only host was asked over http again")
	}
}

func TestNonCanonicalPathsPassThrough(t *testing.T) {
	h := newHarness(t)
	for _, uri := range []string{"/a//b", "/a/./b", "/a/%2e%2e/b", "/a%2Fb", "/a%5cb", "/a%41b", "/a%00b", "/a%0ab?q=1"} {
		resp, _ := h.get("GET", testHost, uri, nil)
		if resp.StatusCode != http.StatusNotFound || resp.Header.Get(cacheStatusHeader) != "" {
			t.Fatalf("%s: status %d", uri, resp.StatusCode)
		}
		reqs := h.origin.requests()
		if last := reqs[len(reqs)-1]; last.uri != uri || last.rng != "" {
			t.Fatalf("%s: upstream got %q", uri, last.uri)
		}
	}
	h.store.mu.Lock()
	n := len(h.store.objs)
	h.store.mu.Unlock()
	if n != 0 {
		t.Fatal("non-canonical path stored")
	}
}

func TestPassThroughModes(t *testing.T) {
	h := newHarness(t)
	data := testData(2000)
	h.origin.set("/p", &originObj{data: data})
	h.origin.set("/server-status", &originObj{data: []byte("load")})
	h.origin.set("/post", &originObj{data: []byte("ok")})

	// Disabled service.
	resp, body := h.get("GET", "off.example", "/p", hdr("Range", "bytes=5-9"))
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, data[5:10]) || resp.Header.Get(cacheStatusHeader) != statusPass {
		t.Fatalf("disabled: %d", resp.StatusCode)
	}
	// Special path.
	if _, body := h.get("GET", testHost, "/server-status", nil); string(body) != "load" {
		t.Fatalf("server-status: %q", body)
	}
	// Other methods with a bounded body.
	req, _ := http.NewRequest("POST", h.proxy.URL+"/post", strings.NewReader("payload"))
	req.Host = testHost
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	reqs := h.origin.requests()
	if last := reqs[len(reqs)-1]; last.method != "POST" || last.body != "payload" {
		t.Fatalf("post: %+v", last)
	}
	h.store.mu.Lock()
	n := len(h.store.objs)
	h.store.mu.Unlock()
	if n != 0 {
		t.Fatal("pass-through stored")
	}
	for _, ev := range h.events(3) {
		if ev.CacheStatus != statusPass {
			t.Fatalf("event %+v", ev)
		}
	}
}

func TestPassThroughWithoutStore(t *testing.T) {
	h := newHarness(t, withoutStore())
	data := testData(2000)
	h.origin.set("/p", &originObj{data: data})
	resp, body := h.get("GET", testHost, "/p", hdr("Range", "bytes=10-19", "Accept-Encoding", "br"))
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, data[10:20]) || resp.Header.Get(cacheStatusHeader) != statusPass {
		t.Fatalf("status %d", resp.StatusCode)
	}
	r := h.origin.requests()[0]
	if r.rng != "bytes=10-19" || r.header.Get("Accept-Encoding") != "br" {
		t.Fatalf("upstream request %+v", r)
	}
	if !h.s.Stats().PassThrough {
		t.Fatal("stats do not report pass-through")
	}
}

func TestStoreClosedServesUncached(t *testing.T) {
	h := newHarness(t)
	h.store.closed = true
	data := testData(3000)
	h.origin.set("/c", &originObj{data: data})
	resp, body := h.get("GET", testHost, "/c", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestStoreFullNotStored(t *testing.T) {
	h := newHarness(t)
	h.full.Store(true)
	data := testData(3000)
	h.origin.set("/c", &originObj{data: data})
	if _, body := h.get("GET", testHost, "/c", nil); !bytes.Equal(body, data) {
		t.Fatal("wrong body")
	}
	if ev := h.events(1)[0]; ev.BytesStored != 0 || ev.BytesWAN != 3000 {
		t.Fatalf("event %+v", ev)
	}
	if h.store.object(testService, "/c") != nil {
		t.Fatal("stored while the store is full")
	}
}

func TestNocache(t *testing.T) {
	h := newHarness(t)
	data := testData(2500)
	h.origin.set("/n", &originObj{data: data})
	h.get("GET", testHost, "/n", nil)
	h.waitSlices(testService, "/n", 3)
	before := len(h.origin.requests())

	// Not allowed: ignored (HIT), the query still goes nowhere.
	resp, _ := h.get("GET", testHost, "/n?nocache=1", nil)
	if resp.Header.Get(cacheStatusHeader) != statusHit || len(h.origin.requests()) != before {
		t.Fatalf("nocache from a client not allowed: %q", resp.Header.Get(cacheStatusHeader))
	}
	h.updateSettings(func(a *settings.All) { a.DownloadCache.NocacheClients = []string{"127.0.0.0/8"} })
	for _, q := range []string{"?nocache=0", "?nocache="} {
		if resp, _ := h.get("GET", testHost, "/n"+q, nil); resp.Header.Get(cacheStatusHeader) != statusHit {
			t.Fatalf("%s: %q", q, resp.Header.Get(cacheStatusHeader))
		}
	}
	h.store.mu.Lock()
	writes := h.store.writes[cachestore.ObjectID(testService, "/n")]
	h.store.mu.Unlock()
	resp, body := h.get("GET", testHost, "/n?nocache=1", nil)
	if resp.Header.Get(cacheStatusHeader) != statusBypass || !bytes.Equal(body, data) {
		t.Fatalf("bypass: %q", resp.Header.Get(cacheStatusHeader))
	}
	if got := len(h.origin.requests()) - before; got != 3 {
		t.Fatalf("bypass fetched %d slices, want 3", got)
	}
	eventually(t, func() bool {
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		return h.store.writes[cachestore.ObjectID(testService, "/n")] == writes+3
	})
	evs := h.events(5)
	if ev := evs[len(evs)-1]; ev.CacheStatus != statusBypass {
		t.Fatalf("event %+v", ev)
	}
}

func TestSteamUserAgent(t *testing.T) {
	h := newHarness(t)
	data := testData(1200)
	h.origin.set("/depot/228990/chunk/0123456789abcdef0123456789abcdef01234567", &originObj{data: data})
	ua := hdr("User-Agent", "Valve/Steam HTTP Client 1.0")
	path := "/depot/228990/chunk/0123456789abcdef0123456789abcdef01234567"
	for _, host := range []string{"cache1-fra1.steamcontent.com", "cache7-ams1.steamcontent.com"} {
		resp, body := h.get("GET", host, path, ua)
		if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
			t.Fatalf("%s: %d", host, resp.StatusCode)
		}
		h.waitSlices("steam", path, 2)
	}
	if got := h.origin.ranges(path); len(got) != 2 {
		t.Fatalf("steam content from two CDN hosts fetched %d times, want once per slice", len(got))
	}
	evs := h.events(2)
	if evs[0].Service != "steam" || evs[0].GroupKey != "steam:depot:228990" || evs[1].CacheStatus != statusHit {
		t.Fatalf("events %+v", evs)
	}
}

func TestPerClientFillCap(t *testing.T) {
	h := newHarness(t)
	h.updateSettings(func(a *settings.All) { a.Cache.MaxFillsPerClient = 1 })
	gate := make(chan struct{})
	defer close(gate)
	a, b := testData(1000), testData(1000)
	h.origin.set("/a", &originObj{data: a, gate: gate})
	h.origin.set("/b", &originObj{data: b})

	respA := h.do("GET", testHost, "/a", nil)
	defer respA.Body.Close()
	buf := make([]byte, 500)
	if _, err := io.ReadFull(respA.Body, buf); err != nil { // the fill of /a holds the client's only slot
		t.Fatal(err)
	}
	start := time.Now()
	resp, body := h.get("GET", testHost, "/b", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, b) {
		t.Fatalf("b: %d", resp.StatusCode)
	}
	if d := time.Since(start); d < h.s.tm.demandWait {
		t.Fatalf("b did not wait for a slot (%v)", d)
	}
	if h.store.object(testService, "/b") != nil {
		t.Fatal("b was stored without a fill slot")
	}
	evs := h.events(1)
	if evs[0].Path != "/b" || evs[0].BytesStored != 0 || evs[0].BytesWAN != 1000 {
		t.Fatalf("event %+v", evs[0])
	}
	if st := h.s.Stats(); st.ActiveFills != 1 || st.ActiveClients != 1 {
		t.Fatalf("stats %+v", st)
	}
	if act := h.s.Active(); len(act) != 1 || act[0].Path != "/a" || act[0].Total != 1000 {
		t.Fatalf("active %+v", act)
	}
	// Both requests belong to one content group (service:host fallback).
	if dl := h.s.ActiveDownloads(); len(dl) != 1 || dl[0].InFlight != 1 || dl[0].Label != "label:"+testService+":"+testHost {
		t.Fatalf("downloads %+v", dl)
	}
}

func TestIgnoreLogs(t *testing.T) {
	h := newHarness(t)
	h.ids.ignore.Store(true)
	h.origin.set("/i", &originObj{data: testData(100)})
	h.get("GET", testHost, "/i", nil)
	h.waitSlices(testService, "/i", 1)
	time.Sleep(50 * time.Millisecond)
	if n := len(h.logs.all()); n != 0 {
		t.Fatalf("%d events for an ignored client", n)
	}
	// Excluded from the raw data only: the event carries NoLog (the logs
	// package counts it in the statistics).
	h.ids.ignore.Store(false)
	h.ids.ignoreLogs.Store(true)
	h.get("GET", testHost, "/i", nil)
	deadline := time.Now().Add(2 * time.Second)
	for len(h.logs.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ev := h.logs.all(); len(ev) != 1 || !ev[0].NoLog || ev[0].NoStats {
		t.Fatalf("events %+v", ev)
	}
}
