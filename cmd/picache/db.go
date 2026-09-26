package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hustenreizjuengling/picache/internal/app"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// picache db check | salvage (docs/DEPLOYMENT.md "Recovering a damaged
// picache.db"). check reads picache.db (also while PiCache runs); salvage
// copies every readable row of a damaged picache.db into a new file with a
// fresh schema of this binary, never modifying the source.

const dbUsage = "usage: picache db check | picache db salvage --out <file> [--force]"

// Exit codes of the db commands.
const (
	exitProblems = 3 // problems found (check), rows lost or skipped (salvage)
	checkLines   = 100
	salvageChunk = 1000
)

func dbCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, dbUsage)
		return 2
	}
	switch {
	case args[0] == "check" && len(args) == 1:
		return dbCheck()
	case args[0] == "salvage":
		fs := newFlags("db salvage")
		out := fs.String("out", "", "new file for the salvaged database")
		force := fs.Bool("force", false, "salvage although PiCache is running")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *out == "" {
			fmt.Fprintln(os.Stderr, dbUsage)
			return 2
		}
		return dbSalvage(*out, *force)
	}
	fmt.Fprintln(os.Stderr, dbUsage)
	return 2
}

// dbReport is the outcome of the checks of a database.
type dbReport struct {
	integrity []string // lines of integrity_check other than "ok"
	foreign   []string // foreign_key_check rows
	versions  []string // components whose version differs from this binary
}

func (r *dbReport) problems() bool {
	return len(r.integrity)+len(r.foreign)+len(r.versions) > 0
}

// checkDB runs PRAGMA integrity_check and foreign_key_check (the first 100
// lines each) and compares the component versions with this binary.
func checkDB(ctx context.Context, q *sql.DB) (dbReport, error) {
	var r dbReport
	// A check that cannot run on a damaged file is a finding, not an error.
	rows, err := q.QueryContext(ctx, fmt.Sprintf(`PRAGMA integrity_check(%d)`, checkLines))
	if err != nil {
		r.integrity = append(r.integrity, "the integrity check failed: "+err.Error())
	} else {
		for rows.Next() {
			var text string
			if err := rows.Scan(&text); err != nil {
				break
			}
			if text == "ok" {
				continue
			}
			// A row can hold several problems, one per line.
			for line := range strings.Lines(text) {
				if line = strings.TrimRight(line, "\r\n"); line != "" && len(r.integrity) < checkLines {
					r.integrity = append(r.integrity, line)
				}
			}
		}
		if err := rows.Err(); err != nil {
			r.integrity = append(r.integrity, "the integrity check failed: "+err.Error())
		}
		rows.Close()
	}
	if rows, err = q.QueryContext(ctx, `PRAGMA foreign_key_check`); err != nil {
		r.foreign = append(r.foreign, "cannot run the foreign key check: "+err.Error())
	} else {
		for rows.Next() && len(r.foreign) < checkLines {
			var table, parent string
			var rowid sql.NullInt64
			var fkid int64
			if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
				break
			}
			r.foreign = append(r.foreign, fmt.Sprintf("%s row %d refers to a missing row of %s", table, rowid.Int64, parent))
		}
		rows.Close()
	}
	have, err := componentVersions(ctx, q)
	if err != nil {
		r.versions = append(r.versions, "cannot read schema_migrations: "+err.Error())
		return r, nil
	}
	want := app.ConfigSchemaVersions()
	for _, c := range slices.Sorted(maps.Keys(mergeKeys(have, want))) {
		switch h, w := have[c], want[c]; {
		case h == w:
		case w == 0:
			r.versions = append(r.versions, fmt.Sprintf("%s v%d is unknown to this binary", c, h))
		case h < w:
			r.versions = append(r.versions, fmt.Sprintf("%s v%d is older than this binary (v%d; the service migrates it at its next start)", c, h, w))
		default:
			r.versions = append(r.versions, fmt.Sprintf("%s v%d is newer than this binary (v%d)", c, h, w))
		}
	}
	return r, nil
}

