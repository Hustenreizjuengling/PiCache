package cachestore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func sliceFile(e *testEnv, id string, idx int64) string {
	return filepath.Join(e.root, filepath.FromSlash(sliceName(id, idx)))
}

func TestVerifyConsistentStore(t *testing.T) {
	s, _ := newStore(t)
	putObject(t, s, "steam", "/v1", "steam:v", 2*testSlice+10)
	putObject(t, s, "steam", "/v2", "steam:v", 10)
	calls := 0
	res, err := s.Verify(context.Background(), false, func(VerifyProgress) { calls++ })
	if err != nil {
		t.Fatal(err)
	}
	if res.FilesScanned != 4 || res.Added != 0 || res.Removed != 0 || res.Corrupt != 0 || res.BytesScanned <= 2*testSlice+20 {
		t.Fatalf("result %+v", res)
	}
	if calls == 0 {
		t.Fatal("progress never reported")
	}
}

func TestVerifyRebuildsLostIndex(t *testing.T) {
	e := newEnv(t)
	e.opt.GroupKey = func(service, host, path string) string { return service + ":rebuilt" }
	s := e.open()
	ctx := context.Background()
	type obj struct {
		id    string
		total int64
	}
	var objs []obj
	for i, total := range []int64{1, testSlice, 3*testSlice + 17, 1000} {
		id, _ := putObject(t, s, "steam", "/rebuild/"+strconv.Itoa(i), "steam:orig", total)
		objs = append(objs, obj{id, total})
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(e.index + suffix)
	}

	s = e.open() // fresh, empty index
	if _, ok, _ := s.Head(ctx, objs[0].id); ok {
		t.Fatal("fresh index must be empty")
	}
	res, err := s.Verify(ctx, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 1+1+4+1 || res.Corrupt != 0 || res.Removed != 0 {
		t.Fatalf("result %+v", res)
	}
	for _, o := range objs {
		h, ok, err := s.Head(ctx, o.id)
		if err != nil || !ok || h.Total != o.total || h.ContentType != "application/x-test" || h.LastModified == "" {
			t.Fatalf("rebuilt head %+v ok=%v err=%v", h, ok, err)
		}
		for i := int64(0); i < slicesFor(o.total, testSlice); i++ {
			if got := readAll(t, s, o.id, h.Gen, i); !bytes.Equal(got, payload(o.id, i, sliceLen(o.total, testSlice, i))) {
				t.Fatalf("rebuilt slice %d differs", i)
			}
		}
	}
	g, err := s.Groups(ctx, GroupQuery{})
	if err != nil || len(g.Items) != 1 || g.Items[0].GroupKey != "steam:rebuilt" || g.Items[0].Objects != 4 {
		t.Fatalf("groups after rebuild: %+v %v", g, err)
	}
	checkAggregates(t, s)
	// A second pass finds nothing to do.
	if res, err := s.Verify(ctx, true, nil); err != nil || res.Added != 0 || res.Removed != 0 || res.Corrupt != 0 {
		t.Fatalf("second pass %+v %v", res, err)
	}
}

func TestBrokenIndexIsRebuiltInBackground(t *testing.T) {
	e := newEnv(t)
	s := e.open()
	id, _ := putObject(t, s, "steam", "/broken", "steam:b", 2*testSlice)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(e.index + suffix)
	}
	if err := os.WriteFile(e.index, bytes.Repeat([]byte("garbage!"), 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	s = e.open()
	if m, _ := filepath.Glob(e.index + ".broken-*"); len(m) != 1 {
		t.Fatalf("broken index not moved aside: %v", m)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if h, ok, _ := s.Head(context.Background(), id); ok && h.Has(1) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("index not rebuilt in the background")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestVerifyRepairs(t *testing.T) {
	s, e := newStore(t)
	ctx := context.Background()
	flipped, gen := putObject(t, s, "steam", "/flipped", "steam:r", 1000)
	deleted, dgen := putObject(t, s, "steam", "/deleted", "steam:r", 2*testSlice)
	stale, sgen := putObject(t, s, "steam", "/stale", "steam:r", 1000)
	mustFlush(t, s)

	// 1. Flip one data byte: crc mismatch.
	b, err := os.ReadFile(sliceFile(e, flipped, 0))
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 0xff
	if err := os.WriteFile(sliceFile(e, flipped, 0), b, 0o640); err != nil {
		t.Fatal(err)
	}
	// 2. A slice file vanished.
	if err := os.Remove(sliceFile(e, deleted, 1)); err != nil {
		t.Fatal(err)
	}
	// 3. A valid file of an older generation (different total) of a known object.
	h := sliceHeader{O: stale, I: 1, S: "steam", H: "cdn.example.com", P: "/stale", T: testSlice + 5, Z: testSlice, C: 1,
		K: crc32c(make([]byte, 5))}
	if err := os.WriteFile(sliceFile(e, stale, 1), craft(t, &h, make([]byte, 5)), 0o640); err != nil {
		t.Fatal(err)
	}
	// 4. Unrelated files in the tree are left alone.
	junk := filepath.Join(filepath.Dir(sliceFile(e, stale, 0)), "notes.txt")
	if err := os.WriteFile(junk, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}

	res, err := s.Verify(ctx, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Corrupt != 2 || res.Removed != 1 || res.Added != 0 {
		t.Fatalf("report-only result %+v", res)
	}
	if !fileExists(t, e, flipped, 0) || !s.HasSlice(ctx, deleted, dgen, 1) {
		t.Fatal("report-only mode must not change anything")
	}

	res, err = s.Verify(ctx, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Corrupt != 2 || res.Removed != 1 || res.Added != 0 {
		t.Fatalf("repair result %+v", res)
	}
	if fileExists(t, e, flipped, 0) || s.HasSlice(ctx, flipped, gen, 0) {
		t.Fatal("corrupt slice not removed")
	}
	if s.HasSlice(ctx, deleted, dgen, 1) || !s.HasSlice(ctx, deleted, dgen, 0) {
		t.Fatal("missing slice not dropped from the index")
	}
	if fileExists(t, e, stale, 1) || !s.HasSlice(ctx, stale, sgen, 0) {
		t.Fatal("stale generation file not removed")
	}
	if _, err := os.Stat(junk); err != nil {
		t.Fatal("unrelated file must not be deleted")
	}
	checkAggregates(t, s)
	if res, err := s.Verify(ctx, false, nil); err != nil || res.Corrupt != 0 || res.Removed != 0 || res.Added != 0 {
		t.Fatalf("after repair %+v %v", res, err)
	}
}

func TestReadSliceMissingFileDropsSlice(t *testing.T) {
	s, e := newStore(t)
	ctx := context.Background()
	id, gen := putObject(t, s, "steam", "/gone", "steam:g", 1000)
	if err := os.Remove(sliceFile(e, id, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadSlice(ctx, id, gen, 0); err != ErrSliceMissing {
		t.Fatalf("ReadSlice: %v", err)
	}
	if s.HasSlice(ctx, id, gen, 0) {
		t.Fatal("slice still listed")
	}
	checkAggregates(t, s)

	// Without the store marker (unmounted NAS) the index is left alone.
	id2, gen2 := putObject(t, s, "steam", "/gone2", "steam:g", 1000)
	if err := os.Remove(sliceFile(e, id2, 0)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(e.root, MarkerFile), filepath.Join(e.root, "marker.bak")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadSlice(ctx, id2, gen2, 0); err == nil || err == ErrSliceMissing {
		t.Fatalf("ReadSlice without marker: %v", err)
	}
	if !s.HasSlice(ctx, id2, gen2, 0) {
		t.Fatal("slice dropped although the store root is unavailable")
	}
}
