package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/app"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// dbEnv creates a picache.db with this binary's schema and 3000 clients in
// a new data directory; nothing answers on the web and DNS ports.
func dbEnv(t *testing.T) string {
	t.Helper()
	dir := cliEnv(t)
	t.Setenv("PICACHE_WEB_LISTEN", "127.0.0.1:"+closedPort(t))
	t.Setenv("PICACHE_DNS_LISTEN", "127.0.0.1:"+closedPort(t))
	path := filepath.Join(dir, "picache.db")
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if err := app.MigrateConfigDB(ctx, d); err != nil {
		t.Fatal(err)
	}
	tx, err := d.W.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3000 {
		if _, err := tx.Exec(`INSERT INTO client_clients (name, comment, created_at, updated_at) VALUES (?, ?, 1, 1)`,
			fmt.Sprintf("client %d", i), strings.Repeat("c", 200)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO client_memberships (client_id, group_id) VALUES (1, 1), (2, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`INSERT INTO settings (id, doc, updated_at) VALUES (1, '{"web":{"language":"de"}}', 1)`); err != nil {
		t.Fatal(err)
	}
	return path
}

// closedPort returns a TCP port nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	l.Close()
	return port
}

// corruptPage overwrites the header of a leaf page of table.
func corruptPage(t *testing.T, path, table string) {
	t.Helper()
	overwriteLeaf(t, path, table, 0)
}

// overwriteLeaf overwrites 64 bytes at offset off of a leaf page of table.
func overwriteLeaf(t *testing.T, path, table string, off int64) {
	t.Helper()
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	var page int64
	err = d.R.QueryRow(`SELECT pageno FROM dbstat WHERE name = ? AND pagetype = 'leaf' ORDER BY pageno LIMIT 1 OFFSET 20`, table).Scan(&page)
	d.Close()
	if err != nil {
		t.Skipf("dbstat unavailable: %v", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xff}, 64), (page-1)*4096+off); err != nil {
		t.Fatal(err)
	}
}

// salvageCounts returns the copied and lost rows of table in a salvage
// report.
func salvageCounts(t *testing.T, report, table string) (copied, lost int) {
	t.Helper()
	for _, l := range strings.Split(report, "\n") {
		if f := strings.Fields(l); len(f) >= 3 && f[0] == table {
			if _, err := fmt.Sscanf(f[1]+" "+f[2], "%d %d", &copied, &lost); err == nil {
				return copied, lost
			}
		}
	}
	t.Fatalf("no line for %s in %q", table, report)
	return 0, 0
}

// A damaged cell pointer array hides rows without an error: SQLite returns
// a garbage row, and a read that continues after it skips the rest of the
// page. check prints every problem on its own line; salvage counts the
// hidden rows as lost (from the source's row count) instead of reporting
// them neither copied nor lost.
func TestDBSalvageHiddenRows(t *testing.T) {
	path := dbEnv(t)
	overwriteLeaf(t, path, "client_clients", 12) // the cell pointers after the first two
	code, out, errOut := capture(t, func() int { return run([]string{"db", "check"}) })
	if code != exitProblems || strings.Contains(out, "integrity: ok") {
		t.Fatalf("check: %d %q %q", code, out, errOut)
	}
	if strings.Contains(out, `\u000a`) {
		t.Errorf("a problem line holds several lines: %q", out)
	}
	if n := strings.Count(out, "\n"); n > checkLines+10 {
		t.Errorf("check printed %d lines", n)
	}
	dst := filepath.Join(filepath.Dir(path), "picache.db.salvaged")
	code, stdout, errOut := capture(t, func() int { return run([]string{"db", "salvage", "--out", dst}) })
	if code != exitProblems {
		t.Fatalf("salvage: %d %q %q", code, stdout, errOut)
	}
	copied, lost := salvageCounts(t, stdout, "client_clients")
	s, err := db.OpenReadOnly(dst)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	s.R.QueryRow(`SELECT COUNT(*) FROM client_clients`).Scan(&n)
	s.Close()
	if n != copied || lost == 0 || copied+lost < 3000 {
		t.Fatalf("client_clients: copied %d, lost %d, %d rows in the new file (3000 before)", copied, lost, n)
	}
}

