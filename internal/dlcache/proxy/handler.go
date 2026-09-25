package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/dlcache/services"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

const (
	heartbeatPath = "/lancache-heartbeat" // the heartbeat path that prefill tools and monitoring probe
	maxLogField   = 256                   // user agent and range in cache events
	maxLogPath    = 2048
)

// Cache statuses (X-Upstream-Cache-Status and logs.CacheEvent.CacheStatus).
const (
	statusHit     = "HIT"
	statusMiss    = "MISS"
	statusPartial = "PARTIAL"
	statusBypass  = "BYPASS"
	statusPass    = "PASS"
	statusError   = "ERROR"
)

var errClientGone = errors.New("client gone")

// serveHTTP is the request pipeline of ARCHITECTURE 8.2 (steps 1–9). While
// the download cache is disabled, only the heartbeat is answered (like the SNI
// pass-through, nothing is proxied or stored).
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(s.tm.write))
	// The response is flushed after the handler returns.
	defer func() { _ = rc.SetWriteDeadline(time.Now().Add(s.tm.write)) }()

	ip := netutil.AddrFromRemote(r.RemoteAddr)
	if !ip.IsValid() || s.d.ACL == nil || !s.d.ACL.Get().Allowed(ip) {
		s.stats.refused.Add(1)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.URL.Path == heartbeatPath && (r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions) {
		s.heartbeat(w, r)
		return
	}
	if !s.settings().DownloadCache.Enabled {
		s.stats.refused.Add(1)
		http.Error(w, "forbidden: the download cache is disabled", http.StatusForbidden)
		return
	}
	s.stats.requests.Add(1) // heartbeats are not counted
	if s.isLoop(r.Header) {
		s.stats.errors.Add(1)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusLoopDetected)
		return
	}
	host, isIP, ok := netutil.NormalizeHost(r.Host)
	if !ok || isIP || r.URL.Opaque != "" || !strings.HasPrefix(r.URL.Path, "/") {
		s.stats.refused.Add(1)
		http.Error(w, "bad request: the Host must be a host name", http.StatusBadRequest)
		return
	}
	cacheMethod := r.Method == http.MethodGet || r.Method == http.MethodHead
	canonical := canonicalPath(r.URL.EscapedPath(), r.URL.Path)
	keyPath := r.URL.Path
	if canonical {
		keyPath = cacheKeyPath(r.URL.EscapedPath())
		canonical = len(keyPath) <= maxPathLen
	}
	ua := r.UserAgent()
	classUA := ua
	if !cacheMethod || !canonical {
		classUA = "" // the Steam rule applies to canonical GET/HEAD only
	}
	var service string
	var enabled, known bool
	if s.d.Services != nil {
		service, enabled, known = s.d.Services.Classify(host, classUA, r.URL.Path)
	}
	if !known {
		s.stats.refused.Add(1)
		if strings.HasSuffix(ua, services.SteamUserAgentSuffix) {
			s.stats.steamRefused(host)
		}
		http.Error(w, "forbidden: host is not a download service", http.StatusForbidden)
		return
	}

	rq := s.newRequest(w, r, rc, ip, host, service)
	rq.keyPath = keyPath
	defer rq.finish()
	switch {
	case !enabled, services.IsBypassPath(r.URL.Path), !cacheMethod, !canonical:
		rq.passThrough()
	default:
		rq.bypass = s.nocacheRequested(r.URL, ip)
		st := s.store()
		if st == nil {
			rq.passThrough()
			return
		}
		rq.serveCache(st)
	}
}

// heartbeat answers the probe of prefill tools and monitoring on the
// heartbeat path (any Host, incl. IP literals).
func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set(processedByHeader, s.d.InstanceID)
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Expose-Headers", "*")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Private-Network", "true")
		h.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	}
	w.WriteHeader(http.StatusNoContent)
}

// isLoop reports whether the request already passed through this instance.
func (s *Server) isLoop(h http.Header) bool {
	if s.d.InstanceID == "" {
		return false
	}
	for _, v := range h.Values(processedByHeader) {
		for id := range strings.SplitSeq(v, ",") {
			if strings.TrimSpace(id) == s.d.InstanceID {
				return true
			}
		}
	}
	return false
}

// nocacheRequested reports a ?nocache=<non-empty, not "0"> from a client
// allowed to force refetches (downloadCache.nocacheClients).
func (s *Server) nocacheRequested(u *url.URL, ip netip.Addr) bool {
	if !strings.Contains(u.RawQuery, "nocache") {
		return false
	}
	v := u.Query().Get("nocache")
	return v != "" && v != "0" && s.nocacheAllowed(ip)
}

// acct accumulates the bytes of one client request. Fills started by the
// request keep adding upstream and stored bytes after the handler
// returned; the cache event is emitted when the last holder releases it.
type acct struct {
	s                      *Server
	sent, hit, wan, stored atomic.Int64
	refs                   atomic.Int32
	ev                     logs.CacheEvent // completed by the handler before its release
	emit                   bool
}

func (a *acct) retain() { a.refs.Add(1) }

func (a *acct) release() {
	if a.refs.Add(-1) != 0 || !a.emit || a.s.d.Logs == nil {
		return
	}
	ev := a.ev
	ev.BytesSent, ev.BytesHit = a.sent.Load(), a.hit.Load()
	ev.BytesWAN, ev.BytesStored = a.wan.Load(), a.stored.Load()
	a.s.d.Logs.LogCache(ev)
}

