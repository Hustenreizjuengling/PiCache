package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	cachestore "github.com/hustenreizjuengling/picache/internal/dlcache/store"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

func TestParseRange(t *testing.T) {
	many := make([]string, 17)
	for i := range many {
		many[i] = strconv.Itoa(i) + "-" + strconv.Itoa(i)
	}
	tests := []struct {
		in      []string
		want    []rangeSpec // nil: no usable Range
		tooMany bool
	}{
		{nil, nil, false},
		{[]string{"bytes=0-9"}, []rangeSpec{{0, 9}}, false},
		{[]string{"bytes=5-"}, []rangeSpec{{5, -1}}, false},
		{[]string{"bytes=-100"}, []rangeSpec{{-1, 100}}, false},
		{[]string{"BYTES = 0-1 , 4-5"}, []rangeSpec{{0, 1}, {4, 5}}, false},
		{[]string{"bytes=0-1,"}, []rangeSpec{{0, 1}}, false},
		{[]string{"items=0-1"}, nil, false},
		{[]string{"bytes=10-5"}, nil, false},
		{[]string{"bytes=a-b"}, nil, false},
		{[]string{"bytes=-"}, nil, false},
		{[]string{"bytes="}, nil, false},
		{[]string{"bytes=+1-2"}, nil, false},
		{[]string{"bytes=0-9", "bytes=1-2"}, nil, false},
		{[]string{"bytes=0-" + strings.Repeat("9", 19)}, nil, false},
		{[]string{"bytes=0-" + strings.Repeat("1", maxRangeHeader)}, nil, false},
		{[]string{"bytes=" + strings.Join(many, ",")}, []rangeSpec{}, true},
	}
	for _, tc := range tests {
		h := http.Header{"Range": tc.in}
		rh := parseRange(h)
		switch {
		case tc.want == nil && rh != nil:
			t.Errorf("%q: got %+v, want nil", tc.in, rh)
		case tc.want != nil && rh == nil:
			t.Errorf("%q: got nil", tc.in)
		case rh != nil && rh.tooMany != tc.tooMany:
			t.Errorf("%q: tooMany %v", tc.in, rh.tooMany)
		case rh != nil && !tc.tooMany && !slices.Equal(rh.specs, tc.want):
			t.Errorf("%q: specs %+v, want %+v", tc.in, rh.specs, tc.want)
		}
	}
}

func TestFirstSliceIndex(t *testing.T) {
	tests := []struct {
		rng, ifRange string
		want         int64
	}{
		{"", "", 0},
		{"bytes=2500-", "", 2},
		{"bytes=2500-2600", "Wed, 21 Oct 2015 07:28:00 GMT", 0},
		{"bytes=-100", "", 0},
		{"bytes=2500-2600,3000-3001", "", 0},
		{"bytes=1023-1024", "", 0},
		{"bytes=1024-", "", 1},
	}
	for _, tc := range tests {
		h := http.Header{}
		if tc.rng != "" {
			h.Set("Range", tc.rng)
		}
		if tc.ifRange != "" {
			h.Set("If-Range", tc.ifRange)
		}
		if got := firstSliceIndex(parseRange(h), h, 1024); got != tc.want {
			t.Errorf("%q/%q: %d, want %d", tc.rng, tc.ifRange, got, tc.want)
		}
	}
}