func TestDBCheck(t *testing.T) {
	path := dbEnv(t)
	code, out, errOut := capture(t, func() int { return run([]string{"db", "check"}) })
	if code != 0 || !strings.Contains(out, "integrity: ok") || !strings.Contains(out, "schema versions: ok") {
		t.Fatalf("good: %d %q %q", code, out, errOut)
	}
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`INSERT INTO client_memberships (client_id, group_id) VALUES (99999, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`DELETE FROM schema_migrations WHERE component = 'dns' AND version > 1`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	code, out, _ = capture(t, func() int { return run([]string{"db", "check"}) })
	if code != exitProblems || !strings.Contains(out, "foreign keys: 1 problem") || !strings.Contains(out, "dns v1 is older than this binary") {
		t.Fatalf("problems: %d %q", code, out)
	}
	for _, bad := range [][]string{{"db"}, {"db", "check", "x"}, {"db", "nope"}, {"db", "salvage"}, {"db", "salvage", "--out"}} {
		if code, _, _ := capture(t, func() int { return run(bad) }); code != 2 {
			t.Errorf("%v: exit %d", bad, code)
		}
	}
}

// A damaged copy: check finds the damage; salvage copies every readable
// row into a new file (never touching the source), reports what was lost
// and skipped, and the new file passes the checks.
func TestDBSalvageDamagedCopy(t *testing.T) {
	path := dbEnv(t)
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`CREATE TABLE planted (x)`, `INSERT INTO planted VALUES (1)`, `CREATE VIEW v AS SELECT 1`,
		`CREATE TRIGGER keep BEFORE DELETE ON auth_tokens BEGIN SELECT RAISE(IGNORE); END`} {
		if _, err := d.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()
	corruptPage(t, path, "client_clients")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := capture(t, func() int { return run([]string{"db", "check"}) }); code != exitProblems || !strings.Contains(out, "integrity:") ||
		strings.Contains(out, "integrity: ok") {
		t.Fatalf("check of the damaged copy: %d %q %q", code, out, errOut)
	}
	out := filepath.Join(filepath.Dir(path), "picache.db.salvaged")
	code, stdout, errOut := capture(t, func() int { return run([]string{"db", "salvage", "--out", out}) })
	if code != exitProblems {
		t.Fatalf("salvage: %d %q %q", code, stdout, errOut)
	}
	lines := map[string]string{}
	for _, l := range strings.Split(stdout, "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			lines[f[0]] = l
		}
	}
	var copied, lost int
	if _, err := fmt.Sscanf(strings.Join(strings.Fields(lines["client_clients"])[1:3], " "), "%d %d", &copied, &lost); err != nil ||
		lost == 0 || copied == 0 || copied+lost < 3000 {
		t.Fatalf("client_clients: %q (%v)", lines["client_clients"], err)
	}
	for table, want := range map[string]string{"client_groups": "1 0", "settings": "1 0", "client_memberships": "2 0"} {
		if f := strings.Fields(lines[table]); len(f) < 3 || f[1]+" "+f[2] != want {
			t.Errorf("%s: %q", table, lines[table])
		}
	}
	for _, s := range []string{"planted", "v", "keep"} {
		if !strings.Contains(lines[s], "skipped") {
			t.Errorf("%s not reported as skipped: %q", s, lines[s])
		}
	}
	if !strings.Contains(stdout, "integrity: ok") || !strings.Contains(stdout, "keep it private") {
		t.Fatalf("report %q", stdout)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("the source was modified")
	}
	if fi, err := os.Lstat(out); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("output %v %v", fi, err)
	}
	s, err := db.OpenReadOnly(out)
	if err != nil {
		t.Fatal(err)
	}
	var doc string
	var n int
	s.R.QueryRow(`SELECT doc FROM settings WHERE id = 1`).Scan(&doc)
	s.R.QueryRow(`SELECT COUNT(*) FROM client_clients`).Scan(&n)
	var objects int
	s.R.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type IN ('view', 'trigger') OR name = 'planted'`).Scan(&objects)
	s.Close()
	if doc != `{"web":{"language":"de"}}` || n != copied || objects != 0 {
		t.Fatalf("salvaged: doc %q, %d clients, %d planted objects", doc, n, objects)
	}
	// The output path exists now: refused.
	if code, _, errOut := capture(t, func() int { return run([]string{"db", "salvage", "--out", out}) }); code != 1 || !strings.Contains(errOut, "exists") {
		t.Fatalf("existing output: %d %q", code, errOut)
	}
	// A symbolic link (also a dangling one) is never followed.
	target := filepath.Join(t.TempDir(), "elsewhere.db")
	link := filepath.Join(filepath.Dir(path), "link.db")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if code, _, _ := capture(t, func() int { return run([]string{"db", "salvage", "--out", link}) }); code != 1 {
		t.Fatalf("link: exit %d", code)
	}
	if _, err := os.Lstat(target); err == nil {
		t.Fatal("a file was created through the link")
	}
}

// salvage refuses while PiCache answers (unless --force) and when the
// schema versions differ from this binary.
func TestDBSalvageRefusals(t *testing.T) {
	path := dbEnv(t)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	defer web.Close()
	t.Setenv("PICACHE_WEB_LISTEN", strings.TrimPrefix(web.URL, "http://"))
	out := filepath.Join(t.TempDir(), "s.db")
	code, _, errOut := capture(t, func() int { return run([]string{"db", "salvage", "--out", out}) })
	if code != 1 || !strings.Contains(errOut, "PiCache is running") {
		t.Fatalf("running: %d %q", code, errOut)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("output created although refused")
	}
	if code, stdout, errOut := capture(t, func() int { return run([]string{"db", "salvage", "--force", "--out", out}) }); code != 0 {
		t.Fatalf("--force: %d %q %q", code, stdout, errOut)
	}
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`INSERT INTO schema_migrations (component, version, applied_at) VALUES ('dns', 999, 1)`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	out2 := filepath.Join(t.TempDir(), "s2.db")
	code, _, errOut = capture(t, func() int { return run([]string{"db", "salvage", "--force", "--out", out2}) })
	if code != 1 || !strings.Contains(errOut, "salvage with the PiCache version that wrote it") {
		t.Fatalf("version mismatch: %d %q", code, errOut)
	}
}