// request is the state of one proxied client request.
type request struct {
	s     *Server
	w     http.ResponseWriter // never wrapped (sendfile for cached slices)
	r     *http.Request
	rc    *http.ResponseController
	ctx   context.Context
	start time.Time
	ip    netip.Addr
	ckey  netip.Prefix // client key for per-client fill limits
	ident *clients.Identity

	host, service, path string
	keyPath             string // path of the cache key (cacheKeyPath)
	group               services.Group
	label               string
	target              *url.URL    // upstream URL: the client's escaped path and query
	fillHeader          http.Header // upstream headers for fills and direct slice fetches

	acct *acct
	tr   *transfer

	status      int    // HTTP status sent (0 = none yet)
	cacheStatus string // for the event; "" = derived from the bytes
	predicted   string // X-Upstream-Cache-Status sent on the cache path
	bypass      bool
	head        bool

	cache // cache-path state (serve.go)
}

func (s *Server) newRequest(w http.ResponseWriter, r *http.Request, rc *http.ResponseController, ip netip.Addr, host, service string) *request {
	target := *r.URL
	target.Scheme, target.Host, target.User = "http", host, nil
	target.Fragment, target.RawFragment = "", ""
	rq := &request{
		s: s, w: w, r: r, rc: rc, ctx: r.Context(), start: time.Now(),
		ip: ip, ckey: netutil.ClientKey(ip),
		host: host, service: service, path: r.URL.Path,
		group:  services.GroupFor(service, host, r.URL.Path),
		target: &target,
		head:   r.Method == http.MethodHead,
	}
	if s.d.Clients != nil {
		rq.ident = s.d.Clients.Identify(ip)
	}
	rq.label = rq.group.Label
	if s.d.Services != nil {
		if l := s.d.Services.Label(rq.group.Key); l != "" {
			rq.label = l
		}
	}
	rq.fillHeader = forwardHeaders(r.Header, s.d.InstanceID, true)
	rq.acct = &acct{s: s, emit: rq.ident == nil || !rq.ident.IgnoreLogs}
	rq.acct.refs.Store(1)
	rq.tr = s.live.begin(rq)
	return rq
}

func (rq *request) clientName() string {
	if rq.ident == nil {
		return ""
	}
	return rq.ident.Name
}

// finish releases the request's resources and completes its cache event
// (also when the handler aborts the response with a panic).
func (rq *request) finish() {
	rq.releaseSources()
	s := rq.s
	if sent := rq.acct.sent.Load(); rq.st != nil && rq.plan.status != 0 && rq.cacheStatus != statusPass && sent > 0 {
		// Served on the cache path: an access, and a hit with the bytes
		// served from the cache (none for a MISS).
		rq.st.Touch(rq.id, rq.acct.hit.Load())
	}
	if rq.release != nil {
		rq.release() // after Touch: eviction skips the object either way
		rq.release = nil
	}
	if rq.probe && !rq.probeUsed {
		s.noslice.returnProbe(rq.host, rq.probeAt) // no range request went upstream
	}
	s.live.end(rq.tr, time.Now())
	a := rq.acct
	a.ev = logs.CacheEvent{
		Time:        rq.start.UTC(),
		ClientIP:    rq.ip.String(),
		ClientName:  rq.clientName(),
		Service:     rq.service,
		Host:        rq.host,
		Path:        clip(rq.path, maxLogPath),
		Method:      rq.r.Method,
		Status:      rq.status,
		CacheStatus: rq.eventStatus(),
		Range:       clip(rq.r.Header.Get("Range"), maxLogField),
		DurationMs:  time.Since(rq.start).Milliseconds(),
		GroupKey:    rq.group.Key,
		Label:       rq.label,
		UserAgent:   clip(rq.r.UserAgent(), maxLogField),
	}
	a.release()
}

// eventStatus derives the event's cache status from what was served.
func (rq *request) eventStatus() string {
	switch {
	case rq.cacheStatus != "":
		return rq.cacheStatus
	case rq.bypass:
		return statusBypass
	}
	sent, hit := rq.acct.sent.Load(), rq.acct.hit.Load()
	switch {
	case sent == 0 && rq.predicted != "":
		return rq.predicted
	case sent == 0:
		return statusMiss
	case hit >= sent:
		return statusHit
	case hit == 0:
		return statusMiss
	}
	return statusPartial
}

// writeHeader sends the status line (once).
func (rq *request) writeHeader(status int) {
	if rq.status != 0 {
		return
	}
	rq.status = status
	rq.w.WriteHeader(status)
}

// writeBody writes p in chunks, renewing the write deadline before each,
// and flushes: the next bytes may take a while (growing fill, slow
// upstream), and the client must see what is available now. Large writes
// bypass net/http's buffers, so the flush is then a no-op. On the cache
// path the planned headers are sent first.
func (rq *request) writeBody(p []byte) error {
	rq.commit()
	for len(p) > 0 {
		k := min(len(p), writeChunk)
		_ = rq.rc.SetWriteDeadline(time.Now().Add(rq.s.tm.write))
		n, err := rq.w.Write(p[:k])
		rq.acct.sent.Add(int64(n))
		if err != nil {
			return errClientGone
		}
		p = p[k:]
	}
	if err := rq.rc.Flush(); err != nil {
		return errClientGone
	}
	return nil
}

// fail answers an error before anything was sent, or aborts the response.
func (rq *request) fail(err error) {
	if errors.Is(err, errClientGone) || rq.ctx.Err() != nil {
		return
	}
	rq.s.stats.errors.Add(1)
	rq.cacheStatus = statusError
	if rq.status != 0 {
		// Headers (with a length) are out: abort so the client notices.
		panic(http.ErrAbortHandler)
	}
	http.Error(rq.w, "bad gateway", http.StatusBadGateway)
	rq.status = http.StatusBadGateway
}

// clip truncates s to at most n bytes without splitting a UTF-8 sequence.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
