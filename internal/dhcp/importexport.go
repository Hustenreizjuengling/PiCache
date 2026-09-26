package dhcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// Import bounds (POST /dhcp/static/import; the API's 1 MiB body limit
// applies too).
const (
	maxImportText = 256 << 10
	maxImportRows = 1024
)

// csvHeader is the header of the CSV export (and the one an import skips).
var csvHeader = []string{"mac", "ip", "hostname", "comment", "clientId", "leaseSeconds"}

// ExportStatics returns the reservations in format csv (header, one row
// per reservation by address; fields a spreadsheet would run as a formula
// get a leading ') or hosts (one "<ip>\t<hostname>\t# <mac>" line per
// reservation with a host name).
func (s *Service) ExportStatics(format string) ([]byte, error) {
	list := s.Statics()
	var b bytes.Buffer
	switch format {
	case FormatCSV:
		w := csv.NewWriter(&b)
		_ = w.Write(csvHeader)
		for _, st := range list {
			_ = w.Write([]string{st.MAC, st.IP, csvSafe(st.Hostname), csvSafe(st.Comment), csvSafe(st.ClientID), strconv.Itoa(st.LeaseSeconds)})
		}
		w.Flush()
		if err := w.Error(); err != nil {
			return nil, err
		}
	case FormatHosts:
		b.WriteString("# PiCache reserved addresses\n")
		for _, st := range list {
			if st.Hostname != "" {
				fmt.Fprintf(&b, "%s\t%s\t# %s\n", st.IP, st.Hostname, st.MAC)
			}
		}
	default:
		return nil, apperr.Invalid("format", "must be csv or hosts")
	}
	return b.Bytes(), nil
}

// csvSafe prefixes a field a spreadsheet would treat as a formula with '.
func csvSafe(f string) string {
	if f != "" && strings.ContainsRune("=+-@\t\r", rune(f[0])) {
		return "'" + f
	}
	return f
}

// csvUnsafe removes the prefix csvSafe added.
func csvUnsafe(f string) string {
	if len(f) > 1 && f[0] == '\'' && strings.ContainsRune("=+-@\t\r", rune(f[1])) {
		return f[1:]
	}
	return f
}

// importRow is one data row of an import. A nil member is an absent
// column: the stored value is kept; an empty one clears it.
type importRow struct {
	line                                      int
	mac, ip                                   string
	hostname, comment, clientID, leaseSeconds *string
}

// ImportStatics imports reservations (POST /dhcp/static/import): csv
// (mac,ip,hostname,comment[,clientId[,leaseSeconds]], an optional header),
// hosts ("ip name [aliases] # mac") or lines ("mac ip [hostname]"). Every
// row is validated like POST /dhcp/static, plus: MAC, address, host name
// and client identifier unique within the batch and against the
// reservations that stay (with replace: none), at most 1024 reservations
// in the end. A known MAC is updated, an unknown one added; with replace
// the reservations whose MAC is not in the batch are removed. With any
// error, or with dryRun, nothing is written and the counts say what the
// valid rows would do. Otherwise everything is written in one transaction.
// Request errors (format, text too long or with too many rows) are
// apperr.Invalid.
func (s *Service) ImportStatics(ctx context.Context, in ImportInput) (ImportResult, error) {
	if len(in.Text) > maxImportText {
		return ImportResult{}, apperr.Invalid("text", "must be at most %d KiB", maxImportText>>10)
	}
	var parse func(string, importErrors) ([]importRow, error)
	switch in.Format {
	case FormatCSV:
		parse = parseCSV
	case FormatHosts:
		parse = parseHosts
	case FormatLines:
		parse = parseLines
	default:
		return ImportResult{}, apperr.Invalid("format", "must be csv, hosts or lines")
	}
	errs := importErrors{}
	rows, err := parse(in.Text, errs)
	if err != nil {
		return ImportResult{}, err
	}

	// Validate every row like a single reservation (the stored values fill
	// the absent columns).
	s.mu.Lock()
	snapshot := make(map[string]*static, len(s.t.statics))
	for mac, st := range s.t.statics {
		snapshot[mac] = st
	}
	s.mu.Unlock()
	var cands []candidate
	inBatch := map[string]int{} // MAC → line
	checkIP := s.staticIPCheck()
	for _, r := range rows {
		mac, ok := NormalizeMAC(r.mac)
		if !ok {
			errs.add(r.line, "mac", msgMAC)
			continue
		}
		if first, dup := inBatch[mac]; dup {
			errs.add(r.line, "mac", fmt.Sprintf("%s is listed twice (line %d)", mac, first))
			continue
		}
		inBatch[mac] = r.line
		old := snapshot[mac]
		upd := StaticUpdate{IP: r.ip}
		if old != nil {
			upd.Hostname, upd.Comment, upd.ClientID, upd.LeaseSeconds = old.hostname, old.comment, old.clientID, old.leaseSeconds
		}
		for _, f := range []struct {
			v   *string
			dst *string
		}{{r.hostname, &upd.Hostname}, {r.comment, &upd.Comment}, {r.clientID, &upd.ClientID}} {
			if f.v != nil {
				*f.dst = *f.v
			}
		}
		if r.leaseSeconds != nil {
			upd.LeaseSeconds = 0
			if v := strings.TrimSpace(*r.leaseSeconds); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil {
					errs.add(r.line, "leaseSeconds", "must be a number of seconds")
					continue
				}
				upd.LeaseSeconds = n
			}
		}
		st, err := validateStatic(checkIP, mac, upd)
		if err != nil {
			field, msg := "row", err.Error()
			if e, ok := apperr.As(err); ok {
				field, msg = e.Field, e.Message
			}
			errs.add(r.line, field, msg)
			continue
		}
		cands = append(cands, candidate{line: r.line, st: st, old: old})
	}

	s.mu.Lock()
	res, err := s.applyImport(ctx, in, snapshot, cands, inBatch, errs)
	s.mu.Unlock()
	if res.Applied {
		s.refreshNames()
	}
	return res, err
}