func TestMakePlan(t *testing.T) {
	const total = 1000
	lm := "Wed, 21 Oct 2015 07:28:00 GMT"
	stored := http.Header{"Last-Modified": {lm}, "Content-Type": {"application/x-test"}}
	tests := []struct {
		name   string
		header []string // key, value pairs
		status int
		ranges []byteRange
		multi  bool
	}{
		{"no range", nil, 200, []byteRange{{0, 999}}, false},
		{"single", []string{"Range", "bytes=0-9"}, 206, []byteRange{{0, 9}}, false},
		{"end clamped", []string{"Range", "bytes=990-2000"}, 206, []byteRange{{990, 999}}, false},
		{"open", []string{"Range", "bytes=500-"}, 206, []byteRange{{500, 999}}, false},
		{"suffix", []string{"Range", "bytes=-5"}, 206, []byteRange{{995, 999}}, false},
		{"suffix larger than object", []string{"Range", "bytes=-5000"}, 206, []byteRange{{0, 999}}, false},
		{"beyond end", []string{"Range", "bytes=1000-"}, 416, nil, false},
		{"empty suffix", []string{"Range", "bytes=-0"}, 416, nil, false},
		{"multi", []string{"Range", "bytes=0-1,5-6,998-"}, 206, []byteRange{{0, 1}, {5, 6}, {998, 999}}, true},
		{"multi adjacent", []string{"Range", "bytes=0-1,2-3"}, 206, []byteRange{{0, 1}, {2, 3}}, true},
		{"multi overlapping", []string{"Range", "bytes=0-1,1-2"}, 200, []byteRange{{0, 999}}, false},
		{"multi descending", []string{"Range", "bytes=5-6,0-1"}, 200, []byteRange{{0, 999}}, false},
		{"multi, one satisfiable", []string{"Range", "bytes=0-1,2000-3000"}, 206, []byteRange{{0, 1}}, false},
		{"if-range date matches", []string{"Range", "bytes=0-9", "If-Range", lm}, 206, []byteRange{{0, 9}}, false},
		{"if-range date differs", []string{"Range", "bytes=0-9", "If-Range", "Thu, 22 Oct 2015 07:28:00 GMT"}, 200, []byteRange{{0, 999}}, false},
		{"if-range etag", []string{"Range", "bytes=0-9", "If-Range", `"abc"`}, 200, []byteRange{{0, 999}}, false},
		{"if-range weak etag", []string{"Range", "bytes=0-9", "If-Range", `W/"abc"`}, 200, []byteRange{{0, 999}}, false},
		{"if-modified-since equal", []string{"If-Modified-Since", lm}, 304, nil, false},
		{"if-modified-since later", []string{"If-Modified-Since", "Thu, 22 Oct 2015 07:28:00 GMT"}, 304, nil, false},
		{"if-modified-since earlier", []string{"If-Modified-Since", "Tue, 20 Oct 2015 07:28:00 GMT"}, 200, []byteRange{{0, 999}}, false},
		{"if-modified-since invalid", []string{"If-Modified-Since", "yesterday"}, 200, []byteRange{{0, 999}}, false},
		{"if-none-match wins", []string{"If-Modified-Since", lm, "If-None-Match", `"x"`}, 200, []byteRange{{0, 999}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := hdr(tc.header...)
			p := makePlan(parseRange(h), h, total, stored)
			if p.status != tc.status || !slices.Equal(p.ranges, tc.ranges) || p.multipart != tc.multi {
				t.Fatalf("plan %+v", p)
			}
			// The precomputed length is exactly what servePlan writes.
			var n int64
			for i, r := range p.ranges {
				if p.multipart {
					n += int64(len(p.parts[i]))
					if !strings.Contains(p.parts[i], "Content-Range: "+contentRange(r, total)+"\r\n") ||
						!strings.Contains(p.parts[i], "Content-Type: application/x-test\r\n") {
						t.Fatalf("part header %q", p.parts[i])
					}
				}
				n += r.length()
			}
			n += int64(len(p.trailer))
			if p.contentLength != n {
				t.Fatalf("content length %d, body %d", p.contentLength, n)
			}
		})
	}
	// Without a stored Last-Modified conditionals never match.
	h := hdr("If-Modified-Since", lm, "Range", "bytes=0-1", "If-Range", lm)
	if p := makePlan(parseRange(h), h, total, http.Header{}); p.status != 200 {
		t.Fatalf("no Last-Modified: %d", p.status)
	}
}

