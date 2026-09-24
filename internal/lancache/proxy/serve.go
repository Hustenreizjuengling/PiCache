package proxy

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	cachestore "github.com/hustenreizjuengling/picache/internal/lancache/store"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// maxRestarts bounds how often a response is re-planned because the object
// changed before anything was sent.
const maxRestarts = 2

var (
	// errChanged: the object's size differs from the one the response
	// was planned with.
	errChanged = errors.New("object changed upstream")
	// errUpstreamAnswer: an unusable upstream answer or body.
	errUpstreamAnswer = errors.New("unusable upstream response")
	// errDone: the response was completed another way (an upstream status
	// relayed, or the request passed through) before anything was sent.
	errDone     = errors.New("response completed")
	errMissing  = errors.New("slice not cached")
	errDiskRead = errors.New("cached slice unreadable")
)

// cache is the cache-path state of a request.
type cache struct {
	st      SliceStore // nil after the store closed mid-request
	storeID string
	S       int64 // slice size
	id      string
	rh      *rangeHeader
	// useSlices is false for hosts marked no-slice: missing data is then
	// fetched without Range.
	useSlices bool

	obj  object
	plan plan

	pending     *fill         // attached fill of the first fetch
	ds          *directSource // current direct upstream body
	slotWaited  bool          // waited once for a fill slot: no more waits
	failIdx     int64         // slice whose sources failed failCount times
	failCount   int
	diskSkip    int64 // slice whose cached copy could not be read
	lastRefresh int64 // slice index of the last head refresh
	capGen      uint64
	capGenState int8 // 0 unknown, 1 ok, -1 unavailable
	// forceFirst re-plans from a fresh first fetch (the size changed).
	forceFirst bool
	// own holds the recent slices whose fills this request started: their
	// bytes count as fetched upstream, not as hits, also when read back
	// from the store (and a bypass request may read them back).
	own    [ownRing]int64
	ownLen int
}

// object is what the request knows about the cached object.
type object struct {
	known    bool // recorded in the store when the request started
	total    int64
	header   http.Header // stored headers to replay
	gen      uint64
	genKnown bool
	present  []uint64 // slice bitmap of the last head
}

func (o *object) has(i int64) bool {
	h := cachestore.ObjectHead{Present: o.present}
	return h.Has(i)
}

// serveCache serves a canonical GET/HEAD of an enabled service from the
// store and fills (ARCHITECTURE 8.2 steps 10–15).
func (rq *request) serveCache(st SliceStore) {
	S := st.SliceSize()
	id := cachestore.ObjectID(rq.service, rq.path)
	if S <= 0 || !cachestore.ValidObjectID(id) {
		rq.passThrough()
		return
	}
	rq.cache = cache{
		st: st, storeID: st.ID(), S: S, id: id,
		rh:        parseRange(rq.r.Header),
		useSlices: rq.s.noslice.useSlicing(rq.host, time.Now()),
	}
	for attempt := 0; ; attempt++ {
		rq.failIdx, rq.failCount, rq.diskSkip, rq.lastRefresh, rq.capGenState = -1, 0, -1, -1, 0
		err := rq.serveObject()
		if err == nil || errors.Is(err, errDone) {
			return
		}
		if errors.Is(err, errChanged) && rq.status == 0 && attempt < maxRestarts {
			rq.releaseSources()
			rq.forceFirst = true
			continue
		}
		rq.fail(err)
		return
	}
}

func (rq *request) serveObject() error {
	rq.obj = object{}
	if !rq.bypass && !rq.forceFirst && rq.st != nil {
		h, ok, err := rq.st.Head(rq.ctx, rq.id)
		switch {
		case err != nil:
			rq.storeFailed("head", err)
		case ok && h.Total > 0 && h.Total <= cachestore.MaxTotal && (h.SliceSize == 0 || h.SliceSize == rq.S):
			rq.obj = object{known: true, total: h.Total, header: headHeader(&h), gen: h.Gen, genKnown: true, present: h.Present}
		}
	}
	if !rq.obj.known {
		if done, err := rq.firstFetch(); done || err != nil {
			return err
		}
	}
	rq.plan = makePlan(rq.rh, rq.r.Header, rq.obj.total, rq.obj.header)
	rq.predicted = rq.predictStatus()
	rq.tr.total.Store(rq.plan.contentLength)
	if rq.head || len(rq.plan.ranges) == 0 {
		rq.commit()
		return nil
	}
	return rq.servePlan()
}

