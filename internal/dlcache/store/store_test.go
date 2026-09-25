package cachestore

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestOpenValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	bad := e.opt
	bad.StoreID = "ffffffffffffffffffffffffffffffff"
	if _, err := Open(ctx, bad); err == nil {
		t.Fatal("marker with another store id must be refused")
	}
	bad = e.opt
	bad.Root = t.TempDir() // no marker
	if _, err := Open(ctx, bad); !errors.Is(err, ErrNoMarker) {
		t.Fatalf("uninitialised root: err = %v", err)
	}
	s := e.open()
	if s.SliceSize() != testSlice || s.ID() != testStoreID || s.Root() != e.root {
		t.Fatalf("accessors: %d %s %s", s.SliceSize(), s.ID(), s.Root())
	}
	if u := s.Usage(); u.Objects != 0 || u.StoreID != testStoreID || u.SliceSize != testSlice {
		t.Fatalf("usage of an empty store: %+v", u)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	s, e := newStore(t)
	ctx := context.Background()
	total := int64(2*testSlice + 12345) // partial last slice
	id, m := testMeta("steam", "/depot/1/chunk/abc", "steam:depot:1", total)
	if _, ok, err := s.Head(ctx, id); ok || err != nil {
		t.Fatalf("Head of unknown object: ok=%v err=%v", ok, err)
	}
	gen, err := s.SetMeta(ctx, id, m)
	if err != nil || gen == 0 {
		t.Fatalf("SetMeta: gen=%d err=%v", gen, err)
	}

	// Length and range checks.
	if err := s.WriteSlice(ctx, id, gen, 2, make([]byte, testSlice)); err == nil {
		t.Fatal("a full-size last slice must be refused")
	}
	if err := s.WriteSlice(ctx, id, gen, 0, make([]byte, 100)); err == nil {
		t.Fatal("a short slice must be refused")
	}
	if err := s.WriteSlice(ctx, id, gen, 3, make([]byte, 1)); err == nil {
		t.Fatal("an index beyond the object must be refused")
	}
	if err := s.WriteSlice(ctx, id, gen+1, 0, make([]byte, testSlice)); !errors.Is(err, ErrStale) {
		t.Fatalf("wrong generation: err = %v", err)
	}
	if _, err := s.ReadSlice(ctx, id, gen, 0); !errors.Is(err, ErrSliceMissing) {
		t.Fatalf("ReadSlice before write: %v", err)
	}

	want := map[int64][]byte{}
	for i := int64(0); i < 3; i++ {
		want[i] = payload(id, i, sliceLen(total, testSlice, i))
		if err := s.WriteSlice(ctx, id, gen, i, want[i]); err != nil {
			t.Fatal(err)
		}
	}
	h, ok, err := s.Head(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Head: ok=%v err=%v", ok, err)
	}
	if h.Gen != gen || h.Total != total || h.SliceSize != testSlice || h.ContentType != "application/x-test" ||
		h.LastModified == "" || !h.Has(0) || !h.Has(2) || h.Has(3) || h.Header.Get("Content-Type") != "application/x-test" {
		t.Fatalf("Head = %+v", h)
	}
	h.Present[0] = 0 // callers get a copy
	if !s.HasSlice(ctx, id, gen, 0) || s.HasSlice(ctx, id, gen+1, 0) || s.HasSlice(ctx, id, gen, 3) {
		t.Fatal("HasSlice")
	}
	for i := int64(0); i < 3; i++ {
		if got := readAll(t, s, id, gen, i); !bytes.Equal(got, want[i]) {
			t.Fatalf("slice %d content differs", i)
		}
	}

	// On-disk format: PCS1 + header with crc32c of the data.
	b, err := os.ReadFile(filepath.Join(e.root, filepath.FromSlash(sliceName(id, 2))))
	if err != nil {
		t.Fatal(err)
	}
	hl := binary.LittleEndian.Uint32(b[4:8])
	var hdr sliceHeader
	if string(b[:4]) != "PCS1" || json.Unmarshal(b[8:8+hl], &hdr) != nil {
		t.Fatal("bad file prefix")
	}
	if hdr.O != id || hdr.I != 2 || hdr.T != total || hdr.Z != testSlice || hdr.S != "steam" || hdr.P != m.Path ||
		hdr.K != crc32c(want[2]) || int64(len(b)) != 8+int64(hl)+12345 {
		t.Fatalf("header %+v, file size %d", hdr, len(b))
	}

	// Headers and the no-slice flag are replayed from the head.
	nid, nm := testMeta("steam", "/noslice", "steam:depot:1", 10)
	nm.NoSlice = true
	nm.Header.Set("X-Custom", "v")
	if _, err := s.SetMeta(ctx, nid, nm); err != nil {
		t.Fatal(err)
	}
	if h, ok, _ := s.Head(ctx, nid); !ok || !h.NoSlice || h.Header.Get("X-Custom") != "v" {
		t.Fatalf("head %+v", h)
	}

	// Persisted: reopen and read again.
	mustFlush(t, s)
	checkAggregates(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := e.open()
	if h, ok, _ := s2.Head(ctx, nid); !ok || !h.NoSlice || h.Header.Get("X-Custom") != "v" {
		t.Fatalf("head after reopen %+v", h)
	}
	if u := s2.Usage(); u.Objects != 2 || u.Slices != 3 || u.CachedBytes != total {
		t.Fatalf("usage after reopen: %+v", u)
	}
	if got := readAll(t, s2, id, gen, 2); !bytes.Equal(got, want[2]) {
		t.Fatal("content after reopen differs")
	}
}

func TestSliceReader(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	total := int64(testSlice + 5000)
	id, gen := putObject(t, s, "steam", "/r", "steam:r", total)
	want := payload(id, 1, 5000)
	r, err := s.ReadSlice(ctx, id, gen, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Size() != 5000 {
		t.Fatalf("Size = %d", r.Size())
	}
	buf := make([]byte, 100)
	if n, err := r.ReadAt(buf, 10); n != 100 || err != nil || !bytes.Equal(buf, want[10:110]) {
		t.Fatalf("ReadAt: n=%d err=%v", n, err)
	}
	if n, err := r.ReadAt(buf, 4950); n != 50 || err != io.EOF || !bytes.Equal(buf[:50], want[4950:]) {
		t.Fatalf("ReadAt at end: n=%d err=%v", n, err)
	}
	if _, err := r.ReadAt(buf, 5000); err != io.EOF {
		t.Fatalf("ReadAt past end: %v", err)
	}
	if _, err := r.ReadAt(buf, -1); err == nil {
		t.Fatal("negative offset accepted")
	}
	for _, tc := range []struct{ off, n int64 }{{0, 5000}, {0, 0}, {1, 1}, {4999, 1}, {1234, 3000}} {
		var out bytes.Buffer
		n, err := r.WriteRange(&out, tc.off, tc.n)
		if err != nil || n != tc.n || !bytes.Equal(out.Bytes(), want[tc.off:tc.off+tc.n]) {
			t.Fatalf("WriteRange(%d, %d): n=%d err=%v", tc.off, tc.n, n, err)
		}
	}
	for _, tc := range []struct{ off, n int64 }{{-1, 1}, {0, 5001}, {5000, 1}, {10, -1}} {
		if _, err := r.WriteRange(io.Discard, tc.off, tc.n); err == nil {
			t.Fatalf("WriteRange(%d, %d) accepted", tc.off, tc.n)
		}
	}
	if err := r.Close(); err != nil || r.Close() != nil {
		t.Fatal("Close must be idempotent")
	}
	if _, err := r.ReadAt(buf, 0); err == nil {
		t.Fatal("ReadAt after Close must fail")
	}
}

// TestWriteRangeHTTP serves slice ranges through net/http (the path that
// uses sendfile/TransmitFile) and checks the bytes received.
func TestWriteRangeHTTP(t *testing.T) {
	s := newEnvSize(t, 1<<20).open() // larger than writeChunk: ranges span several chunks
	total := int64(1<<20 - 3)
	id, gen := putObject(t, s, "steam", "/http", "steam:h", total)
	want := payload(id, 0, total)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		off, _ := strconv.ParseInt(r.URL.Query().Get("off"), 10, 64)
		n, _ := strconv.ParseInt(r.URL.Query().Get("n"), 10, 64)
		sr, err := s.ReadSlice(r.Context(), id, gen, 0)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer sr.Close()
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		if _, err := sr.WriteRange(w, off, n); err != nil {
			panic(http.ErrAbortHandler)
		}
	}))
	defer srv.Close()
	for _, tc := range []struct{ off, n int64 }{{0, total}, {1, 700_000}, {total - 1, 1}, {100, 3 * writeChunk / 2}, {writeChunk - 1, 2}} {
		resp, err := http.Get(srv.URL + "/?off=" + strconv.FormatInt(tc.off, 10) + "&n=" + strconv.FormatInt(tc.n, 10))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || !bytes.Equal(got, want[tc.off:tc.off+tc.n]) {
			t.Fatalf("range %d+%d: %d bytes, err %v", tc.off, tc.n, len(got), err)
		}
	}
}