func TestParseContentRange(t *testing.T) {
	tests := []struct {
		in          string
		a, b, total int64
		ok          bool
	}{
		{"bytes 0-1023/5000", 0, 1023, 5000, true},
		{"BYTES 4096-4999/5000", 4096, 4999, 5000, true},
		{"bytes 0-1023/*", 0, 1023, -1, true},
		{"bytes 0-5000/5000", 0, 0, 0, false},
		{"bytes 10-5/50", 0, 0, 0, false},
		{"bytes */5000", 0, 0, 0, false},
		{"items 0-1/2", 0, 0, 0, false},
		{"bytes 0-1", 0, 0, 0, false},
		{"bytes -1-2/3", 0, 0, 0, false},
		{"", 0, 0, 0, false},
	}
	for _, tc := range tests {
		a, b, total, ok := parseContentRange(tc.in)
		if ok != tc.ok || (ok && (a != tc.a || b != tc.b || total != tc.total)) {
			t.Errorf("%q: %d %d %d %v", tc.in, a, b, total, ok)
		}
	}
}

func TestCanonicalPath(t *testing.T) {
	tests := []struct {
		uri  string
		want bool
	}{
		{"/", true},
		{"/depot/228990/chunk/0123abcd", true},
		{"/a%20b", true},
		{"/a,b;c=d", true},
		{"/file(1).bin", true},
		{"/a%2cb", true}, // reserved characters may stay encoded
		{"/%C3%A4", true},
		{"//a", false},
		{"/a//b", false},
		{"/a/./b", false},
		{"/a/../b", false},
		{"/.", false},
		{"/a/..", false},
		{"/a/.b/c..", true},
		{"/a%2Fb", false},
		{"/a%2fb", false},
		{"/a%5Cb", false},
		{"/a%00b", false},
		{"/a%41b", false},
		{"/a%7Eb", false},
		{"/a%2Eb", false},
		{"/a%2e%2e/b", false},
		{"/a%0Ab", false},
		{"/a%7Fb", false},
		{"/%FF", false},
		{"/" + strings.Repeat("a", maxPathLen), false},
	}
	for _, tc := range tests {
		u, err := url.ParseRequestURI(tc.uri)
		if err != nil {
			t.Fatalf("%s: %v", tc.uri, err)
		}
		if got := canonicalPath(u.EscapedPath(), u.Path); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.uri, got, tc.want)
		}
	}
	for _, escaped := range []string{"", "a", "/a%", "/a%zz", `/a\b`} {
		if canonicalPath(escaped, escaped) {
			t.Errorf("%q accepted", escaped)
		}
	}
}

