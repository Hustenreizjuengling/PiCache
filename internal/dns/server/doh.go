package dnsserver

import (
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// DNS over HTTPS (RFC 8484) for clients: /dns-query and
// /dns-query/<ClientID> on the web listeners and the dedicated DoH
// listeners (docs/ARCHITECTURE.md 19). The callers check that DoH is on
// and the listener's own rules (proxy headers, the effective scheme) and
// pass the source: the effective client on the web listeners, the TCP
// peer on the dedicated ones.

// Bounds of DoH.
const (
	dohMaxBody       = 64 << 10 // a POST body
	dohMaxPerClient  = 64       // concurrent requests of one client key
	dohMaxClientKeys = 4096     // client keys with requests in flight
	dohMediaType     = "application/dns-message"
)

// DoHPath is the path of DoH; DoHPrefix the subtree with a ClientID.
const (
	DoHPath   = "/dns-query"
	DoHPrefix = "/dns-query/"
)

// IsDoHPath reports whether a request path is a DoH path (the HTTPS
// redirect of the web UI skips them).
func IsDoHPath(p string) bool { return p == DoHPath || strings.HasPrefix(p, DoHPrefix) }

// dohLimiter counts the DoH requests in flight per client key; only keys
// with requests in flight are held.
type dohLimiter struct {
	mu  sync.Mutex
	per map[netip.Prefix]int
}

// acquire takes a slot of key: 429 when the key has 64 requests in
// flight, 503 when 4096 other keys have.
func (l *dohLimiter) acquire(key netip.Prefix) (release func(), status int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.per == nil {
		l.per = map[netip.Prefix]int{}
	}
	n, ok := l.per[key]
	switch {
	case !ok && len(l.per) >= dohMaxClientKeys:
		return nil, http.StatusServiceUnavailable
	case n >= dohMaxPerClient:
		return nil, http.StatusTooManyRequests
	}
	l.per[key] = n + 1
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.per[key] <= 1 {
			delete(l.per, key)
		} else {
			l.per[key]--
		}
	}, 0
}

// dohError answers with a short plain-text error that is never cached.
func dohError(w http.ResponseWriter, status int, msg string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}

// DoHNotFound answers a request for a DoH path while DoH is off (or a path
// below it with more than one segment), like an unknown path.
func DoHNotFound(w http.ResponseWriter) { dohError(w, http.StatusNotFound, "404 page not found") }

// DoHClientID returns the ClientID of a DoH path: none for /dns-query and
// /dns-query/, the segment of /dns-query/<ClientID>. found is false for a
// path that is no DoH path (more segments); valid is false for a segment
// that is no ClientID.
func DoHClientID(path string) (id string, found, valid bool) {
	if path == DoHPath || path == DoHPrefix {
		return "", true, true
	}
	rest, ok := strings.CutPrefix(path, DoHPrefix)
	if !ok || strings.Contains(rest, "/") {
		return "", false, false
	}
	id, valid = settings.NormalizeClientID(rest)
	return id, true, valid
}

