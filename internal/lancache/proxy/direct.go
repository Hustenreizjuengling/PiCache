package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"sync"
)

// copyBufs are the buffers for streaming upstream bodies to clients.
var copyBufs = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

// directSource is an upstream body positioned at an absolute object
// offset. It serves sequentially (skipping forward when needed) and, with
// capture, stores every complete slice passing through when a fill slot is
// free (hosts without range support, range failures).
type directSource struct {
	body    io.ReadCloser
	off     int64 // absolute offset of the next body byte
	end     int64 // last offset the body provides
	capture bool
	failed  bool
}

func newDirect(body io.ReadCloser, start, end int64, capture bool) *directSource {
	return &directSource{body: body, off: start, end: end, capture: capture}
}

func (d *directSource) covers(pos int64) bool { return !d.failed && pos >= d.off && pos <= d.end }

func (d *directSource) close() {
	if !d.failed && d.off > d.end {
		_, _ = io.CopyN(io.Discard, d.body, 512) // see EOF: keep the connection
	}
	_ = d.body.Close()
}

// serve writes object bytes from pos up to end (or the body's end).
func (d *directSource) serve(rq *request, pos, end int64) (int64, error) {
	S, T := rq.S, rq.obj.total
	var written int64
	for {
		cur := pos + written
		if cur > end || d.off > d.end {
			return written, nil
		}
		i := d.off / S
		sStart := i * S
		sEnd := sStart + sliceLen(i, S, T) - 1
		if d.capture && d.off == sStart && sEnd <= d.end {
			if buf, ok := rq.captureBuffer(i); ok {
				data := buf[:sEnd-sStart+1]
				if err := d.readFull(rq, data); err != nil {
					rq.releaseCapture(buf)
					return written, err
				}
				var werr error
				if cur <= sEnd {
					k := min(end, sEnd) - cur + 1
					if werr = rq.writeBody(data[cur-sStart : cur-sStart+k]); werr == nil {
						written += k
					}
				}
				rq.storeCaptured(i, data)
				if werr != nil {
					return written, werr
				}
				continue
			}
		}
		stop := min(sEnd, d.end) // stay within the slice: the next one may be captured
		if d.off < cur {
			if err := d.skip(rq, min(cur, stop+1)-d.off); err != nil {
				return written, err
			}
			continue
		}
		k := min(end, stop) - cur + 1
		n, err := d.copyTo(rq, k)
		written += n
		if err != nil {
			return written, err
		}
	}
}

// readFull reads exactly len(p) body bytes.
func (d *directSource) readFull(rq *request, p []byte) error {
	n, err := io.ReadFull(d.body, p)
	d.off += int64(n)
	rq.countWAN(int64(n))
	if err != nil {
		d.failed = true
		return errUpstreamAnswer
	}
	return nil
}

// skip reads and drops n body bytes.
func (d *directSource) skip(rq *request, n int64) error {
	m, err := io.CopyN(io.Discard, d.body, n)
	d.off += m
	rq.countWAN(m)
	if err != nil {
		d.failed = true
		return errUpstreamAnswer
	}
	return nil
}

// copyTo streams n body bytes to the client.
func (d *directSource) copyTo(rq *request, n int64) (int64, error) {
	bp := copyBufs.Get().(*[]byte)
	defer copyBufs.Put(bp)
	buf := *bp
	var done int64
	for done < n {
		k, err := d.body.Read(buf[:min(int64(len(buf)), n-done)])
		if k > 0 {
			d.off += int64(k)
			rq.countWAN(int64(k))
			if werr := rq.writeBody(buf[:k]); werr != nil {
				return done, werr
			}
			done += int64(k)
		}
		if err != nil && done < n {
			d.failed = true
			return done, errUpstreamAnswer
		}
	}
	return done, nil
}

func (rq *request) countWAN(n int64) {
	rq.acct.wan.Add(n)
	rq.s.stats.bytesWAN.Add(n)
}

// captureBuffer takes a fill slot and a buffer to store slice i, unless it
// is cached already, nothing may be stored or no slot is free.
func (rq *request) captureBuffer(i int64) ([]byte, bool) {
	s := rq.s
	if rq.st == nil || s.storeFull() || (rq.obj.genKnown && rq.obj.has(i)) {
		return nil, false
	}
	if !s.slots.tryAcquire(rq.ckey, s.fillLimits(rq.S)) {
		return nil, false
	}
	return s.bufs.get(rq.S), true
}

