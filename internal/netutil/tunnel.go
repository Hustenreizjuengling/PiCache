package netutil

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"golang.org/x/net/proxy"
)

// Tunnel is an outbound proxy (settings network.proxy, docs/ARCHITECTURE.md
// 6.1 "Outbound proxy"). SafeDialer resolves and checks a target exactly
// as without a proxy and then, instead of dialing the checked address,
// asks the proxy for a tunnel to that IP literal: an HTTP CONNECT to
// <ip>:<port> or a SOCKS5 CONNECT with the address type IPv4 or IPv6
// (never a host name, never socks5h), so the proxy cannot resolve a name
// to an address the SSRF rules refuse. TLS runs through the tunnel with
// the original host name; plain http targets use the same tunnel (never
// absolute-form requests to the proxy).
type Tunnel struct {
	Scheme   string // http | socks5
	Host     string // the proxy's host name or IP literal
	Port     int
	Username string // Basic (HTTP) or RFC 1929 (SOCKS5) credentials when set
	Password string
	// Resolve resolves the proxy's host name (the host resolver: a proxy
	// on the LAN is found by its name).
	Resolve Resolver
	// Refuse returns why an address of the proxy must not be used ("" if
	// it may): this machine's addresses on PiCache's own listener ports.
	Refuse  func(ip netip.Addr, port int) string
	Timeout time.Duration // per connection attempt and handshake (default 10 s)
}

// maxConnectResponse bounds the header of a proxy's CONNECT answer.
const maxConnectResponse = 16 << 10

// Origin returns scheme://host:port (errors name the proxy by it, never
// by its credentials).
func (t *Tunnel) Origin() string {
	return t.Scheme + "://" + net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
}

// Dial connects to the proxy and opens a tunnel to target.
func (t *Tunnel) Dial(ctx context.Context, target netip.AddrPort) (net.Conn, error) {
	conn, err := t.dial(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("proxy %s: %w", t.Origin(), err)
	}
	return conn, nil
}

func (t *Tunnel) dial(ctx context.Context, target netip.AddrPort) (net.Conn, error) {
	timeout := t.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	addrs, err := t.proxyAddrs(ctx)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, a := range addrs {
		var conn net.Conn
		switch t.Scheme {
		case "http":
			conn, err = t.connect(ctx, a, target, timeout)
		case "socks5":
			conn, err = t.socks5(ctx, a, target, timeout)
		default:
			return nil, errors.New("unsupported proxy scheme")
		}
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

// proxyAddrs resolves the proxy: link-local, multicast and unspecified
// addresses are refused, loopback and private ones allowed, and Refuse
// decides about this machine's own listeners.
func (t *Tunnel) proxyAddrs(ctx context.Context) ([]netip.AddrPort, error) {
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(t.Host); err == nil {
		ips = []netip.Addr{Canon(ip)}
	} else {
		if t.Resolve == nil {
			return nil, errors.New("no resolver for the proxy's name")
		}
		if ips, err = t.Resolve(ctx, t.Host); err != nil {
			return nil, err
		}
	}
	var out []netip.AddrPort
	reason := ""
	for _, ip := range ips {
		ip = Canon(ip)
		if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			reason = "link-local, multicast and unspecified proxy addresses are not allowed"
			continue
		}
		if t.Refuse != nil {
			if why := t.Refuse(ip, t.Port); why != "" {
				reason = why
				continue
			}
		}
		out = append(out, netip.AddrPortFrom(ip, uint16(t.Port)))
	}
	if len(out) == 0 {
		if reason == "" {
			reason = "no address"
		}
		return nil, errors.New(reason)
	}
	return out, nil
}

// connect opens an HTTP CONNECT tunnel: a hand-written request, the
// answer's header at most 16 KiB, only 200 accepted.
func (t *Tunnel) connect(ctx context.Context, proxyAddr, target netip.AddrPort, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", proxyAddr.String())
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)
	req := "CONNECT " + target.String() + " HTTP/1.1\r\nHost: " + target.String() + "\r\n"
	if t.Username != "" {
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(t.Username+":"+t.Password)) + "\r\n"
	}
	req += "\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, err
	}
	lr := &io.LimitedReader{R: conn, N: maxConnectResponse}
	br := bufio.NewReaderSize(lr, 4096)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		if lr.N <= 0 {
			return nil, errors.New("the answer to CONNECT is larger than 16 KiB")
		}
		return nil, fmt.Errorf("reading the answer to CONNECT: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("CONNECT refused: %s", resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	if n := br.Buffered(); n > 0 {
		// Data after the header belongs to the tunnel.
		rest, _ := br.Peek(n)
		return &prefixConn{Conn: conn, prefix: append([]byte(nil), rest...)}, nil
	}
	return conn, nil
}

// socks5 opens a SOCKS5 tunnel to the IP literal (address type IPv4 or
// IPv6; RFC 1929 credentials when a user name is set).
func (t *Tunnel) socks5(ctx context.Context, proxyAddr, target netip.AddrPort, timeout time.Duration) (net.Conn, error) {
	var auth *proxy.Auth
	if t.Username != "" {
		auth = &proxy.Auth{User: t.Username, Password: t.Password}
	}
	fwd := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	d, err := proxy.SOCKS5("tcp", proxyAddr.String(), auth, fwd)
	if err != nil {
		return nil, err
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("SOCKS5 dialer without context")
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return cd.DialContext(cctx, "tcp", target.String())
}

// prefixConn returns prefix before reading from the connection.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}
