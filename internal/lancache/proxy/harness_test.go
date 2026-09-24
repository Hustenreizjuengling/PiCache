package proxy

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/lancache/services"
	cachestore "github.com/hustenreizjuengling/picache/internal/lancache/store"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

const (
	testSlice    = 1024
	testInstance = "test-instance"
	testHost     = "cdn.example.com"
	testService  = "epicgames"
)

// --- fake store ---

type fakeObj struct {
	meta   cachestore.Meta
	gen    uint64
	slices map[int64][]byte
}

type fakeStore struct {
	mu         sync.Mutex
	id         string
	size       int64
	objs       map[string]*fakeObj
	gen        uint64
	closed     bool
	writeDelay time.Duration // WriteSlice takes this long (a slow NAS)
	writers    []string      // dynamic types of the writers given to WriteRange
	writes     map[string]int
	touched    map[string]int64 // bytes served from the cache (Touch)
	hits       map[string]int   // Touch calls with bytes served from the cache
	access     map[string]int   // Touch calls
	inUse      map[string]int   // Use without release
	uses       map[string]int   // Use calls
	// readHook, if set, runs before every ReadSlice (without the lock).
	readHook func(id string, idx int64) error
}

func newFakeStore() *fakeStore {
	return &fakeStore{id: "0123456789abcdef0123456789abcdef", size: testSlice, objs: map[string]*fakeObj{},
		writes: map[string]int{}, touched: map[string]int64{}, hits: map[string]int{}, access: map[string]int{},
		inUse: map[string]int{}, uses: map[string]int{}}
}

func (s *fakeStore) ID() string       { return s.id }
func (s *fakeStore) SliceSize() int64 { return s.size }

func (s *fakeStore) Head(_ context.Context, id string) (cachestore.ObjectHead, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return cachestore.ObjectHead{}, false, cachestore.ErrClosed
	}
	o := s.objs[id]
	if o == nil {
		return cachestore.ObjectHead{}, false, nil
	}
	n := (o.meta.Total + s.size - 1) / s.size
	h := cachestore.ObjectHead{ID: id, Gen: o.gen, Total: o.meta.Total, SliceSize: s.size, Header: o.meta.Header.Clone(),
		ContentType: o.meta.Header.Get("Content-Type"), LastModified: o.meta.Header.Get("Last-Modified"),
		NoSlice: o.meta.NoSlice, Present: make([]uint64, (n+63)/64)}
	for idx := range o.slices {
		h.Present[idx/64] |= 1 << (uint(idx) % 64)
	}
	return h, true, nil
}

func (s *fakeStore) ReadSlice(_ context.Context, id string, gen uint64, idx int64) (cachestore.SliceReader, error) {
	if hook := s.hook(); hook != nil {
		if err := hook(id, idx); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, cachestore.ErrClosed
	}
	o := s.objs[id]
	switch {
	case o == nil:
		return nil, cachestore.ErrSliceMissing
	case o.gen != gen:
		return nil, cachestore.ErrStale
	}
	data, ok := o.slices[idx]
	if !ok {
		return nil, cachestore.ErrSliceMissing
	}
	return &fakeReader{s: s, data: data}, nil
}

func (s *fakeStore) SetMeta(_ context.Context, id string, m cachestore.Meta) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, cachestore.ErrClosed
	}
	o := s.objs[id]
	if o == nil || o.meta.Total != m.Total {
		s.gen++
		o = &fakeObj{gen: s.gen, slices: map[int64][]byte{}}
		s.objs[id] = o
	}
	o.meta = m
	return o.gen, nil
}

func (s *fakeStore) WriteSlice(_ context.Context, id string, gen uint64, idx int64, data []byte) error {
	s.mu.Lock()
	delay := s.writeDelay
	s.mu.Unlock()
	time.Sleep(delay)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return cachestore.ErrClosed
	}
	o := s.objs[id]
	if o == nil || o.gen != gen {
		return cachestore.ErrStale
	}
	if want := min(s.size, o.meta.Total-idx*s.size); int64(len(data)) != want {
		return fmt.Errorf("slice %d: %d bytes, want %d", idx, len(data), want)
	}
	o.slices[idx] = bytes.Clone(data)
	s.writes[id]++
	return nil
}

