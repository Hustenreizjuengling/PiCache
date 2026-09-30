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
			"(Docker, with the container stopped: `docker run --rm --user 65532:65532 -v <data volume>:/data --entrypoint /picache <image> db check`; "+
			"docs/DEPLOYMENT.md \"Recovering a damaged picache.db\")", err, path)
	case sqliteCantOpen, sqliteReadOnly, sqlitePerm:
		if hint := writableHint(path); hint != "" {
			return fmt.Errorf("%w; %s", err, hint)
		}
	}
	return err
}

// checkExistingInstallation refuses to start with a new, empty picache.db
// (dbPath) in the data directory of an existing installation: its
// instance-id exists, which the first start writes. A missing or empty
// file (a file restore that failed on a full disk, fsck, a mistake) would
// otherwise silently become a new installation: first-run setup offered,
// the accounts, rules and parental controls gone, filtering on the
// defaults. Deleting instance-id starts a new installation deliberately.
func checkExistingInstallation(dbPath, dataDir string) error {
	what := "missing"
	fi, err := os.Stat(dbPath)
	switch {
	case err == nil && fi.Size() > 0:
		return nil
	case err == nil:
		what = "empty"
	case !errors.Is(err, os.ErrNotExist):
		return nil // db.Open reports it
	}
	id := filepath.Join(dataDir, "instance-id")
	if _, err := os.Stat(id); err != nil {
		return nil // a new installation
	}
	return fmt.Errorf("%s is %s, but %s belongs to an existing installation (%s exists): PiCache does not start a new, "+
		"empty configuration there. Put the configuration back as picache.db with PiCache stopped: a pre-upgrade copy in %s, "+
		"a scheduled or downloaded backup (docs/DEPLOYMENT.md \"Backup and restore\", restore from files). "+
		"To start a new installation deliberately, delete %s",
		dbPath, what, dataDir, id, filepath.Join(dataDir, "backups"), id)
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
