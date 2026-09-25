//go:build linux

package update

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Only one update runs at a time; the lock goes away with its holder.
func TestLockUpdates(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockUpdates(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockUpdates(dir); err == nil || err.Error() != "another update is running" {
		t.Fatalf("second lock: %v", err)
	}
	unlock()
	unlock, err = lockUpdates(dir)
	if err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	unlock()
}

// The staged binary must live where only root can change files.
func TestCheckRootOwnedChain(t *testing.T) {
	dir := t.TempDir() // below /tmp (writable by everyone) and, unless root, owned by the tester
	if err := checkRootOwnedChain(dir); err == nil {
		t.Fatalf("%s accepted", dir)
	}
	if err := checkRootOwnedChain(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing directory accepted")
	}
	if fi, err := os.Stat("/usr/bin"); err == nil && fi.Sys().(*syscall.Stat_t).Uid == 0 {
		if err := checkRootOwnedChain("/usr/bin"); err != nil {
			t.Fatalf("/usr/bin: %v", err)
		}
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("/usr/bin", link); err != nil {
		t.Fatal(err)
	}
	if err := checkRootOwnedChain(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("link: %v", err)
	}
}

// A restored database keeps the owner and mode of the file it replaces
// (strict runs are root runs).
func TestRestoreDatabaseKeepsOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	data := t.TempDir()
	db := filepath.Join(data, configDBName)
	writeFile(t, db, []byte("migrated"))
	if err := os.Chown(db, 1234, 2345); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(db, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(data, backupsDirName), 0o750); err != nil {
		t.Fatal(err)
	}
	since := time.Now()
	writeFile(t, filepath.Join(data, backupsDirName, "picache-v1.0.0-20260925T101010.db"), []byte("copy"))
	name, err := restoreDatabase(data, "v1.0.0", since, true)
	if err != nil || name != "picache-v1.0.0-20260925T101010.db" {
		t.Fatalf("restoreDatabase = %q, %v", name, err)
	}
	fi, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if readFile(t, db) != "copy" || st.Uid != 1234 || st.Gid != 2345 || fi.Mode().Perm() != 0o640 {
		t.Fatalf("restored: %q uid %d gid %d mode %v", readFile(t, db), st.Uid, st.Gid, fi.Mode())
	}

	// A copy that is a symbolic link (the directory belongs to the service)
	// is never followed.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(data, backupsDirName, name), past, past); err != nil {
		t.Fatal(err)
	}
	since = time.Now()
	if err := os.Symlink("/etc/shadow", filepath.Join(data, backupsDirName, "picache-v1.0.0-20260925T111111.db")); err != nil {
		t.Fatal(err)
	}
	if name, err := restoreDatabase(data, "v1.0.0", since, true); name != "" || err != nil {
		t.Fatalf("linked copy: %q, %v", name, err)
	}
}

// A copy that is a hard link (the service linked someone else's file into
// its backups directory, possible without fs.protected_hardlinks) is never
// copied into the database; the service's own copy still is.
func TestRestoreDatabaseRefusesHardLinks(t *testing.T) {
	data := t.TempDir()
	db := filepath.Join(data, configDBName)
	writeFile(t, db, []byte("migrated"))
	backups := filepath.Join(data, backupsDirName)
	if err := os.MkdirAll(backups, 0o750); err != nil {
		t.Fatal(err)
	}
	since := time.Now()
	secret := filepath.Join(data, "secret")
	writeFile(t, secret, []byte("root only"))
	if err := os.Link(secret, filepath.Join(backups, "picache-v1.0.0-20260925T101010.db")); err != nil {
		t.Fatal(err)
	}
	if name, err := restoreDatabase(data, "v1.0.0", since, false); name != "" || err != nil || readFile(t, db) != "migrated" {
		t.Fatalf("linked copy: %q, %v, database %q", name, err, readFile(t, db))
	}
	later := time.Now().Add(time.Second)
	own := filepath.Join(backups, "picache-v1.0.0-20260925T101011.db")
	writeFile(t, own, []byte("copy"))
	if err := os.Chtimes(own, later, later); err != nil {
		t.Fatal(err)
	}
	if name, err := restoreDatabase(data, "v1.0.0", since, false); name != "picache-v1.0.0-20260925T101011.db" || err != nil || readFile(t, db) != "copy" {
		t.Fatalf("own copy: %q, %v, database %q", name, err, readFile(t, db))
	}
}

// The free space the service may still use is known on Linux.
func TestUserFreeBytes(t *testing.T) {
	if free, ok := userFreeBytes(t.TempDir()); !ok || free == 0 {
		t.Fatalf("userFreeBytes = %d, %v", free, ok)
	}
}
