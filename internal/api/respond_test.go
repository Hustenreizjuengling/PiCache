package api

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/db"
)

// OPS-10: a write that fails because the data disk is full answers 503
// with what to fix instead of the generic 500.
func TestWriteErrorDiskFull(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "full.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.W.Exec(`CREATE TABLE x (b BLOB)`); err != nil {
		t.Fatal(err)
	}
	var pages int
	if err := d.W.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	// The writer pool has one connection: the database cannot grow now.
	if _, err := d.W.Exec(fmt.Sprintf(`PRAGMA max_page_count = %d`, pages)); err != nil {
		t.Fatal(err)
	}
	_, full := d.W.Exec(`INSERT INTO x VALUES (zeroblob(1 << 20))`)
	if full == nil {
		t.Fatal("test setup: the database grew")
	}
	log := slog.New(slog.DiscardHandler)
	r := httptest.NewRequest(http.MethodPatch, "/api/v1/settings/dns", nil)
	for name, err := range map[string]error{
		"SQLITE_FULL": fmt.Errorf("settings: %w", full),
		"ENOSPC":      &fs.PathError{Op: "write", Path: "/var/lib/picache/tmp/x", Err: syscall.ENOSPC},
	} {
		w := httptest.NewRecorder()
		writeError(w, r, log, err)
		coreWantError(t, w, http.StatusServiceUnavailable, "unavailable", "")
		if !strings.Contains(w.Body.String(), "the data disk is full") {
			t.Fatalf("%s: %s", name, w.Body)
		}
	}
	w := httptest.NewRecorder()
	writeError(w, r, log, errors.New("something else"))
	coreWantError(t, w, http.StatusInternalServerError, "internal", "")
}