func mergeKeys(a, b map[string]int) map[string]int {
	out := maps.Clone(a)
	for k := range b {
		out[k] = 1
	}
	return out
}

// componentVersions reads the schema version of every component.
func componentVersions(ctx context.Context, q *sql.DB) (map[string]int, error) {
	rows, err := q.QueryContext(ctx, `SELECT component, MAX(version) FROM schema_migrations GROUP BY component`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var c string
		var v int
		if err := rows.Scan(&c, &v); err != nil {
			return nil, err
		}
		out[c] = v
	}
	return out, rows.Err()
}

// printReport prints the checks of a database.
func printReport(w io.Writer, what string, r dbReport) {
	section := func(name string, lines []string) {
		if len(lines) == 0 {
			fmt.Fprintf(w, "%s: ok\n", name)
			return
		}
		fmt.Fprintf(w, "%s: %d problem(s)\n", name, len(lines))
		for _, l := range lines {
			fmt.Fprintf(w, "  %s\n", escapeControls(l))
		}
	}
	fmt.Fprintf(w, "%s\n", what)
	section("integrity", r.integrity)
	section("foreign keys", r.foreign)
	section("schema versions", r.versions)
}

// dbCheck checks picache.db (read-only).
func dbCheck() int {
	cfg, path, err := configDB()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := becomeOwnerOf(cfg.DataDir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	d, err := db.OpenReadOnly(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "db check:", err)
		return 1
	}
	defer d.Close()
	r, err := checkDB(context.Background(), d.R)
	if err != nil {
		fmt.Fprintln(os.Stderr, "db check:", err)
		return 1
	}
	printReport(os.Stdout, path, r)
	if r.problems() {
		fmt.Println("Problems found: see docs/DEPLOYMENT.md \"Recovering a damaged picache.db\" (picache db salvage).")
		return exitProblems
	}
	return 0
}

// tableResult is the salvage of one table.
type tableResult struct {
	name       string
	copied     int64
	lost       int64
	incomplete bool   // the table could not be read to its end
	skipped    string // why it was not copied at all
}

// dbSalvage copies the readable rows of picache.db into a new file.
func dbSalvage(out string, force bool) int {
	cfg, path, err := configDB()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := becomeOwnerOf(cfg.DataDir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !force && serviceRunning() {
		fmt.Fprintln(os.Stderr, "db salvage: PiCache is running (its web and DNS checks answer); stop it first "+
			"(sudo systemctl stop picache, docker stop <container>) or use --force")
		return 1
	}
	ctx := context.Background()
	src, err := db.OpenReadOnly(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "db salvage:", err)
		return 1
	}
	defer src.Close()
	have, err := componentVersions(ctx, src.R)
	if err != nil {
		fmt.Fprintln(os.Stderr, "db salvage: cannot read schema_migrations of", path+":", err)
		return 1
	}
	if want := app.ConfigSchemaVersions(); !maps.Equal(have, want) {
		fmt.Fprintf(os.Stderr, "db salvage: the schema versions of %s differ from this binary; salvage with the PiCache version that wrote it\n", path)
		return 1
	}
	f, err := createExclusive(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "db salvage:", err)
		return 1
	}
	f.Close()
	results, report, err := salvageInto(ctx, src, out)
	if err != nil {
		for _, sfx := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(out + sfx) // an incomplete file is not kept
		}
		fmt.Fprintln(os.Stderr, "db salvage:", err)
		return 1
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TABLE\tCOPIED\tLOST\tNOTE")
	lost := false
	for _, r := range results {
		note := r.skipped
		if r.incomplete {
			note = "could not be read to its end"
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", escapeControls(r.name), r.copied, r.lost, escapeControls(note))
		lost = lost || r.lost > 0 || r.incomplete || strings.HasPrefix(r.skipped, "unknown table")
	}
	_ = tw.Flush()
	printReport(os.Stdout, out, report)
	fmt.Printf("\n%s holds password hashes, TOTP secrets, sealed secrets and token hashes: keep it private (mode 0600).\n", out)
	if lost || report.problems() {
		return exitProblems
	}
	return 0
}

