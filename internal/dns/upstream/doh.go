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
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

const (
	dohMediaType   = "application/dns-message"
	maxDoHBody     = 64 << 10
	dohIdleTimeout = 60 * time.Second
	// dohMaxConns bounds the connections (dials in progress included) per
	// DoH upstream: further requests wait for one of them instead of
	// dialling on their own (HTTP/2 multiplexes the queries on one).
	dohMaxConns = 4
	// An HTTP/2 connection that received no frame for dohPingAfter is
	// pinged and closed when the ping gets no answer within
	// dohPingTimeout, so a silently dead connection (lost NAT state, a
	// router reboot) is replaced within about 15 s.
	dohPingAfter   = 10 * time.Second
	dohPingTimeout = 5 * time.Second
)

// errNoPin fails a TLS handshake whose verified chain has no certificate
// listed in the stamp's hashes.
var errNoPin = errors.New("no certificate of the verified chain matches the DNS stamp's hashes")

// cutHandshake reports whether err ended a TLS handshake that the server
// cut by closing or resetting the connection (dns.quad9.net does so to a
// share of new connections, on its DoH and DoT ports). Certificate and
// verification errors, alerts, timeouts and refused connections are not.
func cutHandshake(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, errConnReset)
}

// cutHandshakeError marks the error of a TLS handshake the server cut
// (cutHandshake).
type cutHandshakeError struct{ error }

func (e cutHandshakeError) Unwrap() error { return e.error }

// encOpts are the TLS settings every encrypted transport shares: the roots
// of the certificate checks (nil: the system roots) and their clock
// (Resolver.certTime; nil: time.Now).
type encOpts struct {
	roots *x509.CertPool
	now   func() time.Time
}

