package upstream

import (
	"bytes"
	"context"
	"crypto/sha256"
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

// errNoPin fails a TLS handshake whose verified chain has no certificate
// listed in the stamp's hashes.
var errNoPin = errors.New("no certificate of the verified chain matches the DNS stamp's hashes")

// clientTLS returns the TLS configuration of an encrypted upstream: the
// system roots (roots nil) and, for a stamp with certificate hashes, the
// hashes enforced in addition to the normal verification (one
// certificate of the verified chain must have a listed SHA-256 of its TBS
// part; never InsecureSkipVerify).
func clientTLS(serverName string, minVersion uint16, roots *x509.CertPool, pins [][32]byte) *tls.Config {
	c := &tls.Config{ServerName: serverName, MinVersion: minVersion, RootCAs: roots}
	if len(pins) > 0 {
		c.VerifyConnection = func(cs tls.ConnectionState) error {
			for _, chain := range cs.VerifiedChains {
				for _, cert := range chain {
					sum := sha256.Sum256(cert.RawTBSCertificate)
					for _, p := range pins {
						if sum == p {
							return nil
						}
					}
				}
			}
			return errNoPin
		}
	}
	return c
}

// dohTransport is DNS over HTTPS (RFC 8484): POST with ID 0 over HTTP/2
// (HTTP/1.1 if the server does not offer h2). The hostname is resolved via
// the bootstrap servers only (a stamp's address is dialled directly);
// environment proxies are never used.
type dohTransport struct {
	url    string
	tr     *http.Transport
	client *http.Client
}

func newDoH(spec settings.UpstreamSpec, boot *bootstrap, roots *x509.CertPool) *dohTransport {
	d := &bootDialer{boot: boot, fixed: spec.DialAddr}
	tr := &http.Transport{
		Proxy:                  nil,
		DialContext:            d.DialContext,
		ForceAttemptHTTP2:      true,
		TLSClientConfig:        clientTLS("", tls.VersionTLS12, roots, spec.Pins),
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
	return exchangeHTTP(ctx, t.client, t.url, q, wire)
}

func (t *dohTransport) close() { t.tr.CloseIdleConnections() }

// exchangeHTTP posts one query (ID 0) to a DoH URL through client (HTTP/2
// or HTTP/3) and reads the reply: status 200, the DNS media type, at most
// 64 KiB, ID 0 (set back to q.Id).
func exchangeHTTP(ctx context.Context, client *http.Client, u string, q *dns.Msg, wire []byte) (*dns.Msg, error) {
	body := bytes.Clone(wire)
	body[0], body[1] = 0, 0 // RFC 8484 4.1: ID 0 for cache friendliness
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", dohMediaType)
	req.Header.Set("Accept", dohMediaType)
	res, err := client.Do(req)
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

// bootDialer dials host:port with the host resolved via the bootstrap
// servers (IP literals are dialled directly). With fixed (a DNS stamp's
// address) that address is dialled whatever the host is.
type bootDialer struct {
	boot  *bootstrap
	fixed netip.AddrPort
}

// addrs returns the addresses and the port to dial for address.
func (d *bootDialer) addrs(ctx context.Context, address string) ([]netip.Addr, uint16, error) {
	if d.fixed.IsValid() {
		return []netip.Addr{d.fixed.Addr()}, d.fixed.Port(), nil
	}
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, 0, err
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return nil, 0, fmt.Errorf("invalid port %q", portStr)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip}, uint16(port), nil
	}
	addrs, err := d.boot.lookup(ctx, host)
	return addrs, uint16(port), err
}

func (d *bootDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	addrs, port, err := d.addrs(ctx, address)
	if err != nil {
		return nil, err
	}
	var nd net.Dialer
	lastErr := errNoAddrs
	for _, a := range addrs {
		c, err := nd.DialContext(ctx, network, netip.AddrPortFrom(a, port).String())
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
