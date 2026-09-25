package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	cachestore "github.com/hustenreizjuengling/picache/internal/dlcache/store"
)

const (
	// readChunk is the upstream read size of fills (progress granularity).
	readChunk = 64 << 10
	// writeChunk is the largest single write to a client; the write
	// deadline is renewed before each one.
	writeChunk = 256 << 10
)

var (
	errNotSlice    = errors.New("upstream answer is not a slice")
	errStopping    = errors.New("proxy is stopping")
	errFillFailed  = errors.New("fill failed")
	errFillStalled = errors.New("fill stalled")
	errUseStore    = errors.New("slice buffer released after storing")
)

// sliceKey identifies an in-flight fill.
type sliceKey struct {
	store string
	id    string
	idx   int64
}

type fillPhase uint8

const (
	phaseHeaders fillPhase = iota // waiting for the upstream response
	phaseBody                     // valid slice: the buffer is growing
	phaseDone                     // complete, failed or not a slice
)

// expectation is what the creator of a fill knew about the object.
type expectation struct {
	known bool
	total int64
	gen   uint64
}

// fill fetches one slice with one upstream range request. Any number of
// readers stream from its buffer while it grows (sync.Cond); the buffer is
// then written to the store. The fill holds a slot until its buffer is
// released: right after storing (readers switch to the store), or when the
// last reader detached if it could not be stored. Fills continue when
// their clients disconnect.
//
// Two variants share the machinery (collapse.go): a whole-object fill
// (noRange, keyed as slice 0) requests the object without Range, and a
// capture fill has no goroutine of its own: a leading request reads a
// slice of its body into the buffer and stores it.
type fill struct {
	s       *Server
	key     sliceKey
	st      SliceStore // nil: never stored
	size    int64      // slice size
	up      upReq      // request template (Range is added by run)
	meta    cachestore.Meta
	expect  expectation
	bypass  bool
	acct    *acct        // accounting of the request that started the fill
	client  netip.Prefix // slot owner
	owned   bool         // a non-slice answer is handed to the creating request
	noRange bool         // whole-object fill: no Range header

	mu        sync.Mutex
	cond      sync.Cond
	phase     fillPhase
	info      respInfo
	err       error
	handoff   *http.Response
	ownerGone bool
	buf       []byte
	n         int64 // bytes in buf
	stalled   bool
	stored    bool
	gen       uint64 // generation the slice was stored with
	attached  int    // readers that may still read buf
	inWrite   int    // readers currently writing from buf
	freed     bool   // buffer and slot released
	timer     *time.Timer
}

func (f *fill) init() { f.cond.L = &f.mu }

// startFill starts f's goroutine. On failure f is finished with
// errStopping (its creator still detaches as usual).
func (s *Server) startFill(f *fill) bool {
	f.acct.retain()
	s.stats.activeFills.Add(1)
	f.timer = time.AfterFunc(s.tm.stall, f.markStalled)
	if s.goTracked(f.run) {
		return true
	}
	f.finish(respInfo{}, errStopping, nil)
	f.acct.release()
	return false
}

func (f *fill) run() {
	s := f.s
	defer f.acct.release()
	start := f.key.idx * f.size
	q := f.up
	q.header = f.up.header.Clone()
	if !f.noRange {
		q.header.Set("Range", "bytes="+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(start+f.size-1, 10))
	}
	resp, err := s.roundTrip(s.ctx, &q)
	if err != nil {
		s.upstreamError(f.up.host, f.meta.Path, err)
		f.finish(respInfo{}, err, nil)
		return
	}
	info := classifyResponse(resp, f.key.idx, f.size, !f.noRange)
	s.noteRangeOutcome(f.up.host, f.key.id, info)
	if info.kind == kind416 && f.expect.known {
		// The recorded object is shorter than the slice: it changed.
		s.invalidate(f.st, f.key.id, f.expect.gen)
	}
	if info.kind != kindSlice {
		f.finish(info, errNotSlice, resp)
		return
	}
	f.mu.Lock()
	f.info, f.buf, f.phase = info, s.bufs.get(f.size)[:info.length], phaseBody
	f.cond.Broadcast()
	f.mu.Unlock()
	if err := f.readBody(resp.Body); err != nil {
		_ = resp.Body.Close()
		s.upstreamError(f.up.host, f.meta.Path, err)
		f.finish(info, err, nil)
		return
	}
	discardBody(resp)
	f.complete(f.storeSlice())
}

// complete ends a fill whose buffer is complete: stored (readers switch to
// the store with generation gen) or not.
func (f *fill) complete(stored bool, gen uint64) {
	s := f.s
	if !stored {
		s.fills.remove(f)
	}
	f.mu.Lock()
	f.phase, f.stored, f.gen = phaseDone, stored, gen
	f.timer.Stop()
	f.cond.Broadcast()
	f.maybeFreeLocked()
	f.mu.Unlock()
	if stored {
		s.fills.remove(f)
	}
}

