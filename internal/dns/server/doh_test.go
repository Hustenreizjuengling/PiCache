package dnsserver

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// dohEnv serves ServeDoH over HTTP/2 (TLS); the source of every request
// is src (the X-Test-Source header overrides it).
type dohEnv struct {
	*testEnv
	srv    *httptest.Server
	client *http.Client
}

func newDoHEnv(t *testing.T, mutate func(*settings.All)) *dohEnv {
	t.Helper()
	e, _ := encEnv(t, mutate)
	d := &dohEnv{testEnv: e}
	d.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		src := netip.MustParseAddr("192.168.1.50")
		if v := r.Header.Get("X-Test-Source"); v != "" {
			src = netip.MustParseAddr(v)
		}
		e.srv.ServeDoH(w, r, src)
	}))
	d.srv.EnableHTTP2 = true
	d.srv.StartTLS()
	t.Cleanup(d.srv.Close)
	d.client = d.srv.Client()
	return d
}

func packQuery(t *testing.T, name string, id uint16) []byte {
	t.Helper()
	m := question(name, dns.TypeA)
	m.Id = id
	b, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (d *dohEnv) post(t *testing.T, path string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, d.srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/dns-message")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return d.do(t, req)
}

func (d *dohEnv) get(t *testing.T, target string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, d.srv.URL+target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return d.do(t, req)
}

func (d *dohEnv) do(t *testing.T, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	res, err := d.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func TestDoHAnswers(t *testing.T) {
	d := newDoHEnv(t, nil)
	res, body := d.post(t, "/dns-query", packQuery(t, "post.example", 0), nil)
	m := new(dns.Msg)
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/dns-message" || m.Unpack(body) != nil ||
		m.Id != 0 || len(m.Answer) != 1 || res.Header.Get("Cache-Control") != "private, max-age=300" || res.ProtoMajor != 2 {
		t.Fatalf("POST: %d %v %+v", res.StatusCode, res.Header, m)
	}
	wire := packQuery(t, "Get.Example", 4711)
	res, body = d.get(t, "/dns-query/Phone?dns="+base64.URLEncoding.EncodeToString(wire)+"&ct=x", nil)
	if res.StatusCode != http.StatusOK || m.Unpack(body) != nil || m.Id != 4711 || m.Question[0].Name != "Get.Example." {
		t.Fatalf("GET: %d %+v", res.StatusCode, m)
	}
	if ev := d.logs.waitEvent(t, "get.example", 0); ev.Protocol != ProtoDoH || ev.DNSClientID != "phone" {
		t.Fatalf("logged %+v", ev)
	}
	if _, doh := d.srv0().EncryptedQueries(); doh != 2 {
		t.Fatalf("DoH queries %d", doh)
	}
	// Errors carry no-store; an NXDOMAIN without records caches 0 seconds.
	d.up.setAnswer(func(req *dns.Msg, _ []string) (*dns.Msg, upstream.Info, error) {
		return new(dns.Msg).SetRcode(req, dns.RcodeServerFailure), upstream.Info{Upstream: "u"}, nil
	})
	if res, _ := d.post(t, "/dns-query", packQuery(t, "fail.example", 1), nil); res.Header.Get("Cache-Control") != "private, max-age=0" {
		t.Fatalf("SERVFAIL cache control %q", res.Header.Get("Cache-Control"))
	}
}

func (d *dohEnv) srv0() *Server { return d.testEnv.srv }

func TestDoHErrors(t *testing.T) {
	d := newDoHEnv(t, nil)
	good := packQuery(t, "e.example", 1)
	resp := new(dns.Msg)
	resp.SetQuestion("e.example.", dns.TypeA)
	resp.Response = true
	qr, _ := resp.Pack()
	for _, tc := range []struct {
		name   string
		req    func() (*http.Response, []byte)
		status int
		want   string
	}{
		{"put", func() (*http.Response, []byte) {
			r, _ := http.NewRequest(http.MethodPut, d.srv.URL+"/dns-query", nil)
			return d.do(t, r)
		}, 405, "method not allowed"},
		{"content type", func() (*http.Response, []byte) {
			return d.post(t, "/dns-query", good, map[string]string{"Content-Type": "text/plain"})
		}, 415, "Content-Type"},
		{"too large", func() (*http.Response, []byte) {
			return d.post(t, "/dns-query", make([]byte, 64<<10+1), nil)
		}, 413, "64 KiB"},
		{"bad base64", func() (*http.Response, []byte) { return d.get(t, "/dns-query?dns=%%%", nil) }, 400, ""},
		{"missing dns", func() (*http.Response, []byte) { return d.get(t, "/dns-query", nil) }, 400, "missing"},
		{"bad message", func() (*http.Response, []byte) { return d.post(t, "/dns-query", []byte{1, 2, 3}, nil) }, 400, "not a DNS query"},
		{"QR set", func() (*http.Response, []byte) { return d.post(t, "/dns-query", qr, nil) }, 400, "not a DNS query"},
		{"invalid ClientID", func() (*http.Response, []byte) { return d.post(t, "/dns-query/-bad-", good, nil) }, 400, "invalid ClientID"},
		{"more segments", func() (*http.Response, []byte) { return d.post(t, "/dns-query/a/b", good, nil) }, 404, ""},
		{"cross-site", func() (*http.Response, []byte) {
			return d.post(t, "/dns-query", good, map[string]string{"Sec-Fetch-Site": "cross-site"})
		}, 403, "cross-site DoH requests are refused"},
		{"same-site", func() (*http.Response, []byte) {
			return d.post(t, "/dns-query", good, map[string]string{"Sec-Fetch-Site": "same-site"})
		}, 403, "cross-site"},
		{"foreign origin", func() (*http.Response, []byte) {
			return d.post(t, "/dns-query", good, map[string]string{"Origin": "https://evil.example"})
		}, 403, "cross-site"},
		{"same origin", func() (*http.Response, []byte) {
			return d.post(t, "/dns-query", good, map[string]string{"Origin": d.srv.URL, "Sec-Fetch-Site": "same-origin"})
		}, 200, ""},
		{"outside the DNS ACL", func() (*http.Response, []byte) {
			return d.post(t, "/dns-query", good, map[string]string{"X-Test-Source": "203.0.113.9"})
		}, 403, "this address may not use DNS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, body := tc.req()
			if res.StatusCode != tc.status || !strings.Contains(string(body), tc.want) {
				t.Fatalf("%d %q", res.StatusCode, body)
			}
			if tc.status != 200 && res.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control %q", res.Header.Get("Cache-Control"))
			}
			if tc.status == 405 && res.Header.Get("Allow") != "GET, POST" {
				t.Fatalf("Allow %q", res.Header.Get("Allow"))
			}
		})
	}
	if len(d.srv0().refusedSrc.list()) == 0 {
		t.Fatal("the refused source was not counted")
	}
	d.update(func(a *settings.All) { a.DNS.Encrypted.DoH = false })
	if res, _ := d.post(t, "/dns-query", good, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("DoH off: %d", res.StatusCode)
	}
}

