package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// QUIC upstreams (docs/ARCHITECTURE.md 7.4): DNS over QUIC (RFC 9250,
// quic://) and DNS over HTTPS over HTTP/3 (h3://). quic-go is used as a
// client only; PiCache never listens on QUIC.

const (
	quicIdleTimeout  = 60 * time.Second // idle connections close
	doqMaxStreams    = 64               // concurrent queries (streams) per DoQ connection
	doqMaxReply      = 64 << 10         // a DoQ reply is at most 64 KiB (the 2-byte length allows 65535)
	h3MaxUniStreams  = 8                // the server's control and QPACK streams (RFC 9114 6.2)
	doqErrNoError    = 0x0              // DOQ_NO_ERROR (RFC 9250 4.3)
	doqErrCancelled  = 0x3              // DOQ_REQUEST_CANCELLED
	quicBufferEnvVar = "QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING"
)

var quicEnvOnce sync.Once

// quietQUIC keeps quic-go's receive-buffer warning off stderr: the switch
// quic-go reads is set before the first QUIC socket is created (a value
// set by the operator stays).
func quietQUIC() {
	quicEnvOnce.Do(func() {
		if os.Getenv(quicBufferEnvVar) == "" {
			_ = os.Setenv(quicBufferEnvVar, "true")
		}
	})
}

// quicConfig is the QUIC configuration of both QUIC upstreams: idle
// connections close after a minute, no keep-alives, the handshake bounded
// by the attempt, no incoming bidirectional streams, no qlog tracer.
// uni is the limit of incoming unidirectional streams (negative: none;
// HTTP/3 needs the server's control and QPACK streams).
func quicConfig(idle time.Duration, uni int64) *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:        idle,
		HandshakeIdleTimeout:  defaultAttemptTimeout,
		KeepAlivePeriod:       0,
		MaxIncomingStreams:    -1,
		MaxIncomingUniStreams: uni,
	}
}

// quicDialer resolves an upstream's host like DoT (bootstrap servers; a
// stamp's address or an IP literal is dialled directly) and dials QUIC
// without early data (0-RTT queries could be replayed).
type quicDialer struct {
	d bootDialer
}

