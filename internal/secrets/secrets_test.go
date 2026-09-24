package secrets

import (
	"path/filepath"
	"testing"
)

func TestSealOpen(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "keys", "master.key")
	b, err := Open(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.Seal([]byte("hunter2,with,commas"), "picache/storage/a/password")
	if err != nil {
		t.Fatal(err)
	}
	pt, err := b.Open(s, "picache/storage/a/password")
	if err != nil || string(pt) != "hunter2,with,commas" {
		t.Fatalf("roundtrip: %q %v", pt, err)
	}
	if _, err := b.Open(s, "picache/storage/b/password"); err == nil {
		t.Fatal("aad mismatch must fail")
	}
	b2, err := Open(keyFile) // reload same key
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b2.Open(s, "picache/storage/a/password"); err != nil {
		t.Fatalf("reloaded key cannot open: %v", err)
	}
}