// readBody fills the buffer, waking readers after every chunk.
func (f *fill) readBody(body io.Reader) error {
	want := int64(len(f.buf))
	var n int64
	for n < want {
		k, err := body.Read(f.buf[n:min(n+readChunk, want)])
		if k > 0 {
			n += int64(k)
			f.acct.wan.Add(int64(k))
			f.s.stats.bytesWAN.Add(int64(k))
			f.timer.Reset(f.s.tm.stall)
			f.mu.Lock()
			f.n, f.stalled = n, false
			f.cond.Broadcast()
			f.mu.Unlock()
		}
		if err != nil {
			if n == want {
				return nil
			}
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
	return nil
}

// storeSlice records the object (when the creator did not know it with
// this size) and writes the slice. Nothing is stored without a store,
// while the store is full, or when the store refuses.
func (f *fill) storeSlice() (bool, uint64) {
	s := f.s
	if f.st == nil || s.storeFull() {
		return false, 0
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.tm.storeWrite)
	defer cancel()
	setMeta := func() (uint64, error) {
		m := f.meta
		m.Total, m.Header = f.info.total, f.info.header
		return f.st.SetMeta(ctx, f.key.id, m)
	}
	gen := f.expect.gen
	needMeta := !f.expect.known || f.expect.total != f.info.total || f.bypass
	for attempt := 0; ; attempt++ {
		if needMeta {
			g, err := setMeta()
			if err != nil {
				s.storeError("set meta", err)
				return false, 0
			}
			gen = g
		}
		err := f.st.WriteSlice(ctx, f.key.id, gen, f.info.dataIdx, f.buf)
		if err == nil {
			break
		}
		if errors.Is(err, cachestore.ErrStale) && !needMeta && attempt == 0 {
			// Evicted or re-recorded while the slice was fetched: record
			// the object again.
			needMeta = true
			continue
		}
		s.storeError("write slice", err)
		return false, 0
	}
	f.acct.stored.Add(int64(len(f.buf)))
	return true, gen
}

// finish ends a fill that failed or got no slice. A non-slice response is
// handed to the owning request if it still waits, else discarded; a range
// failure handed over announces the owner as the object's leader before
// the fill's other readers wake up (they follow it).
func (f *fill) finish(info respInfo, err error, resp *http.Response) {
	f.s.fills.remove(f)
	f.mu.Lock()
	if f.phase == phaseHeaders {
		f.info = info
	}
	f.err, f.phase = err, phaseDone
	f.timer.Stop()
	if resp != nil && f.owned && !f.ownerGone {
		if info.kind == kindRangeFail {
			resp.Body = f.s.withLead(f, info, resp.Body)
		}
		f.handoff, resp = resp, nil
	}
	f.cond.Broadcast()
	f.maybeFreeLocked()
	f.mu.Unlock()
	if resp != nil {
		discardBody(resp)
	}
}

// maybeFreeLocked releases the buffer and the slot once nobody needs them.
func (f *fill) maybeFreeLocked() {
	if f.freed || f.phase != phaseDone || f.inWrite > 0 || (!f.stored && f.attached > 0) {
		return
	}
	f.freed = true
	if f.buf != nil {
		f.s.bufs.put(f.buf)
		f.buf = nil
	}
	f.s.slots.release(f.client)
	f.s.stats.activeFills.Add(-1)
}

func (f *fill) markStalled() {
	f.mu.Lock()
	if f.phase != phaseDone {
		f.stalled = true
		f.cond.Broadcast()
	}
	f.mu.Unlock()
}

func (f *fill) wake() {
	f.mu.Lock()
	f.cond.Broadcast()
	f.mu.Unlock()
}

// attach registers a reader. It fails for a finished fill whose buffer is
// gone without having been stored.
func (f *fill) attach() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.freed && !f.stored {
		return false
	}
	f.attached++
	return true
}

func (f *fill) detach() {
	f.mu.Lock()
	f.attached--
	f.maybeFreeLocked()
	f.mu.Unlock()
}

// takeHandoff returns the non-slice response handed to the owner (with its
// classification), if any, and marks the owner gone: a later answer is
// discarded.
func (f *fill) takeHandoff() (*http.Response, respInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.handoff
	f.handoff, f.ownerGone = nil, true
	return r, f.info
}

// waitHeaders waits until the fill has an upstream answer. It returns
// errFillStalled when nothing arrived within the stall time, errClientGone
// when ctx ends, or the fill's transport error.
func (f *fill) waitHeaders(ctx context.Context) (respInfo, error) {
	stop := context.AfterFunc(ctx, f.wake)
	defer stop()
	f.mu.Lock()
	defer f.mu.Unlock()
	for f.phase == phaseHeaders {
		if ctx.Err() != nil {
			return respInfo{}, errClientGone
		}
		if f.stalled {
			return respInfo{}, errFillStalled
		}
		f.cond.Wait()
	}
	if f.info.kind == kindFailed {
		return f.info, f.err
	}
	return f.info, nil
}

// read writes bytes [off, off+n) of slice idx (offsets within the slice)
// of an object of size total to the client. It returns the bytes written
// and nil, or errUseStore with the generation to read the rest from the
// store, errFillFailed, errFillStalled, errChanged (the object's size
// differs), errClientGone or a client write error.
func (f *fill) read(rq *request, idx, off, n, total int64) (int64, uint64, error) {
	stop := context.AfterFunc(rq.ctx, f.wake)
	defer stop()
	var written int64
	for written < n {
		pos := off + written
		f.mu.Lock()
		if err := f.awaitLocked(rq.ctx, idx, pos, total); err != nil {
			gen := f.gen
			f.mu.Unlock()
			return written, gen, err
		}
		end := min(f.n, off+n, pos+writeChunk)
		chunk := f.buf[pos:end]
		f.inWrite++
		f.mu.Unlock()
		err := rq.writeBody(chunk)
		f.mu.Lock()
		f.inWrite--
		f.maybeFreeLocked()
		f.mu.Unlock()
		if err != nil {
			return written, 0, err
		}
		written += end - pos
	}
	return written, 0, nil
}

// awaitLocked waits until byte pos of the buffer is available or returns
// why it will not be.
func (f *fill) awaitLocked(ctx context.Context, idx, pos, total int64) error {
	for {
		switch {
		case ctx.Err() != nil:
			return errClientGone
		case f.phase == phaseHeaders:
			if f.stalled {
				return errFillStalled
			}
		case f.info.kind == kind416:
			return errChanged
		case f.info.kind != kindSlice:
			return errFillFailed
		case f.info.total != total || f.info.dataIdx != idx:
			return errChanged
		case f.freed:
			if f.stored {
				return errUseStore
			}
			return errFillFailed
		case f.n > pos:
			return nil
		case f.phase == phaseDone:
			return errFillFailed
		case f.stalled:
			return errFillStalled
		}
		f.cond.Wait()
	}
}

// usable reports whether the fill is expected to provide slice i of an
// object of total bytes (for the predicted cache status).
func (f *fill) usable(i, total int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.phase == phaseHeaders:
		return !f.stalled
	case f.info.kind != kindSlice || f.info.dataIdx != i || f.info.total != total:
		return false
	case f.phase == phaseDone:
		return f.stored
	}
	return !f.stalled
}

