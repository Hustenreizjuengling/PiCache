package api

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// silentDNS is a UDP socket that never answers (a DNSSEC test against it
// runs until its probes time out).
func silentDNS(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc.LocalAddr().String()
}

// newDNSSECEnv is the core environment with a resolver whose only upstream
// is addr.
func newDNSSECEnv(t *testing.T, addr string) *coreEnv {
	t.Helper()
	e := newCoreEnv(t)
	if _, err := e.set.Update(context.Background(), func(a *settings.All) error {
		a.DNS.Upstreams, a.DNS.FallbackUpstreams = []string{addr}, []string{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	up, err := upstream.New(e.set, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = up.Close() })
	e.srv = New(Deps{Config: e.srv.d.Config, Settings: e.set, Auth: e.auth, Runtime: e.rt, Updates: e.upd, Upstream: up, Log: log})
	return e
}

func TestDNSSECTestRoute(t *testing.T) {
	e := newDNSSECEnv(t, upstreamDNS(t))
	admin := e.provisionAndLogin(t)
	readTok := e.createToken(t, admin, "read")
	_, viewer := e.withViewer(t, admin)
	const path = "/api/v1/dns/dnssec/test"
	coreWantError(t, e.do("POST", path, "", ""), http.StatusUnauthorized, "unauthorized", "")
	coreWantError(t, e.do("POST", path, "", viewer), http.StatusForbidden, "forbidden", "")
	coreWantError(t, e.do("POST", path, "", readTok), http.StatusForbidden, "forbidden", "")

	w := e.do("POST", path, "", admin)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var res upstream.DNSSECTest
	coreDecode(t, w, &res)
	// The test upstream returns no DNSSEC data: every check is
	// inconclusive (indeterminate).
	if res.Mode != settings.DNSSECValidate || len(res.Checks) != 4 || len(res.Upstreams) != 1 ||
		res.Upstreams[0].DNSSEC != upstream.ProbeNoDNSSEC {
		t.Fatalf("result %+v", res)
	}
	for _, c := range res.Checks {
		if c.Verdict != upstream.TestInconclusive || c.Status != "indeterminate" {
			t.Errorf("%s: %+v", c.Name, c)
		}
	}
	for _, name := range []string{"example.com", "google.com", "dnssec-failed.org", "sigfail.ippacket.stream"} {
		if !strings.Contains(w.Body.String(), `"name":"`+name+`"`) {
			t.Errorf("%s missing", name)
		}
	}
	var audited bool
	for _, en := range e.auditEntries(t) {
		if en.Action == "dns.dnssec.test" {
			audited = true
			if !strings.Contains(en.Details, `"inconclusive":4`) || !strings.Contains(en.Details, `"passed":0`) {
				t.Errorf("audit details %v", en.Details)
			}
		}
	}
	if !audited {
		t.Fatal("not audited")
	}
	// At most one start per 10 s.
	w = e.do("POST", path, "", admin)
	coreWantError(t, w, http.StatusTooManyRequests, "too_many_requests", "")
	if !strings.Contains(w.Body.String(), "wait 10 seconds between DNSSEC tests") {
		t.Fatalf("429 body %s", w.Body)
	}
}

func TestDNSSECTestConflict(t *testing.T) {
	e := newDNSSECEnv(t, silentDNS(t))
	admin := e.provisionAndLogin(t)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.do("POST", "/api/v1/dns/dnssec/test", "", admin) }()
	time.Sleep(300 * time.Millisecond)
	w := e.do("POST", "/api/v1/dns/dnssec/test", "", admin)
	coreWantError(t, w, http.StatusConflict, "conflict", "")
	if !strings.Contains(w.Body.String(), "a DNSSEC test is running") {
		t.Fatalf("409 body %s", w.Body)
	}
	if first := <-done; first.Code != http.StatusOK {
		t.Fatalf("first test: %d %s", first.Code, first.Body)
	}
}

func TestUpstreamsDNSSECView(t *testing.T) {
	s, _ := newUpstreamTestServer(t)
	w := callUpstream(s, s.handleUpstreamList, http.MethodGet, "")
	var got struct {
		Upstreams []upstream.UpstreamStat `json:"upstreams"`
		Cache     upstream.CacheStat      `json:"cache"`
		DNSSEC    *dnssecView             `json:"dnssec"`
	}
	coreDecodeRec(t, w, &got)
	if got.DNSSEC == nil || got.DNSSEC.TimeChecks != "active" || got.DNSSEC.Forwarders == nil || got.Cache.Validation == nil ||
		got.Upstreams[0].DNSSEC != upstream.ProbeUnknown {
		t.Fatalf("validate mode: %s", w.Body)
	}
	if _, err := s.d.Settings.Update(context.Background(), func(a *settings.All) error {
		a.DNS.DNSSECMode = settings.DNSSECPassthrough
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w = callUpstream(s, s.handleUpstreamList, http.MethodGet, "")
	for _, member := range []string{`"dnssec"`, `"validation"`, `"dnssecError"`} {
		if strings.Contains(w.Body.String(), member) {
			t.Fatalf("passthrough mode shows %s: %s", member, w.Body)
		}
	}
}

func TestQueryFilterDNSSECStatus(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/v1/logs/queries?dnssecStatus=secure,bogus&dnssecStatus=INSECURE", nil)
	f, err := queryFilter(r)
	if err != nil || !slices.Equal(f.DNSSECStatus, []string{"secure", "bogus", "insecure"}) {
		t.Fatalf("%v %v", f.DNSSECStatus, err)
	}
	r = httptest.NewRequest("GET", "/api/v1/logs/queries/export?format=csv&dnssecStatus=valid", nil)
	rec := httptest.NewRecorder()
	if _, err := queryFilter(r); err != nil {
		writeError(rec, r, slog.New(slog.DiscardHandler), err)
	}
	coreWantError(t, rec, http.StatusBadRequest, "invalid", "dnssecStatus")
	if n := len(exportCSVHeader); exportCSVHeader[n-1] != "dnssecStatus" || exportCSVHeader[n-2] != "dnsClientId" {
		t.Fatalf("CSV header %v", exportCSVHeader)
	}
}

func TestForwarderValidateRoute(t *testing.T) {
	e := newDNSTestEnv(t)
	rec := e.call(t, e.srv.dnsForwarderCreate, "POST", "/api/v1/dns/forwarders",
		`{"domains":["(unqualified)"],"upstreams":["192.0.2.1"],"enabled":true,"comment":"","validate":true}`)
	coreWantError(t, rec, http.StatusBadRequest, "invalid", "validate")
	rec = e.call(t, e.srv.dnsForwarderCreate, "POST", "/api/v1/dns/forwarders",
		`{"domains":["public.example"],"upstreams":["192.0.2.1"],"enabled":true,"comment":"","validate":true}`)
	dnsExpect(t, rec, http.StatusCreated)
	if !strings.Contains(rec.Body.String(), `"validate":true`) {
		t.Fatalf("created %s", rec.Body)
	}
}

// coreDecodeRec decodes a recorder's body.
func coreDecodeRec(t *testing.T, w *httptest.ResponseRecorder, v any) { coreDecode(t, w, v) }