func TestStoredHeaders(t *testing.T) {
	src := http.Header{
		"Content-Type":            {"application/octet-stream"},
		"Last-Modified":           {"Wed, 21 Oct 2015 07:28:00 GMT"},
		"Content-Disposition":     {"attachment"},
		"Etag":                    {`"v1"`},
		"Set-Cookie":              {"a=b"},
		"Content-Length":          {"5"},
		"Content-Range":           {"bytes 0-4/5"},
		"Cache-Control":           {"max-age=1"},
		"Age":                     {"3"},
		"Vary":                    {"Accept-Encoding"},
		"X-Cache-Status":          {"HIT"},
		"Cf-Ray":                  {"123"},
		"X-Amz-Cf-Id":             {"abc"},
		"X-Lancache-Processed-By": {"other"},
		"X-Upstream-Status":       {"206"},
		"Connection":              {"X-Hop"},
		"X-Hop":                   {"1"},
		"X-Ctl":                   {"a\x01b"},
		"X-Crlf":                  {"a\r\nInjected: 1"},
		"X-Utf8":                  {"\xff"},
		"Bad Name":                {"x"},
		"X-Multi":                 {"1", "2"},
	}
	got := storedHeaders(src)
	want := http.Header{
		"Content-Type":        {"application/octet-stream"},
		"Last-Modified":       {"Wed, 21 Oct 2015 07:28:00 GMT"},
		"Content-Disposition": {"attachment"},
		"X-Multi":             {"1", "2"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}

	// Caps: 4 KiB and 64 names, Content-Type and Last-Modified first.
	big := http.Header{"Content-Type": {"a/b"}, "Last-Modified": {"x"}}
	for i := range 200 {
		big.Set(fmt.Sprintf("A-%03d", i), strings.Repeat("v", 10))
	}
	got = storedHeaders(big)
	size := 0
	for k, vv := range got {
		for _, v := range vv {
			size += len(k) + len(v) + 4
		}
	}
	if len(got) > maxStoredHeaderNames || size > maxStoredHeaderBytes || got.Get("Content-Type") != "a/b" || got.Get("Last-Modified") != "x" {
		t.Fatalf("%d names, %d bytes", len(got), size)
	}
	huge := http.Header{"Content-Type": {"a/b"}, "X-Huge": {strings.Repeat("v", 5000)}}
	if got := storedHeaders(huge); got.Get("X-Huge") != "" || got.Get("Content-Type") != "a/b" {
		t.Fatalf("huge value kept: %d names", len(got))
	}
}

func TestClassifyResponse(t *testing.T) {
	const S = 1024
	resp := func(status int, cl int64, kv ...string) *http.Response {
		return &http.Response{StatusCode: status, ContentLength: cl, Header: hdr(kv...), Body: http.NoBody}
	}
	tests := []struct {
		name      string
		resp      *http.Response
		idx       int64
		rangeSent bool
		kind      respKind
		failure   bool
		total     int64
		dataIdx   int64
	}{
		{"aligned 206", resp(206, 1024, "Content-Range", "bytes 1024-2047/5000"), 1, true, kindSlice, false, 5000, 1},
		{"last slice", resp(206, 904, "Content-Range", "bytes 4096-4999/5000"), 4, true, kindSlice, false, 5000, 4},
		{"misaligned start", resp(206, 1048, "Content-Range", "bytes 1000-2047/5000"), 1, true, kindRangeFail, true, 5000, 0},
		{"short end", resp(206, 477, "Content-Range", "bytes 1024-1500/5000"), 1, true, kindRangeFail, true, 5000, 0},
		{"unknown total", resp(206, 1024, "Content-Range", "bytes 0-1023/*"), 0, true, kindPassClient, true, -1, 0},
		{"invalid range", resp(206, 1024, "Content-Range", "bytes x"), 0, true, kindPassClient, true, -1, 0},
		{"length mismatch", resp(206, 10, "Content-Range", "bytes 0-1023/5000"), 0, true, kindPassClient, false, -1, 0},
		{"too large", resp(206, 1024, "Content-Range", "bytes 0-1023/"+strconv.FormatInt(cachestore.MaxTotal+1, 10)), 0, true, kindPassClient, false, -1, 0},
		{"encoded 206", resp(206, 1024, "Content-Range", "bytes 0-1023/5000", "Content-Encoding", "gzip"), 0, true, kindPassClient, false, -1, 0},
		{"small 200", resp(200, 800), 0, true, kindSlice, false, 800, 0},
		{"small 200 for a later slice", resp(200, 800), 3, true, kindSlice, false, 800, 0},
		{"large 200", resp(200, 5000), 2, true, kindRangeFail, true, 5000, 0},
		{"large 200 without Range", resp(200, 5000), 0, false, kindRangeFail, false, 5000, 0},
		{"200 identity encoding", resp(200, 800, "Content-Encoding", "identity"), 0, true, kindSlice, false, 800, 0},
		{"200 without length", resp(200, -1), 0, true, kindNoLength, false, -1, 0},
		{"empty 200", resp(200, 0), 0, true, kindUpstream, false, -1, 0},
		{"encoded 200", resp(200, 800, "Content-Encoding", "br"), 0, true, kindPassClient, false, -1, 0},
		{"huge 200", resp(200, cachestore.MaxTotal+1), 0, true, kindPassClient, false, -1, 0},
		{"416 slice 0", resp(416, 0), 0, true, kindPassClient, false, -1, 0},
		{"416 later slice", resp(416, 0), 3, true, kind416, false, -1, 0},
		{"403", resp(403, 10), 0, true, kindUpstream, false, -1, 0},
		{"303", resp(303, 0), 0, true, kindUpstream, false, -1, 0},
	}
	for _, tc := range tests {
		info := classifyResponse(tc.resp, tc.idx, S, tc.rangeSent)
		if info.kind != tc.kind || info.rangeFailure != tc.failure || info.total != tc.total || info.dataIdx != tc.dataIdx {
			t.Errorf("%s: %+v", tc.name, info)
		}
	}
}

func TestForwardHeaders(t *testing.T) {
	in := http.Header{
		"Connection":              {"X-Secret, keep-alive"},
		"X-Secret":                {"1"},
		"Keep-Alive":              {"timeout=5"},
		"Te":                      {"trailers"},
		"Proxy-Authorization":     {"Basic x"},
		"Range":                   {"bytes=0-1"},
		"If-Range":                {"x"},
		"If-None-Match":           {`"x"`},
		"Cookie":                  {"a=b"},
		"Accept-Encoding":         {"gzip"},
		"User-Agent":              {"Valve/Steam HTTP Client 1.0"},
		"Authorization":           {"Bearer t"},
		"X-Forwarded-For":         {"1.2.3.4"},
		"X-Real-Ip":               {"1.2.3.4"},
		"X-Lancache-Processed-By": {"other"},
		"Accept":                  {"*/*"},
	}
	fill := forwardHeaders(in, "me", true)
	want := http.Header{
		"User-Agent":              {"Valve/Steam HTTP Client 1.0"},
		"Authorization":           {"Bearer t"},
		"Accept":                  {"*/*"},
		"Accept-Encoding":         {"identity"},
		"X-Lancache-Processed-By": {"me"},
	}
	if fmt.Sprint(fill) != fmt.Sprint(want) {
		t.Fatalf("fill headers %v", fill)
	}
	pass := forwardHeaders(in, "me", false)
	for _, k := range []string{"Range", "If-Range", "If-None-Match", "Cookie"} {
		if pass.Get(k) == "" {
			t.Errorf("pass-through lost %s", k)
		}
	}
	if pass.Get("Accept-Encoding") != "gzip" || pass.Get("X-Secret") != "" || pass.Get("X-Forwarded-For") != "" || pass.Get(processedByHeader) != "me" {
		t.Fatalf("pass-through headers %v", pass)
	}
	if ua, ok := forwardHeaders(http.Header{}, "", true)["User-Agent"]; !ok || ua[0] != "" {
		t.Fatal("no User-Agent must stay empty (no Go default)")
	}
}

func TestNoSliceTracker(t *testing.T) {
	ctx := context.Background()
	tr, err := newNoSliceTracker(ctx, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	obj := func(n int) string { return cachestore.ObjectID("svc", "/"+strconv.Itoa(n)) }
	const host = "cdn.example.com"
	useSlicing := func(host string, now time.Time) bool {
		use, _ := tr.useSlicing(host, now)
		return use
	}

	tr.failure(host, obj(1), now)
	tr.failure(host, obj(1), now.Add(time.Minute)) // same object: counted once
	tr.failure(host, obj(2), now.Add(2*time.Minute))
	if l := tr.list(); len(l) != 1 || l[0].Failures != 2 || l[0].Marked || !useSlicing(host, now) {
		t.Fatalf("after two objects: %+v", l)
	}
	// The window is 24 h: a failure after it starts over.
	tr.failure(host, obj(3), now.Add(25*time.Hour))
	if l := tr.list(); l[0].Failures != 1 || l[0].Marked {
		t.Fatalf("after the window: %+v", l)
	}
	now = now.Add(25 * time.Hour)
	tr.failure(host, obj(4), now)
	tr.failure(host, obj(5), now)
	l := tr.list()
	if !l[0].Marked || l[0].Failures != 3 || !l[0].Since.Equal(now) {
		t.Fatalf("not marked: %+v", l)
	}
	if useSlicing(host, now.Add(time.Hour)) {
		t.Fatal("a marked host must be fetched without Range")
	}
	if !useSlicing(host, now.Add(noSliceProbe)) || useSlicing(host, now.Add(noSliceProbe+time.Minute)) {
		t.Fatal("one probe per interval")
	}
	// A probe that sent no range request (a cache hit) is given back.
	at := now.Add(2 * noSliceProbe)
	if use, probe := tr.useSlicing(host, at); !use || !probe {
		t.Fatal("probe not due")
	}
	tr.returnProbe(host, at)
	if use, probe := tr.useSlicing(host, at.Add(time.Minute)); !use || !probe {
		t.Fatal("a returned probe must be available again")
	}
	if useSlicing(host, at.Add(2*time.Minute)) {
		t.Fatal("a used probe must not be available again")
	}
	// A valid 206 clears the host.
	tr.success(host)
	if len(tr.list()) != 0 {
		t.Fatal("success did not clear")
	}
	// Decay forgets old unmarked windows, never marks.
	tr.failure("a.example", obj(1), now)
	for i := range 3 {
		tr.failure("b.example", obj(i), now)
	}
	tr.decay(now.Add(noSliceWindow + time.Second))
	if l := tr.list(); len(l) != 1 || l[0].Host != "b.example" {
		t.Fatalf("after decay: %+v", l)
	}
	if err := tr.reset(ctx, "B.Example."); err != nil {
		t.Fatal(err)
	}
	if err := tr.reset(ctx, "b.example"); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("unknown: %v", err)
	}
	for _, bad := range []string{"10.0.0.1", "bad host", ""} {
		if err := tr.reset(ctx, bad); apperr.KindOf(err) != apperr.KindInvalid {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	// Bounded: unmarked entries make room, marks are kept.
	for i := range maxNoSliceHosts + 10 {
		tr.failure(fmt.Sprintf("h%d.example", i), obj(1), now.Add(time.Duration(i)*time.Second))
	}
	if n := len(tr.list()); n > maxNoSliceHosts {
		t.Fatalf("%d hosts tracked", n)
	}
}

func TestFillSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var fs fillSlots
		lim := fillLimits{global: 2, perClient: 1}
		a, b, c := netip.MustParsePrefix("10.0.0.1/32"), netip.MustParsePrefix("10.0.0.2/32"), netip.MustParsePrefix("10.0.0.3/32")
		if !fs.tryAcquire(a, lim) || fs.tryAcquire(a, lim) || !fs.tryAcquire(b, lim) || fs.tryAcquire(c, lim) {
			t.Fatal("limits not applied")
		}
		ctx := context.Background()
		start := time.Now()
		if fs.acquire(ctx, c, lim, 2*time.Second) || time.Since(start) != 2*time.Second {
			t.Fatal("acquire must give up after the wait")
		}
		got := make(chan bool)
		go func() { got <- fs.acquire(ctx, c, lim, 2*time.Second) }()
		time.Sleep(time.Second)
		fs.release(a)
		if !<-got || time.Since(start) != 3*time.Second {
			t.Fatal("a released slot must wake the waiter")
		}
		fs.release(b)
		fs.release(c)
		if fs.used != 0 || len(fs.perClient) != 0 {
			t.Fatalf("leak: %d %v", fs.used, fs.perClient)
		}
	})
}

