package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// listen returns a listener on 127.0.0.1 (closed at the end of the test).
func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func portNum(ln net.Listener) int {
	p, _ := strconv.Atoi(portOf(ln.Addr().String()))
	return p
}

// encApp is an App with the certificate manager of a tlsEnv, the given
// listeners and encrypted DNS configured by fn.
func encApp(t *testing.T, e *tlsEnv, fn func(a *settings.All)) *App {
	t.Helper()
	if fn != nil {
		if _, err := e.set.Update(context.Background(), func(a *settings.All) error { fn(a); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	a := newApp(&config.Config{DataDir: e.dir, DoTListen: []string{":853"}, WebTLSListen: []string{":8443"}}, slog.New(slog.DiscardHandler))
	a.set, a.webTLS = e.set, e.m
	a.ln.failed = map[string]string{}
	return a
}

// The certificate machinery runs while any TLS listener is bound: without
// the HTTPS web listener a DoT listener alone creates the local CA; the
// leaf covers the server name and, while DoT is on, *.<serverName>; a
// change of dns.encrypted re-issues it at once.
func TestCertificateForEncryptedDNS(t *testing.T) {
	e := newTLSEnv(t, "", "")
	a := encApp(t, e, func(s *settings.All) {
		s.DNS.Encrypted = settings.EncryptedDNS{DoT: true, ServerName: "resolver.lan"}
	})
	a.ln.dot = []net.Listener{listen(t)}
	if !a.ln.tlsBound() {
		t.Fatal("a DoT listener is a TLS listener")
	}
	e.m.start()
	leaf := e.served(t).chain[0]
	if !slices.Contains(leaf.DNSNames, "resolver.lan") || !slices.Contains(leaf.DNSNames, "*.resolver.lan") {
		t.Fatalf("leaf names %v", leaf.DNSNames)
	}
	if err := verifyLeaf(leaf, e.m.ca.cert, "phone.resolver.lan"); err != nil {
		t.Fatalf("ClientID name: %v", err)
	}
	st := e.m.Status()
	if !slices.Contains(st.HostsCovered, "resolver.lan") || !slices.Contains(st.HostsCovered, "*.resolver.lan") || st.LocalCA.RenewalNeeded {
		t.Fatalf("covered %v not %v", st.HostsCovered, st.HostsNotCovered)
	}
	// A DoT handshake gets the leaf (ALPN dot); a client that offers only
	// h2 fails; TLS 1.2 is refused while web.tlsMinVersion is 1.3.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", e.m.tlsConfigFor("dot"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	pool := x509.NewCertPool()
	pool.AddCert(e.m.ca.cert)
	c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "kid.resolver.lan", NextProtos: []string{"dot"}})
	if err != nil {
		t.Fatal(err)
	}
	if c.ConnectionState().NegotiatedProtocol != "dot" || !c.ConnectionState().PeerCertificates[0].Equal(leaf) {
		t.Fatal("DoT handshake")
	}
	c.Close()
	if _, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "resolver.lan", NextProtos: []string{"h2"}}); err == nil {
		t.Fatal("ALPN h2 accepted on DoT")
	}
	if _, err := e.set.Update(context.Background(), func(s *settings.All) error { s.Web.TLSMinVersion = "1.3"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "resolver.lan", MaxVersion: tls.VersionTLS12}); err == nil {
		t.Fatal("TLS 1.2 accepted")
	}
	// The web UI keeps working with any SNI.
	web, err := tls.Listen("tcp", "127.0.0.1:0", e.m.tlsConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer web.Close()
	go func() {
		c, err := web.Accept()
		if err == nil {
			_ = c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	c, err = tls.Dial("tcp", web.Addr().String(), &tls.Config{InsecureSkipVerify: true, ServerName: "whatever.example", NextProtos: []string{"h2"}})
	if err != nil || c.ConnectionState().NegotiatedProtocol != "h2" {
		t.Fatalf("web UI: %v", err)
	}
	c.Close()

	// DoT off: the wildcard leaves the leaf at the next tick (no hourly
	// limit for dns.encrypted changes).
	if _, err := e.set.Update(context.Background(), func(s *settings.All) error { s.DNS.Encrypted.DoT = false; s.DNS.Encrypted.DoH = true; return nil }); err != nil {
		t.Fatal(err)
	}
	e.m.tick()
	if leaf := e.served(t).chain[0]; slices.Contains(leaf.DNSNames, "*.resolver.lan") || !slices.Contains(leaf.DNSNames, "resolver.lan") {
		t.Fatalf("after DoT off: %v", leaf.DNSNames)
	}

	// A server name outside the CA's constraints: renewal needed; a new CA
	// covers it.
	if _, err := e.set.Update(context.Background(), func(s *settings.All) error { s.DNS.Encrypted.ServerName = "dns.home.example"; return nil }); err != nil {
		t.Fatal(err)
	}
	e.m.tick()
	st = e.m.Status()
	if !st.LocalCA.RenewalNeeded || !slices.Contains(st.HostsNotCovered, "dns.home.example") {
		t.Fatalf("renewal %v, not covered %v", st.LocalCA.RenewalNeeded, st.HostsNotCovered)
	}
	if _, _, err := e.m.NewLocalCA(); err != nil {
		t.Fatal(err)
	}
	if st := e.m.Status(); st.LocalCA.RenewalNeeded || !slices.Contains(st.HostsCovered, "dns.home.example") {
		t.Fatalf("new CA: %v %v", st.LocalCA.RenewalNeeded, st.HostsNotCovered)
	}

	// Without any TLS listener: nothing is created, the 409 names all three.
	e.m.listener = false
	if _, _, err := e.m.NewLocalCA(); err == nil || err.Error() != api.ErrNoTLSListener {
		t.Fatalf("no TLS listener: %v", err)
	}
}

// A server name equal to (or a parent of) an entry of web.allowedHosts or
// PICACHE_WEB_HOSTS is left out of a new CA's constraints and reported
// not covered, without asking for a new CA.
func TestServerNameExcludedFromCA(t *testing.T) {
	for _, name := range []string{"proxy.example.com", "example.com"} {
		e := newTLSEnv(t, "", "")
		if _, err := e.set.Update(context.Background(), func(s *settings.All) error {
			s.DNS.Encrypted = settings.EncryptedDNS{DoH: true, ServerName: name}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		e.m.start()
		if slices.Contains(e.m.ca.cert.PermittedDNSDomains, name) {
			t.Fatalf("%s permitted: %v", name, e.m.ca.cert.PermittedDNSDomains)
		}
		st := e.m.Status()
		if st.LocalCA.RenewalNeeded || !slices.Contains(st.HostsNotCovered, name) {
			t.Fatalf("%s: renewal %v, not covered %v", name, st.LocalCA.RenewalNeeded, st.HostsNotCovered)
		}
	}
}

func TestEncryptedStatusAndHealth(t *testing.T) {
	e := newTLSEnv(t, "", "")
	a := encApp(t, e, func(s *settings.All) {
		s.DNS.Encrypted = settings.EncryptedDNS{DoT: true, DoH: true, ServerName: "resolver.lan"}
	})
	dot, web := listen(t), listen(t)
	a.ln.dot, a.ln.webTLS = []net.Listener{dot}, []net.Listener{web}
	e.m.listener = true
	e.m.start()
	a.refreshEncrypted()
	snap := a.encrypted()
	if !snap.DoT || !snap.DoH || !slices.Equal(snap.DoTPorts, []uint16{uint16(portNum(dot))}) ||
		!slices.Contains(snap.LeafIPs, netip.MustParseAddr("192.168.1.10")) {
		t.Fatalf("snapshot %+v", snap)
	}
	st := a.EncryptedStatus()
	wantURL := "https://resolver.lan:" + strconv.Itoa(portNum(web)) + "/dns-query"
	if !st.DoT.Serving || st.DoT.Host != "resolver.lan" || st.DoT.Port != portNum(dot) || st.DoT.Error != "" ||
		!slices.Equal(st.DoH.URLs, []string{wantURL}) || len(st.DoH.Listeners) != 1 || st.DoH.Listeners[0].Role != "web-tls" ||
		!st.Certificate.Usable || !st.Certificate.Covered || !st.Certificate.WildcardCovered || st.Certificate.Source != sourceLocalCA ||
		!st.DDR.Active || st.PlainDNS.Served != true {
		t.Fatalf("status %+v", st)
	}
	if s, _, _, show := a.encryptedHealth(); !show || s != "ok" {
		t.Fatalf("health %s", s)
	}

	// Plain DNS off while serving: closed; health ok.
	if _, err := e.set.Update(context.Background(), func(s *settings.All) error { s.DNS.PlainDNS = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if st := a.EncryptedStatus(); st.PlainDNS.Enabled || st.PlainDNS.Served {
		t.Fatalf("plain %+v", st.PlainDNS)
	}

	// DoT without a listener: "PICACHE_DOT_LISTEN is off", or the bind error.
	a.ln.dot = nil
	a.cfg.DoTListen = nil
	a.refreshEncrypted()
	if st := a.EncryptedStatus(); st.DoT.Serving || st.DoT.Error != "PICACHE_DOT_LISTEN is off" || st.DoT.Listeners == nil {
		t.Fatalf("dot %+v", st.DoT)
	}
	a.cfg.DoTListen = []string{":853"}
	a.ln.failed["dot"] = "bind DNS over TLS on :853: address already in use"
	if st := a.EncryptedStatus(); st.DoT.Error != a.ln.failed["dot"] {
		t.Fatalf("dot %+v", st.DoT)
	}
	s, msg, _, _ := a.encryptedHealth()
	if s != "warn" || msg != "DoT is enabled but not serving: bind DNS over TLS on :853: address already in use" {
		t.Fatalf("health %s %q", s, msg)
	}
	// The listeners check counts a failed DoT listener only while DoT is on.
	if s, _, _ := listenersHealth(a.Listeners(), settings.EncryptedDNS{}); s != "ok" {
		t.Fatal("failed DoT listener counted while DoT is off")
	}
	if s, msg, _ := listenersHealth(a.Listeners(), settings.EncryptedDNS{DoT: true}); s != "warn" || !strings.Contains(msg, "dot: bind") {
		t.Fatalf("listeners %s %q", s, msg)
	}

	// Nothing serves while plain DNS is off: fail (fail open).
	a.ln.webTLS = nil
	a.cfg.WebTLSListen = nil
	a.refreshEncrypted()
	st = a.EncryptedStatus()
	if !st.PlainDNS.Served || st.DoH.Error != "no DoH listener: PICACHE_WEB_TLS_LISTEN and PICACHE_DOH_LISTEN are off" {
		t.Fatalf("fail open %+v", st)
	}
	s, msg, _, _ = a.encryptedHealth()
	if s != "fail" || msg != "plain DNS is still served: no encrypted DNS listener is running" {
		t.Fatalf("health %s %q", s, msg)
	}

	// Serving, but the certificate does not cover the server name.
	a.ln.webTLS = []net.Listener{web}
	a.ln.dot = []net.Listener{dot}
	if _, err := e.set.Update(context.Background(), func(s *settings.All) error {
		s.DNS.PlainDNS = true
		s.DNS.Encrypted.ServerName = "other.example.net"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.refreshEncrypted()
	s, msg, _, _ = a.encryptedHealth()
	if s != "warn" || msg != "the certificate does not cover other.example.net: devices refuse encrypted DNS" {
		t.Fatalf("health %s %q", s, msg)
	}
	if st := a.EncryptedStatus(); st.Certificate.Covered || !st.Certificate.LocalCARenewalNeeded {
		t.Fatalf("certificate %+v", st.Certificate)
	}

	// Both off and plain DNS on: no check.
	if _, err := e.set.Update(context.Background(), func(s *settings.All) error {
		s.DNS.Encrypted = settings.EncryptedDNS{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, show := a.encryptedHealth(); show {
		t.Fatal("check shown while everything is off")
	}
	if st := a.EncryptedStatus(); st.DDR.Reason != "off" || st.DDR.Active || len(st.DoH.URLs) != 0 {
		t.Fatalf("ddr %+v", st.DDR)
	}
}

// Refreshes run one at a time: while the tick refreshes the snapshot over
// and over, the snapshot after every settings change (its subscriber
// refreshes before Update returns) carries that change; a refresh that
// read the older settings never overwrites it.
func TestEncryptedRefreshSerialised(t *testing.T) {
	e := newTLSEnv(t, "", "")
	a := encApp(t, e, func(s *settings.All) {
		s.DNS.Encrypted = settings.EncryptedDNS{DoH: true, ServerName: "resolver.lan"}
	})
	a.ln.dot, a.ln.webTLS = []net.Listener{listen(t)}, []net.Listener{listen(t)}
	e.m.start()
	e.set.Subscribe(func(_, _ *settings.All) { a.refreshEncrypted() })
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					a.refreshEncrypted()
				}
			}
		})
	}
	defer func() {
		close(stop)
		wg.Wait()
	}()
	for i := range 100 {
		on := i%2 == 0
		if _, err := e.set.Update(context.Background(), func(s *settings.All) error { s.DNS.Encrypted.DoT = on; return nil }); err != nil {
			t.Fatal(err)
		}
		if got := a.encrypted(); got.DoT != on || !got.DoH {
			t.Fatalf("change %d: snapshot %+v, want DoT %v", i, got, on)
		}
	}
}

// The ERROR of plain DNS failing open is logged once per change of that
// state.
func TestEncryptedFailOpenLogged(t *testing.T) {
	e := newTLSEnv(t, "", "")
	a := encApp(t, e, func(s *settings.All) {
		s.DNS.Encrypted = settings.EncryptedDNS{DoT: true, ServerName: "resolver.lan"}
		s.DNS.PlainDNS = false
	})
	buf := &syncBuffer{}
	a.log = slog.New(slog.NewTextHandler(buf, nil))
	for range 3 {
		a.refreshEncrypted()
	}
	if n := strings.Count(buf.String(), "plain DNS is still served"); n != 1 {
		t.Fatalf("logged %d times", n)
	}
	a.ln.dot = []net.Listener{listen(t)}
	e.m.start()
	a.refreshEncrypted()
	a.ln.dot = nil
	a.refreshEncrypted()
	if n := strings.Count(buf.String(), "plain DNS is still served"); n != 2 {
		t.Fatalf("logged %d times after a change", n)
	}
}

func TestListenersRolesAndClash(t *testing.T) {
	var l listeners
	info := l.info()
	for _, role := range []string{"dns-udp", "dns-tcp", "cache", "sni", "web", "web-tls", "dot", "doh"} {
		if v, ok := info.Bound[role]; !ok || v == nil {
			t.Fatalf("role %s missing or null", role)
		}
	}
	// PICACHE_DOH_LISTEN on the port of the SNI pass-through: the bind
	// error names the clash.
	sni := listen(t)
	port := portOf(sni.Addr().String())
	sni.Close()
	cfg := testConfig(t)
	cfg.DHCP = config.DHCPOff
	cfg.SNIListen = []string{"127.0.0.1:" + port}
	cfg.DoHListen = []string{"127.0.0.1:" + port}
	cfg.DoTListen = []string{"127.0.0.1:0"}
	a := newApp(cfg, slog.New(slog.DiscardHandler))
	if err := a.bindListeners(); err != nil {
		t.Fatal(err)
	}
	defer a.ln.closeAll()
	if msg := a.ln.failed["doh"]; !strings.Contains(msg, "address already in use") {
		t.Logf("the address-in-use error is not recognised on this system: %s", msg)
	} else if !strings.Contains(msg, "is used by the SNI pass-through of the download cache: set PICACHE_SNI_LISTEN=off, give PICACHE_DOH_LISTEN another port, or bind each to its own address") {
		t.Fatalf("doh failure %q", msg)
	}
	if info := a.Listeners(); len(info.Bound["dot"]) != 1 || len(info.Bound["doh"]) != 0 {
		t.Fatalf("bound %v", info.Bound)
	}
}

// The dedicated DoH listener serves only the DoH paths: everything else is
// a bare 404 without cookies; proxy headers are refused.
func TestDedicatedDoHHandler(t *testing.T) {
	e := newTLSEnv(t, "", "")
	a := encApp(t, e, nil)
	h := a.dohHandler()
	for _, path := range []string{"/", "/api/v1/auth/status", "/healthz", "/metrics", "/dns-query", "/dns-query/x"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound || len(w.Result().Cookies()) != 0 || w.Header().Get("Content-Security-Policy") != "" {
			t.Fatalf("%s: %d %v", path, w.Code, w.Header())
		}
	}
	if _, err := e.set.Update(context.Background(), func(s *settings.All) error {
		s.DNS.Encrypted = settings.EncryptedDNS{DoH: true, ServerName: "resolver.lan"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/dns-query", nil)
	r.Header.Set("X-Forwarded-For", "192.168.1.9")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "DoH through a proxy is served on the web listener only") {
		t.Fatalf("proxy: %d %s", w.Code, w.Body)
	}
	if w := httptest.NewRecorder(); true {
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if w.Code != http.StatusNotFound {
			t.Fatal("healthz served")
		}
	}
}
