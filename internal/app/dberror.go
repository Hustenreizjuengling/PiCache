package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"modernc.org/sqlite"
)

// SQLite primary result codes (the low byte of sqlite.Error.Code).
const (
	sqlitePerm     = 3
	sqliteReadOnly = 8
	sqliteCorrupt  = 11
	sqliteCantOpen = 14
	sqliteNotADB   = 26
)

// explainConfigDBError adds the cause and the fix to an error SQLite raised
// for picache.db while PiCache starts (docs/DEPLOYMENT.md
// "Troubleshooting"): a damaged file, or one SQLite cannot open for writing
// because the file system is read-only (a failing SD card) or the file or
// the data directory is not writable by this user (a restore or a move
// without the chown). Other errors are returned as they are.
func explainConfigDBError(path string, err error) error {
	var se *sqlite.Error
	if err == nil || !errors.As(err, &se) {
		return err
	}
	switch se.Code() & 0xff {
	case sqliteCorrupt, sqliteNotADB:
		return fmt.Errorf("%w; %s is damaged: stop PiCache and run `picache db check` "+
			"(docs/DEPLOYMENT.md \"Recovering a damaged picache.db\")", err, path)
	case sqliteCantOpen, sqliteReadOnly, sqlitePerm:
		if hint := writableHint(path); hint != "" {
			return fmt.Errorf("%w; %s", err, hint)
		}
	}
	return err
}

// writableHint probes the directory of path, then path and its -wal and
// -shm files, for what keeps SQLite from writing them: a read-only file
// system or missing permissions (with the owner). "" when nothing is found.
func writableHint(path string) string {
	dir := filepath.Dir(path)
	hint := func(p string, err error) string {
		switch {
		case errors.Is(err, syscall.EROFS):
			return fmt.Sprintf("%s is on a read-only file system (a failing SD card or disk is often switched to read-only: check `dmesg` and the disk)", p)
		case errors.Is(err, os.ErrPermission):
			return fmt.Sprintf("%s is not writable by PiCache (uid %d%s): give the data directory back to the PiCache user, "+
				"e.g. `chown -R picache:picache %s` (Docker: the uid:gid of PICACHE_RUN_AS)", p, os.Getuid(), ownerOf(p), dir)
		}
		return ""
	}
	if f, err := os.CreateTemp(dir, ".picache-probe-*"); err != nil {
		if h := hint(dir, err); h != "" {
			return h
		}
	} else {
		f.Close()
		os.Remove(f.Name())
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		f, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			if h := hint(p, err); h != "" {
				return h
			}
			continue
		}
		f.Close()
	}
	return ""
}