// fillTable holds the in-flight fills (bounded by the fill slots).
type fillTable struct {
	mu sync.Mutex
	m  map[sliceKey]*fill
}

func (t *fillTable) get(k sliceKey) *fill {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[k]
}

// ofObject returns the in-flight fills of object id of store by slice.
func (t *fillTable) ofObject(store, id string) map[int64]*fill {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out map[int64]*fill
	for k, f := range t.m {
		if k.store == store && k.id == id {
			if out == nil {
				out = make(map[int64]*fill)
			}
			out[k.idx] = f
		}
	}
	return out
}

// insert adds f unless a fill for its key exists, which is returned.
func (t *fillTable) insert(f *fill) *fill {
	t.mu.Lock()
	defer t.mu.Unlock()
	if old := t.m[f.key]; old != nil {
		return old
	}
	if t.m == nil {
		t.m = make(map[sliceKey]*fill)
	}
	t.m[f.key] = f
	return nil
}

func (t *fillTable) remove(f *fill) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m[f.key] == f {
		delete(t.m, f.key)
	}
}

// join attaches to the in-flight fill for k, if any.
func (t *fillTable) join(k sliceKey) *fill {
	if f := t.get(k); f != nil && f.attach() {
		return f
	}
	return nil
}

// invalidate removes object id from st if it still has generation gen: a
// recorded object that changed upstream (ARCHITECTURE 8.2 step 10, 416).
func (s *Server) invalidate(st SliceStore, id string, gen uint64) {
	if st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.tm.storeWrite)
	defer cancel()
	if h, ok, err := st.Head(ctx, id); err != nil || !ok || h.Gen != gen {
		return
	}
	if err := st.Invalidate(ctx, id, "invalidated"); err != nil {
		s.storeError("invalidate", err)
	}
}

// noteRangeOutcome feeds the no-slice detection of the host.
func (s *Server) noteRangeOutcome(host, id string, info respInfo) {
	switch {
	case info.kind == kindSlice && info.status == http.StatusPartialContent:
		s.noslice.success(host)
	case info.rangeFailure:
		s.noslice.failure(host, id, time.Now())
	}
}
