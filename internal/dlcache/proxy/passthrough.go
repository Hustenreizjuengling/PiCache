package proxy

import (
	"errors"
	"io"
	"net/http"
	"time"
)

const (
	// maxPassBody bounds request bodies of passed-through requests.
	maxPassBody = 16 << 20
	// passBodyTimeout bounds reading such a body from the client.
	passBodyTimeout = 60 * time.Second
)

// passThrough forwards the client's request unchanged in substance (its
// Range, conditionals, cookies and Accept-Encoding are kept; hop-by-hop
// headers are dropped) and relays the answer uncached (PASS): disabled
// services, special paths, other methods, non-canonical paths, no store,
// and uncacheable answers on the cache path.
func (rq *request) passThrough() { rq.forward(false) }

// forward passes the request through; withoutRange drops Range and
// If-Range (the retry after a 416 for slice 0, ARCHITECTURE 8.2 step 10).
func (rq *request) forward(withoutRange bool) {
	r := rq.r
	rq.cacheStatus = statusPass
	var body io.Reader
	var length int64
	if r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0 {
		if r.ContentLength > maxPassBody {
			rq.status = http.StatusRequestEntityTooLarge
			http.Error(rq.w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		_ = rq.rc.SetReadDeadline(time.Now().Add(passBodyTimeout))
		body, length = http.MaxBytesReader(rq.w, r.Body, maxPassBody), r.ContentLength
	}
	hdr := forwardHeaders(r.Header, rq.s.d.InstanceID, false)
	if withoutRange {
		hdr.Del("Range")
		hdr.Del("If-Range")
	}
	resp, err := rq.s.roundTrip(rq.ctx, &upReq{method: r.Method, target: rq.target, host: rq.host, header: hdr, body: body, length: length})
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			rq.status = http.StatusRequestEntityTooLarge
			http.Error(rq.w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		rq.s.upstreamError(rq.host, rq.path, err)
		rq.fail(err)
		return
	}
	rq.relay(resp, statusPass)
}