func (rq *request) releaseCapture(buf []byte) {
	rq.s.bufs.put(buf)
	rq.s.slots.release(rq.ckey)
}

// storeCaptured writes a captured slice in the background and then
// releases its buffer and slot.
func (rq *request) storeCaptured(i int64, data []byte) {
	s := rq.s
	gen, ok := rq.captureGen()
	st := rq.st
	if !ok || st == nil {
		rq.releaseCapture(data)
		return
	}
	id, a, ckey := rq.id, rq.acct, rq.ckey
	a.retain()
	started := s.goTracked(func() {
		defer a.release()
		ctx, cancel := context.WithTimeout(s.ctx, s.tm.storeWrite)
		defer cancel()
		if err := st.WriteSlice(ctx, id, gen, i, data); err != nil {
			s.storeError("write slice", err)
		} else {
			a.stored.Add(int64(len(data)))
		}
		s.bufs.put(data)
		s.slots.release(ckey)
	})
	if !started {
		a.release()
		rq.releaseCapture(data)
	}
}

// captureGen records the object (once per request) for captured slices
// and returns its generation.
func (rq *request) captureGen() (uint64, bool) {
	if rq.capGenState != 0 {
		return rq.capGen, rq.capGenState > 0
	}
	rq.capGenState = -1
	if rq.st == nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(rq.ctx, rq.s.tm.storeWrite)
	defer cancel()
	m := rq.meta()
	m.Total, m.Header, m.NoSlice = rq.obj.total, rq.obj.header, true
	gen, err := rq.st.SetMeta(ctx, rq.id, m)
	if err != nil {
		rq.storeFailed("set meta", err)
		return 0, false
	}
	rq.capGen, rq.capGenState = gen, 1
	return gen, true
}

// streamWithoutLength serves a 200 without Content-Length as it comes
// (never stored; the client's Range cannot be honoured).
func (rq *request) streamWithoutLength(resp *http.Response, info respInfo) {
	defer resp.Body.Close()
	h := rq.w.Header()
	for k, vv := range info.header {
		h[k] = slices.Clone(vv)
	}
	if h.Get("Content-Type") == "" {
		h["Content-Type"] = nil
	}
	h.Set(processedByHeader, rq.s.d.InstanceID)
	h.Set(cacheStatusHeader, statusPass)
	rq.cacheStatus = statusPass
	rq.writeHeader(http.StatusOK)
	if !rq.head {
		if err := rq.copyBody(resp.Body, -1); err != nil {
			rq.fail(err)
		}
	}
}

// relay passes an upstream response through: status, end-to-end headers
// and body. Error statuses (≥ 400) are passed unmodified; others get
// X-LanCache-Processed-By and X-Upstream-Cache-Status.
func (rq *request) relay(resp *http.Response, cacheStatus string) {
	defer resp.Body.Close()
	h := rq.w.Header()
	copyResponseHeaders(h, resp.Header)
	if resp.ContentLength >= 0 {
		h.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	if resp.StatusCode < 400 {
		h.Set(processedByHeader, rq.s.d.InstanceID)
		h.Set(cacheStatusHeader, cacheStatus)
	}
	rq.cacheStatus = cacheStatus
	rq.tr.total.Store(max(0, resp.ContentLength))
	rq.writeHeader(resp.StatusCode)
	if rq.head || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return
	}
	if err := rq.copyBody(resp.Body, resp.ContentLength); err != nil {
		rq.fail(err)
	}
}

// copyBody streams an upstream body (n bytes, or to EOF for n < 0).
func (rq *request) copyBody(src io.Reader, n int64) error {
	bp := copyBufs.Get().(*[]byte)
	defer copyBufs.Put(bp)
	buf := *bp
	var done int64
	for n < 0 || done < n {
		want := int64(len(buf))
		if n >= 0 {
			want = min(want, n-done)
		}
		k, err := src.Read(buf[:want])
		if k > 0 {
			done += int64(k)
			rq.countWAN(int64(k))
			if werr := rq.writeBody(buf[:k]); werr != nil {
				return werr
			}
		}
		if errors.Is(err, io.EOF) && (n < 0 || done == n) {
			return nil
		}
		if err != nil {
			return errUpstreamAnswer
		}
	}
	return nil
}