func TestFillLimits(t *testing.T) {
	h := newHarness(t, withoutStore())
	if l := h.s.fillLimits(1 << 20); l.global != 64 || l.perClient != 32 {
		t.Fatalf("1 MiB: %+v", l)
	}
	if l := h.s.fillLimits(64 << 20); l.global != 16 || l.perClient != 16 {
		t.Fatalf("64 MiB: %+v", l) // 1 GiB of buffers at most
	}
}

func TestBufPool(t *testing.T) {
	var p bufPool
	b := p.get(1024)
	if len(b) != 1024 {
		t.Fatal(len(b))
	}
	p.put(b[:10])
	if c := p.get(1024); &c[0] != &b[0] || len(c) != 1024 {
		t.Fatal("buffer not reused")
	}
	// The store changed to another slice size: its buffers are reused, the
	// old ones are dropped (not pinned) and not taken back.
	old := [][]byte{p.get(1024), p.get(1024)}
	for _, o := range old {
		p.put(o)
	}
	nb := p.get(2048)
	if len(nb) != 2048 || p.size != 2048 || len(p.free) != 0 {
		t.Fatalf("size switch: len %d, size %d, %d free", len(nb), p.size, len(p.free))
	}
	p.put(nb)
	p.put(make([]byte, 1024)) // a buffer of the old size returned late
	if len(p.free) != 1 || cap(p.free[0]) != 2048 {
		t.Fatalf("free list %d of %d", len(p.free), p.size)
	}
	if allocs := testing.AllocsPerRun(20, func() { p.put(p.get(2048)) }); allocs != 0 {
		t.Fatalf("new-size buffers are not reused: %v allocations", allocs)
	}
	for range maxFreeBufferBytes/2048 + 5 {
		p.put(make([]byte, 2048))
	}
	if int64(len(p.free))*2048 > maxFreeBufferBytes {
		t.Fatal("free list unbounded")
	}
}

