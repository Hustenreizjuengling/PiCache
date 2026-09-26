package netutil

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

func webSettings(edit func(*settings.All)) *settings.All {
	s := settings.Defaults()
	if edit != nil {
		edit(&s)
	}
	s.Normalize()
	return &s
}

// The allowed set: this machine always (a non-private own address such as
// 203.0.113.7 included, so `picache healthcheck` against a listener bound
// to it passes); while restricted also the private ranges, the connected
// subnets (a global IPv6 on-link prefix included), dns.allowedNetworks,
// web.allowedNetworks and the trusted proxies; everyone when not
// restricted.
func TestWebACL(t *testing.T) {
	own := []netip.Addr{netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("2001:db8:5::10")}
	connected := []netip.Prefix{netip.MustParsePrefix("2001:db8:5::/64"), netip.MustParsePrefix("198.18.0.0/24")}
	s := webSettings(func(a *settings.All) {
		a.DNS.AllowedNetworks = []string{"198.51.100.0/24"}
		a.Web.AllowedNetworks = []string{"192.0.2.0/24", "2001:db8:77::5"}
		a.Web.TrustedProxies = []string{"203.0.114.9"}
	})
	acl := newWebACL(s, own, connected)
	if !acl.Restricted() {
		t.Fatal("new installations restrict the web UI")
	}
	for addr, want := range map[string]bool{
		"127.0.0.1": true, "127.8.9.10": true, "::1": true, "::ffff:127.0.0.1": true, // loopback
		"203.0.113.7": true, "2001:db8:5::10": true, // own addresses
		"192.168.1.20": true, "10.1.2.3": true, "172.16.9.9": true, "100.64.1.1": true, "fd00::5": true,
		"fe80::1%eth0": true, "169.254.3.4": true, // private ranges, link-local
		"2001:db8:5::99": true, "198.18.0.77": true, // connected subnets (global IPv6 prefix, public IPv4)
		"198.51.100.9": true,                         // dns.allowedNetworks
		"192.0.2.44":   true, "2001:db8:77::5": true, // web.allowedNetworks
		"203.0.114.9": true, // trusted proxy
		"203.0.113.8": false, "8.8.8.8": false, "2001:db8:77::6": false, "2001:db8:6::1": false,
	} {
		if got := acl.Allowed(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Allowed(%s) = %v, want %v", addr, got, want)
		}
	}
	if acl.Allowed(netip.Addr{}) {
		t.Error("an invalid address must be refused while restricted")
	}
	if !acl.TrustedProxy(netip.MustParseAddr("::ffff:203.0.114.9")) || acl.TrustedProxy(netip.MustParseAddr("203.0.114.10")) {
		t.Error("trusted proxy matching")
	}
	// dns.allowAllNetworks and dns.trustConnectedNetworks do not change it.
	s.DNS.AllowAllNetworks, s.DNS.TrustConnectedNetworks = true, true
	if newWebACL(s, own, connected).Allowed(netip.MustParseAddr("8.8.8.8")) {
		t.Error("dns.allowAllNetworks must not open the web UI")
	}
	open := newWebACL(webSettings(func(a *settings.All) { a.Web.RestrictToNetworks = false }), nil, nil)
	for _, addr := range []string{"8.8.8.8", "2a00::1", "127.0.0.1"} {
		if !open.Allowed(netip.MustParseAddr(addr)) {
			t.Errorf("unrestricted: %s refused", addr)
		}
	}
	var nilACL *WebACL
	if nilACL.Allowed(netip.MustParseAddr("127.0.0.1")) || nilACL.TrustedProxy(netip.MustParseAddr("127.0.0.1")) {
		t.Error("a nil ACL allows nothing")
	}
}

// NewWebACL reads this machine's addresses and connected subnets.
func TestNewWebACLUsesInterfaces(t *testing.T) {
	fakeInterfaces(t, "203.0.113.7/24")
	fakeHostAddrs(t, []HostAddr{up("eth0", "203.0.113.7/24"), up("eth0", "2001:db8:5::10/64")})
	acl := NewWebACL(webSettings(nil))
	for addr, want := range map[string]bool{"203.0.113.7": true, "203.0.113.99": true, "2001:db8:5::1": true, "8.8.8.8": false} {
		if got := acl.Allowed(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Allowed(%s) = %v, want %v", addr, got, want)
		}
	}
}

func webAccessEnv(t *testing.T) (*settings.Store, *WebAccess, *bytes.Buffer, *[]netip.Addr) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	st, err := settings.Open(context.Background(), d, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	var logBuf bytes.Buffer
	own := &[]netip.Addr{}
	w := newWebAccess(st, slog.New(slog.NewTextHandler(&logBuf, nil)), func(s *settings.All) *WebACL {
		return newWebACL(s, *own, nil)
	})
	return st, w, &logBuf, own
}