func (s *fakeStore) Invalidate(_ context.Context, id string, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objs, id)
	return nil
}

func (s *fakeStore) Touch(id string, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touched[id] += n
	s.access[id]++
	if n > 0 {
		s.hits[id]++
	}
}

func (s *fakeStore) Use(id string) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inUse[id]++
	s.uses[id]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.inUse[id]--
		})
	}
}

func (s *fakeStore) hook() func(string, int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readHook
}

func (s *fakeStore) setReadHook(fn func(id string, idx int64) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readHook = fn
}

func (s *fakeStore) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

// sliceCount returns the cached slices of the object at path.
func (s *fakeStore) sliceCount(service, path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.objs[cachestore.ObjectID(service, path)]; o != nil {
		return len(o.slices)
	}
	return 0
}

func (s *fakeStore) object(service, path string) *fakeObj {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objs[cachestore.ObjectID(service, path)]
}

func (s *fakeStore) dropSlice(service, path string, idx int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objs[cachestore.ObjectID(service, path)].slices, idx)
}

func (s *fakeStore) writerTypes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.writers)
}

type fakeReader struct {
	s    *fakeStore
	data []byte
}

func (r *fakeReader) ReadAt(p []byte, off int64) (int, error) {
	return bytes.NewReader(r.data).ReadAt(p, off)
}
func (r *fakeReader) Size() int64  { return int64(len(r.data)) }
func (r *fakeReader) Close() error { return nil }

func (r *fakeReader) WriteRange(w io.Writer, off, n int64) (int64, error) {
	r.s.mu.Lock()
	r.s.writers = append(r.s.writers, fmt.Sprintf("%T", w))
	r.s.mu.Unlock()
	m, err := w.Write(r.data[off : off+n])
	return int64(m), err
}

// --- fake classifier, clients, logs ---

var steamPathRE = regexp.MustCompile(`^/depot/[0-9]+/`)

type fakeClassifier struct {
	mu    sync.Mutex
	hosts map[string]bool // host → enabled (service testService)
}

func (c *fakeClassifier) Classify(host, ua, path string) (string, bool, bool) {
	if strings.HasSuffix(ua, services.SteamUserAgentSuffix) && (steamPathRE.MatchString(path) || path == "/server-status") {
		return "steam", true, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	enabled, ok := c.hosts[host]
	if !ok {
		return "", false, false
	}
	return testService, enabled, true
}

func (c *fakeClassifier) Label(key string) string { return "label:" + key }

type fakeClients struct{ ignore atomic.Bool }

func (c *fakeClients) Identify(ip netip.Addr) *clients.Identity {
	return &clients.Identity{IP: ip, Name: "tester", IgnoreLogs: c.ignore.Load()}
}

type fakeLogger struct {
	mu     sync.Mutex
	events []logs.CacheEvent
}

func (l *fakeLogger) LogCache(e logs.CacheEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *fakeLogger) all() []logs.CacheEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// --- origin ---

type originObj struct {
	data     []byte
	modTime  time.Time
	noRange  bool          // ignore Range: 200 with the whole object
	noLength bool          // no Content-Length (chunked)
	gzip     bool          // Content-Encoding: gzip
	status   int           // fixed status with a short body
	location string        // redirect target (with status 302 unless status is set)
	gate     chan struct{} // write half of each body, then wait for close
	// waitRange delays the answer to this range until waitFor was requested.
	waitRange, waitFor string
	// failOn: requests with a matching Range get half of their body, then
	// the connection is aborted.
	failOn func(rng string) bool
	// stallOn: requests with a matching Range get half of their body, then
	// nothing until the request ends.
	stallOn func(rng string) bool
}

type originReq struct {
	host, uri, rng, method string
	header                 http.Header
	body                   string
	tls                    bool
}

type origin struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	objs     map[string]*originObj
	reqs     []originReq
	seen     chan struct{} // signalled on every request
	status   int           // non-zero: every request gets this status (e.g. 426)
	tlsCheck bool          // status applies only to plain-http requests
}

