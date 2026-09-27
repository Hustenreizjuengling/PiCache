package netutil

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProxy is an HTTP CONNECT or SOCKS5 proxy that records the targets it
// is asked for and connects every tunnel to backend.
type fakeProxy struct {
	ln      net.Listener
	backend string
	socks   bool
	status  string // CONNECT answer (default 200)

	mu      sync.Mutex
	targets []string // CONNECT targets, or SOCKS5 "<atyp>|<addr>"
	auth    []string // Proxy-Authorization values or SOCKS5 user:password
	conns   int
}

func newFakeProxy(t *testing.T, backend string, socks bool) *fakeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProxy{ln: ln, backend: backend, socks: socks}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.conns++
			p.mu.Unlock()
			go p.serve(c)
		}
	}()
	return p
}

func (p *fakeProxy) port() int { return p.ln.Addr().(*net.TCPAddr).Port }

func (p *fakeProxy) record(target, auth string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.targets = append(p.targets, target)
	if auth != "" {
		p.auth = append(p.auth, auth)
	}
}

func (p *fakeProxy) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	if p.socks {
		if !p.socksHandshake(c, br) {
			return
		}
	} else {
		req, err := http.ReadRequest(br)
		if err != nil || req.Method != http.MethodConnect {
			return
		}
		p.record(req.RequestURI, req.Header.Get("Proxy-Authorization"))
		status := p.status
		if status == "" {
			status = "200 Connection established"
		}
		io.WriteString(c, "HTTP/1.1 "+status+"\r\n\r\n")
		if !strings.HasPrefix(status, "200") {
			return
		}
	}
	b, err := net.Dial("tcp", p.backend)
	if err != nil {
		return
	}
	defer b.Close()
	go io.Copy(b, br)
	io.Copy(c, b)
}

func (p *fakeProxy) socksHandshake(c net.Conn, br *bufio.Reader) bool {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil || hdr[0] != 5 {
		return false
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(br, methods); err != nil {
		return false
	}
	auth := ""
	if strings.Contains(string(methods), "\x02") {
		c.Write([]byte{5, 2})
		v := make([]byte, 2)
		io.ReadFull(br, v)
		user := make([]byte, v[1])
		io.ReadFull(br, user)
		l := make([]byte, 1)
		io.ReadFull(br, l)
		pw := make([]byte, l[0])
		io.ReadFull(br, pw)
		auth = string(user) + ":" + string(pw)
		c.Write([]byte{1, 0})
	} else {
		c.Write([]byte{5, 0})
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil {
		return false
	}
	var addr string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		io.ReadFull(br, b)
		addr = "ipv4|" + netip.AddrFrom4([4]byte(b)).String()
	case 4:
		b := make([]byte, 16)
		io.ReadFull(br, b)
		addr = "ipv6|" + netip.AddrFrom16([16]byte(b)).String()
	case 3:
		l := make([]byte, 1)
		io.ReadFull(br, l)
		b := make([]byte, l[0])
		io.ReadFull(br, b)
		addr = "domain|" + string(b)
	}
	port := make([]byte, 2)
	io.ReadFull(br, port)
	p.record(addr+"|"+strconv.Itoa(int(binary.BigEndian.Uint16(port))), auth)
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	return true
}

func (p *fakeProxy) seen() ([]string, []string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...), append([]string(nil), p.auth...), p.conns
}