// applyImport checks the validated rows as a batch and writes them (s.mu
// held).
func (s *Service) applyImport(ctx context.Context, in ImportInput, snapshot map[string]*static, cands []candidate,
	inBatch map[string]int, errs importErrors) (ImportResult, error) {
	if len(s.t.statics) != len(snapshot) || !mapsSame(s.t.statics, snapshot) {
		errs.add(0, "text", "the reservations changed while they were imported; import again")
	}
	// The batch must be consistent in itself and with the reservations
	// that stay.
	var stay []*static
	if !in.Replace {
		for mac, st := range s.t.statics {
			if _, ok := inBatch[mac]; !ok {
				stay = append(stay, st)
			}
		}
	}
	res := ImportResult{Errors: []ImportError{}}
	var valid []candidate
	for i, c := range cands {
		var conflict error
		for _, prev := range cands[:i] {
			if err := staticConflict(c.st, prev.st); err != nil {
				e, _ := apperr.As(err)
				conflict = apperr.Invalid(e.Field, "%s (line %d)", strings.Replace(e.Message, "the static lease of", "the row of", 1), prev.line)
				break
			}
		}
		for _, other := range stay {
			if conflict != nil {
				break
			}
			conflict = staticConflict(c.st, other)
		}
		if conflict != nil {
			e, _ := apperr.As(conflict)
			errs.add(c.line, e.Field, e.Message)
			continue
		}
		valid = append(valid, c)
		switch {
		case c.old == nil:
			res.Added++
		case c.old.sameContent(c.st):
			res.Unchanged++
		default:
			res.Updated++
		}
	}
	var removed []string
	if in.Replace {
		for mac := range s.t.statics {
			if _, ok := inBatch[mac]; !ok {
				removed = append(removed, mac)
			}
		}
	}
	res.Removed = len(removed)
	if total := len(stay) + len(inBatch); total > maxStatics {
		errs.add(0, "text", fmt.Sprintf("the import would leave %d reservations; at most %d are allowed", total, maxStatics))
	}
	res.Errors = errs.list()
	if len(res.Errors) > 0 || in.DryRun {
		return res, nil
	}

	// One transaction: rows that are removed or updated are deleted first,
	// then the added and updated ones inserted, so addresses can swap.
	now := s.now()
	var write []*static
	drop := slices.Clone(removed)
	for _, c := range valid {
		if c.old != nil && c.old.sameContent(c.st) {
			continue
		}
		c.st.created, c.st.updated = now, now
		if c.old != nil {
			c.st.created = c.old.created
			drop = append(drop, c.st.mac)
		}
		write = append(write, c.st)
	}
	wctx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	err := s.d.DB.Tx(wctx, func(tx *sql.Tx) error {
		for _, mac := range drop {
			if _, err := tx.ExecContext(wctx, `DELETE FROM dhcp_static WHERE mac = ?`, mac); err != nil {
				return err
			}
		}
		for _, st := range write {
			if err := insertStatic(wctx, tx, st); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ImportResult{}, err
	}
	for _, mac := range drop {
		s.t.dropStatic(mac)
	}
	for _, st := range write {
		s.t.putStatic(st)
	}
	res.Applied = true
	return res, nil
}

// candidate is a validated import row: the reservation it makes and the
// one it replaces (nil: added).
type candidate struct {
	line int
	st   *static
	old  *static
}

// mapsSame reports whether two reservation maps hold the same entries.
func mapsSame(a, b map[string]*static) bool {
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// importErrors keeps the first error of each line.
type importErrors map[int]ImportError

// errTooManyRows is the request error of a text with more than
// maxImportRows data rows.
var errTooManyRows = apperr.Invalid("text", "at most %d rows can be imported at once", maxImportRows)

// tooMany reports whether the data rows read so far (valid and erroneous)
// exceed maxImportRows. The parsers then stop at once, so neither their
// work nor the error map grows with the text.
func tooMany(rows []importRow, errs importErrors) bool {
	return len(rows)+len(errs) > maxImportRows
}

func (e importErrors) add(line int, field, msg string) {
	if _, ok := e[line]; !ok || line == 0 {
		if cur, ok := e[line]; ok && line == 0 {
			msg = cur.Message + "; " + msg
			field = cur.Field
		}
		e[line] = ImportError{Line: line, Field: field, Message: msg}
	}
}

func (e importErrors) list() []ImportError {
	out := make([]ImportError, 0, len(e))
	for _, x := range e {
		out = append(out, x)
	}
	slices.SortFunc(out, func(a, b ImportError) int { return a.Line - b.Line })
	return out
}

// parseCSV reads RFC 4180 records of 4 to 6 fields (the first record is a
// header when its first field is "mac"); a ' before =, +, -, @, TAB or CR
// is removed. Line numbers count every line of the text. More than
// maxImportRows data rows are errTooManyRows.
func parseCSV(text string, errs importErrors) ([]importRow, error) {
	r := csv.NewReader(strings.NewReader(text))
	r.FieldsPerRecord = -1
	var rows []importRow
	first := true
	// Every record or error consumes at least one line, so the reader
	// cannot return more of them than the text has lines.
	limit := strings.Count(text, "\n") + 1
	for guard := 0; guard <= limit; guard++ {
		if tooMany(rows, errs) {
			return nil, errTooManyRows
		}
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				errs.add(pe.StartLine, "row", pe.Err.Error())
				first = false
				continue
			}
			errs.add(0, "text", err.Error())
			break
		}
		line, _ := r.FieldPos(0)
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue
		}
		if first {
			first = false
			if strings.EqualFold(strings.TrimSpace(rec[0]), "mac") {
				continue
			}
		}
		if len(rec) < 4 || len(rec) > 6 {
			errs.add(line, "row", fmt.Sprintf("has %d fields; a row is mac,ip,hostname,comment[,clientId[,leaseSeconds]]", len(rec)))
			continue
		}
		for i := range rec {
			rec[i] = csvUnsafe(rec[i])
		}
		row := importRow{line: line, mac: rec[0], ip: rec[1], hostname: &rec[2], comment: &rec[3]}
		if len(rec) > 4 {
			row.clientID = &rec[4]
		}
		if len(rec) > 5 {
			row.leaseSeconds = &rec[5]
		}
		rows = append(rows, row)
	}
	if tooMany(rows, errs) {
		return nil, errTooManyRows
	}
	return rows, nil
}

