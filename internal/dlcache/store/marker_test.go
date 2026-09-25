package cachestore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMarker(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadMarker(dir); err != ErrNoMarker {
		t.Fatalf("want ErrNoMarker, got %v", err)
	}
	id := "0123456789abcdef0123456789abcdef"
	if _, err := InitRoot(dir, id, 1<<20); err != nil {
		t.Fatal(err)
	}
	m, err := ReadMarker(dir)
	if err != nil || m.StoreID != id || m.SliceSize != 1<<20 {
		t.Fatalf("marker %+v %v", m, err)
	}
	if _, err := InitRoot(dir, id, 1<<20); err == nil {
		t.Fatal("second init must fail")
	}
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "foo"), []byte("x"), 0o600)
	if _, err := InitRoot(other, id, 1<<20); err == nil {
		t.Fatal("non-empty dir must be refused")
	}
	if _, err := InitRoot(t.TempDir(), "bad", 1<<20); err == nil {
		t.Fatal("invalid id must be refused")
	}
}