func (q *quicDialer) dial(ctx context.Context, address string, tlsConf *tls.Config, conf *quic.Config) (*quic.Conn, error) {
	addrs, port, err := q.d.addrs(ctx, address)
	if err != nil {
		return nil, err
	}
	lastErr := errNoAddrs
	for _, a := range addrs {
		c, err := quic.DialAddr(ctx, netip.AddrPortFrom(a, port).String(), tlsConf, conf)
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

// doqTransport is DNS over QUIC (RFC 9250, ALPN "doq"): one QUIC
// connection per upstream, reused and dialled again after it failed or
// idled out; one bidirectional stream per query (2-byte length prefix,
// message ID 0, the send side closed after the query); at most 64
// streams at a time.
type doqTransport struct {
	address string // host:port for the dialer
	dialer  quicDialer
	tlsConf *tls.Config
	qconf   *quic.Config
	sem     chan struct{}
	dialSem chan struct{} // serialises dials (waiting honours the context)

	mu     sync.Mutex
	conn   *quic.Conn
	closed bool
}

func newDoQ(spec settings.UpstreamSpec, boot *bootstrap, roots *x509.CertPool, idle time.Duration) *doqTransport {
	quietQUIC()
	conf := clientTLS(spec.Host, tls.VersionTLS13, roots, spec.Pins)
	conf.NextProtos = []string{"doq"}
	return &doqTransport{
		address: spec.Addr(),
		dialer:  quicDialer{d: bootDialer{boot: boot, fixed: spec.DialAddr}},
		tlsConf: conf,
		qconf:   quicConfig(idle, -1),
		sem:     make(chan struct{}, doqMaxStreams),
		dialSem: make(chan struct{}, 1),
	}
}

func (t *doqTransport) exchange(ctx context.Context, q *dns.Msg, wire []byte) (*dns.Msg, error) {
	select {
	case t.sem <- struct{}{}:
		defer func() { <-t.sem }()
	case <-ctx.Done():
		return nil, ctxErr(ctx, ctx.Err())
	}
	conn, err := t.connection(ctx)
	if err != nil {
		return nil, ctxErr(ctx, err)
	}
	m, err := t.roundTrip(ctx, conn, wire)
	if err != nil {
		if conn.Context().Err() != nil {
			t.drop(conn) // the connection failed or idled out: dial again next time
		}
		return nil, ctxErr(ctx, err)
	}
	m.Id = q.Id
	return m, nil
}

// connection returns the open connection or dials a new one.
func (t *doqTransport) connection(ctx context.Context) (*quic.Conn, error) {
	if c := t.current(); c != nil {
		return c, nil
	}
	select {
	case t.dialSem <- struct{}{}:
		defer func() { <-t.dialSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if c := t.current(); c != nil {
		return c, nil // dialled while this query waited
	}
	c, err := t.dialer.dial(ctx, t.address, t.tlsConf, t.qconf)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		_ = c.CloseWithError(doqErrNoError, "")
		return nil, errClosed
	}
	t.conn = c
	return c, nil
}

// current returns the open connection (nil if there is none or it ended).
func (t *doqTransport) current() *quic.Conn {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn != nil && t.conn.Context().Err() != nil {
		t.conn = nil
	}
	return t.conn
}

func (t *doqTransport) drop(c *quic.Conn) {
	t.mu.Lock()
	if t.conn == c {
		t.conn = nil
	}
	t.mu.Unlock()
	_ = c.CloseWithError(doqErrNoError, "")
}

// roundTrip sends one query on a new stream and reads its reply. quic-go
// keeps a stream (and the data it buffered) until both of its sides have
// ended, so every failure abandons the sides that are still open: a
// connection in steady use never idles out, and streams left half open
// by bad replies would pile up on it.
func (t *doqTransport) roundTrip(ctx context.Context, conn *quic.Conn, wire []byte) (_ *dns.Msg, err error) {
	str, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	sent := false // the send side is closed (the FIN is queued)
	defer func() {
		if err != nil {
			str.CancelRead(doqErrCancelled) // harmless after a complete read
			if !sent {
				str.CancelWrite(doqErrCancelled)
			}
		}
	}()
	if dl, ok := ctx.Deadline(); ok {
		_ = str.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() {
		str.CancelRead(doqErrCancelled)
		str.CancelWrite(doqErrCancelled)
	})
	defer stop()
	buf := make([]byte, 2+len(wire))
	binary.BigEndian.PutUint16(buf, uint16(len(wire)))
	copy(buf[2:], wire)
	buf[2], buf[3] = 0, 0 // RFC 9250 4.2.1: the message ID is 0
	if _, err := str.Write(buf); err != nil {
		return nil, err
	}
	if err := str.Close(); err != nil { // closes the send side only
		return nil, err
	}
	sent = true
	var hdr [2]byte
	if _, err := io.ReadFull(str, hdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n < minMsgSize || n > doqMaxReply {
		return nil, errShortReply
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(str, body); err != nil {
		return nil, err
	}
	str.CancelRead(doqErrNoError) // nothing else is expected on this stream
	m := new(dns.Msg)
	if err := m.Unpack(body); err != nil {
		return nil, fmt.Errorf("malformed reply: %w", err)
	}
	if m.Id != 0 {
		return nil, errIDMismatch
	}
	return m, nil
}

func (t *doqTransport) close() {
	t.mu.Lock()
	c := t.conn
	t.conn, t.closed = nil, true
	t.mu.Unlock()
	if c != nil {
		_ = c.CloseWithError(doqErrNoError, "")
	}
}

// h3Transport is DNS over HTTPS over HTTP/3 (h3://): RFC 8484 POST with ID
// 0 through an http3.Transport per upstream (the QUIC configuration of
// DoQ, response headers at most 16 KiB, bodies at most 64 KiB, no
// redirects, no proxies). https:// upstreams never switch to HTTP/3.
type h3Transport struct {
	url    string
	tr     *http3.Transport
	client *http.Client
	stop   context.CancelFunc // ends the dial in progress (close)
}

func newH3(spec settings.UpstreamSpec, boot *bootstrap, roots *x509.CertPool, idle time.Duration) *h3Transport {
	quietQUIC()
	d := &quicDialer{d: bootDialer{boot: boot, fixed: spec.DialAddr}}
	life, stop := context.WithCancel(context.Background())
	tr := &http3.Transport{
		TLSClientConfig:        clientTLS(spec.Host, tls.VersionTLS13, roots, spec.Pins),
		QUICConfig:             quicConfig(idle, h3MaxUniStreams),
		Dial:                   sharedDial(life, d.dial),
		MaxResponseHeaderBytes: 16 << 10,
		DisableCompression:     true,
	}
	return &h3Transport{url: spec.URL, tr: tr, stop: stop, client: &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// quicDialFunc dials a QUIC connection (quicDialer.dial, the Dial hook of
// http3.Transport).
type quicDialFunc func(ctx context.Context, addr string, tlsConf *tls.Config, conf *quic.Config) (*quic.Conn, error)

// sharedDial adapts dial for http3.Transport, which dials a host once, with
// the context of the request that needed the connection, and hands the
// result to every request waiting for it. The dial must not end with that
// request (a parallel loser, a query given up): the requests waiting for
// the connection would fail with its cancellation. It ends with the
// attempt timeout or when the transport is closed (life); a request that
// stops waiting leaves the connection to the next one, as net/http does.
func sharedDial(life context.Context, dial quicDialFunc) quicDialFunc {
	return func(ctx context.Context, addr string, tlsConf *tls.Config, conf *quic.Config) (*quic.Conn, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultAttemptTimeout)
		defer cancel()
		defer context.AfterFunc(life, cancel)()
		return dial(ctx, addr, tlsConf, conf)
	}
}

func (t *h3Transport) exchange(ctx context.Context, q *dns.Msg, wire []byte) (*dns.Msg, error) {
	m, err := exchangeHTTP(ctx, t.client, t.url, q, wire)
	if err != nil && errors.Is(err, http3.ErrTransportClosed) {
		return nil, errClosed
	}
	return m, err
}

func (t *h3Transport) close() {
	t.stop()
	_ = t.tr.Close()
}