func TestLiveState(t *testing.T) {
	var l liveState
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	newReq := func(ip string) *request {
		return &request{start: start, ip: netip.MustParseAddr(ip), service: "steam", host: "h", path: "/p",
			label: "L", acct: &acct{}}
	}
	r1, r2 := newReq("10.0.0.1"), newReq("10.0.0.1")
	t1, t2 := l.begin(r1), l.begin(r2)
	r1.acct.sent.Add(10_000)
	r2.acct.sent.Add(30_000)
	r2.acct.hit.Add(30_000)
	l.tick(start.Add(2 * time.Second))
	dl := l.downloadsSnapshot(start.Add(2 * time.Second))
	if len(dl) != 1 || dl[0].InFlight != 2 || dl[0].BytesSent != 40_000 || dl[0].BytesHit != 30_000 || dl[0].RateBps != 20_000 {
		t.Fatalf("downloads %+v", dl)
	}
	if tr := l.transfersSnapshot(start.Add(2 * time.Second)); len(tr) != 2 || tr[0].ID != "1" || tr[1].BytesSent != 30_000 {
		t.Fatalf("transfers %+v", tr)
	}
	if n := l.activeClients(); n != 1 {
		t.Fatalf("active clients %d", n)
	}
	l.end(t1, start.Add(3*time.Second))
	l.end(t2, start.Add(3*time.Second))
	if len(l.transfersSnapshot(start)) != 0 {
		t.Fatal("transfers not removed")
	}
	// The rate window is 10 s; entries linger 30 s after the last request.
	later := start.Add(20 * time.Second)
	l.tick(later)
	if dl := l.downloadsSnapshot(later); len(dl) != 1 || dl[0].RateBps != 0 || dl[0].InFlight != 0 {
		t.Fatalf("lingering download %+v", dl)
	}
	gone := start.Add(3*time.Second + downloadLinger + time.Second)
	l.tick(gone)
	if len(l.downloadsSnapshot(gone)) != 0 || len(l.downloads) != 0 {
		t.Fatal("download not expired")
	}
}