// headHeader returns the stored headers of h (with its content type and
// last-modified when the header set lacks them).
func headHeader(h *cachestore.ObjectHead) http.Header {
	out := h.Header.Clone()
	if out == nil {
		out = http.Header{}
	}
	if out.Get("Content-Type") == "" && h.ContentType != "" {
		out.Set("Content-Type", h.ContentType)
	}
	if out.Get("Last-Modified") == "" && h.LastModified != "" {
		out.Set("Last-Modified", h.LastModified)
	}
	return out
}

func (rq *request) storeFailed(op string, err error) {
	if errors.Is(err, cachestore.ErrClosed) {
		rq.st = nil
		return
	}
	rq.s.storeError(op, err)
}

// firstFetch learns size and headers of an unknown object from the first
// slice it needs. done=true means the response was completed another way
// (pass-through, relayed upstream status, stream without length).
func (rq *request) firstFetch() (bool, error) {
	idx := firstSliceIndex(rq.rh, rq.r.Header, rq.S)
	for attempt := 0; ; attempt++ {
		resp, info, capture, err := rq.firstAnswer(idx)
		switch {
		case err != nil && attempt == 0 && retryable(err):
			continue
		case err != nil:
			return false, err
		case resp == nil && info.kind == kindSlice:
			return false, nil // streaming from a fill (rq.pending)
		case info.kind == kind416 && attempt < 2:
			// The object is shorter than the slice asked for: learn its
			// size from slice 0.
			discardBody(resp)
			idx = 0
			continue
		}
		return rq.useFirstAnswer(resp, info, capture), nil
	}
}

// retryable reports whether a failed upstream fetch is worth another try.
func retryable(err error) bool {
	return !errors.Is(err, errClientGone) && !errors.Is(err, netutil.ErrForbiddenDestination) &&
		!errors.Is(err, errBadRedirect) && !errors.Is(err, errTooManyRedirects) && !errors.Is(err, errStopping) &&
		!errors.Is(err, errChanged) && !errors.Is(err, errDone)
}

// firstAnswer obtains the first answer for slice idx: from a fill (joined
// or created; resp == nil for a valid slice), from the response a created
// fill hands over when it is no slice, or from a direct request when no
// fill slot is free or the fill stalls. capture reports whether complete
// slices of a returned body may be stored.
func (rq *request) firstAnswer(idx int64) (*http.Response, respInfo, bool, error) {
	if !rq.useSlices {
		resp, err := rq.upstreamGet(-1, 0)
		if err != nil {
			return nil, respInfo{}, false, err
		}
		return resp, classifyResponse(resp, 0, rq.S, false), true, nil
	}
	if f, created, ok := rq.acquireFill(idx); ok {
		info, err := f.waitHeaders(rq.ctx)
		var resp *http.Response
		if created {
			resp, _ = f.takeHandoff()
		}
		switch {
		case err == nil && info.kind == kindSlice:
			rq.obj.total, rq.obj.header = info.total, info.header
			if info.dataIdx == idx {
				rq.pending = f
			} else {
				f.detach()
			}
			return nil, info, false, nil
		case err == nil && resp != nil:
			f.detach()
			return resp, info, true, nil
		case err == nil:
			// Joined another request's fill whose answer is no slice.
			f.detach()
			return nil, respInfo{kind: kindPassClient}, false, nil
		}
		if resp != nil {
			discardBody(resp)
		}
		f.detach()
		if !errors.Is(err, errFillStalled) {
			return nil, respInfo{}, false, err
		}
	}
	if rq.ctx.Err() != nil {
		return nil, respInfo{}, false, errClientGone
	}
	start := idx * rq.S
	resp, err := rq.upstreamGet(start, start+rq.S-1)
	if err != nil {
		return nil, respInfo{}, false, err
	}
	info := classifyResponse(resp, idx, rq.S, true)
	rq.s.noteRangeOutcome(rq.host, rq.id, info)
	return resp, info, info.kind == kindRangeFail, nil
}