func TestGenerations(t *testing.T) {
	s, e := newStore(t)
	ctx := context.Background()
	id, m := testMeta("epicgames", "/Builds/x/file", "epic:x", 2*testSlice)
	gen, err := s.SetMeta(ctx, id, m)
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < 2; i++ {
		if err := s.WriteSlice(ctx, id, gen, i, payload(id, i, testSlice)); err != nil {
			t.Fatal(err)
		}
	}
	// Same total: same generation, slices kept, even when metadata changes.
	if g, err := s.SetMeta(ctx, id, m); err != nil || g != gen {
		t.Fatalf("unchanged meta: gen %d → %d (%v)", gen, g, err)
	}
	m.Header = http.Header{"Content-Type": {"text/plain"}}
	m.GroupKey = "epic:y"
	if g, err := s.SetMeta(ctx, id, m); err != nil || g != gen {
		t.Fatalf("changed headers: gen %d → %d (%v)", gen, g, err)
	}
	if h, _, _ := s.Head(ctx, id); h.ContentType != "text/plain" || !h.Has(1) {
		t.Fatalf("head after meta update: %+v", h)
	}
	r, err := s.ReadSlice(ctx, id, gen, 0) // opened before the change
	if err != nil {
		t.Fatal(err)
	}
	r.Close()

	// A different total: new generation, old slices discarded.
	m.Total = 3 * testSlice
	gen2, err := s.SetMeta(ctx, id, m)
	if err != nil || gen2 <= gen {
		t.Fatalf("new total: gen %d → %d (%v)", gen, gen2, err)
	}
	waitRemovals(t, s) // old slice files are deleted in the background
	if fileExists(t, e, id, 0) || fileExists(t, e, id, 1) {
		t.Fatal("old slice files must be removed")
	}
	if s.HasSlice(ctx, id, gen2, 0) || s.HasSlice(ctx, id, gen, 0) {
		t.Fatal("no slice may be present after a new generation")
	}
	if _, err := s.ReadSlice(ctx, id, gen, 0); !errors.Is(err, ErrStale) {
		t.Fatalf("ReadSlice with old gen: %v", err)
	}
	if err := s.WriteSlice(ctx, id, gen, 0, payload(id, 0, testSlice)); !errors.Is(err, ErrStale) {
		t.Fatalf("WriteSlice with old gen: %v", err)
	}
	if err := s.WriteSlice(ctx, id, gen2, 2, payload(id, 2, testSlice)); err != nil {
		t.Fatal(err)
	}

	// Invalidate removes the record; a new record never reuses a generation.
	var evicted []string
	s.OnEvict(func(o Object, reason string) { evicted = append(evicted, o.ID+"/"+reason) })
	if err := s.Invalidate(ctx, id, "invalidated"); err != nil {
		t.Fatal(err)
	}
	if len(evicted) != 1 || evicted[0] != id+"/invalidated" {
		t.Fatalf("OnEvict calls: %v", evicted)
	}
	if _, ok, _ := s.Head(ctx, id); ok {
		t.Fatal("object still present after Invalidate")
	}
	if _, err := s.ReadSlice(ctx, id, gen2, 2); !errors.Is(err, ErrStale) {
		t.Fatalf("ReadSlice after Invalidate: %v", err)
	}
	if fileExists(t, e, id, 2) {
		t.Fatal("files must be removed by Invalidate")
	}
	gen3, err := s.SetMeta(ctx, id, m)
	if err != nil || gen3 <= gen2 {
		t.Fatalf("recreated: gen %d → %d", gen2, gen3)
	}
	checkAggregates(t, s)

	// Generations stay monotonic across a restart.
	_ = s.Close()
	s2 := e.open()
	id2, m2 := testMeta("epicgames", "/other", "epic:x", 10)
	if g, err := s2.SetMeta(ctx, id2, m2); err != nil || g <= gen3 {
		t.Fatalf("gen after reopen %d, before %d (%v)", g, gen3, err)
	}
}