// serviceRunning reports whether PiCache answers on this machine (the
// checks of `picache healthcheck`).
func serviceRunning() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return webCheck(ctx, localURL(), true) == nil || dnsCheck() == nil
}

// quoteIdent quotes an SQL identifier.
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// salvageInto builds this binary's schema in the new file out and copies
// every known table of src into it, then checks the new file.
func salvageInto(ctx context.Context, src *db.DB, out string) ([]tableResult, dbReport, error) {
	dst, err := db.Open(out, 1)
	if err != nil {
		return nil, dbReport{}, err
	}
	defer dst.Close()
	if err := app.MigrateConfigDB(ctx, dst); err != nil {
		return nil, dbReport{}, fmt.Errorf("build the schema: %w", err)
	}
	// Foreign keys are checked after the copy (the order of the tables is
	// arbitrary and orphans are reported, not dropped).
	if _, err := dst.W.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return nil, dbReport{}, err
	}
	known, err := tables(ctx, dst.R)
	if err != nil {
		return nil, dbReport{}, err
	}
	objects, err := src.R.QueryContext(ctx, `SELECT type, name FROM sqlite_master WHERE type IN ('table', 'view', 'trigger')
		ORDER BY type, name`)
	if err != nil {
		return nil, dbReport{}, fmt.Errorf("read the tables of the source: %w", err)
	}
	type object struct{ typ, name string }
	var list []object
	for objects.Next() {
		var o object
		if err := objects.Scan(&o.typ, &o.name); err != nil {
			objects.Close()
			return nil, dbReport{}, fmt.Errorf("read the tables of the source: %w", err)
		}
		list = append(list, o)
	}
	objects.Close()
	var results []tableResult
	for _, o := range list {
		switch {
		case strings.HasPrefix(o.name, "sqlite_") || o.name == "schema_migrations":
			continue // SQLite's own tables; the new file has this binary's versions
		case o.typ != "table":
			results = append(results, tableResult{name: o.name, skipped: o.typ + " skipped (PiCache creates none)"})
		case !slices.Contains(known, o.name):
			results = append(results, tableResult{name: o.name, skipped: "unknown table skipped"})
		default:
			results = append(results, copyTable(ctx, src.R, dst, o.name))
		}
	}
	if _, err := dst.W.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return nil, dbReport{}, err
	}
	if _, err := dst.W.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return nil, dbReport{}, err
	}
	report, err := checkDB(ctx, dst.W)
	return results, report, err
}

// tables lists the tables of a database.
func tables(ctx context.Context, q *sql.DB) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// columns lists the columns of a table.
func columns(ctx context.Context, q *sql.DB, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// copyTable copies a table (copyRows) and compares the result with the
// source's row count: a damaged page can end a read early or hide rows
// without an error, so rows the source counts that were neither copied nor
// counted as lost are lost as well. A table whose rows cannot be counted
// could not be read to its end.
func copyTable(ctx context.Context, src *sql.DB, dst *db.DB, table string) tableResult {
	res := copyRows(ctx, src, dst, table)
	if res.skipped != "" {
		return res
	}
	var n int64
	if err := src.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+quoteIdent(table)).Scan(&n); err != nil {
		res.incomplete = true
	} else if n > res.copied+res.lost {
		res.lost = n - res.copied
	}
	return res
}

