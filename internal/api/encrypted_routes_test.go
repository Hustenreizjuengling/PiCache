package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/clients"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// fakeEncrypted is the app's view of encrypted DNS in the tests.
type fakeEncrypted struct {
	mu    sync.Mutex
	st    EncryptedDNSStatus
	addrs []netip.Addr
}

func (f *fakeEncrypted) EncryptedStatus() EncryptedDNSStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *fakeEncrypted) ProfileAddresses() []netip.Addr { return f.addrs }

// logBuffer captures the API's log lines.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// encEnv is an API server with a real DNS server (DoH on the web
// listeners), encrypted DNS on (server name dns.lan) and a captured log.
type encEnv struct {
	*coreEnv
	enc     *fakeEncrypted
	reg     *clients.Registry
	log     *logBuffer
	session string
}

func newEncEnv(t *testing.T) *encEnv {
	t.Helper()
	ce := newCoreEnv(t)
	ctx := context.Background()
	logs := &logBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	reg, err := clients.New(ctx, ce.db, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ce.set.Update(ctx, func(a *settings.All) error {
		a.DNS.Encrypted = settings.EncryptedDNS{DoT: true, DoH: true, ServerName: "dns.lan"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	d, err := dnsserver.New(ctx, dnsserver.Deps{DB: ce.db, Settings: ce.set, Clients: reg, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	enc := &fakeEncrypted{st: EncryptedDNSStatus{ServerName: "dns.lan",
		DoT: EncryptedDoT{Enabled: true, Serving: true, Listeners: []string{"0.0.0.0:853"}},
		DoH: EncryptedDoH{Enabled: true, Serving: true}}, addrs: []netip.Addr{netip.MustParseAddr("192.168.1.2")}}
	ce.rt.tlsAddr = "0.0.0.0:8443"
	ce.rt.bound = map[string][]string{"dot": {"0.0.0.0:853"}}
	ce.srv = New(Deps{Config: ce.srv.d.Config, Settings: ce.set, Auth: ce.auth, Runtime: ce.rt, Updates: ce.upd,
		DNS: d, Clients: reg, Encrypted: enc, Log: log})
	e := &encEnv{coreEnv: ce, enc: enc, reg: reg, log: logs}
	e.session = e.provisionAndLogin(t)
	return e
}

// dohRequest builds a DoH POST for name from peer.
func dohRequest(t *testing.T, path, peer, name string, https bool) *http.Request {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	m.Id = 0
	b, _ := m.Pack()
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	r.Host = coreHost
	r.RemoteAddr = peer
	r.Header.Set("Content-Type", "application/dns-message")
	if https {
		r.TLS = &tls.ConnectionState{}
	}
	return r
}

func TestWebDoH(t *testing.T) {
	e := newEncEnv(t)
	lan := "192.168.1.44:50000"

	// Over HTTPS: answered (the server name locally at step 6).
	w := e.serve(dohRequest(t, "/dns-query", lan, "dns.lan", true))
	m := new(dns.Msg)
	if w.Code != http.StatusOK || m.Unpack(w.Body.Bytes()) != nil || len(m.Answer) == 0 {
		t.Fatalf("https: %d %s", w.Code, w.Body)
	}
	// Host: <serverName> passes the host allowlist.
	r := dohRequest(t, "/dns-query/phone", lan, "dns.lan", true)
	r.Host = "dns.lan:8443"
	if w := e.serve(r); w.Code != http.StatusOK {
		t.Fatalf("Host dns.lan: %d %s", w.Code, w.Body)
	}
	// Names below the server name are no allowed hosts.
	r = dohRequest(t, "/dns-query", lan, "dns.lan", true)
	r.Host = "phone.dns.lan"
	if w := e.serve(r); w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("Host phone.dns.lan: %d", w.Code)
	}

	// Plain HTTP without a trusted proxy: 404, never a redirect.
	if _, err := e.set.Update(context.Background(), func(a *settings.All) error { a.Web.RedirectToHTTPS = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if w := e.serve(dohRequest(t, "/dns-query", lan, "dns.lan", false)); w.Code != http.StatusNotFound {
		t.Fatalf("plain http: %d %v", w.Code, w.Header())
	}

	// An untrusted peer (also loopback) with proxy headers: 403.
	for _, peer := range []string{lan, "127.0.0.1:50000"} {
		for _, h := range []string{"X-Forwarded-For", "Forwarded", "X-Real-IP"} {
			r := dohRequest(t, "/dns-query", peer, "dns.lan", true)
			r.Header.Set(h, "192.168.1.99")
			w := e.serve(r)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not in web.trustedProxies") {
				t.Fatalf("%s via %s: %d %s", h, peer, w.Code, w.Body)
			}
		}
	}

	// A trusted proxy that terminates TLS: answered for the effective
	// client (its DNS ACL).
	if _, err := e.set.Update(context.Background(), func(a *settings.All) error {
		a.Web.TrustedProxies = []string{"192.168.1.5"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r = dohRequest(t, "/dns-query", "192.168.1.5:40000", "dns.lan", false)
	r.Header.Set("X-Forwarded-For", "192.168.1.77")
	r.Header.Set("X-Forwarded-Proto", "https")
	if w := e.serve(r); w.Code != http.StatusOK {
		t.Fatalf("trusted proxy: %d %s", w.Code, w.Body)
	}
	r = dohRequest(t, "/dns-query", "192.168.1.5:40000", "dns.lan", false)
	r.Header.Set("X-Forwarded-For", "198.51.100.7") // outside the DNS ACL
	r.Header.Set("X-Forwarded-Proto", "https")
	if w := e.serve(r); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "may not use DNS") {
		t.Fatalf("effective client outside the DNS ACL: %d %s", w.Code, w.Body)
	}

	// The web ACL allows everyone (restrictToNetworks off), the DNS ACL
	// refuses a public peer.
	if w := e.serve(dohRequest(t, "/dns-query", "198.51.100.7:1234", "dns.lan", true)); w.Code != http.StatusForbidden {
		t.Fatalf("DNS ACL: %d", w.Code)
	}

	// DoH off: 404.
	if _, err := e.set.Update(context.Background(), func(a *settings.All) error { a.DNS.Encrypted.DoH = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if w := e.serve(dohRequest(t, "/dns-query", lan, "dns.lan", true)); w.Code != http.StatusNotFound {
		t.Fatalf("DoH off: %d", w.Code)
	}
}

func TestEncryptedStatusRoute(t *testing.T) {
	e := newEncEnv(t)
	w := e.do("GET", "/api/v1/dns/encrypted", "", e.session)
	var st EncryptedDNSStatus
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	coreDecode(t, w, &st)
	if st.ServerName != "dns.lan" || !st.DoT.Serving {
		t.Fatalf("%+v", st)
	}
	// dot.port is absent without a port (encoding/json/v2 keeps a zero
	// number under omitempty).
	if strings.Contains(w.Body.String(), `"port"`) {
		t.Fatalf("dot.port without a DoT port: %s", w.Body)
	}
	e.enc.mu.Lock()
	e.enc.st.DoT.Port = 853
	e.enc.mu.Unlock()
	if w := e.do("GET", "/api/v1/dns/encrypted", "", e.session); !strings.Contains(w.Body.String(), `"port":853`) {
		t.Fatalf("dot.port missing: %s", w.Body)
	}
	e.srv.d.Encrypted = nil
	coreWantError(t, e.do("GET", "/api/v1/dns/encrypted", "", e.session), http.StatusServiceUnavailable, "unavailable", "")
}

// TestEncryptedSettingsRules: the plain-DNS rule on PUT /settings and
// PATCH /settings/dns in both directions, the runtime listener rule and
// the restore warning. The rules of settings.Validate win over the
// runtime rules (their messages are the contract's).
func TestEncryptedSettingsRules(t *testing.T) {
	e := newEncEnv(t)
	patch := func(body string) *httptest.ResponseRecorder {
		return e.do("PATCH", "/api/v1/settings/dns", body, e.session)
	}
	wantMessage := func(w *httptest.ResponseRecorder, field, msg string) {
		t.Helper()
		var b errorBody
		coreWantError(t, w, 400, "invalid", field)
		coreDecode(t, w, &b)
		if b.Error.Message != msg {
			t.Fatalf("message %q, want %q", b.Error.Message, msg)
		}
	}
	const plainRule = "plain DNS can only be switched off while DoT or DoH is enabled"
	// Plain DNS off while DoT is on and its listener is bound: accepted.
	if w := patch(`{"plainDns":false}`); w.Code != 200 {
		t.Fatalf("plain off: %d %s", w.Code, w.Body)
	}
	// Switching the last protocol off while plain DNS is off: refused with
	// the rule of settings.Validate, not the runtime listener rule.
	wantMessage(patch(`{"encrypted":{"dot":false,"doh":false,"serverName":"dns.lan"}}`), "dns.plainDns", plainRule)
	// PUT /settings is judged the same way.
	cur := e.set.Get().Clone()
	cur.DNS.Encrypted.DoT, cur.DNS.Encrypted.DoH = false, false
	body, _ := jsonString(cur)
	wantMessage(e.do("PUT", "/api/v1/settings", body, e.session), "dns.plainDns", plainRule)
	// Plain DNS switched off while both protocols are off: the same.
	if w := patch(`{"plainDns":true,"encrypted":{"dot":false,"doh":false,"serverName":"dns.lan"}}`); w.Code != 200 {
		t.Fatalf("both off: %d %s", w.Code, w.Body)
	}
	wantMessage(patch(`{"plainDns":false}`), "dns.plainDns", plainRule)
	cur = e.set.Get().Clone()
	cur.DNS.PlainDNS = false
	body, _ = jsonString(cur)
	wantMessage(e.do("PUT", "/api/v1/settings", body, e.session), "dns.plainDns", plainRule)
	if w := patch(`{"encrypted":{"dot":true,"doh":true,"serverName":"dns.lan"}}`); w.Code != 200 {
		t.Fatalf("both on: %d %s", w.Code, w.Body)
	}

	// Only DoH on and no DoH listener bound: refused by the runtime rule.
	if w := patch(`{"plainDns":true}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	e.rt.mu.Lock()
	e.rt.tlsAddr, e.rt.bound = "", nil
	e.rt.mu.Unlock()
	w := patch(`{"plainDns":false,"encrypted":{"dot":false,"doh":true,"serverName":"dns.lan"}}`)
	coreWantError(t, w, 400, "invalid", "dns.plainDns")
	if !strings.Contains(w.Body.String(), "and its listener is running") {
		t.Fatal(w.Body)
	}
	// The local domain is refused by settings.Validate, a search domain or a
	// parent of one by the API; a name Validate refuses gets its message
	// even when it is also a search domain.
	coreWantError(t, patch(`{"encrypted":{"dot":true,"doh":false,"serverName":"lan"}}`), 400, "invalid", "dns.encrypted.serverName")
	e.srv.searchDomains = func() []string { return []string{"lan", "corp.example.com"} }
	wantMessage(patch(`{"encrypted":{"dot":true,"doh":false,"serverName":"lan"}}`), "dns.encrypted.serverName",
		"must be a host name with at least two labels, e.g. dns.example.com")
	for _, name := range []string{"corp.example.com", "example.com"} {
		w := patch(`{"encrypted":{"dot":true,"doh":false,"serverName":"` + name + `"}}`)
		coreWantError(t, w, 400, "invalid", "dns.encrypted.serverName")
		if !strings.Contains(w.Body.String(), "must not be a search domain or a parent of one") {
			t.Fatal(w.Body)
		}
	}
	if w := patch(`{"encrypted":{"dot":true,"doh":false,"serverName":"dns.corp.example.com"}}`); w.Code != 200 {
		t.Fatalf("below a search domain: %d %s", w.Code, w.Body)
	}

	// Restore: staged settings with plain DNS off and no listener → warning.
	staged := settings.Defaults()
	staged.DNS.PlainDNS = false
	staged.DNS.Encrypted = settings.EncryptedDNS{DoT: true, ServerName: "dns.lan"}
	e.rt.mu.Lock()
	e.rt.staged = &staged
	e.rt.mu.Unlock()
	r := coreRequest("POST", "/api/v1/system/restore", "")
	r.Body = io.NopCloser(strings.NewReader("backup"))
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set(restorePasswordHeader, corePassword)
	r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: e.session})
	w = e.serve(r)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"dnsWarning":"After the restart the restored settings turn plain DNS off`) {
		t.Fatalf("restore: %d %s", w.Code, w.Body)
	}
	e.rt.mu.Lock()
	e.rt.bound = map[string][]string{"dot": {"0.0.0.0:853"}}
	e.rt.mu.Unlock()
	r = coreRequest("POST", "/api/v1/system/restore", "")
	r.Body = io.NopCloser(strings.NewReader("backup"))
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set(restorePasswordHeader, corePassword)
	r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: e.session})
	if w := e.serve(r); strings.Contains(w.Body.String(), "dnsWarning") {
		t.Fatalf("warning with a DoT listener: %s", w.Body)
	}
}

// --- profiles ---

type plistNode struct {
	XMLName xml.Name
	Content string      `xml:",chardata"`
	Nodes   []plistNode `xml:",any"`
}

// profileStrings returns the texts of a profile, parsed back with
// encoding/xml (so it is well-formed).
func profileStrings(t *testing.T, b []byte) []string {
	t.Helper()
	var root plistNode
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = true
	if err := d.Decode(&root); err != nil {
		t.Fatalf("profile XML: %v\n%s", err, b)
	}
	var out []string
	var walk func(n plistNode)
	walk = func(n plistNode) {
		if n.XMLName.Local == "string" || n.XMLName.Local == "key" || n.XMLName.Local == "integer" {
			out = append(out, n.Content)
		}
		for _, c := range n.Nodes {
			walk(c)
		}
	}
	walk(root)
	return out
}

func TestBuildProfile(t *testing.T) {
	o := ProfileOptions{Protocol: "doh", DNSClientID: "Kid-1", SSIDs: []string{`Home <&>"'`, "Büro"}, Addresses: true}
	if err := o.normalize(); err != nil {
		t.Fatal(err)
	}
	tg := profileTarget{serverName: "dns.lan", dohPort: 8443, instance: "picache-0123456789ab",
		addrs: []netip.Addr{netip.MustParseAddr("192.168.1.2"), netip.MustParseAddr("fd00::2")}}
	b := buildProfile(o, tg)
	s := strings.Join(profileStrings(t, b), "\n")
	for _, want := range []string{
		"https://dns.lan:8443/dns-query/kid-1", `Home <&>"'`, "Büro", "com.apple.dnsSettings.managed",
		"org.picache.dns.01234567.doh.kid-1", "org.picache.dns.01234567.doh.kid-1.dns", "PiCache DNS (DoH) – kid-1",
		"Sends the DNS queries of this device to PiCache (dns.lan) over DNS over HTTPS", "192.168.1.2", "fd00::2",
		"SSIDMatch", "Disconnect", "OnDemandEnabled", "System", "Configuration",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in\n%s", want, s)
		}
	}
	if !bytes.Contains(b, []byte("Home &lt;&amp;&gt;&#34;&#39;")) {
		t.Fatalf("SSID not escaped:\n%s", b)
	}
	// Stable identifiers and UUIDs; the UUID has version and variant bits.
	if !bytes.Equal(b, buildProfile(o, tg)) {
		t.Fatal("profile not stable")
	}
	u := profileUUID("org.picache.dns.01234567.doh.kid-1")
	if len(u) != 36 || u[14] != '8' || strings.IndexByte("89AB", u[19]) < 0 || u != strings.ToUpper(u) {
		t.Fatalf("UUID %s", u)
	}
	// DoT without SSIDs: everywhere; port 443 omitted for DoH.
	b = buildProfile(ProfileOptions{Protocol: "dot"}, profileTarget{serverName: "dns.lan", instance: "x"})
	s = strings.Join(profileStrings(t, b), "\n")
	if !strings.Contains(s, "TLS\nServerName\ndns.lan") || strings.Contains(s, "SSIDMatch") || !strings.Contains(s, "Action\nConnect") ||
		strings.Contains(s, "ServerAddresses") || !strings.Contains(s, "org.picache.dns.x.dot") {
		t.Fatalf("dot profile:\n%s", s)
	}
	if got := profileURL("dns.lan", 443, ""); got != "https://dns.lan/dns-query" {
		t.Fatal(got)
	}
	if profileFilename(ProfileOptions{Protocol: "dot", DNSClientID: "tv"}) != "picache-dot-tv.mobileconfig" {
		t.Fatal("file name")
	}
	for _, bad := range []ProfileOptions{
		{Protocol: "https"},
		{Protocol: "doh", DNSClientID: "-x"},
		{Protocol: "doh", SSIDs: []string{""}},
		{Protocol: "doh", SSIDs: []string{strings.Repeat("x", 33)}},
		{Protocol: "doh", SSIDs: []string{"a\nb"}},
		{Protocol: "doh", SSIDs: make([]string, 17)},
	} {
		if err := bad.normalize(); err == nil {
			t.Fatalf("%+v accepted", bad)
		}
	}
}
