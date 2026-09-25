package proxy

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Collapsing without ranges (ARCHITECTURE 8.2 steps 10 b/d and 14): when an
// upstream ignores Range for an object, one request (the leader) streams
// the whole body and captures each complete slice as a fill in the fill
// table (captureFill), which is then stored. Concurrent requests for the
// object (followers) stream those fills and read the stored slices instead
// of fetching the object once more:
//
//   - The first fetch goes through a fill in both cases: the demand fill of
//     a host not yet marked no-slice, whose range failure is handed to its
//     creator, or a whole-object fill (fill.noRange) for a marked host. The
//     creator becomes the leader when the answer is handed to it (the
//     registration happens before the fill's readers wake up); the fill's
//     other readers follow it.
//   - A follower waits for the leader only while it captures and will reach
//     the follower's slice within followAhead slices; when the leader
//     stalls (no body bytes for the stall time), stops capturing or ends,
//     the follower fetches the object itself (and may lead).
//
// The leader reads the body at its own client's pace, so a follower is at
// most as fast as the leader. Nothing is shared while nothing can be stored
// (no store, store full, no fill slot).

// followAhead is how many slices ahead of a leader a follower waits for it.
const followAhead = 8

// objKey identifies an object of a store.
type objKey struct{ store, id string }

// objFetch is a leader's announcement of a whole-object body.
type objFetch struct {
	t      *objFetches
	key    objKey
	last   int64       // last slice the body reaches
	total  int64       // object size
	header http.Header // stored headers of the answer (read-only)

	progress atomic.Int64 // unix nanoseconds of the last body read

	mu        sync.Mutex
	next      int64 // slices < next were passed (captured, cached or skipped)
	capturing bool  // the leader captures (the last slice passed is or will be available)
	done      bool
	wake      chan struct{} // closed and replaced on every change
}

// objFetches holds the leaders (at most one per object; bounded by the
// requests in flight).
type objFetches struct {
	mu sync.Mutex
	m  map[objKey]*objFetch
}

// lead registers a leader for key whose body covers [start, end] of an
// object of total bytes (slice size S). nil if the object has a leader.
func (t *objFetches) lead(key objKey, start, end, total, S int64, header http.Header) *objFetch {
	if S <= 0 || total <= 0 || end < start {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m[key] != nil {
		return nil
	}
	if t.m == nil {
		t.m = make(map[objKey]*objFetch)
	}
	f := &objFetch{t: t, key: key, last: end / S, total: total, header: header.Clone(),
		next: start / S, capturing: true, wake: make(chan struct{})}
	f.progress.Store(time.Now().UnixNano())
	t.m[key] = f
	return f
}

// get returns the leader of key, if any.
func (t *objFetches) get(key objKey) *objFetch {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[key]
}

// passed records that the leader reached slice i: captured reports that
// the slice is (or will be) available from a fill or the store.
func (f *objFetch) passed(i int64, captured bool) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.next = max(f.next, i+1)
	f.capturing = captured
	f.signalLocked()
	f.mu.Unlock()
}

// bump records body progress.
func (f *objFetch) bump() {
	if f != nil {
		f.progress.Store(time.Now().UnixNano())
	}
}

// end withdraws the announcement (idempotent).
func (f *objFetch) end() {
	if f == nil {
		return
	}
	f.mu.Lock()
	if f.done {
		f.mu.Unlock()
		return
	}
	f.done = true
	f.signalLocked()
	f.mu.Unlock()
	f.t.mu.Lock()
	if f.t.m[f.key] == f {
		delete(f.t.m, f.key)
	}
	f.t.mu.Unlock()
}

func (f *objFetch) signalLocked() {
	close(f.wake)
	f.wake = make(chan struct{})
}

// state returns the leader's position.
func (f *objFetch) state() (next int64, capturing, done bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.next, f.capturing, f.done
}

// worthWaiting reports whether a follower at slice i should wait for the
// leader (next: the leader's position when the follower started looking).
func (f *objFetch) worthWaiting(i, next int64, capturing, done bool) bool {
	return !done && capturing && next <= i && i <= f.last && i-next < followAhead
}

// wait waits until the leader passed slice i or ended (true: look again).
// It gives up (false) when the leader stops capturing, reads no body bytes
// for stall, or ctx ends.
func (f *objFetch) wait(ctx context.Context, i int64, stall time.Duration) bool {
	t := time.NewTimer(stall)
	defer t.Stop()
	for {
		f.mu.Lock()
		switch {
		case f.done || f.next > i:
			f.mu.Unlock()
			return true
		case !f.capturing:
			f.mu.Unlock()
			return false
		}
		ch := f.wake
		f.mu.Unlock()
		select {
		case <-ch:
		case <-t.C:
			idle := time.Since(time.Unix(0, f.progress.Load()))
			if idle >= stall {
				return false
			}
			t.Reset(stall - idle)
		case <-ctx.Done():
			return false
		}
	}
}

// leadBody is an upstream body with a leader announcement; closing the
// body withdraws it.
type leadBody struct {
	io.ReadCloser
	lead *objFetch
}

func (b *leadBody) Close() error {
	b.lead.end()
	return b.ReadCloser.Close()
}

// withLead announces the range-failure answer info of the fill f as a
// leader (before the fill's readers wake up) and returns the body that
// carries the announcement to the request it is handed to.
func (s *Server) withLead(f *fill, info respInfo, body io.ReadCloser) io.ReadCloser {
	if f.st == nil || s.storeFull() || info.length <= 0 {
		return body
	}
	lead := s.leaders.lead(objKey{store: f.key.store, id: f.key.id}, info.start, info.start+info.length-1,
		info.total, f.size, info.header)
	if lead == nil {
		return body
	}
	return &leadBody{ReadCloser: body, lead: lead}
}