// ServeDoH answers one DoH request from source (docs/ARCHITECTURE.md 19):
// DoH off → 404; method GET or POST (405); cross-site browser requests
// (Sec-Fetch-Site, Origin) → 403; the path's ClientID (400); the message
// (GET ?dns= base64url, POST application/dns-message ≤ 64 KiB; 400, 413,
// 415); the DNS ACL on the source (403); then the pipeline with protocol
// doh. A dropped query resets the stream (panic http.ErrAbortHandler);
// more than 64 requests of the source's client key in flight → 429,
// overload → 503. The reply keeps the query's ID and question case and
// is cached privately for its smallest TTL.
func (s *Server) ServeDoH(w http.ResponseWriter, r *http.Request, source netip.Addr) {
	set := s.d.Settings.Get()
	id, found, valid := DoHClientID(r.URL.Path)
	if !found || !set.DNS.Encrypted.DoH {
		DoHNotFound(w)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		dohError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if crossSite(r) {
		dohError(w, http.StatusForbidden, "cross-site DoH requests are refused")
		return
	}
	if !valid {
		dohError(w, http.StatusBadRequest, "invalid ClientID")
		return
	}
	req, status, msg := readDoHQuery(w, r)
	if status != 0 {
		dohError(w, status, msg)
		return
	}
	source = netutil.Canon(source)
	if !s.allowed(source) {
		s.refused.Add(1)
		s.refusedSrc.add(source, time.Now())
		dohError(w, http.StatusForbidden, "this address may not use DNS")
		return
	}
	release, status := s.doh.acquire(netutil.ClientKey(source))
	if status != 0 {
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "1")
		}
		dohError(w, status, http.StatusText(status))
		return
	}
	defer release()
	dw := &dohWriter{source: source}
	overloaded := false
	s.serve(r.Context(), dw, req, queryConn{proto: ProtoDoH, source: source, clientID: id, overload: func() { overloaded = true }})
	switch {
	case dw.closed:
		panic(http.ErrAbortHandler) // a drop: reset only this stream
	case overloaded:
		dohError(w, http.StatusServiceUnavailable, "the DNS server is overloaded")
		return
	case dw.msg == nil:
		dohError(w, http.StatusInternalServerError, "no answer")
		return
	}
	b, err := dw.msg.Pack()
	if err != nil {
		dohError(w, http.StatusInternalServerError, "no answer")
		return
	}
	h := w.Header()
	h.Set("Content-Type", dohMediaType)
	h.Set("Cache-Control", "private, max-age="+strconv.FormatUint(uint64(dohMaxAge(dw.msg)), 10))
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// crossSite reports a browser request made by a page of another site:
// Sec-Fetch-Site cross-site or same-site, or an Origin whose host differs
// from the request's Host. Native and browser-internal DoH clients send
// neither.
func crossSite(r *http.Request) bool {
	switch strings.ToLower(r.Header.Get("Sec-Fetch-Site")) {
	case "cross-site", "same-site":
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	return err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host)
}

// readDoHQuery reads the query of a request (status 0 = ok).
func readDoHQuery(w http.ResponseWriter, r *http.Request) (*dns.Msg, int, string) {
	var wire []byte
	if r.Method == http.MethodGet {
		v := r.URL.Query().Get("dns")
		if v == "" {
			return nil, http.StatusBadRequest, "missing dns parameter"
		}
		b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(v, "="))
		if err != nil {
			return nil, http.StatusBadRequest, "the dns parameter is not base64url"
		}
		wire = b
	} else {
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != dohMediaType {
			return nil, http.StatusUnsupportedMediaType, "Content-Type must be " + dohMediaType
		}
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, dohMaxBody))
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				return nil, http.StatusRequestEntityTooLarge, "the message is larger than 64 KiB"
			}
			return nil, http.StatusBadRequest, "the body could not be read"
		}
		wire = b
	}
	m := new(dns.Msg)
	if err := m.Unpack(wire); err != nil || m.Response || len(m.Question) == 0 {
		return nil, http.StatusBadRequest, "not a DNS query"
	}
	return m, 0, ""
}

// dohMaxAge is the lifetime of a DoH reply: the smallest TTL of its
// records outside OPT; 0 without records and for rcodes other than
// NOERROR and NXDOMAIN.
func dohMaxAge(m *dns.Msg) uint32 {
	if m.Rcode != dns.RcodeSuccess && m.Rcode != dns.RcodeNameError {
		return 0
	}
	var n uint32
	first := true
	for _, sec := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range sec {
			if rr.Header().Rrtype == dns.TypeOPT {
				continue
			}
			if ttl := rr.Header().Ttl; first || ttl < n {
				n, first = ttl, false
			}
		}
	}
	return n
}

// dohWriter collects the reply of the pipeline for a DoH request; Close
// means the query is dropped.
type dohWriter struct {
	source netip.Addr
	msg    *dns.Msg
	closed bool
}

func (w *dohWriter) LocalAddr() net.Addr { return &net.TCPAddr{} }
func (w *dohWriter) RemoteAddr() net.Addr {
	return net.TCPAddrFromAddrPort(netip.AddrPortFrom(w.source, 0))
}
func (w *dohWriter) WriteMsg(m *dns.Msg) error {
	w.msg = m
	return nil
}
func (w *dohWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *dohWriter) Close() error                { w.closed = true; return nil }
func (w *dohWriter) TsigStatus() error           { return nil }
func (w *dohWriter) TsigTimersOnly(bool)         {}
func (w *dohWriter) Hijack()                     {}