func TestClip(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{{"hello", 10, "hello"}, {"hello", 3, "hel"}, {"hé", 2, "h"}, {"€x", 2, ""}, {"ab€", 5, "ab€"}} {
		if got := clip(tc.in, tc.n); got != tc.want {
			t.Errorf("clip(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestRedirectTarget(t *testing.T) {
	s, err := New(context.Background(), Deps{Lookup: lookup})
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := url.Parse("http://cdn.example.com/a/b?x=1")
	tests := []struct {
		loc, want string // want "" = refused
	}{
		{"/c", "http://cdn.example.com/c"},
		{"c#frag", "http://cdn.example.com/a/c"},
		{"https://other.example/y?q=1", "https://other.example/y?q=1"},
		{"//other.example/z", "http://other.example/z"},
		{"http://other.example:8080/z", "http://other.example:8080/z"},
		{"http://other.example:443/z", "http://other.example:443/z"},
		{"http://other.example:22/z", ""},
		{"ftp://other.example/", ""},
		{"http://user:pw@other.example/", ""},
		{"http://10.0.0.1/", ""},
		{"http://169.254.169.254/latest", ""},
		{"http://[::1]/", ""},
		{"http://127.0.0.1/", ""},
		{"http://bad_host!/", ""},
		{"", ""},
	}
	for _, tc := range tests {
		got, err := s.redirectTarget(context.Background(), cur, tc.loc)
		switch {
		case tc.want == "" && err == nil:
			t.Errorf("%q accepted as %s", tc.loc, got)
		case tc.want != "" && (err != nil || got.String() != tc.want):
			t.Errorf("%q: %v %v, want %s", tc.loc, got, err, tc.want)
		}
	}
	if _, err := s.redirectTarget(context.Background(), cur, "http://10.0.0.1/"); !errors.Is(err, netutil.ErrForbiddenDestination) {
		t.Fatalf("private redirect error %v", err)
	}
}

func TestHTTPSOnlyHosts(t *testing.T) {
	var h httpsOnlyHosts
	now := time.Now()
	h.add("a.example", now)
	if !h.has("a.example", now.Add(httpsOnlyFor-time.Second)) || h.has("a.example", now.Add(httpsOnlyFor)) || h.has("b.example", now) {
		t.Fatal("24 h memory")
	}
	for i := range maxHTTPSOnlyHosts + 10 {
		h.add(fmt.Sprintf("h%d.example", i), now.Add(time.Duration(i)*time.Millisecond))
	}
	if len(h.m) > maxHTTPSOnlyHosts || !h.has(fmt.Sprintf("h%d.example", maxHTTPSOnlyHosts+9), now) {
		t.Fatalf("%d hosts", len(h.m))
	}
	h.expire(now.Add(2 * httpsOnlyFor))
	if len(h.m) != 0 {
		t.Fatal("not expired")
	}
}

func TestWarnLimiter(t *testing.T) {
	var l warnLimiter
	now := time.Now()
	if !l.allow("k", now) || l.allow("k", now.Add(59*time.Minute)) || !l.allow("k", now.Add(time.Hour)) || !l.allow("other", now) {
		t.Fatal("hourly limit")
	}
	for i := range maxWarnKeys * 2 {
		l.allow(strconv.Itoa(i), now)
	}
	if len(l.last) > maxWarnKeys {
		t.Fatal("unbounded")
	}
}

func TestIdleBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pr, pw := io.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		context.AfterFunc(ctx, func() { pr.CloseWithError(context.Canceled) })
		b := newIdleBody(pr, cancel, time.Minute)
		go func() {
			_, _ = pw.Write([]byte("x"))
			time.Sleep(59 * time.Second)
			_, _ = pw.Write([]byte("y")) // then silence
		}()
		buf := make([]byte, 1)
		for range 2 {
			if _, err := b.Read(buf); err != nil {
				t.Fatalf("progressing read failed: %v", err)
			}
		}
		start := time.Now()
		if _, err := b.Read(buf); err == nil || time.Since(start) != time.Minute {
			t.Fatalf("idle read: %v after %v", err, time.Since(start))
		}
		_ = b.Close()
		_ = pw.Close()
	})
}