func TestConcurrentSetMeta(t *testing.T) {
	s, _ := newStore(t)
	id, m := testMeta("steam", "/same", "steam:c", 1000)
	gens := make(chan uint64, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			g, err := s.SetMeta(context.Background(), id, m)
			if err != nil {
				t.Error(err)
			}
			gens <- g
		})
	}
	wg.Wait()
	close(gens)
	first := <-gens
	for g := range gens {
		if g != first {
			t.Fatalf("concurrent SetMeta returned generations %d and %d", first, g)
		}
	}
	checkAggregates(t, s)
}

func TestConcurrentWritersAndReaders(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	total := int64(4*testSlice - 7)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			id, m := testMeta("steam", "/c/"+strconv.Itoa(w), "steam:c", total)
			gen, err := s.SetMeta(ctx, id, m)
			if err != nil {
				t.Error(err)
				return
			}
			for i := int64(0); i < 4; i++ {
				data := payload(id, i, sliceLen(total, testSlice, i))
				if err := s.WriteSlice(ctx, id, gen, i, data); err != nil {
					t.Error(err)
					return
				}
				r, err := s.ReadSlice(ctx, id, gen, i)
				if err != nil {
					t.Error(err)
					return
				}
				got := make([]byte, r.Size())
				_, _ = r.ReadAt(got, 0)
				r.Close()
				if !bytes.Equal(got, data) {
					t.Error("content differs")
				}
				s.Touch(id, int64(len(data)))
			}
		})
	}
	wg.Wait()
	checkAggregates(t, s)
	if u := s.Usage(); u.Objects != 8 || u.Slices != 32 || u.CachedBytes != 8*total {
		t.Fatalf("usage %+v", u)
	}
	objs, err := s.Objects(ctx, ObjectQuery{Service: "steam"})
	if err != nil || objs.Total != 8 {
		t.Fatalf("objects: %+v %v", objs, err)
	}
	for _, o := range objs.Items {
		if o.Hits != 4 || o.BytesServed != total || o.SliceCount != 4 || o.SlicesTotal != 4 {
			t.Fatalf("object stats %+v", o)
		}
	}
}