// useFirstAnswer continues with a first answer that came with a body.
// It returns true when the response was completed.
func (rq *request) useFirstAnswer(resp *http.Response, info respInfo, capture bool) bool {
	switch info.kind {
	case kindSlice, kindRangeFail:
		rq.obj.total, rq.obj.header = info.total, info.header
		rq.ds = newDirect(resp.Body, info.start, info.start+info.length-1, capture)
		return false
	case kindNoLength:
		rq.streamWithoutLength(resp, info)
	case kindUpstream:
		rq.relay(resp, statusPass)
	default: // kindPassClient: not cacheable, or a 416 for slice 0
		if resp != nil {
			discardBody(resp)
		}
		rq.forward(info.status == http.StatusRequestedRangeNotSatisfiable)
	}
	return true
}

// upstreamGet requests bytes [from, to] of the object with fill headers
// (from < 0: without Range).
func (rq *request) upstreamGet(from, to int64) (*http.Response, error) {
	h := rq.fillHeader.Clone()
	if from >= 0 {
		h.Set("Range", "bytes="+strconv.FormatInt(from, 10)+"-"+strconv.FormatInt(to, 10))
	}
	resp, err := rq.s.roundTrip(rq.ctx, &upReq{method: http.MethodGet, target: rq.target, host: rq.host, header: h, length: -1})
	if err != nil {
		rq.s.upstreamError(rq.host, rq.path, err)
		return nil, err
	}
	return resp, nil
}

// predictStatus is the X-Upstream-Cache-Status of the planned response.
func (rq *request) predictStatus() string {
	if rq.bypass {
		return statusBypass
	}
	if !rq.obj.known {
		return statusMiss
	}
	var have, all int64
	for _, r := range rq.plan.ranges {
		for i := r.start / rq.S; i <= r.end/rq.S; i++ {
			all++
			if rq.obj.has(i) {
				have++
			}
		}
	}
	switch {
	case have == all:
		return statusHit
	case have == 0:
		return statusMiss
	}
	return statusPartial
}

// commit sends the planned status and headers (once, right before the
// first body byte).
func (rq *request) commit() {
	if rq.status != 0 || rq.plan.status == 0 {
		return
	}
	p := &rq.plan
	h := rq.w.Header()
	switch p.status {
	case http.StatusNotModified:
		if lm := rq.obj.header.Get("Last-Modified"); lm != "" {
			h.Set("Last-Modified", lm)
		}
	case http.StatusRequestedRangeNotSatisfiable:
		h.Set("Content-Range", "bytes */"+strconv.FormatInt(p.total, 10))
		h.Set("Content-Length", "0")
	default:
		for k, vv := range rq.obj.header {
			h[k] = slices.Clone(vv)
		}
		h.Del("Etag")
		h.Del("Set-Cookie")
		switch {
		case p.multipart:
			h.Set("Content-Type", "multipart/byteranges; boundary="+p.boundary)
		case h.Get("Content-Type") == "":
			h["Content-Type"] = nil // no content sniffing
		}
		if p.status == http.StatusPartialContent && !p.multipart {
			h.Set("Content-Range", contentRange(p.ranges[0], p.total))
		}
		h.Set("Content-Length", strconv.FormatInt(p.contentLength, 10))
	}
	h.Set("Accept-Ranges", "bytes")
	h.Set(processedByHeader, rq.s.d.InstanceID)
	h.Set(cacheStatusHeader, rq.predicted)
	rq.writeHeader(p.status)
}

// servePlan writes the planned body ranges. A failing source is retried
// once through the other sources and once directly (ARCHITECTURE 8.2
// step 12); the response is aborted only if that fails too.
func (rq *request) servePlan() error {
	p := &rq.plan
	for i, r := range p.ranges {
		if p.multipart {
			if err := rq.writeBody([]byte(p.parts[i])); err != nil {
				return err
			}
		}
		for pos := r.start; pos <= r.end; {
			n, err := rq.serveAt(pos, r.end)
			pos += n
			if err != nil && !rq.retry(pos, err) {
				return err
			}
		}
	}
	if p.multipart {
		return rq.writeBody([]byte(p.trailer))
	}
	return nil
}