// importLines calls fn with every line that is neither empty nor a
// comment (starting with #) and its 1-based number; fn returns the data
// row of the line, or nil after recording an error. It stops with
// errTooManyRows as soon as more than maxImportRows data rows (valid or
// erroneous) have been read.
func importLines(text string, errs importErrors, fn func(line int, s string) *importRow) ([]importRow, error) {
	var rows []importRow
	line := 0
	for l := range strings.SplitSeq(text, "\n") {
		line++
		l = strings.TrimSpace(strings.TrimSuffix(l, "\r"))
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if row := fn(line, l); row != nil {
			rows = append(rows, *row)
		}
		if tooMany(rows, errs) {
			return nil, errTooManyRows
		}
	}
	return rows, nil
}

// parseHosts reads hosts lines "ip name [aliases…] # mac [text]": the
// name is the host name (aliases and the text are ignored); the comment
// naming the MAC is required.
func parseHosts(text string, errs importErrors) ([]importRow, error) {
	return importLines(text, errs, func(line int, l string) *importRow {
		head, tail, _ := strings.Cut(l, "#")
		f, t := strings.Fields(head), strings.Fields(tail)
		switch {
		case len(f) < 2:
			errs.add(line, "row", "a line is: ip name [aliases] # mac")
		case len(t) == 0:
			errs.add(line, "mac", "the comment must name the MAC address: ip name # mac")
		default:
			return &importRow{line: line, mac: t[0], ip: f[0], hostname: &f[1]}
		}
		return nil
	})
}

// parseLines reads "mac ip [hostname]" lines.
func parseLines(text string, errs importErrors) ([]importRow, error) {
	return importLines(text, errs, func(line int, l string) *importRow {
		f := strings.Fields(l)
		if len(f) < 2 || len(f) > 3 {
			errs.add(line, "row", "a line is: mac ip [hostname]")
			return nil
		}
		row := &importRow{line: line, mac: f[0], ip: f[1]}
		if len(f) == 3 {
			row.hostname = &f[2]
		}
		return row
	})
}