func TestCloseSemantics(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	id, gen := putObject(t, s, "steam", "/close", "steam:c", testSlice)
	r, err := s.ReadSlice(ctx, id, gen, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Hammer the store from several goroutines while it closes.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := range 6 {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				wid, m := testMeta("steam", "/w/"+strconv.Itoa(w)+"/"+strconv.Itoa(i%20), "steam:w", 1000)
				// Operations cut off by Close may fail in several ways; they
				// must never panic or hang.
				g, err := s.SetMeta(ctx, wid, m)
				if err == nil {
					_ = s.WriteSlice(ctx, wid, g, 0, payload(wid, 0, 1000))
				}
				_, _, _ = s.Head(ctx, wid)
				s.Touch(wid, 1)
				if rr, err := s.ReadSlice(ctx, wid, g, 0); err == nil {
					rr.Close()
				}
				_, _ = s.Groups(ctx, GroupQuery{})
			}
		})
	}
	time.Sleep(50 * time.Millisecond)
	var cwg sync.WaitGroup
	for range 3 { // concurrent Close calls
		cwg.Go(func() {
			if err := s.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
	}
	cwg.Wait()
	close(stop)
	wg.Wait()

	if _, ok, err := s.Head(ctx, id); ok || !errors.Is(err, ErrClosed) {
		t.Fatalf("Head after Close: ok=%v err=%v", ok, err)
	}
	if s.HasSlice(ctx, id, gen, 0) {
		t.Fatal("HasSlice after Close")
	}
	if _, err := s.ReadSlice(ctx, id, gen, 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("ReadSlice after Close: %v", err)
	}
	if _, err := s.SetMeta(ctx, id, Meta{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("SetMeta after Close: %v", err)
	}
	if err := s.WriteSlice(ctx, id, gen, 0, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("WriteSlice after Close: %v", err)
	}
	if _, err := s.Evict(ctx, Policy{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Evict after Close: %v", err)
	}
	if _, err := s.Verify(ctx, false, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Verify after Close: %v", err)
	}
	if _, err := s.Objects(ctx, ObjectQuery{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Objects after Close: %v", err)
	}
	s.Touch(id, 1)
	// A reader opened before Close stays readable.
	got := make([]byte, 10)
	if _, err := r.ReadAt(got, 0); err != nil || !bytes.Equal(got, payload(id, 0, 10)) {
		t.Fatalf("reader after Close: %v", err)
	}
}

// TestCloseCancelsWaiters blocks Verify and Evict (all I/O slots taken,
// eviction lock held) and checks that Close cancels them promptly.
func TestCloseCancelsWaiters(t *testing.T) {
	s, _ := newStore(t)
	putObject(t, s, "steam", "/v", "steam:v", 1000)
	for range cap(s.sem) {
		s.sem <- struct{}{}
	}
	s.evictSem <- struct{}{}
	verr, eerr := make(chan error, 1), make(chan error, 1)
	go func() { _, err := s.Verify(context.Background(), true, nil); verr <- err }()
	go func() { _, err := s.Evict(context.Background(), Policy{}); eerr <- err }()
	time.Sleep(100 * time.Millisecond)
	begin := time.Now()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(begin); d > closeWait/2 {
		t.Fatalf("Close took %v", d)
	}
	if err := <-verr; !errors.Is(err, context.Canceled) {
		t.Fatalf("Verify: %v", err)
	}
	if err := <-eerr; !errors.Is(err, context.Canceled) {
		t.Fatalf("Evict: %v", err)
	}
}

func TestHeadLRUBound(t *testing.T) {
	e := newEnv(t)
	e.opt.LRUEntries = 16
	s := e.open()
	ctx := context.Background()
	ids := make([]string, 100)
	for i := range ids {
		ids[i], _ = putObject(t, s, "steam", "/lru/"+strconv.Itoa(i), "steam:l", 10)
	}
	mustFlush(t, s)
	for _, id := range ids {
		if _, ok, err := s.Head(ctx, id); !ok || err != nil {
			t.Fatalf("Head(%s): %v %v", id, ok, err)
		}
	}
	if n, overlay := s.heads.counts(); n > 16 || overlay != 0 {
		t.Fatalf("LRU holds %d entries (max 16), overlay %d", n, overlay)
	}
}

func TestHeadsLRU(t *testing.T) {
	h := newHeads(4, 1<<20)
	mk := func(n int) *entry { return &entry{path: string(make([]byte, n)), present: []uint64{1}} }
	for i := range 10 {
		h.setClean(strconv.Itoa(i), mk(1))
	}
	if n, _ := h.counts(); n != 4 {
		t.Fatalf("entries = %d, want 4", n)
	}
	if h.get("0") != nil || h.get("9") == nil {
		t.Fatal("least recently used entries must go first")
	}
	h.get("6") // most recently used now
	h.setClean("10", mk(1))
	if h.get("6") == nil || h.get("7") != nil {
		t.Fatal("LRU order not maintained")
	}
	// Dirty entries live in the overlay and are never evicted.
	dirty := mk(1)
	h.set("dirty", dirty)
	for i := range 10 {
		h.setClean("x"+strconv.Itoa(i), mk(1))
	}
	if h.get("dirty") != dirty {
		t.Fatal("overlay entry lost")
	}
	h.flushed("dirty", dirty)
	if _, overlay := h.counts(); overlay != 0 {
		t.Fatal("flushed entry still in the overlay")
	}
	// Byte bound: entries above an eighth of the budget are not cached.
	hb := newHeads(100, 8000)
	hb.setClean("big", mk(2000))
	if hb.get("big") != nil {
		t.Fatal("oversized entry cached")
	}
	for i := range 20 {
		hb.setClean(strconv.Itoa(i), mk(700))
	}
	if hb.bytes > 8000 {
		t.Fatalf("byte budget exceeded: %d", hb.bytes)
	}
}