// retry records a failed source at pos and reports whether another source
// may be tried: the first failure of a slice goes back to the store and
// fills, the second one directly upstream (serveAt), and a failure of that
// direct fetch ends the response.
func (rq *request) retry(pos int64, err error) bool {
	if !retryable(err) || rq.ctx.Err() != nil {
		return false
	}
	i := pos / rq.S
	if rq.failIdx == i && rq.failCount >= 2 {
		return false
	}
	rq.closeDirect()
	rq.noteFail(i, 1)
	return true
}

// serveAt writes bytes from pos (at most up to end) from the best source:
// the current direct body, the first fetch's fill, the store, an in-flight
// fill, a new fill, or a direct request. It returns the bytes written;
// zero bytes without an error means "try again" (the state changed).
func (rq *request) serveAt(pos, end int64) (int64, error) {
	if ds := rq.ds; ds != nil {
		if ds.covers(pos) {
			return ds.serve(rq, pos, end)
		}
		rq.closeDirect()
	}
	S := rq.S
	i := pos / S
	e := min(end, min((i+1)*S, rq.obj.total)-1)
	if f := rq.pending; f != nil {
		rq.pending = nil
		if f.key.idx == i {
			return rq.fromFill(f, i, pos, e, end)
		}
		f.detach()
	}
	if rq.failIdx == i && rq.failCount >= 2 {
		// Mid-stream failure after a retry: the rest of the range directly.
		return rq.openDirect(pos, end, end, true, false)
	}
	if n, ok, err := rq.tryDisk(i, pos, e, false); ok {
		rq.consumed(i, pos, n, end, err)
		return n, err
	}
	if !rq.useSlices {
		return rq.openDirect(pos, end, 0, false, true)
	}
	if f := rq.s.fills.join(rq.key(i)); f != nil {
		return rq.fromFill(f, i, pos, e, end)
	}
	if n, ok, err := rq.tryDisk(i, pos, e, true); ok {
		rq.consumed(i, pos, n, end, err)
		return n, err
	}
	f, _, ok := rq.acquireFill(i)
	if !ok {
		// No fill slot: this slice directly, uncached.
		return rq.openDirect(pos, end, e, true, false)
	}
	return rq.fromFill(f, i, pos, e, end)
}

// tryDisk serves [pos, e] of slice i from the store if it is cached
// (refresh: re-read the object head first, once per slice). ok=false means
// another source must be used. A bypass request reads back only the slices
// it fetched itself.
func (rq *request) tryDisk(i, pos, e int64, refresh bool) (int64, bool, error) {
	own := rq.isOwn(i)
	if rq.st == nil || rq.diskSkip == i || (rq.bypass && !own) {
		return 0, false, nil
	}
	if refresh && rq.lastRefresh != i {
		rq.lastRefresh = i
		rq.refreshHead()
	}
	if !rq.obj.genKnown || !rq.obj.has(i) {
		return 0, false, nil
	}
	n, err := rq.fromDisk(rq.obj.gen, i, pos, e, !own)
	switch {
	case err == nil:
		return n, true, nil
	case errors.Is(err, errMissing):
		return 0, false, nil
	case errors.Is(err, errDiskRead):
		rq.diskSkip = i
		return n, n > 0, nil
	}
	return n, true, err
}

// refreshHead re-reads the object head (slices stored meanwhile, the
// generation of an object recorded during this request).
func (rq *request) refreshHead() {
	if rq.st == nil {
		return
	}
	h, ok, err := rq.st.Head(rq.ctx, rq.id)
	if err != nil {
		rq.storeFailed("head", err)
		return
	}
	if ok && h.Total == rq.obj.total {
		rq.obj.gen, rq.obj.genKnown, rq.obj.present = h.Gen, true, h.Present
	}
}

// afterStale re-reads the head after the store reported a stale
// generation: errChanged if the object now has another size, else
// errMissing (it was evicted or recorded again with the same size: the
// slice is fetched again or read with the new generation).
func (rq *request) afterStale() error {
	h, ok, err := rq.st.Head(rq.ctx, rq.id)
	switch {
	case err != nil:
		rq.storeFailed("head", err)
		return errMissing
	case !ok:
		rq.obj.genKnown, rq.obj.present = false, nil
		return errMissing
	case h.Total != rq.obj.total:
		return errChanged
	}
	rq.obj.gen, rq.obj.genKnown, rq.obj.present = h.Gen, true, h.Present
	return errMissing
}

