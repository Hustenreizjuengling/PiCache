package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	cachestore "github.com/hustenreizjuengling/picache/internal/lancache/store"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

const (
	connectTimeout        = 10 * time.Second
	responseHeaderTimeout = 15 * time.Second
	maxRedirects          = 5
	httpsOnlyFor          = 24 * time.Hour
	maxHTTPSOnlyHosts     = 1024
	// maxDrain is how much of an unwanted body is read to keep the
	// connection reusable.
	maxDrain = 64 << 10
)

var (
	errTooManyRedirects = errors.New("too many redirects")
	errBadRedirect      = errors.New("invalid redirect target")
)

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// newTransport builds the upstream transport (ARCHITECTURE 8.2 step 16):
// HTTP/1.1 keep-alive, no environment proxy, no transparent decompression,
// verified TLS. dial is the SafeDialer in production.
func newTransport(dial dialFunc, roots *x509.CertPool) *http.Transport {
	var protos http.Protocols
	protos.SetHTTP1(true)
	return &http.Transport{
		Protocols:              &protos,
		Proxy:                  nil,
		DialContext:            dial,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		TLSHandshakeTimeout:    connectTimeout,
		MaxIdleConns:           1024,
		MaxIdleConnsPerHost:    32,
		IdleConnTimeout:        90 * time.Second,
		ResponseHeaderTimeout:  responseHeaderTimeout,
		ExpectContinueTimeout:  time.Second,
		DisableCompression:     true,
		MaxResponseHeaderBytes: 64 << 10,
	}
}

// setUpstreamForTest replaces the upstream dialer (and trusted roots for
// https upstreams). Tests route connections to 127.0.0.1 origins with it;
// production always dials through the SafeDialer.
func (s *Server) setUpstreamForTest(dial dialFunc, roots *x509.CertPool) {
	s.transport.CloseIdleConnections()
	s.transport = newTransport(dial, roots)
}

// upReq describes one logical upstream request.
type upReq struct {
	method string
	target *url.URL // http://host/escaped-path?query
	host   string   // normalised request host (https-only memory, logs)
	header http.Header
	body   io.Reader // pass-through request body (nil: none)
	length int64     // body length, -1 unknown
}

// roundTrip performs q and returns the final response. It follows
// 301/302/307/308 for GET and HEAD (at most 5 hops, each target checked),
// retries a 426 once over verified https and remembers the host as
// https-only for 24 h, and retries a plain-http 404 once on the next
// address of the host. The returned body cancels the request when closed
// and fails after 60 s without progress while being read.
func (s *Server) roundTrip(ctx context.Context, q *upReq) (*http.Response, error) {
	ctx, cancel := context.WithCancel(ctx)
	cur := *q.target
	if cur.Scheme == "http" && s.https.has(cur.Hostname(), time.Now()) {
		cur.Scheme = "https"
	}
	header := q.header
	replayable := q.body == nil
	follow := replayable && (q.method == http.MethodGet || q.method == http.MethodHead)
	var pinned string // IP literal to use instead of the hostname (404 retry)
	var tried426, tried404 bool
	for hops := 0; ; {
		reqURL := cur
		hostHeader := ""
		if pinned != "" {
			reqURL.Host, hostHeader = pinned, cur.Host
			pinned = ""
		}
		req := (&http.Request{
			Method:     q.method,
			URL:        &reqURL,
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     header,
			Host:       hostHeader,
		}).WithContext(ctx)
		if q.body != nil {
			req.Body, req.ContentLength = io.NopCloser(q.body), q.length
		}
		resp, err := s.transport.RoundTrip(req)
		if err != nil {
			cancel()
			return nil, err
		}
		switch code := resp.StatusCode; {
		case code == http.StatusUpgradeRequired && cur.Scheme == "http" && replayable && !tried426:
			tried426 = true
			s.https.add(cur.Hostname(), time.Now())
			discardBody(resp)
			cur.Scheme, cur.Host = "https", hostOnly(&cur)
			continue
		case follow && isRedirect(code):
			loc := resp.Header.Get("Location")
			discardBody(resp)
			if hops >= maxRedirects {
				cancel()
				return nil, errTooManyRedirects
			}
			next, err := s.redirectTarget(ctx, &cur, loc)
			if err != nil {
				cancel()
				return nil, err
			}
			if next.Host != cur.Host {
				header = withoutCredentials(header)
			}
			cur = *next
			hops++
			continue
		case code == http.StatusNotFound && follow && cur.Scheme == "http" && !tried404:
			tried404 = true
			if ip, ok := s.nextAddr(ctx, cur.Hostname()); ok {
				discardBody(resp)
				pinned = ip.String()
				if port := cur.Port(); port != "" {
					pinned = net.JoinHostPort(pinned, port)
				}
				continue
			}
		}
		resp.Body = newIdleBody(resp.Body, cancel, s.tm.idleRead)
		return resp, nil
	}
}

// hostOnly returns u's host without a port (IPv6 literals keep brackets).
func hostOnly(u *url.URL) string {
	h := u.Hostname()
	if ip, err := netip.ParseAddr(h); err == nil && ip.Is6() {
		return "[" + h + "]"
	}
	return h
}

func isRedirect(code int) bool {
	return code == http.StatusMovedPermanently || code == http.StatusFound ||
		code == http.StatusTemporaryRedirect || code == http.StatusPermanentRedirect
}