// TestDoHDropResetsStream: a dropped query resets only its stream; another
// stream on the same HTTP/2 connection completes.
func TestDoHDropResetsStream(t *testing.T) {
	d := newDoHEnv(t, func(a *settings.All) { a.DNS.DroppedDomains = []string{"drop.example"} })
	release := make(chan struct{})
	d.up.setAnswer(func(req *dns.Msg, _ []string) (*dns.Msg, upstream.Info, error) {
		if strings.HasPrefix(req.Question[0].Name, "slow.") {
			<-release
		}
		return upAnswer(req), upstream.Info{Upstream: "u"}, nil
	})
	// Establish the connection.
	if res, _ := d.post(t, "/dns-query", packQuery(t, "warm.example", 1), nil); res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	slow := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, d.srv.URL+"/dns-query", bytes.NewReader(packQuery(t, "slow.example", 2)))
		req.Header.Set("Content-Type", "application/dns-message")
		res, err := d.client.Do(req)
		if err != nil {
			slow <- 0
			return
		}
		res.Body.Close()
		slow <- res.StatusCode
	}()
	time.Sleep(100 * time.Millisecond)
	req, _ := http.NewRequest(http.MethodPost, d.srv.URL+"/dns-query", bytes.NewReader(packQuery(t, "drop.example", 3)))
	req.Header.Set("Content-Type", "application/dns-message")
	if res, err := d.client.Do(req); err == nil {
		res.Body.Close()
		t.Fatalf("dropped query answered: %d", res.StatusCode)
	}
	close(release)
	if st := <-slow; st != 200 {
		t.Fatalf("the other stream: %d", st)
	}
	if d.logs.count("drop.example") != 0 {
		t.Fatal("dropped query logged")
	}
}