// fromDisk writes [pos, e] of cached slice i (generation gen) with
// SliceReader.WriteRange on the unwrapped ResponseWriter (sendfile).
// hit=false for bytes this request fetched itself (already counted as WAN).
func (rq *request) fromDisk(gen uint64, i, pos, e int64, hit bool) (int64, error) {
	sr, err := rq.st.ReadSlice(rq.ctx, rq.id, gen, i)
	if err != nil {
		switch {
		case errors.Is(err, cachestore.ErrStale):
			return 0, rq.afterStale()
		case errors.Is(err, cachestore.ErrClosed):
			rq.st = nil
		case !errors.Is(err, cachestore.ErrSliceMissing):
			rq.s.storeError("read slice", err)
		}
		return 0, errMissing
	}
	defer sr.Close()
	if sr.Size() != sliceLen(i, rq.S, rq.obj.total) {
		return 0, errMissing
	}
	rq.commit()
	off, n := pos-i*rq.S, e-pos+1
	var done int64
	for done < n {
		k := min(n-done, writeChunk)
		_ = rq.rc.SetWriteDeadline(time.Now().Add(rq.s.tm.write))
		m, err := sr.WriteRange(rq.w, off+done, k)
		done += m
		rq.acct.sent.Add(m)
		if hit {
			rq.acct.hit.Add(m)
			rq.s.stats.bytesHit.Add(m)
		}
		if err == nil && m < k {
			err = errDiskRead
		}
		if err != nil {
			if rq.ctx.Err() != nil || rq.rc.Flush() != nil {
				return done, errClientGone
			}
			return done, errDiskRead
		}
	}
	return done, nil
}

// fromFill streams [pos, e] of slice i from fill f (attached; detached
// here). Failures are recorded so the next attempt retries once and then
// goes direct; a usable non-slice answer of a fill this request created is
// continued with (ARCHITECTURE 8.2 step 10 b).
func (rq *request) fromFill(f *fill, i, pos, e, end int64) (int64, error) {
	own := f.acct == rq.acct
	n, gen, err := f.read(rq, i, pos-i*rq.S, e-pos+1, rq.obj.total)
	var resp *http.Response
	var info respInfo
	if own {
		resp, info = f.takeHandoff()
	}
	f.detach()
	if !own {
		rq.acct.hit.Add(n)
		rq.s.stats.bytesHit.Add(n)
	}
	if resp != nil {
		if errors.Is(err, errFillFailed) {
			return n, rq.useHandoff(resp, info, i)
		}
		discardBody(resp)
	}
	switch {
	case errors.Is(err, errUseStore) && rq.st != nil:
		m, derr := rq.fromDisk(gen, i, pos+n, e, !own)
		n += m
		switch {
		case derr == nil:
			err = nil
		case errors.Is(derr, errMissing), errors.Is(derr, errDiskRead):
			rq.noteFail(i, 1)
			return n, nil
		default:
			return n, derr
		}
	case errors.Is(err, errUseStore), errors.Is(err, errFillFailed):
		rq.noteFail(i, 1)
		return n, nil
	case errors.Is(err, errFillStalled):
		rq.noteFail(i, 2)
		return n, nil
	case err != nil:
		return n, err
	}
	rq.consumed(i, pos, n, end, nil)
	return n, nil
}

// useHandoff continues with the non-slice answer of a demand fill this
// request created for slice i of an object of known size: a range failure
// of the same size is streamed from its actual start (complete slices are
// stored), a different size is recorded, and before anything was sent an
// upstream status is relayed and an uncacheable answer passed through.
func (rq *request) useHandoff(resp *http.Response, info respInfo, i int64) error {
	switch {
	case info.kind == kindRangeFail && info.total == rq.obj.total:
		rq.closeDirect()
		rq.ds = newDirect(resp.Body, info.start, info.start+info.length-1, true)
		rq.noteFail(i, 1) // bounds the attempts if the body does not cover the position
		return nil
	case info.kind == kindRangeFail:
		discardBody(resp)
		rq.recordNewSize(info.total, info.header)
		return errChanged
	case info.kind == kindUpstream && rq.status == 0:
		rq.relay(resp, statusPass)
		return errDone
	case info.kind == kindPassClient && rq.status == 0:
		discardBody(resp)
		rq.passThrough()
		return errDone
	}
	discardBody(resp)
	rq.noteFail(i, 1)
	return nil
}