// clientTLS returns the TLS configuration of an encrypted upstream: the
// roots and the clock of enc and, for a stamp with certificate hashes,
// the hashes enforced in addition to the normal verification (one
// certificate of the verified chain must have a listed SHA-256 of its TBS
// part; never InsecureSkipVerify).
func clientTLS(serverName string, minVersion uint16, enc encOpts, pins [][32]byte) *tls.Config {
	c := &tls.Config{ServerName: serverName, MinVersion: minVersion, RootCAs: enc.roots, Time: enc.now}
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
// environment proxies are never used. At most dohMaxConns connections per
// upstream, each dial bounded by the attempt timeout, and the HTTP/2
// health check of dohPingAfter and dohPingTimeout. Queries are padded
// (padQuery).
type dohTransport struct {
	url    string
	tr     *http.Transport
	client *http.Client
}

// dohTuning are the DoH knobs tests shorten: the bound of one dial (all
// addresses; 0: the attempt timeout), the HTTP/2 health check and the
// dial of one address (nil: net.Dialer).
type dohTuning struct {
	dialTimeout, pingAfter, pingTimeout time.Duration
	dial                                func(ctx context.Context, network, address string) (net.Conn, error)
}

func newDoH(spec settings.UpstreamSpec, boot *bootstrap, enc encOpts, tune dohTuning) *dohTransport {
	d := &bootDialer{boot: boot, fixed: spec.DialAddr, timeout: tune.dialTimeout, dial: tune.dial}
	tr := &http.Transport{
		Proxy:                  nil,
		DialContext:            d.DialContext,
		ForceAttemptHTTP2:      true,
		TLSClientConfig:        clientTLS("", tls.VersionTLS12, enc, spec.Pins),
		TLSHandshakeTimeout:    defaultAttemptTimeout,
		MaxIdleConns:           4,
		MaxIdleConnsPerHost:    2,
		MaxConnsPerHost:        dohMaxConns,
		IdleConnTimeout:        dohIdleTimeout,
		MaxResponseHeaderBytes: 16 << 10,
		DisableCompression:     true,
		HTTP2:                  &http.HTTP2Config{SendPingTimeout: tune.pingAfter, PingTimeout: tune.pingTimeout},
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
	return exchangeHTTP(ctx, t.client, t.url, q, padQuery(q, wire))
}

func (t *dohTransport) close() { t.tr.CloseIdleConnections() }

// exchangeHTTP posts one query (ID 0) to a DoH URL through client (HTTP/2
// or HTTP/3) and reads the reply: status 200, the DNS media type, at most
// 64 KiB, ID 0 (set back to q.Id).
func exchangeHTTP(ctx context.Context, client *http.Client, u string, q *dns.Msg, wire []byte) (*dns.Msg, error) {
	body := bytes.Clone(wire)
	body[0], body[1] = 0, 0 // RFC 8484 4.1: ID 0 for cache friendliness
	res, sent, err := postHTTP(ctx, client, u, body)
	if err != nil && ctx.Err() == nil && (sent.reused || sent.cut) {
		// The server closed the connection while the query was on its way
		// (EOF, a reset, GOAWAY, an idle HTTP/1.1 connection; net/http and
		// quic-go send a POST again only when it surely was not processed),
		// or cut the TLS handshake of the new connection. A query is
		// idempotent: send it again once, at once, within ctx; after a
		// reused connection over a fresh one (the idle ones are likely
		// stale too).
		if sent.reused {
			client.CloseIdleConnections()
		}
		res, _, err = postHTTP(ctx, client, u, body)
	}
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
	dropPadding(m)
	return m, nil
}

// sendInfo says how postHTTP sent a request: on a connection that had
// carried a request before (reused), or on a new connection whose TLS
// handshake the server cut (cut; net/http reports the handshake through
// httptrace, quic-go does not).
type sendInfo struct{ reused, cut bool }

// postHTTP posts body to u.
func postHTTP(ctx context.Context, client *http.Client, u string, body []byte) (*http.Response, sendInfo, error) {
	var reused, cut atomic.Bool // the dial runs in a goroutine of its own
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(ci httptrace.GotConnInfo) { reused.Store(ci.Reused) },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err != nil && cutHandshake(err) {
				cut.Store(true)
			}
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, sendInfo{}, err
	}
	req.Header.Set("Content-Type", dohMediaType)
	req.Header.Set("Accept", dohMediaType)
	res, err := client.Do(req)
	return res, sendInfo{reused: reused.Load(), cut: cut.Load()}, err
}

// bootDialer dials host:port with the host resolved via the bootstrap
// servers (IP literals are dialled directly). With fixed (a DNS stamp's
// address) that address is dialled whatever the host is. The addresses of
// one dial get timeout in total (0: the attempt timeout), shared among them
// (dialAddrs): net/http dials detached from the request, so without it a
// black-holed upstream would leave a pending dial (a goroutine and a
// socket) behind every request for minutes.
type bootDialer struct {
	boot    *bootstrap
	fixed   netip.AddrPort
	timeout time.Duration
	dial    func(ctx context.Context, network, address string) (net.Conn, error) // nil: net.Dialer
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
	timeout := d.timeout
	if timeout <= 0 {
		timeout = defaultAttemptTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dial := d.dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: timeout}).DialContext
	}
	return dialAddrs(ctx, addrs, port, func(ctx context.Context, address string) (net.Conn, error) {
		return dial(ctx, network, address)
	})
}

// minAddrDial is the least time dialAddrs gives an address that is not the
// last one (as net.Dialer's partial deadlines).
const minAddrDial = 750 * time.Millisecond

// dialAddrs dials the addresses one after another until one connects. With
// a deadline on ctx each address but the last gets an equal share of the
// time left (at least minAddrDial) and the last one the rest, so a first
// address that drops the packets silently (a broken IPv6 path, a filtered
// anycast address) does not use up every dial and the next address is
// reached; bootstrap.lookup returns the same order every time.
func dialAddrs[C any](ctx context.Context, addrs []netip.Addr, port uint16, dial func(ctx context.Context, address string) (C, error)) (C, error) {
	var zero C
	lastErr := errNoAddrs
	for i, a := range addrs {
		actx, cancel := ctx, context.CancelFunc(func() {})
		if deadline, ok := ctx.Deadline(); ok && i < len(addrs)-1 {
			share := max(time.Until(deadline)/time.Duration(len(addrs)-i), minAddrDial)
			actx, cancel = context.WithTimeout(ctx, share)
		}
		c, err := dial(actx, netip.AddrPortFrom(a, port).String())
		cancel()
		if err == nil {
			return c, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return zero, lastErr
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
