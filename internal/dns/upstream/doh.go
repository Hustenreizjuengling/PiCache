package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

const (
	dohMediaType   = "application/dns-message"
	maxDoHBody     = 64 << 10
	dohIdleTimeout = 60 * time.Second
)

// dohTransport is DNS over HTTPS (RFC 8484): POST with ID 0 over HTTP/2
// (HTTP/1.1 if the server does not offer h2). The hostname is resolved via
// the bootstrap servers only; environment proxies are never used.
type dohTransport struct {
	url    string
	tr     *http.Transport
	client *http.Client
}

func newDoH(spec settings.UpstreamSpec, boot *bootstrap, roots *x509.CertPool) *dohTransport {
	d := &bootDialer{boot: boot}
	tr := &http.Transport{
		Proxy:                  nil,
		DialContext:            d.DialContext,
		ForceAttemptHTTP2:      true,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		TLSHandshakeTimeout:    defaultAttemptTimeout,
		MaxIdleConns:           4,
		MaxIdleConnsPerHost:    2,
		IdleConnTimeout:        dohIdleTimeout,
		MaxResponseHeaderBytes: 16 << 10,
		DisableCompression:     true,
	}
	return &dohTransport{
		url: spec.URL,
		tr:  tr,
		client: &http.Client{
			Transport:     tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (t *dohTransport) exchange(ctx context.Context, q *dns.Msg, wire []byte) (*dns.Msg, error) {
	body := bytes.Clone(wire)
	body[0], body[1] = 0, 0 // RFC 8484 4.1: ID 0 for cache friendliness
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", dohMediaType)
	req.Header.Set("Accept", dohMediaType)
	res, err := t.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is already known to the caller
		}
		return nil, ctxErr(ctx, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	if mt, _, err := mime.ParseMediaType(res.Header.Get("Content-Type")); err != nil || mt != dohMediaType {
		return nil, fmt.Errorf("unexpected content type %q", truncate(res.Header.Get("Content-Type"), 64))
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxDoHBody+1))
	if err != nil {
		return nil, ctxErr(ctx, err)
	}
	if len(b) > maxDoHBody {
		return nil, errors.New("response too large")
	}
	m := new(dns.Msg)
	if err := m.Unpack(b); err != nil {
		return nil, fmt.Errorf("malformed reply: %w", err)
	}
	if m.Id != 0 {
		return nil, errIDMismatch
	}
	m.Id = q.Id
	return m, nil
}

func (t *dohTransport) close() { t.tr.CloseIdleConnections() }

// bootDialer dials host:port with the host resolved via the bootstrap
// servers (IP literals are dialled directly).
type bootDialer struct{ boot *bootstrap }

func (d *bootDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return nil, fmt.Errorf("invalid port %q", portStr)
	}
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip}
	} else if addrs, err = d.boot.lookup(ctx, host); err != nil {
		return nil, err
	}
	var nd net.Dialer
	lastErr := errNoAddrs
	for _, a := range addrs {
		c, err := nd.DialContext(ctx, network, netip.AddrPortFrom(a, uint16(port)).String())
		if err == nil {
			return c, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