func (rq *request) noteFail(i int64, weight int) {
	if rq.failIdx != i {
		rq.failIdx, rq.failCount = i, 0
	}
	rq.failCount += weight
}

// consumed starts read-ahead once the client got a full slice from the
// store or a fill.
func (rq *request) consumed(i, pos, n, end int64, err error) {
	if err == nil && pos == i*rq.S && pos+n == min((i+1)*rq.S, rq.obj.total) {
		rq.readAhead(i, end/rq.S)
	}
}

// readAhead starts fills for up to readAheadSlices uncached slices after
// slice i (not beyond slice last of the current range). Slots are only
// taken if free. Nothing is read ahead while nothing can be stored: the
// data would be fetched twice.
func (rq *request) readAhead(i, last int64) {
	s := rq.s
	n := int64(s.settings().Cache.ReadAheadSlices)
	if n <= 0 || i >= last || rq.ctx.Err() != nil || !rq.useSlices || rq.st == nil || s.storeFull() {
		return
	}
	last = min(last, i+n)
	wanted := func(j int64) bool {
		return !rq.isOwn(j) && (rq.bypass || !rq.obj.genKnown || !rq.obj.has(j))
	}
	need := false
	for j := i + 1; j <= last && !need; j++ {
		need = wanted(j)
	}
	if !need {
		return
	}
	if !rq.bypass {
		rq.refreshHead()
	}
	lim := s.fillLimits(rq.S)
	for j := i + 1; j <= last; j++ {
		if !wanted(j) || s.fills.get(rq.key(j)) != nil {
			continue
		}
		if !s.slots.tryAcquire(rq.ckey, lim) {
			return
		}
		f := rq.newFill(j, false)
		if s.fills.insert(f) != nil {
			s.slots.release(rq.ckey)
			continue
		}
		if s.startFill(f) {
			rq.markOwn(j)
		}
	}
}

func (rq *request) key(idx int64) sliceKey {
	return sliceKey{store: rq.storeID, id: rq.id, idx: idx}
}

// newFill prepares a fill of slice idx for this request (owned: a demand
// fill whose non-slice answer is handed to this request).
func (rq *request) newFill(idx int64, owned bool) *fill {
	f := &fill{
		s:      rq.s,
		key:    rq.key(idx),
		st:     rq.st,
		size:   rq.S,
		up:     upReq{method: http.MethodGet, target: rq.target, host: rq.host, header: rq.fillHeader, length: -1},
		meta:   rq.meta(),
		expect: expectation{known: rq.obj.genKnown, total: rq.obj.total, gen: rq.obj.gen},
		bypass: rq.bypass,
		acct:   rq.acct,
		client: rq.ckey,
		owned:  owned,
	}
	f.init()
	return f
}

// acquireFill joins the in-flight fill of slice idx or starts a demand
// fill (waiting up to 2 s for a slot, once per request). created reports a
// new fill; ok=false means no fill is available.
func (rq *request) acquireFill(idx int64) (f *fill, created, ok bool) {
	s := rq.s
	for range 2 {
		if f := s.fills.join(rq.key(idx)); f != nil {
			return f, false, true
		}
		if rq.ctx.Err() != nil {
			return nil, false, false // no new slices for a gone client
		}
		wait := s.tm.demandWait
		if rq.slotWaited {
			wait = 0
		}
		if !s.slots.acquire(rq.ctx, rq.ckey, s.fillLimits(rq.S), wait) {
			rq.slotWaited = true
			return nil, false, false
		}
		f := rq.newFill(idx, true)
		f.attached = 1
		if s.fills.insert(f) != nil {
			s.slots.release(rq.ckey)
			continue
		}
		if !s.startFill(f) {
			f.detach()
			return nil, false, false
		}
		rq.markOwn(idx)
		return f, true, true
	}
	return nil, false, false
}