func (rq *request) objKey() objKey { return objKey{store: rq.storeID, id: rq.id} }

// leader returns the leader of the request's object if the request may
// follow it: not for a forced refetch, and only for the same object size.
func (rq *request) leader() *objFetch {
	if rq.bypass {
		return nil
	}
	lead := rq.s.leaders.get(rq.objKey())
	if lead == nil || (rq.obj.total > 0 && lead.total != rq.obj.total) {
		return nil
	}
	return lead
}

// followLeader lets a request for an unknown object follow its leader: the
// object's size and headers come from the leader's answer.
func (rq *request) followLeader() (respInfo, bool) {
	lead := rq.leader()
	if lead == nil {
		return respInfo{}, false
	}
	rq.follow(lead.total, lead.header)
	return respInfo{kind: kindRangeFail, total: lead.total, header: lead.header}, true
}

// follow continues a request for an object of total bytes (stored headers
// header) without Range, reading what its leader captured.
func (rq *request) follow(total int64, header http.Header) {
	rq.obj.total, rq.obj.header = total, header
	rq.useSlices = false
	if !rq.bypass {
		rq.refreshHead() // the slices the leader stored already
	}
}

// serveWithoutRanges serves [pos, e] of slice i (at most up to end) of an
// object fetched without Range: from a fill (a leader's capture or any
// other fill of the slice), from the store, after waiting for a leader that
// is about to capture the slice, or from a fetch of its own (through a
// shared whole-object fill, so concurrent requests collapse).
func (rq *request) serveWithoutRanges(i, pos, e, end int64) (int64, error) {
	lead := rq.leader()
	var next int64
	var capturing, done bool
	if lead != nil {
		next, capturing, done = lead.state()
	}
	if f := rq.s.fills.join(rq.key(i)); f != nil {
		return rq.fromFill(f, i, pos, e, end)
	}
	if n, ok, err := rq.tryDisk(i, pos, e, true); ok {
		rq.consumed(i, pos, n, end, err)
		return n, err
	}
	if lead != nil && lead.worthWaiting(i, next, capturing, done) {
		if lead.wait(rq.ctx, i, rq.s.tm.stall) {
			rq.lastRefresh = -1 // look at the store again too
			return 0, nil       // the leader got there (or ended): look again
		}
		if rq.ctx.Err() != nil {
			return 0, errClientGone
		}
	}
	if lead == nil || done {
		if f, ok := rq.acquireWholeFill(); ok {
			return rq.fromFill(f, i, pos, e, end)
		}
	}
	return rq.openDirect(pos, end, 0, false, true)
}

// acquireWholeFill joins or starts the whole-object fill of the object (a
// fill of slice 0 without Range). ok=false if a sliced fill of slice 0 is
// in flight or no fill slot is free.
func (rq *request) acquireWholeFill() (f *fill, ok bool) {
	s := rq.s
	for range 2 {
		if f := s.fills.get(rq.key(0)); f != nil {
			if !f.noRange {
				return nil, false
			}
			if f.attach() {
				return f, true
			}
		}
		if rq.ctx.Err() != nil {
			return nil, false
		}
		wait := s.tm.demandWait
		if rq.slotWaited {
			wait = 0
		}
		if !s.slots.acquire(rq.ctx, rq.ckey, s.fillLimits(rq.S), wait) {
			rq.slotWaited = true
			return nil, false
		}
		f := rq.newFill(0, true)
		f.noRange, f.meta.NoSlice = true, true
		f.attached = 1
		if s.fills.insert(f) != nil {
			s.slots.release(rq.ckey)
			continue
		}
		if !s.startFill(f) {
			f.detach()
			return nil, false
		}
		rq.markOwn(0)
		return f, true
	}
	return nil, false
}

// captureFill prepares the capture of slice i (n bytes, the next bytes of
// the leader's body) as a fill that other requests can stream while it is
// read: nil if the slice is cached or in flight, nothing may be stored or
// no fill slot is free. The generation to store it with is returned too.
func (rq *request) captureFill(i, n int64) (*fill, uint64) {
	s := rq.s
	if rq.st == nil || s.storeFull() || (rq.obj.genKnown && rq.obj.has(i)) || s.fills.get(rq.key(i)) != nil {
		return nil, 0
	}
	gen, ok := rq.captureGen()
	if !ok || !s.slots.tryAcquire(rq.ckey, s.fillLimits(rq.S)) {
		return nil, 0
	}
	f := rq.newFill(i, false)
	f.info = respInfo{kind: kindSlice, status: http.StatusOK, total: rq.obj.total, start: i * rq.S, length: n,
		dataIdx: i, header: rq.obj.header}
	f.phase = phaseBody
	f.buf = s.bufs.get(rq.S)[:n]
	f.timer = time.AfterFunc(s.tm.stall, f.markStalled)
	if s.fills.insert(f) != nil {
		f.timer.Stop()
		s.bufs.put(f.buf)
		s.slots.release(rq.ckey)
		return nil, 0
	}
	s.stats.activeFills.Add(1)
	f.acct.retain()
	return f, gen
}

// sliceAvailable reports whether slice i is cached or in flight.
func (rq *request) sliceAvailable(i int64) bool {
	return (rq.obj.genKnown && rq.obj.has(i)) || rq.s.fills.get(rq.key(i)) != nil
}