// redirectTarget resolves and checks a Location: http or https only, no
// credentials, a valid host, port 80, 443 or ≥ 1024, and IP literals must
// pass the SSRF guard (host names are checked when they are dialed).
func (s *Server) redirectTarget(ctx context.Context, cur *url.URL, loc string) (*url.URL, error) {
	if loc == "" {
		return nil, errBadRedirect
	}
	ref, err := url.Parse(loc)
	if err != nil {
		return nil, errBadRedirect
	}
	next := cur.ResolveReference(ref)
	if (next.Scheme != "http" && next.Scheme != "https") || next.User != nil || next.Opaque != "" {
		return nil, errBadRedirect
	}
	host, isIP, ok := netutil.NormalizeHost(next.Hostname())
	if !ok {
		return nil, errBadRedirect
	}
	if p := next.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 || (n != 80 && n != 443 && n < 1024) {
			return nil, errBadRedirect
		}
	}
	if isIP {
		if _, err := s.safe.Filter(ctx, []netip.Addr{netip.MustParseAddr(host)}); err != nil {
			return nil, fmt.Errorf("redirect to %s: %w", host, err)
		}
	}
	next.Fragment, next.RawFragment = "", ""
	return next, nil
}

// nextAddr returns the second allowed IPv4 address of host (the first one
// answered 404).
func (s *Server) nextAddr(ctx context.Context, host string) (netip.Addr, bool) {
	if s.d.Lookup == nil {
		return netip.Addr{}, false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return netip.Addr{}, false
	}
	addrs, err := s.d.Lookup(ctx, host)
	if err != nil {
		return netip.Addr{}, false
	}
	allowed, err := s.safe.Filter(ctx, addrs)
	if err != nil {
		return netip.Addr{}, false
	}
	allowed = slices.DeleteFunc(allowed, func(ip netip.Addr) bool { return !ip.Is4() })
	if len(allowed) < 2 {
		return netip.Addr{}, false
	}
	return allowed[1], true
}

// discardBody drains a little of an unwanted body (for connection reuse)
// and closes it.
func discardBody(resp *http.Response) {
	_, _ = io.CopyN(io.Discard, resp.Body, maxDrain)
	_ = resp.Body.Close()
}

// idleBody wraps an upstream body: a Read that makes no progress for idle
// cancels the request; Close cancels it too (releasing the connection).
type idleBody struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
	timer  *time.Timer
	idle   time.Duration
}

func newIdleBody(rc io.ReadCloser, cancel context.CancelFunc, idle time.Duration) *idleBody {
	b := &idleBody{rc: rc, cancel: cancel, idle: idle}
	b.timer = time.AfterFunc(idle, cancel)
	b.timer.Stop()
	return b
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.timer.Reset(b.idle)
	n, err := b.rc.Read(p)
	b.timer.Stop()
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	err := b.rc.Close()
	b.cancel()
	return err
}

// httpsOnlyHosts remembers hosts that answered 426 (bounded, 24 h).
type httpsOnlyHosts struct {
	mu sync.Mutex
	m  map[string]time.Time // host → expiry
}

func (h *httpsOnlyHosts) has(host string, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	exp, ok := h.m[host]
	return ok && now.Before(exp)
}

func (h *httpsOnlyHosts) add(host string, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		h.m = make(map[string]time.Time)
	}
	if _, ok := h.m[host]; !ok && len(h.m) >= maxHTTPSOnlyHosts {
		h.expireLocked(now)
		if len(h.m) >= maxHTTPSOnlyHosts {
			var oldest string
			for k, exp := range h.m {
				if oldest == "" || exp.Before(h.m[oldest]) {
					oldest = k
				}
			}
			delete(h.m, oldest)
		}
	}
	h.m[host] = now.Add(httpsOnlyFor)
}

func (h *httpsOnlyHosts) expire(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expireLocked(now)
}

func (h *httpsOnlyHosts) expireLocked(now time.Time) {
	for k, exp := range h.m {
		if !now.Before(exp) {
			delete(h.m, k)
		}
	}
}

// upstreamError logs an upstream failure (at most hourly per host and
// error) without the query string.
func (s *Server) upstreamError(host, path string, err error) {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	lvl := slog.LevelDebug
	if errors.Is(err, netutil.ErrForbiddenDestination) || errors.Is(err, errBadRedirect) || errors.Is(err, errTooManyRedirects) {
		lvl = slog.LevelWarn
	}
	if lvl == slog.LevelWarn && !s.warns.allow("upstream "+host+" "+err.Error(), time.Now()) {
		return
	}
	s.log.Log(context.Background(), lvl, "upstream request failed",
		slog.String("host", host), slog.String("path", path), slog.Any("err", err))
}

// storeError logs a store failure at most hourly per operation and error.
// A closed store (switched or shutting down), a changed object and an
// ended request are expected and only logged at debug level.
func (s *Server) storeError(op string, err error) {
	if errors.Is(err, cachestore.ErrClosed) || errors.Is(err, cachestore.ErrStale) || errors.Is(err, context.Canceled) {
		s.log.Debug("cache store operation not done", slog.String("op", op), slog.Any("err", err))
		return
	}
	if s.warns.allow("store "+op+" "+err.Error(), time.Now()) {
		s.log.Warn("cache store operation failed", slog.String("op", op), slog.Any("err", err))
	}
}
