package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

// capture runs fn with stdout and stderr redirected and returns both.
func capture(t *testing.T, fn func() int) (code int, stdout, stderr string) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	outC, errC := make(chan string), make(chan string)
	go func() { b, _ := io.ReadAll(outR); outC <- string(b) }()
	go func() { b, _ := io.ReadAll(errR); errC <- string(b) }()
	defer func() {
		os.Stdout, os.Stderr = oldOut, oldErr
	}()
	code = fn()
	outW.Close()
	errW.Close()
	return code, <-outC, <-errC
}

func TestHelpAndVersionFlags(t *testing.T) {
	t.Setenv("PICACHE_ENV_FILE", "")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"help"}, "usage: picache"},
		{[]string{"--help"}, "usage: picache"},
		{[]string{"-help"}, "usage: picache"},
		{[]string{"-h"}, "usage: picache"},
		{[]string{"serve", "-h"}, "usage: picache"},
		{[]string{"version"}, "picache "},
		{[]string{"--version"}, "picache "},
		{[]string{"-version"}, "picache "},
	} {
		code, out, errOut := capture(t, func() int { return run(tc.args) })
		if code != 0 || !strings.HasPrefix(out, tc.want) {
			t.Errorf("picache %s: exit %d, stdout %q, stderr %q", strings.Join(tc.args, " "), code, out, errOut)
		}
	}
	// Other flags still belong to serve.
	if code, _, errOut := capture(t, func() int { return run([]string{"--no-such-flag"}) }); code != 2 ||
		!strings.Contains(errOut, "no-such-flag") {
		t.Errorf("unknown serve flag: exit %d, stderr %q", code, errOut)
	}
}

func TestStorageRemoveUsage(t *testing.T) {
	t.Setenv("PICACHE_ENV_FILE", "")
	t.Setenv("PICACHE_DATA_DIR", t.TempDir()) // no database: remove does not need one
	for _, args := range [][]string{{"storage"}, {"storage", "remove"}, {"storage", "remove", "a", "b"}} {
		if code, _, errOut := capture(t, func() int { return run(args) }); code != 2 || !strings.Contains(errOut, "storage remove <target-id>") {
			t.Errorf("picache %s: exit %d, stderr %q", strings.Join(args, " "), code, errOut)
		}
	}
	code, _, errOut := capture(t, func() int { return run([]string{"storage", "remove", "not-an-id"}) })
	if code != 1 || !strings.HasPrefix(errOut, "storage remove:") {
		t.Errorf("storage remove not-an-id: exit %d, stderr %q", code, errOut)
	}
}

// localDNS answers "localhost." like PiCache and returns its address.
func localDNS(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg).SetReply(r)
		m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET},
			A: net.IPv4(127, 0, 0, 1)}}
		_ = w.WriteMsg(m)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().String()
}

// With plain HTTP off, the healthcheck talks to PiCache's own HTTPS
// listener, whose certificate is self-signed, also when that listener is
// bound to a specific or IPv6 address.
func TestHealthcheckOwnTLSListenerOnSpecificAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = io.WriteString(w, "ok")
		}
	}))
	srv.Listener = ln
	srv.StartTLS()
	defer srv.Close()

	t.Setenv("PICACHE_WEB_LISTEN", "off")
	t.Setenv("PICACHE_WEB_TLS_LISTEN", ln.Addr().String())
	t.Setenv("PICACHE_DNS_LISTEN", localDNS(t))
	if code, _, errOut := capture(t, func() int { return healthcheck(nil) }); code != 0 {
		t.Fatalf("healthcheck: exit %d: %s", code, errOut)
	}
	// An explicit loopback URL is not verified either.
	if code, _, errOut := capture(t, func() int { return healthcheck([]string{srv.URL + "/healthz"}) }); code != 0 {
		t.Fatalf("healthcheck %s: exit %d: %s", srv.URL, code, errOut)
	}
}

func TestIsLoopbackURL(t *testing.T) {
	for url, want := range map[string]bool{
		"https://127.0.0.1:8443/healthz": true,
		"https://127.1.2.3/healthz":      true,
		"https://[::1]:8443/healthz":     true,
		"https://localhost:8443/":        true,
		"https://192.168.1.5:8443/":      false,
		"https://picache.lan/healthz":    false,
		"https://127.0.0.1.example.com/": false,
	} {
		if got := isLoopbackURL(url); got != want {
			t.Errorf("isLoopbackURL(%q) = %v", url, got)
		}
	}
}