func newOrigin(t *testing.T, tlsServer bool) *origin {
	o := &origin{t: t, objs: map[string]*originObj{}, seen: make(chan struct{}, 1024)}
	if tlsServer {
		o.srv = httptest.NewTLSServer(http.HandlerFunc(o.serve))
	} else {
		o.srv = httptest.NewServer(http.HandlerFunc(o.serve))
	}
	return o
}

// setStatus makes the origin answer every request with code (tlsOnly: only
// plain-http requests).
func (o *origin) setStatus(code int, plainOnly bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.status, o.tlsCheck = code, plainOnly
}

func (o *origin) set(path string, obj *originObj) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if obj.modTime.IsZero() {
		obj.modTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	}
	o.objs[path] = obj
}

func (o *origin) requests() []originReq {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.reqs)
}

// ranges returns the Range headers of the requests for path.
func (o *origin) ranges(path string) []string {
	var out []string
	for _, r := range o.requests() {
		if strings.SplitN(r.uri, "?", 2)[0] == path {
			out = append(out, r.rng)
		}
	}
	return out
}

func (o *origin) sawRange(rng string) bool {
	for _, r := range o.requests() {
		if r.rng == rng {
			return true
		}
	}
	return false
}

func (o *origin) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	o.mu.Lock()
	o.reqs = append(o.reqs, originReq{host: r.Host, uri: r.RequestURI, rng: r.Header.Get("Range"), method: r.Method,
		header: r.Header.Clone(), body: string(body), tls: r.TLS != nil})
	obj := o.objs[r.URL.EscapedPath()] // objects can be set for an escaped form
	if obj == nil {
		obj = o.objs[r.URL.Path]
	}
	status := o.status
	if o.tlsCheck && r.TLS != nil {
		status = 0
	}
	o.mu.Unlock()
	select {
	case o.seen <- struct{}{}:
	default:
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if obj == nil {
		http.Error(w, "no such object", http.StatusNotFound)
		return
	}
	h := w.Header()
	if obj.location != "" {
		h.Set("Location", obj.location)
		w.WriteHeader(cmpOr(obj.status, http.StatusFound))
		return
	}
	if obj.status != 0 {
		h.Set("X-Origin", "yes")
		h.Set("Content-Type", "text/plain")
		w.WriteHeader(obj.status)
		_, _ = io.WriteString(w, "origin says "+strconv.Itoa(obj.status))
		return
	}
	if obj.waitRange != "" && r.Header.Get("Range") == obj.waitRange {
		deadline := time.Now().Add(3 * time.Second)
		for !o.sawRange(obj.waitFor) && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	}
	data := obj.data
	start, end, status := 0, len(data)-1, http.StatusOK
	if rng := r.Header.Get("Range"); rng != "" && !obj.noRange {
		a, b, ok := parseOriginRange(rng, len(data))
		if !ok {
			h.Set("Content-Range", "bytes */"+strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start, end, status = a, b, http.StatusPartialContent
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
	}
	h.Set("Content-Type", "application/x-test")
	h.Set("Last-Modified", obj.modTime.Format(http.TimeFormat))
	h.Set("ETag", `"v1"`)
	h.Set("Set-Cookie", "edge=1")
	h.Set("X-Cache", "HIT from edge")
	h.Set("X-Keep", "kept")
	if obj.gzip {
		h.Set("Content-Encoding", "gzip")
	}
	part := data[start : end+1]
	if !obj.noLength {
		h.Set("Content-Length", strconv.Itoa(len(part)))
	}
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	rng := r.Header.Get("Range")
	switch {
	case obj.failOn != nil && obj.failOn(rng):
		_, _ = w.Write(part[:len(part)/2])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	case obj.stallOn != nil && obj.stallOn(rng):
		_, _ = w.Write(part[:len(part)/2])
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		return
	case obj.gate != nil:
		half := len(part) / 2
		_, _ = w.Write(part[:half])
		w.(http.Flusher).Flush()
		<-obj.gate
		part = part[half:]
	}
	if obj.noLength {
		for len(part) > 0 {
			n := min(len(part), 300)
			_, _ = w.Write(part[:n])
			w.(http.Flusher).Flush()
			part = part[n:]
		}
		return
	}
	_, _ = w.Write(part)
}

func cmpOr(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// parseOriginRange handles one "bytes=a-b", "a-" or "-n" range.
func parseOriginRange(v string, size int) (int, int, bool) {
	spec, ok := strings.CutPrefix(v, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	a, b, _ := strings.Cut(spec, "-")
	if a == "" {
		n, _ := strconv.Atoi(b)
		return max(0, size-n), size - 1, n > 0 && size > 0
	}
	start, _ := strconv.Atoi(a)
	end := size - 1
	if b != "" {
		e, _ := strconv.Atoi(b)
		end = min(e, size-1)
	}
	return start, end, start < size
}

// --- harness ---

type harness struct {
	t      *testing.T
	s      *Server
	proxy  *httptest.Server
	store  *fakeStore
	origin *origin
	tls    *origin
	alt    *origin // answers for the second address (1.0.0.1)
	logs   *fakeLogger
	cls    *fakeClassifier
	set    *settings.Store
	db     *db.DB
	client *http.Client
	ids    *fakeClients
	full   atomic.Bool
	noSt   bool
	// switched: Deps.Store returns nil (the store was switched away or
	// went offline); may be set while requests run.
	switched atomic.Bool
	real     SliceStore // used instead of the fake store
	// untrusted: the TLS origin's certificate is not trusted.
	untrusted bool
}

type harnessOpt func(*harness)

func withoutStore() harnessOpt  { return func(h *harness) { h.noSt = true } }
func withTLSOrigin() harnessOpt { return func(h *harness) { h.tls = newOrigin(h.t, true) } }

// withUntrustedTLSOrigin adds a TLS origin whose certificate the proxy
// does not trust.
func withUntrustedTLSOrigin() harnessOpt {
	return func(h *harness) { h.tls, h.untrusted = newOrigin(h.t, true), true }
}

// withSliceStore serves from st instead of the fake store.
func withSliceStore(st SliceStore) harnessOpt { return func(h *harness) { h.real = st } }

// lookup is the harness resolver: most hosts have two public addresses
// (the second one routes to the alternative origin); some resolve to
// addresses the SSRF guard must refuse.
func lookup(_ context.Context, host string) ([]netip.Addr, error) {
	switch host {
	case "private.example":
		return []netip.Addr{netip.MustParseAddr("10.0.0.7")}, nil
	case "linklocal.example":
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
	case "nxdomain.example":
		return nil, errors.New("no such host")
	}
	return []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("1.0.0.1")}, nil
}

func newHarness(t *testing.T, opts ...harnessOpt) *harness {
	t.Helper()
	ctx := context.Background()
	h := &harness{t: t, store: newFakeStore(), ids: &fakeClients{}, origin: newOrigin(t, false), alt: newOrigin(t, false), logs: &fakeLogger{},
		cls: &fakeClassifier{hosts: map[string]bool{testHost: true, "example.com": true, "noslice.example": true, "off.example": false}}}
	for _, o := range opts {
		o(h)
	}
	d, err := db.Open(filepath.Join(t.TempDir(), "picache.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	h.db = d
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	set, err := settings.Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	h.set = set
	h.updateSettings(func(a *settings.All) { a.LanCache.Enabled = true })
	s, err := New(ctx, Deps{
		DB: d, Settings: set, Services: h.cls, Lookup: lookup,
		Store: func() SliceStore {
			switch {
			case h.noSt, h.switched.Load():
				return nil
			case h.real != nil:
				return h.real
			}
			return h.store
		},
		StoreFull:  h.full.Load,
		Clients:    h.ids,
		Logs:       h.logs,
		ACL:        netutil.NewACLWatcher(set),
		InstanceID: testInstance,
		Log:        log,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.tm = timings{demandWait: 200 * time.Millisecond, stall: 3 * time.Second, idleRead: 5 * time.Second, write: 5 * time.Second, storeWrite: 2 * time.Second}
	var roots *x509.CertPool
	if h.tls != nil && !h.untrusted {
		roots = h.tls.srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	}
	s.setUpstreamForTest(h.dial, roots)
	h.s = s
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Start(runCtx)
		close(done)
	}()
	h.proxy = httptest.NewServer(s.Handler())
	h.client = &http.Client{
		Transport:     &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 64},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       20 * time.Second,
	}
	t.Cleanup(func() {
		h.client.CloseIdleConnections()
		h.proxy.Close()
		cancel()
		<-done
		for _, o := range []*origin{h.origin, h.alt, h.tls} {
			if o != nil {
				o.srv.Close()
			}
		}
		d.Close()
	})
	return h
}

// dial replaces the SafeDialer in tests: it resolves and filters the
// destination exactly like production (the proxy's SSRF guard) and then
// routes the connection to a local origin: :443 to the TLS origin, 1.0.0.1
// to the alternative origin, everything else to the main origin.
func (h *harness) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip}
	} else if addrs, err = lookup(ctx, host); err != nil {
		return nil, err
	}
	allowed, err := h.s.safe.Filter(ctx, addrs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", host, err)
	}
	target := h.origin.srv.Listener.Addr().String()
	switch {
	case port == "443":
		if h.tls == nil {
			return nil, errors.New("no TLS origin")
		}
		target = h.tls.srv.Listener.Addr().String()
	case allowed[0] == netip.MustParseAddr("1.0.0.1"):
		target = h.alt.srv.Listener.Addr().String()
	}
	var d net.Dialer
	return d.DialContext(ctx, network, target)
}

func (h *harness) updateSettings(fn func(*settings.All)) {
	h.t.Helper()
	if _, err := h.set.Update(context.Background(), func(a *settings.All) error { fn(a); return nil }); err != nil {
		h.t.Fatal(err)
	}
}

// do sends a request through the proxy with the given Host.
func (h *harness) do(method, host, uri string, hdr http.Header) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.proxy.URL+uri, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Host = host
	for k, vv := range hdr {
		req.Header[k] = vv
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

// get performs a request and returns the response with its body read.
func (h *harness) get(method, host, uri string, hdr http.Header) (*http.Response, []byte) {
	h.t.Helper()
	resp := h.do(method, host, uri, hdr)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("%s %s: read body: %v", method, uri, err)
	}
	return resp, body
}

// events waits until n cache events were logged and returns them.
func (h *harness) events(n int) []logs.CacheEvent {
	h.t.Helper()
	var evs []logs.CacheEvent
	eventually(h.t, func() bool { evs = h.logs.all(); return len(evs) >= n })
	return evs
}

// waitWrites waits until the fake store has stored n slices in total.
func (h *harness) waitWrites(n int) {
	h.t.Helper()
	eventually(h.t, func() bool {
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		sum := 0
		for _, w := range h.store.writes {
			sum += w
		}
		return sum >= n
	})
}

// waitSlices waits until the object at path has n cached slices.
func (h *harness) waitSlices(service, path string, n int) {
	h.t.Helper()
	eventually(h.t, func() bool { return h.store.sliceCount(service, path) == n })
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5 s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// testData returns n deterministic bytes.
func testData(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}
