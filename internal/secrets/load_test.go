package secrets

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The root helper loads the key from the data directory the service owns:
// it must not follow a symbolic link there, must refuse anything but a
// regular file and must not read an unbounded amount of data.
func TestLoadRefusesUntrustedKeyFiles(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	dir := t.TempDir()
	good := filepath.Join(dir, "real.key")
	if err := os.WriteFile(good, []byte(strings.Repeat("ab", 32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := Load(good); err != nil || b.Source != good {
		t.Fatalf("regular key file: %v", err)
	}

	big := filepath.Join(dir, "big.key")
	if err := os.WriteFile(big, make([]byte, maxKeyFileSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized key file: %v", err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory as key file: %v", err)
	}
	if _, err := Load(filepath.Join(dir, "missing.key")); err == nil || !strings.Contains(err.Error(), "no master key found") {
		t.Fatalf("missing key file: %v", err)
	}

	link := filepath.Join(dir, "master.key")
	if err := os.Symlink(good, link); err != nil {
		t.Skipf("cannot create symbolic links here: %v", err)
	}
	if _, err := Load(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Load followed a symbolic link: %v", err)
	}
	// The service itself may use a linked key (e.g. a Kubernetes secret volume).
	b, err := Open(link)
	if err != nil {
		t.Fatalf("Open through a link: %v", err)
	}
	if hex.EncodeToString(b.key) != strings.Repeat("ab", 32) {
		t.Fatal("wrong key through the link")
	}
}