// openDirect fetches [pos, fetchTo] directly (useRange=false: the whole
// object) and serves from it up to end.
func (rq *request) openDirect(pos, end, fetchTo int64, useRange, capture bool) (int64, error) {
	if rq.ctx.Err() != nil {
		return 0, errClientGone
	}
	ds, err := rq.direct(pos, fetchTo, useRange, capture)
	if err != nil {
		return 0, err
	}
	rq.ds = ds
	return ds.serve(rq, pos, end)
}

// direct requests object bytes from pos and checks the answer against the
// planned size. A different size is recorded (new generation) and
// reported as errChanged, as is a 416 (a recorded object is invalidated).
// Before anything was sent, an error status is relayed and an encoded
// answer passed through (errDone).
func (rq *request) direct(pos, to int64, useRange, capture bool) (*directSource, error) {
	from := int64(-1)
	if useRange {
		from = pos
	}
	resp, err := rq.upstreamGet(from, to)
	if err != nil {
		return nil, err
	}
	T := rq.obj.total
	fail := func(err error) (*directSource, error) {
		discardBody(resp)
		return nil, err
	}
	switch code := resp.StatusCode; {
	case (code == http.StatusOK || code == http.StatusPartialContent) && !isIdentity(resp.Header):
		if rq.status != 0 {
			return fail(errUpstreamAnswer)
		}
		discardBody(resp)
		rq.passThrough()
		return nil, errDone
	case code == http.StatusPartialContent:
		a, b, t, ok := parseContentRange(resp.Header.Get("Content-Range"))
		switch {
		case !ok || a > pos || b < pos:
			return fail(errUpstreamAnswer)
		case t >= 0 && t != T:
			rq.recordNewSize(t, storedHeaders(resp.Header))
			return fail(errChanged)
		}
		return newDirect(resp.Body, a, b, capture && t == T), nil
	case code == http.StatusOK:
		if cl := resp.ContentLength; cl >= 0 && cl != T {
			rq.recordNewSize(cl, storedHeaders(resp.Header))
			return fail(errChanged)
		}
		return newDirect(resp.Body, 0, T-1, capture && resp.ContentLength == T), nil
	case code == http.StatusRequestedRangeNotSatisfiable:
		if rq.obj.genKnown {
			rq.s.invalidate(rq.st, rq.id, rq.obj.gen)
		}
		return fail(errChanged)
	case code >= http.StatusMultipleChoices && rq.status == 0:
		rq.relay(resp, statusPass)
		return nil, errDone
	}
	return fail(errUpstreamAnswer)
}

// recordNewSize records a changed object size (h: stored headers) so the
// store discards the old slices (new generation) and the next request
// plans with it.
func (rq *request) recordNewSize(total int64, h http.Header) {
	if rq.st == nil || rq.s.storeFull() || total <= 0 || total > cachestore.MaxTotal {
		return
	}
	ctx, cancel := context.WithTimeout(rq.ctx, rq.s.tm.storeWrite)
	defer cancel()
	m := rq.meta()
	m.Total, m.Header, m.NoSlice = total, h, !rq.useSlices
	if _, err := rq.st.SetMeta(ctx, rq.id, m); err != nil {
		rq.storeFailed("set meta", err)
	}
}

func (rq *request) meta() cachestore.Meta {
	return cachestore.Meta{Service: rq.service, Host: rq.host, Path: rq.path, GroupKey: rq.group.Key}
}

// releaseSources detaches from fills and closes direct bodies.
func (rq *request) releaseSources() {
	if f := rq.pending; f != nil {
		rq.pending = nil
		f.detach()
	}
	rq.closeDirect()
}

func (rq *request) closeDirect() {
	if rq.ds != nil {
		rq.ds.close()
		rq.ds = nil
	}
}

// ownRing is the number of recent own fills remembered (≥ read-ahead + 1).
const ownRing = 32

func (rq *request) markOwn(i int64) {
	rq.own[rq.ownLen%ownRing] = i
	rq.ownLen++
}

func (rq *request) isOwn(i int64) bool {
	for k := range min(rq.ownLen, ownRing) {
		if rq.own[k] == i {
			return true
		}
	}
	return false
}
