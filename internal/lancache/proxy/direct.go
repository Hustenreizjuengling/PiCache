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
// free (hosts without range support, range failures). A capturing source
// may lead (collapse.go): its captured slices are fills other requests
// stream, and it announces its progress.
type directSource struct {
	body      io.ReadCloser
	off       int64 // absolute offset of the next body byte
	end       int64 // last offset the body provides
	capture   bool
	failed    bool
	lead      *objFetch // nil unless this source leads
	announced int64     // slices < announced were passed (capture decided)
}

func newDirect(body io.ReadCloser, start, end int64, capture bool) *directSource {
	d := &directSource{body: body, off: start, end: end, capture: capture}
	if lb, ok := body.(*leadBody); ok {
		d.lead = lb.lead
	}
	return d
}

// newDirect wraps an upstream body; a capturing source announces itself as
// the object's leader unless it already carries an announcement or the
// object has a leader.
func (rq *request) newDirect(body io.ReadCloser, start, end int64, capture bool) *directSource {
	d := newDirect(body, start, end, capture)
	if d.lead == nil && capture && !rq.bypass && rq.st != nil && !rq.s.storeFull() {
		d.lead = rq.s.leaders.lead(rq.objKey(), start, end, rq.obj.total, rq.S, rq.obj.header)
	}
	return d
}

func (d *directSource) covers(pos int64) bool { return !d.failed && pos >= d.off && pos <= d.end }

func (d *directSource) close() {
	d.lead.end()
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
		if i >= d.announced {
			d.announced = i + 1
			var f *fill
			var gen uint64
			if d.capture && d.off == sStart && sEnd <= d.end {
				f, gen = rq.captureFill(i, sEnd-sStart+1)
			}
			if d.lead != nil {
				d.lead.passed(i, f != nil || rq.sliceAvailable(i))
			}
			if f != nil {
				n, err := d.captureInto(rq, f, gen, cur, end)
				written += n
				if err != nil {
					return written, err
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

// captureInto reads the slice of capture fill f (the next body bytes),
// starts storing it and writes the client's part of it (from cur, at most
// up to end). The slice is stored even when the client is gone.
func (d *directSource) captureInto(rq *request, f *fill, gen uint64, cur, end int64) (int64, error) {
	if err := d.readInto(rq, f); err != nil {
		f.finish(f.info, err, nil)
		f.acct.release()
		return 0, err
	}
	sStart := f.key.idx * rq.S
	sEnd := sStart + int64(len(f.buf)) - 1
	f.mu.Lock()
	f.inWrite++ // the buffer stays while the client's part is written
	f.mu.Unlock()
	rq.s.storeCapture(f, gen)
	var written int64
	var err error
	if cur <= sEnd {
		k := min(end, sEnd) - cur + 1
		if err = rq.writeBody(f.buf[cur-sStart : cur-sStart+k]); err == nil {
			written = k
		}
	}
	f.mu.Lock()
	f.inWrite--
	f.maybeFreeLocked()
	f.mu.Unlock()
	return written, err
}

// readInto fills the buffer of capture fill f from the body, waking the
// fill's readers after every chunk.
func (d *directSource) readInto(rq *request, f *fill) error {
	want := int64(len(f.buf))
	var n int64
	for n < want {
		k, err := d.body.Read(f.buf[n:min(n+readChunk, want)])
		if k > 0 {
			n += int64(k)
			d.off += int64(k)
			rq.countWAN(int64(k))
			d.lead.bump()
			f.timer.Reset(rq.s.tm.stall)
			f.mu.Lock()
			f.n, f.stalled = n, false
			f.cond.Broadcast()
			f.mu.Unlock()
		}
		if err != nil && n < want {
			d.failed = true
			return errUpstreamAnswer
		}
	}
	return nil
}

// skip reads and drops n body bytes.
func (d *directSource) skip(rq *request, n int64) error {
	m, err := io.CopyN(io.Discard, d.body, n)
	d.off += m
	rq.countWAN(m)
	d.lead.bump()
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
			d.lead.bump()
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

// storeCapture writes the slice of capture fill f (generation gen) in the
// background and then completes the fill (readers switch to the store).
func (s *Server) storeCapture(f *fill, gen uint64) {
	started := s.goTracked(func() {
		defer f.acct.release()
		ctx, cancel := context.WithTimeout(s.ctx, s.tm.storeWrite)
		err := f.st.WriteSlice(ctx, f.key.id, gen, f.key.idx, f.buf)
		cancel()
		if err != nil {
			s.storeError("write slice", err)
		} else {
			f.acct.stored.Add(int64(len(f.buf)))
		}
		f.complete(err == nil, gen)
	})
	if !started {
		f.finish(f.info, errStopping, nil)
		f.acct.release()
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