// A fetch through the proxy tunnels to the checked IP literal; TLS keeps
// the original host name; names and redirect targets that resolve
// privately are refused before the proxy is asked.
func TestProxyTunnel(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello "+r.Host)
	}))
	defer backend.Close()
	roots := backend.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	public := netip.MustParseAddr("93.184.216.34")
	resolve := func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "example.com":
			return []netip.Addr{public}, nil
		case "internal.example.com":
			return []netip.Addr{netip.MustParseAddr("192.168.1.10")}, nil
		case "proxy.lan":
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return nil, errors.New("no such host")
	}
	for _, socks := range []bool{false, true} {
		name := "http"
		if socks {
			name = "socks5"
		}
		t.Run(name, func(t *testing.T) {
			fp := newFakeProxy(t, backend.Listener.Addr().String(), socks)
			tun := &Tunnel{Scheme: name, Host: "proxy.lan", Port: fp.port(), Username: "joe", Password: "s3cret", Resolve: resolve}
			d := &SafeDialer{Resolve: resolve, OwnAddrs: func() []netip.Addr { return nil },
				Proxy: func(context.Context) *Tunnel { return tun }}
			client := &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: d.DialContext,
				TLSClientConfig: &tls.Config{RootCAs: roots}}, Timeout: 10 * time.Second}
			resp, err := client.Get("https://example.com/x")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(body) != "hello example.com" {
				t.Fatalf("body %q", body)
			}
			targets, auth, _ := fp.seen()
			want := "93.184.216.34:443"
			if socks {
				want = "ipv4|93.184.216.34|443"
			}
			if len(targets) != 1 || targets[0] != want {
				t.Fatalf("proxy targets %v, want [%s]", targets, want)
			}
			wantAuth := "Basic am9lOnMzY3JldA=="
			if socks {
				wantAuth = "joe:s3cret"
			}
			if len(auth) != 1 || auth[0] != wantAuth {
				t.Fatalf("proxy credentials %v", auth)
			}
			// Refused before the proxy is asked: a name that resolves
			// privately, a private IP literal (a redirect hop is dialled the
			// same way), loopback.
			for _, u := range []string{"https://internal.example.com/", "http://10.0.0.1/", "https://127.0.0.1/"} {
				if _, err := client.Get(u); err == nil || !strings.Contains(err.Error(), ErrForbiddenDestination.Error()) {
					t.Fatalf("%s: %v", u, err)
				}
			}
			if targets, _, conns := fp.seen(); len(targets) != 1 || conns != 1 {
				t.Fatalf("the proxy was asked for refused targets: %v (%d connections)", targets, conns)
			}
		})
	}
}

func TestProxyTunnelErrors(t *testing.T) {
	fp := newFakeProxy(t, "127.0.0.1:1", false)
	fp.status = "407 Proxy Authentication Required"
	tun := &Tunnel{Scheme: "http", Host: "127.0.0.1", Port: fp.port(), Username: "joe", Password: "topsecret"}
	_, err := tun.Dial(context.Background(), netip.MustParseAddrPort("93.184.216.34:443"))
	if err == nil || !strings.Contains(err.Error(), "proxy http://127.0.0.1:"+strconv.Itoa(fp.port())) ||
		!strings.Contains(err.Error(), "407") || strings.Contains(err.Error(), "topsecret") || strings.Contains(err.Error(), "joe") {
		t.Fatalf("error %v", err)
	}
	// The proxy's own address: link-local refused, this machine's own
	// listener refused.
	for _, tc := range []struct {
		host   string
		refuse func(netip.Addr, int) string
		want   string
	}{
		{"169.254.169.254", nil, "link-local"},
		{"127.0.0.1", func(netip.Addr, int) string { return "this is PiCache's own web listener" }, "own web listener"},
	} {
		tun := &Tunnel{Scheme: "socks5", Host: tc.host, Port: 8080, Refuse: tc.refuse}
		if _, err := tun.Dial(context.Background(), netip.MustParseAddrPort("93.184.216.34:443")); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v", tc.host, err)
		}
	}
	// An oversized CONNECT answer.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		bufio.NewReader(c).ReadString('\n')
		io.WriteString(c, "HTTP/1.1 200 OK\r\nX: "+strings.Repeat("a", 20<<10)+"\r\n\r\n")
	}()
	tun = &Tunnel{Scheme: "http", Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port}
	if _, err := tun.Dial(context.Background(), netip.MustParseAddrPort("93.184.216.34:443")); err == nil {
		t.Fatal("an oversized CONNECT answer was accepted")
	}
}