// The ACL follows settings changes at once and interface changes with
// Refresh (the 60 s tick and SIGHUP).
func TestWebAccessFollowsSettingsAndRefresh(t *testing.T) {
	st, w, _, own := webAccessEnv(t)
	pub := netip.MustParseAddr("203.0.113.7")
	if w.Get().Allowed(pub) {
		t.Fatal("public address allowed by default")
	}
	if _, err := st.Update(context.Background(), func(a *settings.All) error {
		a.Web.AllowedNetworks = []string{"203.0.113.0/24"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !w.Get().Allowed(pub) {
		t.Fatal("settings change not applied")
	}
	if _, err := st.Update(context.Background(), func(a *settings.All) error { a.Web.AllowedNetworks = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	*own = []netip.Addr{pub}
	if w.Get().Allowed(pub) {
		t.Fatal("an own address must only count after the refresh")
	}
	w.Refresh()
	if !w.Get().Allowed(pub) {
		t.Fatal("Refresh must read the own addresses again")
	}
}

// A Refresh that is still building from the previous settings when a
// settings change is saved never stores its stale ACL last.
func TestWebAccessRefreshRacesSettingsChange(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	st, err := settings.Open(context.Background(), d, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(context.Background(), func(a *settings.All) error { a.Web.RestrictToNetworks = false; return nil }); err != nil {
		t.Fatal(err)
	}
	var (
		mu      sync.Mutex
		hold    bool
		entered = make(chan struct{})
		release = make(chan struct{})
	)
	w := newWebAccess(st, slog.New(slog.DiscardHandler), func(s *settings.All) *WebACL {
		mu.Lock()
		h := hold
		hold = false
		mu.Unlock()
		if h { // the slow Refresh: interfaces are read while the settings change
			close(entered)
			<-release
		}
		return newWebACL(s, nil, nil)
	})
	mu.Lock()
	hold = true
	mu.Unlock()
	refreshed := make(chan struct{})
	go func() { w.Refresh(); close(refreshed) }()
	<-entered
	updated := make(chan error, 1)
	go func() {
		_, err := st.Update(context.Background(), func(a *settings.All) error { a.Web.RestrictToNetworks = true; return nil })
		updated <- err
	}()
	done := false
	select {
	case err := <-updated: // the subscriber did not wait for the Refresh
		if err != nil {
			t.Fatal(err)
		}
		done = true
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-refreshed
	if !done {
		if err := <-updated; err != nil {
			t.Fatal(err)
		}
	}
	if !w.Get().Restricted() {
		t.Fatal("the stale ACL of the previous settings was stored last")
	}
}

// Refusals are counted always and logged once per address per 10 minutes,
// for at most 256 addresses.
func TestWebAccessRefusalLog(t *testing.T) {
	_, w, logBuf, _ := webAccessEnv(t)
	a := netip.MustParseAddr("203.0.113.7")
	proxy := netip.MustParseAddr("192.168.1.5")
	w.Refuse(a, proxy)
	w.Refuse(a, proxy)
	if n := strings.Count(logBuf.String(), "refused web UI access"); n != 1 || w.Refused() != 2 {
		t.Fatalf("logged %d times, counted %d", n, w.Refused())
	}
	if !strings.Contains(logBuf.String(), "client=203.0.113.7") || !strings.Contains(logBuf.String(), "peer=192.168.1.5") ||
		!strings.Contains(logBuf.String(), "picache web-access --reset") {
		t.Fatalf("log line: %s", logBuf)
	}
	for i := range 300 {
		w.Refuse(netip.AddrFrom4([4]byte{198, 51, byte(i >> 8), byte(i)}), netip.Addr{})
	}
	if n := strings.Count(logBuf.String(), "refused web UI access"); n != webWarnTracked {
		t.Fatalf("logged %d lines, want %d (bounded)", n, webWarnTracked)
	}
	if len(w.lastWarn) != webWarnTracked || w.Refused() != 302 {
		t.Fatalf("tracked %d, counted %d", len(w.lastWarn), w.Refused())
	}
	// An entry older than 10 minutes makes room and is logged again.
	w.mu.Lock()
	w.lastWarn[a] = time.Now().Add(-11 * time.Minute)
	w.mu.Unlock()
	w.Refuse(a, netip.Addr{})
	if n := strings.Count(logBuf.String(), "client=203.0.113.7"); n != 2 {
		t.Fatalf("expired entry logged %d times", n)
	}
}

// Stage 1: a connection whose peer is not allowed is closed right after
// accept and counted; an allowed one is handed on.
func TestWebAccessListener(t *testing.T) {
	st, w, logBuf, _ := webAccessEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wl := w.Listener(ln)
	defer wl.Close()
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := wl.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	c1, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("loopback must always be accepted")
	}
	// Refuse everything: loopback is always allowed, so swap in an ACL
	// that allows nothing.
	w.build = func(*settings.All) *WebACL { return nil }
	w.Refresh()
	_ = st
	c2, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	_ = c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c2.Read(make([]byte, 1)); err == nil {
		t.Fatal("a refused connection must be closed")
	}
	select {
	case <-accepted:
		t.Fatal("a refused connection must not be handed on")
	default:
	}
	if w.Refused() != 1 || !strings.Contains(logBuf.String(), "client=127.0.0.1") || strings.Contains(logBuf.String(), "peer=") {
		t.Fatalf("refused %d, log %s", w.Refused(), logBuf)
	}
}
