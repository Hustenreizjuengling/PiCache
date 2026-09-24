package api

import (
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// upstreamDNS serves A 192.0.2.1 for every question on 127.0.0.1 (UDP).
func upstreamDNS(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	srv := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) },
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
			m := new(dns.Msg)
			m.SetReply(q)
			m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300}, A: net.IPv4(192, 0, 2, 1)}}
			_ = w.WriteMsg(m)
		})}
	go func() { _ = srv.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().String()
}

// newUpstreamTestServer wires a real resolver (default upstreams replaced
// by upstreams, if given) into a Server whose handlers are called directly.
func newUpstreamTestServer(t *testing.T, upstreams ...string) (*Server, *upstream.Resolver) {
	t.Helper()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "picache.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	st, err := settings.Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	if len(upstreams) > 0 {
		if _, err := st.Update(ctx, func(a *settings.All) error { a.DNS.Upstreams = upstreams; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	up, err := upstream.New(st, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = up.Close() })
	box, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	as, err := auth.New(ctx, d, st, box, filepath.Join(dir, "setup-token"), log)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{d: Deps{Settings: st, Upstream: up, Auth: as, Log: log}, log: log}
	return s, up
}

// callUpstream runs h like s.route does after authentication (admin
// principal in the context).
func callUpstream(s *Server, h handlerFunc, method, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/v1/dns/upstreams", strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	p := &auth.Principal{UserID: 1, Username: "admin", Scope: auth.ScopeAdmin}
	r = r.WithContext(context.WithValue(r.Context(), principalKey, p))
	w := httptest.NewRecorder()
	if err := h(w, r); err != nil {
		writeError(w, r, s.log, err)
	}
	return w
}

func TestUpstreamRoutesList(t *testing.T) {
	s, _ := newUpstreamTestServer(t)
	w := callUpstream(s, s.handleUpstreamList, http.MethodGet, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var got upstreamsView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	def := settings.Defaults().DNS
	if len(got.Upstreams) != len(def.Upstreams) || got.Upstreams[0].Upstream != def.Upstreams[0] || !got.Upstreams[0].Healthy {
		t.Errorf("upstreams = %+v", got.Upstreams)
	}
	if got.Cache.Capacity != def.CacheSize || got.ClockGuard {
		t.Errorf("cache = %+v clockGuard = %v", got.Cache, got.ClockGuard)
	}
	for _, member := range []string{`"upstreams":[`, `"cache":{`, `"clockGuard":false`, `"avgRttMs"`, `"staleHits"`} {
		if !strings.Contains(w.Body.String(), member) {
			t.Errorf("response lacks %s: %s", member, w.Body)
		}
	}
}

func TestUpstreamRoutesTest(t *testing.T) {
	addr := upstreamDNS(t)
	s, _ := newUpstreamTestServer(t)
	tests := []struct {
		name   string
		body   string
		status int
		field  string
		ok     bool
	}{
		{name: "working upstream", body: `{"upstream":"` + addr + `"}`, status: http.StatusOK, ok: true},
		{name: "invalid scheme", body: `{"upstream":"ftp://dns.example"}`, status: http.StatusBadRequest, field: "upstream"},
		{name: "empty", body: `{"upstream":""}`, status: http.StatusBadRequest, field: "upstream"},
		{name: "too long", body: `{"upstream":"https://` + strings.Repeat("a", 600) + `.example/dns-query"}`, status: http.StatusBadRequest, field: "upstream"},
		{name: "unknown member", body: `{"upstream":"` + addr + `","x":1}`, status: http.StatusBadRequest, field: "body"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := callUpstream(s, s.handleUpstreamTest, http.MethodPost, tc.body)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body)
			}
			if tc.status != http.StatusOK {
				var eb errorBody
				if err := json.Unmarshal(w.Body.Bytes(), &eb); err != nil || eb.Error.Field != tc.field || eb.Error.Code != "invalid" {
					t.Errorf("error body = %s", w.Body)
				}
				return
			}
			var res upstream.TestResult
			if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
				t.Fatal(err)
			}
			if res.OK != tc.ok || res.Upstream != addr || res.Answer != "192.0.2.1" {
				t.Errorf("result = %+v", res)
			}
		})
	}
}

func TestUpstreamRoutesFlush(t *testing.T) {
	addr := upstreamDNS(t)
	s, up := newUpstreamTestServer(t, addr)
	q := new(dns.Msg)
	q.SetQuestion("cached.example.", dns.TypeA)
	if _, _, err := up.Resolve(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if n := up.CacheStats().Entries; n != 1 {
		t.Fatalf("entries before flush = %d", n)
	}
	w := callUpstream(s, s.handleDNSCacheFlush, http.MethodPost, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if n := up.CacheStats().Entries; n != 0 {
		t.Errorf("entries after flush = %d", n)
	}
}