// TestDoHLimits: rate limited → 200 REFUSED; 64 requests of a client key
// in flight → 429; overload → 503.
func TestDoHLimits(t *testing.T) {
	d := newDoHEnv(t, nil)
	var held atomic.Int32
	release := make(chan struct{})
	d.up.setAnswer(func(req *dns.Msg, _ []string) (*dns.Msg, upstream.Info, error) {
		if strings.HasPrefix(req.Question[0].Name, "hold") {
			held.Add(1)
			<-release
		}
		return upAnswer(req), upstream.Info{Upstream: "u"}, nil
	})
	var wg sync.WaitGroup
	for i := range dohMaxPerClient {
		wg.Go(func() {
			req, _ := http.NewRequest(http.MethodPost, d.srv.URL+"/dns-query", bytes.NewReader(packQuery(t, "hold"+string(rune('a'+i%26))+".example", uint16(i))))
			req.Header.Set("Content-Type", "application/dns-message")
			if res, err := d.client.Do(req); err == nil {
				res.Body.Close()
			}
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	for held.Load() < dohMaxPerClient && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	res, _ := d.post(t, "/dns-query", packQuery(t, "extra.example", 1), nil)
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") != "1" {
		t.Fatalf("65th request: %d %v (held %d)", res.StatusCode, res.Header, held.Load())
	}
	// Another client key is not affected.
	if res, _ := d.post(t, "/dns-query", packQuery(t, "other.example", 1), map[string]string{"X-Test-Source": "192.168.1.51"}); res.StatusCode != 200 {
		t.Fatalf("other key: %d", res.StatusCode)
	}
	close(release)
	wg.Wait()

	d.srv0().inFlight.Store(maxInFlight)
	res, _ = d.post(t, "/dns-query", packQuery(t, "busy.example", 1), nil)
	d.srv0().inFlight.Store(0)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("overload: %d", res.StatusCode)
	}

	d.update(func(a *settings.All) { a.DNS.RateLimitQPS, a.DNS.RateLimitBurst = 1, 1 })
	var refused bool
	for i := range 5 {
		res, body := d.post(t, "/dns-query", packQuery(t, "rl.example", uint16(i)), nil)
		m := new(dns.Msg)
		if res.StatusCode != 200 || m.Unpack(body) != nil {
			t.Fatalf("rate limited: %d", res.StatusCode)
		}
		refused = refused || m.Rcode == dns.RcodeRefused
	}
	if !refused {
		t.Fatal("never rate limited")
	}
}

func TestDoHClientIDPath(t *testing.T) {
	for path, want := range map[string][3]string{
		"/dns-query": {"", "true", "true"}, "/dns-query/": {"", "true", "true"}, "/dns-query/Kid-1": {"kid-1", "true", "true"},
		"/dns-query/a/b": {"", "false", "false"}, "/dns-query/-x": {"", "true", "false"}, "/other": {"", "false", "false"},
	} {
		id, found, valid := DoHClientID(path)
		if id != want[0] || boolStr(found) != want[1] || boolStr(valid) != want[2] {
			t.Fatalf("%s: %q %v %v", path, id, found, valid)
		}
	}
	if !IsDoHPath("/dns-query") || !IsDoHPath("/dns-query/x") || IsDoHPath("/dns-queryx") {
		t.Fatal("IsDoHPath")
	}
	var l dohLimiter
	for i := range dohMaxClientKeys {
		if _, st := l.acquire(netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 1}), 32)); st != 0 {
			t.Fatalf("key %d: %d", i, st)
		}
	}
	if _, st := l.acquire(netip.MustParsePrefix("192.0.2.1/32")); st != http.StatusServiceUnavailable {
		t.Fatalf("beyond the key bound: %d", st)
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
