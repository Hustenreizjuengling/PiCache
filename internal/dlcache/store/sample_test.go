package cachestore

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
)

func TestSampleSlices(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if refs, err := s.SampleSlices(ctx, 10); err != nil || len(refs) != 0 {
		t.Fatalf("empty store: %v %v", refs, err)
	}
	id1, _ := putObject(t, s, "steam", "/old", "steam:a", 6*testSlice+3) // 7 slices
	id2, _ := putObject(t, s, "steam", "/new", "steam:a", 3*testSlice)   // 3 slices
	mustFlush(t, s)                                                      // the index lags by one batch

	all, err := s.SampleSlices(ctx, 100)
	if err != nil || len(all) != 10 {
		t.Fatalf("all: %v %v", all, err)
	}
	// Small samples are uniform: every slice shows up, not only the newest.
	seen := map[SliceRef]int{}
	for range 200 {
		refs, err := s.SampleSlices(ctx, 2)
		if err != nil || len(refs) != 2 || refs[0] == refs[1] {
			t.Fatalf("sample of 2: %v %v", refs, err)
		}
		for _, r := range refs {
			seen[r]++
		}
	}
	if len(seen) != 10 {
		t.Fatalf("seen %d distinct slices: %v", len(seen), seen)
	}
	for _, r := range all {
		if r.ID != id1 && r.ID != id2 || r.Idx < 0 || r.Idx > 6 {
			t.Fatalf("ref %+v", r)
		}
	}
	if refs, err := s.SampleSlices(ctx, 0); err != nil || refs != nil {
		t.Fatalf("n=0: %v %v", refs, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.SampleSlices(cctx, 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	s.Close()
	if _, err := s.SampleSlices(ctx, 5); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed: %v", err)
	}
}

func TestReadSliceUncached(t *testing.T) {
	s, e := newStore(t)
	ctx := context.Background()
	total := int64(2*testSlice + 100)
	id, gen := putObject(t, s, "steam", "/depot/1/x", "steam:1", total)
	buf := make([]byte, 64<<10) // smaller than a slice: read in chunks

	n, dropped, err := s.ReadSliceUncached(ctx, SliceRef{ID: id, Idx: 0}, buf)
	if err != nil || n != testSlice || dropped != (runtime.GOOS == "linux") {
		t.Fatalf("slice 0: %d %v %v", n, dropped, err)
	}
	if n, _, err := s.ReadSliceUncached(ctx, SliceRef{ID: id, Idx: 2}, buf); err != nil || n != 100 {
		t.Fatalf("last slice: %d %v", n, err)
	}
	// Nothing is read or repaired that the index does not list.
	for _, ref := range []SliceRef{{ID: id, Idx: 3}, {ID: ObjectID("steam", "/unknown"), Idx: 0}} {
		if _, _, err := s.ReadSliceUncached(ctx, ref, buf); !errors.Is(err, ErrSliceMissing) {
			t.Fatalf("%+v: %v", ref, err)
		}
	}
	if _, _, err := s.ReadSliceUncached(ctx, SliceRef{ID: "../x", Idx: 0}, buf); err == nil {
		t.Fatal("invalid id accepted")
	}
	if _, _, err := s.ReadSliceUncached(ctx, SliceRef{ID: id, Idx: 0}, nil); err == nil {
		t.Fatal("empty buffer accepted")
	}
	// A deleted file is reported as missing and stays in the index.
	if err := os.Remove(sliceFile(e, id, 1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReadSliceUncached(ctx, SliceRef{ID: id, Idx: 1}, buf); !errors.Is(err, ErrSliceMissing) {
		t.Fatalf("deleted file: %v", err)
	}
	if !s.HasSlice(ctx, id, gen, 1) {
		t.Fatal("the speed test read dropped a slice from the index")
	}
	// A damaged file is reported (not deleted).
	if err := os.WriteFile(sliceFile(e, id, 0), []byte("PCS1garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReadSliceUncached(ctx, SliceRef{ID: id, Idx: 0}, buf); !isCorrupt(err) {
		t.Fatalf("damaged file: %v", err)
	}
	if _, err := os.Stat(sliceFile(e, id, 0)); err != nil {
		t.Fatalf("damaged file removed: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := s.ReadSliceUncached(cctx, SliceRef{ID: id, Idx: 2}, buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	s.Close()
	if _, _, err := s.ReadSliceUncached(ctx, SliceRef{ID: id, Idx: 2}, buf); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed: %v", err)
	}
}