// copyRows copies the columns both schemas have, by name, with INSERT OR
// REPLACE (seeded rows take the salvaged values): rowid tables in rowid
// order in chunks of 1000, a chunk that cannot be read retried row by row;
// WITHOUT ROWID tables all or nothing.
func copyRows(ctx context.Context, src *sql.DB, dst *db.DB, table string) tableResult {
	res := tableResult{name: table}
	srcCols, err := columns(ctx, src, table)
	if err != nil {
		res.skipped, res.incomplete = "columns unreadable: "+err.Error(), true
		return res
	}
	dstCols, err := columns(ctx, dst.R, table)
	if err != nil {
		res.skipped, res.incomplete = "columns unreadable: "+err.Error(), true
		return res
	}
	var cols []string
	for _, c := range dstCols {
		if slices.Contains(srcCols, c) {
			cols = append(cols, quoteIdent(c))
		}
	}
	if len(cols) == 0 {
		res.skipped = "no common columns"
		return res
	}
	colList := strings.Join(cols, ", ")
	insert := `INSERT OR REPLACE INTO ` + quoteIdent(table) + ` (` + colList + `) VALUES (?` + strings.Repeat(", ?", len(cols)-1) + `)`
	var ddl string
	_ = dst.R.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&ddl)
	if strings.Contains(strings.ToUpper(ddl), "WITHOUT ROWID") {
		rows, err := readRows(ctx, src, `SELECT `+colList+` FROM `+quoteIdent(table), len(cols))
		if err != nil {
			res.incomplete = true
			_ = src.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+quoteIdent(table)).Scan(&res.lost)
			return res
		}
		n, lost := insertRows(ctx, dst, insert, rows, true)
		res.copied, res.lost = n, lost
		return res
	}
	var maxRowid sql.NullInt64
	if err := src.QueryRowContext(ctx, `SELECT MAX(rowid) FROM `+quoteIdent(table)).Scan(&maxRowid); err != nil {
		res.incomplete = true
	}
	var last int64
	failedWindows := 0
	for {
		rows, err := readRows(ctx, src, `SELECT rowid, `+colList+` FROM `+quoteIdent(table)+
			` WHERE rowid > `+fmt.Sprint(last)+` ORDER BY rowid LIMIT `+fmt.Sprint(salvageChunk), len(cols)+1)
		if err == nil {
			if len(rows) == 0 {
				return res
			}
			failedWindows = 0
			last = rows[len(rows)-1][0].(int64)
			for i := range rows {
				rows[i] = rows[i][1:]
			}
			n, lost := insertRows(ctx, dst, insert, rows, false)
			res.copied += n
			res.lost += lost
			continue
		}
		// Row by row through the rowids of the next window.
		end := last + salvageChunk
		for id := last + 1; id <= end; id++ {
			row, err := readRows(ctx, src, `SELECT `+colList+` FROM `+quoteIdent(table)+` WHERE rowid = `+fmt.Sprint(id), len(cols))
			if err != nil {
				res.lost++
				continue
			}
			n, lost := insertRows(ctx, dst, insert, row, false)
			res.copied += n
			res.lost += lost
		}
		last = end
		failedWindows++
		// Past the largest rowid (or, when it is unreadable, after 1000
		// unreadable windows) the rest of the table is lost.
		if (maxRowid.Valid && last >= maxRowid.Int64) || (!maxRowid.Valid && failedWindows >= 1000) {
			res.incomplete = !maxRowid.Valid
			return res
		}
	}
}

// readRows reads all rows of a query with n columns.
func readRows(ctx context.Context, q *sql.DB, query string, n int) ([][]any, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		vals := make([]any, n)
		ptrs := make([]any, n)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, vals)
	}
	return out, rows.Err()
}

// insertRows inserts rows in one transaction and returns the rows copied
// and lost (rows the new schema refused). allOrNothing loses every row
// when one fails.
func insertRows(ctx context.Context, dst *db.DB, insert string, rows [][]any, allOrNothing bool) (copied, lost int64) {
	errAbort := errors.New("abort")
	err := dst.Tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, insert)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range rows {
			if _, err := stmt.ExecContext(ctx, r...); err != nil {
				if allOrNothing {
					return errAbort
				}
				lost++
				continue
			}
			copied++
		}
		return nil
	})
	if err != nil {
		return 0, int64(len(rows))
	}
	return copied, lost
}
