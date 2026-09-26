package filter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// listServer is a test list server. body and etag can be changed between
// requests; conditional records whether the last request carried validators.
type listServer struct {
	*httptest.Server
	mu          sync.Mutex
	body        string
	etag        string
	status      int
	ignoreCond  bool
	conditional bool
	requests    atomic.Int32
}

func newListServer(t *testing.T) *listServer {
	s := &listServer{status: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/list", func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.conditional = r.Header.Get("If-None-Match") != ""
		if s.status != http.StatusOK {
			http.Error(w, "boom", s.status)
			return
		}
		if s.etag != "" {
			w.Header().Set("ETag", s.etag)
			if !s.ignoreCond && r.Header.Get("If-None-Match") == s.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		_, _ = io.WriteString(w, s.body)
	})
	mux.HandleFunc("/redirect-private", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.0.0.1/list", http.StatusFound)
	})
	mux.HandleFunc("/redirect-loopback-https", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://127.0.0.2/list", http.StatusFound)
	})
	mux.HandleFunc("/redirect-other-http", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://lists.example/list", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/redirect-same", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/list", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		for range 64 {
			_, _ = io.WriteString(w, "||"+strings.Repeat("a", 50)+".example^\n")
		}
	})
	mux.HandleFunc("/html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "\n<!DOCTYPE html>\n<html><body>Rate limited</body></html>\n")
	})
	mux.HandleFunc("/binary", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("PK\x03\x04\x14\x00\x00\x00"))
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *listServer) set(fn func(s *listServer)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

func TestDownloadConditionalGet(t *testing.T) {
	srv := newListServer(t)
	srv.set(func(s *listServer) { s.body, s.etag = "||a.example^\n||b.example^\n", `"v1"` })
	e := newTestEngine(t)
	ctx := context.Background()
	l, err := e.CreateList(ctx, ListInput{URL: srv.URL + "/list", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if l, err = e.RefreshList(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	if l.Status != statusOK || l.Entries != 2 || srv.conditional {
		t.Fatalf("first download: %+v (conditional %v)", l, srv.conditional)
	}
	if !e.Check("x.a.example", qtypeA, []int64{1}).Blocked() {
		t.Fatal("list not applied")
	}
	firstUpdate := l.LastUpdated

	// 304 Not Modified: nothing is parsed.
	changed, err := e.refresh(ctx, l.ID, true)
	if err != nil || changed {
		t.Fatalf("304: changed=%v err=%v", changed, err)
	}
	l, _ = e.list(l.ID)
	if l.Status != statusUnchanged || !srv.conditional || l.Entries != 2 || !l.LastUpdated.Equal(firstUpdate) {
		t.Errorf("after 304: %+v (conditional %v)", l, srv.conditional)
	}

	// The server ignores validators but the content is identical: unchanged.
	srv.set(func(s *listServer) { s.ignoreCond = true })
	if changed, err := e.refresh(ctx, l.ID, true); err != nil || changed {
		t.Fatalf("same content: changed=%v err=%v", changed, err)
	}
	if l, _ = e.list(l.ID); l.Status != statusUnchanged {
		t.Errorf("same content: %+v", l)
	}

	// New content is parsed and replaces the old entries.
	srv.set(func(s *listServer) { s.body, s.etag = "||c.example^\n", `"v2"` })
	if l, err = e.RefreshList(ctx, l.ID); err != nil || l.Status != statusOK || l.Entries != 1 {
		t.Fatalf("new content: %+v %v", l, err)
	}
	if e.Check("a.example", qtypeA, []int64{1}).Blocked() || !e.Check("c.example", qtypeA, []int64{1}).Blocked() {
		t.Error("matcher not rebuilt from the new content")
	}

	// A failing server keeps the last good copy.
	srv.set(func(s *listServer) { s.status = http.StatusInternalServerError })
	if l, err = e.RefreshList(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	if l.Status != statusFailedCached || !strings.Contains(l.LastError, "500") || !e.Check("c.example", qtypeA, []int64{1}).Blocked() {
		t.Errorf("failure: %+v", l)
	}
	// An HTML error page with status 200 is rejected, the copy is kept.
	srv.set(func(s *listServer) {
		s.status, s.body, s.etag = http.StatusOK, "<html><body>captive portal</body></html>", ""
	})
	if l, err = e.RefreshList(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	if l.Status != statusFailedCached || !strings.Contains(l.LastError, "HTML") || !e.Check("c.example", qtypeA, []int64{1}).Blocked() {
		t.Errorf("html: %+v", l)
	}
}

// TestEmptyDownloadKeepsLastGoodCopy: a changed download without entries
// (empty, blank or comment-only body) does not replace a cached copy that
// has entries; the list reports failed-cached and keeps blocking.
func TestEmptyDownloadKeepsLastGoodCopy(t *testing.T) {
	srv := newListServer(t)
	srv.set(func(s *listServer) { s.body, s.etag = "||a.example^\n||b.example^\n", `"good"` })
	e := newTestEngine(t)
	ctx := context.Background()
	l, err := e.CreateList(ctx, ListInput{URL: srv.URL + "/list", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if l, err = e.RefreshList(ctx, l.ID); err != nil || l.Status != statusOK || l.Entries != 2 {
		t.Fatalf("first download: %+v %v", l, err)
	}
	good, err := os.ReadFile(e.cachePath(l.ID))
	if err != nil {
		t.Fatal(err)
	}
	for i, body := range []string{"", "\n\n  \r\n", "# maintenance\n", "! Title: gone\nnot a domain\n"} {
		srv.set(func(s *listServer) { s.body, s.etag = body, `"empty"` })
		// Twice: the validators of the rejected version must not be stored,
		// or the second attempt would be answered 304 and count as unchanged.
		for range 2 {
			if l, err = e.RefreshList(ctx, l.ID); err != nil {
				t.Fatal(err)
			}
			if l.Status != statusFailedCached || !strings.Contains(l.LastError, "no entries") || l.Entries != 2 {
				t.Errorf("body %d: status %q error %q entries %d, want failed-cached with 2 entries", i, l.Status, l.LastError, l.Entries)
			}
			if !e.Check("x.a.example", qtypeA, []int64{1}).Blocked() || !e.Check("b.example", qtypeA, []int64{1}).Blocked() {
				t.Errorf("body %d: the last good copy is no longer applied", i)
			}
			if cached, _ := os.ReadFile(e.cachePath(l.ID)); string(cached) != string(good) {
				t.Errorf("body %d: cached copy replaced by %q", i, cached)
			}
		}
	}

	// A list with entries again is accepted.
	srv.set(func(s *listServer) { s.body, s.etag = "||c.example^\n", `"new"` })
	if l, err = e.RefreshList(ctx, l.ID); err != nil || l.Status != statusOK || l.Entries != 1 {
		t.Fatalf("recovery: %+v %v", l, err)
	}
	if e.Check("a.example", qtypeA, []int64{1}).Blocked() || !e.Check("c.example", qtypeA, []int64{1}).Blocked() {
		t.Error("matcher not rebuilt after recovery")
	}

	// Without a copy that has entries, an empty list is accepted as is.
	srv2 := newListServer(t)
	srv2.set(func(s *listServer) { s.body = "# nothing yet\n" })
	l2, err := e.CreateList(ctx, ListInput{URL: srv2.URL + "/list", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if l2, err = e.RefreshList(ctx, l2.ID); err != nil || l2.Status != statusOK || l2.Entries != 0 {
		t.Fatalf("empty first download: %+v %v", l2, err)
	}
	srv2.set(func(s *listServer) { s.body = "" })
	if l2, err = e.RefreshList(ctx, l2.ID); err != nil || l2.Status != statusOK || l2.Entries != 0 {
		t.Fatalf("empty after empty: %+v %v", l2, err)
	}
}

func TestDownloadFailures(t *testing.T) {
	srv := newListServer(t)
	srv.set(func(s *listServer) { s.body = "||ok.example^\n" })
	e := newTestEngine(t)
	e.maxBytes = 1024
	ctx := context.Background()
	cases := []struct {
		path    string
		status  string
		errPart string
	}{
		{"/redirect-private", statusFailedEmpty, "private address"},
		{"/redirect-loopback-https", statusFailedEmpty, "private address"},
		{"/redirect-other-http", statusFailedEmpty, "https required"},
		{"/loop", statusFailedEmpty, "redirects"},
		{"/big", statusFailedEmpty, "larger than"},
		{"/html", statusFailedEmpty, "HTML"},
		{"/binary", statusFailedEmpty, "binary"},
		{"/missing", statusFailedEmpty, "404"},
		{"/redirect-same", statusOK, ""},
	}
	for _, c := range cases {
		l, err := e.CreateList(ctx, ListInput{URL: srv.URL + c.path, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		if l, err = e.RefreshList(ctx, l.ID); err != nil {
			t.Fatal(err)
		}
		if l.Status != c.status || !strings.Contains(l.LastError, c.errPart) {
			t.Errorf("%s: status %q error %q, want %q containing %q", c.path, l.Status, l.LastError, c.status, c.errPart)
		}
		if e.hasCache(l.ID) != (c.status == statusOK) {
			t.Errorf("%s: cached copy present = %v", c.path, e.hasCache(l.ID))
		}
	}
	if st := e.Stats(); st.FailedLists != len(cases)-1 {
		t.Errorf("FailedLists = %d", st.FailedLists)
	}
}

func TestCheckRedirect(t *testing.T) {
	cases := []struct {
		to      string
		orig    string
		private bool
		ok      bool
	}{
		{"https://cdn.example/list", "lists.example", false, true},
		{"https://8.8.8.8/list", "lists.example", false, true},
		{"http://cdn.example/list", "lists.example", false, false},
		{"https://192.168.1.2/list", "lists.example", false, false},
		{"https://[fd00::1]/list", "lists.example", false, false},
		{"https://127.0.0.1/list", "lists.example", false, false},
		{"https://169.254.169.254/latest", "lists.example", false, false},
		{"http://192.168.1.2/other", "192.168.1.2", true, true},
		{"http://192.168.1.3/other", "192.168.1.2", true, false},
		{"https://user:pw@cdn.example/list", "lists.example", false, false},
		{"ftp://cdn.example/list", "lists.example", false, false},
	}
	for _, c := range cases {
		u, _ := url.Parse(c.to)
		if err := checkRedirect(u, c.orig, c.private); (err == nil) != c.ok {
			t.Errorf("redirect to %s from %s: err = %v", c.to, c.orig, err)
		}
	}
}

// fakeTransport serves list content from memory (no network), counting requests.
type fakeTransport struct {
	requests atomic.Int32
	body     string
}

func (f *fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.requests.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{}, Request: r,
		Body: io.NopCloser(strings.NewReader(f.body)),
	}, nil
}

// TestSchedulerJitter runs the update loop on a fake clock: a list is
// downloaded once at start and again after the jittered interval.
func TestSchedulerJitter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		d := openTestDB(t, dir)
		defer d.Close()
		ft := &fakeTransport{body: "||sched.example^\n"}
		e := newEngineAt(t, dir, d, &http.Client{Transport: ft})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if _, err := e.set.Update(ctx, func(a *settings.All) error { a.Filter.UpdateIntervalHours = 1; return nil }); err != nil {
			t.Fatal(err)
		}
		if err := e.DeleteList(ctx, 1); err != nil {
			t.Fatal(err)
		}
		l, err := e.CreateList(ctx, ListInput{URL: "https://lists.example/sched.txt", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { e.Start(ctx); close(done) }()
		synctest.Wait()
		if n := ft.requests.Load(); n != 1 {
			t.Fatalf("requests after start = %d, want 1", n)
		}
		if !e.Check("sched.example", qtypeA, []int64{1}).Blocked() {
			t.Fatal("list not compiled after the first download")
		}
		time.Sleep(53 * time.Minute) // below 0.9 × 1 h
		synctest.Wait()
		if n := ft.requests.Load(); n != 1 {
			t.Fatalf("requests after 53 min = %d, want 1", n)
		}
		time.Sleep(15 * time.Minute) // 68 min: beyond 1.1 × 1 h (+ one tick)
		synctest.Wait()
		if n := ft.requests.Load(); n != 2 {
			t.Fatalf("requests after 68 min = %d, want 2", n)
		}
		if got, _ := e.list(l.ID); got.Status != statusUnchanged {
			t.Errorf("status %q", got.Status)
		}
		if err := e.RefreshAll(ctx); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if n := ft.requests.Load(); n != 3 {
			t.Fatalf("requests after RefreshAll = %d, want 3", n)
		}
		cancel()
		<-done
	})
}
