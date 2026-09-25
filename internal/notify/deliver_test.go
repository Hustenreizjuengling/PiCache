package notify

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// capture is an httptest server that records the requests it gets.
type capture struct {
	srv    *httptest.Server
	status int // answer (default 200)
	mu     sync.Mutex
	reqs   []capturedRequest
}

type capturedRequest struct {
	method, path, query string
	header              http.Header
	body                string
}

func newCapture(t *testing.T) *capture {
	t.Helper()
	c := &capture{status: http.StatusOK}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.reqs = append(c.reqs, capturedRequest{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(b)})
		status := c.status
		c.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, strings.Repeat("x", 64<<10)) // longer than what is read
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *capture) requests() []capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedRequest(nil), c.reqs...)
}

var testMessage = Message{Event: EventHealthFailed, Severity: SeverityError, Title: "Health check failed: upstreams",
	Message: "no upstream DNS server is answering\nHint: check the internet connection", Time: time.Date(2026, 9, 25, 1, 30, 0, 0, time.UTC)}

// sendTo creates a channel and sends testMessage to it once.
func sendTo(t *testing.T, s *Service, in ChannelInput) (int, error) {
	t.Helper()
	c, err := s.Create(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	ch, sealed := s.worker(c.ID).snapshot()
	return s.send(t.Context(), ch, sealed, testMessage)
}

func TestWebhookFormat(t *testing.T) {
	s, _ := newTestService(t)
	cp := newCapture(t)
	if _, err := sendTo(t, s, ChannelInput{Name: "ha", Kind: KindWebhook, URL: cp.srv.URL + "/api/webhook/picache?x=1",
		Secret: strp("Bearer abc123")}); err != nil {
		t.Fatal(err)
	}
	r := cp.requests()[0]
	if r.method != http.MethodPost || r.path != "/api/webhook/picache" || r.query != "x=1" ||
		r.header.Get("Content-Type") != "application/json" || r.header.Get("Authorization") != "Bearer abc123" ||
		r.header.Get("User-Agent") != "PiCache/v0.4.0" {
		t.Fatalf("request %+v", r)
	}
	want := `{"event":"health.failed","severity":"error","title":"Health check failed: upstreams",` +
		`"message":"no upstream DNS server is answering\nHint: check the internet connection","time":"2026-09-25T01:30:00Z",` +
		`"instance":"picache-0123456789ab","hostname":"pi","version":"v0.4.0"}`
	if r.body != want {
		t.Fatalf("body\n%s\nwant\n%s", r.body, want)
	}
	// Without a secret there is no Authorization header.
	if _, err := sendTo(t, s, ChannelInput{Name: "ha2", Kind: KindWebhook, URL: cp.srv.URL + "/h"}); err != nil {
		t.Fatal(err)
	}
	if r := cp.requests()[1]; r.header.Get("Authorization") != "" {
		t.Fatalf("authorization without a secret: %v", r.header)
	}
}

func TestNtfyFormat(t *testing.T) {
	s, _ := newTestService(t)
	cp := newCapture(t)
	if _, err := sendTo(t, s, ChannelInput{Name: "ntfy", Kind: KindNtfy, URL: cp.srv.URL + "/picache-alerts", Secret: strp("tk_abc")}); err != nil {
		t.Fatal(err)
	}
	r := cp.requests()[0]
	if r.method != http.MethodPost || r.path != "/picache-alerts" || r.body != testMessage.Message ||
		r.header.Get("Title") != testMessage.Title || r.header.Get("Priority") != "5" ||
		r.header.Get("Tags") != "health.failed,error" || r.header.Get("Authorization") != "Bearer tk_abc" ||
		!strings.HasPrefix(r.header.Get("Content-Type"), "text/plain") {
		t.Fatalf("request %+v", r)
	}
	// Priorities per severity; non-ASCII titles as RFC 2047 encoded words.
	c, _ := s.Create(t.Context(), ChannelInput{Name: "n2", Kind: KindNtfy, URL: cp.srv.URL + "/t"})
	ch, _ := s.worker(c.ID).snapshot()
	for sev, prio := range map[Severity]string{SeverityInfo: "3", SeverityWarning: "4", SeverityError: "5"} {
		m := testMessage
		m.Severity, m.Title = sev, "Speicher „NAS Büro“ offline"
		if _, err := s.send(t.Context(), ch, "", m); err != nil {
			t.Fatal(err)
		}
		reqs := cp.requests()
		r := reqs[len(reqs)-1]
		if r.header.Get("Priority") != prio || r.header.Get("Authorization") != "" || !strings.HasPrefix(r.header.Get("Title"), "=?utf-8?q?") ||
			r.header.Get("Tags") != "health.failed,"+string(sev) {
			t.Fatalf("%s: %v", sev, r.header)
		}
	}
}

func TestGotifyFormat(t *testing.T) {
	s, _ := newTestService(t)
	cp := newCapture(t)
	if _, err := sendTo(t, s, ChannelInput{Name: "g", Kind: KindGotify, URL: cp.srv.URL + "/gotify/", Secret: strp("AppTok.1")}); err != nil {
		t.Fatal(err)
	}
	r := cp.requests()[0]
	if r.method != http.MethodPost || r.path != "/gotify/message" || r.header.Get("X-Gotify-Key") != "AppTok.1" ||
		r.header.Get("Content-Type") != "application/json" || r.header.Get("Authorization") != "" {
		t.Fatalf("request %+v", r)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(r.body), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 3 || body["title"] != testMessage.Title || body["message"] != testMessage.Message || body["priority"] != float64(8) {
		t.Fatalf("body %s", r.body)
	}
	for sev, prio := range map[Severity]float64{SeverityInfo: 4, SeverityWarning: 6} {
		c, _ := s.Create(t.Context(), ChannelInput{Name: "g" + string(sev), Kind: KindGotify, URL: cp.srv.URL, Secret: strp("t")})
		ch, sealed := s.worker(c.ID).snapshot()
		m := testMessage
		m.Severity = sev
		if _, err := s.send(t.Context(), ch, sealed, m); err != nil {
			t.Fatal(err)
		}
		reqs := cp.requests()
		if err := json.Unmarshal([]byte(reqs[len(reqs)-1].body), &body); err != nil || body["priority"] != prio ||
			reqs[len(reqs)-1].path != "/message" {
			t.Fatalf("%s: %+v", sev, reqs[len(reqs)-1])
		}
	}
}

// Redirects are never followed; the error says so and has no URL.
func TestNoRedirects(t *testing.T) {
	s, _ := newTestService(t)
	target := newCapture(t)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.srv.URL+"/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	status, err := sendTo(t, s, ChannelInput{Name: "r", Kind: KindWebhook, URL: redirect.URL + "/hook?auth=tk_secret", Secret: strp("x")})
	if status != http.StatusTemporaryRedirect || err == nil || !strings.Contains(err.Error(), "redirects are not followed") {
		t.Fatalf("status %d, err %v", status, err)
	}
	if strings.Contains(err.Error(), "tk_secret") || strings.Contains(err.Error(), redirect.URL) {
		t.Fatalf("error leaks the URL: %v", err)
	}
	if n := len(target.requests()); n != 0 {
		t.Fatalf("redirect followed (%d requests)", n)
	}
	if retryable(status, err) {
		t.Fatal("a redirect is not retried")
	}
}

// Error answers and network errors: status, message without the URL,
// retry decision; TLS certificates are verified.
func TestSendErrors(t *testing.T) {
	s, _ := newTestService(t)
	cp := newCapture(t)
	cp.status = http.StatusUnauthorized
	status, err := sendTo(t, s, ChannelInput{Name: "a", Kind: KindWebhook, URL: cp.srv.URL + "/h?token=tk_secret"})
	if status != 401 || err == nil || err.Error() != "HTTP 401 Unauthorized" || retryable(status, err) {
		t.Fatalf("401: %d %v", status, err)
	}
	cp.status = http.StatusServiceUnavailable
	if status, err := sendTo(t, s, ChannelInput{Name: "b", Kind: KindWebhook, URL: cp.srv.URL}); status != 503 || !retryable(status, err) {
		t.Fatalf("503: %d %v", status, err)
	}
	cp.status = http.StatusTooManyRequests
	if status, err := sendTo(t, s, ChannelInput{Name: "c", Kind: KindWebhook, URL: cp.srv.URL}); !retryable(status, err) {
		t.Fatalf("429: %d %v", status, err)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	status, err = sendTo(t, s, ChannelInput{Name: "d", Kind: KindWebhook, URL: closed.URL + "/h?token=tk_secret"})
	if status != 0 || err == nil || !retryable(status, err) || strings.Contains(err.Error(), "tk_secret") ||
		strings.Contains(err.Error(), "/h") {
		t.Fatalf("closed: %d %v", status, err)
	}

	tlsSrv := httptest.NewTLSServer(http.NotFoundHandler())
	defer tlsSrv.Close()
	if _, err := sendTo(t, s, ChannelInput{Name: "e", Kind: KindWebhook, URL: tlsSrv.URL}); err == nil ||
		!strings.Contains(err.Error(), "certificate") {
		t.Fatalf("self-signed certificate accepted: %v", err)
	}

	// A stored secret that cannot be opened (another master key) is a
	// configuration error.
	c, _ := s.Create(t.Context(), ChannelInput{Name: "f", Kind: KindGotify, URL: cp.srv.URL, Secret: strp("t")})
	ch, _ := s.worker(c.ID).snapshot()
	sealed, _ := testBox(t).Seal([]byte("t"), secretAAD("other"))
	if _, err := s.send(t.Context(), ch, sealed, testMessage); err == nil || retryable(0, err) || !strings.Contains(err.Error(), "enter it again") {
		t.Fatalf("undecryptable secret: %v", err)
	}
}

func TestForbiddenAddresses(t *testing.T) {
	for addr, forbidden := range map[string]bool{
		"169.254.169.254": true, "fe80::1": true, "224.0.0.251": true, "ff02::fb": true, "0.0.0.0": true, "::": true,
		"0.1.2.3": true, "255.255.255.255": true, "64:ff9b::a9fe:a9fe": true, "2002:a9fe:a9fe::1": true,
		"127.0.0.1": false, "::1": false, "192.168.1.20": false, "10.0.0.2": false, "fd00::20": false, "100.64.0.1": false,
		"9.9.9.9": false, "2620:fe::fe": false, "::ffff:192.168.1.2": false,
	} {
		if got := forbiddenAddr(netip.MustParseAddr(addr)); got != forbidden {
			t.Errorf("forbiddenAddr(%s) = %v", addr, got)
		}
		err := dialControl("tcp", netip.AddrPortFrom(netip.MustParseAddr(addr), 443).String(), nil)
		if (err != nil) != forbidden {
			t.Errorf("dialControl(%s) = %v", addr, err)
		}
	}
	if err := requestError(context.Background(), errForbiddenAddr); retryable(0, err) || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("forbidden: %v", err)
	}
}

// The test button sends synchronously, ignores the filters (also a
// disabled channel) and is logged.
func TestTestNotification(t *testing.T) {
	s, _ := newTestService(t)
	cp := newCapture(t)
	c, err := s.Create(t.Context(), ChannelInput{Name: "Phone", Kind: KindNtfy, URL: cp.srv.URL + "/t", MinSeverity: SeverityError,
		Events: []string{EventBackupFailed}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Test(t.Context(), c.ID)
	if err != nil || !res.OK || res.Status != 200 || res.Error != "" || res.DurationMs < 0 {
		t.Fatalf("test %+v, %v", res, err)
	}
	r := cp.requests()[0]
	if r.header.Get("Tags") != "notify.test,info" || !strings.Contains(r.body, `"Phone"`) {
		t.Fatalf("test request %+v", r)
	}
	cp.status = 500
	if res, _ := s.Test(t.Context(), c.ID); res.OK || res.Status != 500 || res.Error != "HTTP 500 Internal Server Error" {
		t.Fatalf("failed test %+v", res)
	}
	log := s.Log(10)
	if len(log) != 2 || log[0].OK || !log[1].OK || log[0].Event != EventTest || log[0].ChannelName != "Phone" || log[0].Attempt != 1 {
		t.Fatalf("log %+v", log)
	}
	if _, err := s.Test(t.Context(), newID()); err == nil {
		t.Fatal("unknown channel")
	}
}
