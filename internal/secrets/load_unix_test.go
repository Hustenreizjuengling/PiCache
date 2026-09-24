//go:build unix

package secrets

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A FIFO as the key file must neither block Load nor be read.
func TestLoadRefusesFIFO(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	fifo := filepath.Join(t.TempDir(), "master.key")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := Load(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("FIFO key file: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Load blocked on a FIFO")
	}
}
