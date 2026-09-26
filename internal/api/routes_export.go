package api

import (
	"bufio"
	"encoding/csv"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/logs"
)

// Query-log export (GET /logs/queries/export, docs/API.md): the filters
// of GET /logs/queries as NDJSON or CSV, newest first, read in keyset
// chunks (logs.ExportQueries) so no read transaction spans chunks. One
// export at a time; it stops when the client disconnects, after
// exportMaxRows rows or after exportMaxTime.
const (
	exportMaxRows  = 1_000_000
	exportMaxTime  = 15 * time.Minute
	exportPerChunk = 60 * time.Second // write deadline added per chunk
)

// exportCSVHeader is the header row of the CSV export.
var exportCSVHeader = []string{"time", "clientIp", "clientName", "qname", "qtype", "status", "rcode", "reason", "listId",
	"ruleId", "service", "upstream", "durationUs", "answer", "upstreamAnswer", "dnssec", "protocol", "upstreamEdeCode",
	"upstreamEdeText", "ecs"}

// queryFilter reads the query-log filters shared by GET /logs/queries and
// the export (range default 1 h).
func queryFilter(r *http.Request) (logs.QueryFilter, error) {
	from, to, err := qRange(r, logsQueryRange)
	if err != nil {
		return logs.QueryFilter{}, err
	}
	f := logs.QueryFilter{
		From: from, To: to,
		Clients:  qStrings(r, "client"),
		Domain:   qString(r, "domain"),
		Status:   qList(r, "status"),
		QType:    qString(r, "qtype"),
		Upstream: qString(r, "upstream"),
		RCode:    qList(r, "rcode"),
	}
	switch v := qString(r, "dnssec"); v {
	case "":
	case "true", "false":
		b := v == "true"
		f.DNSSEC = &b
	default:
		return f, apperr.Invalid("dnssec", "must be true or false")
	}
	return f, nil
}

// exportFileTime formats a time for the export's file name.
func exportFileTime(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// csvCell removes control and bidi characters and protects a cell a
// spreadsheet would run as a formula with a leading '.
func csvCell(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f,
			r == 0x061c, r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// exportCSVRow is the CSV record of a query event.
func exportCSVRow(e *logs.QueryEvent) []string {
	id := func(v int64) string {
		if v == 0 {
			return ""
		}
		return strconv.FormatInt(v, 10)
	}
	edeCode, edeText := "", ""
	if e.UpstreamEDE != nil {
		edeCode, edeText = strconv.Itoa(e.UpstreamEDE.Code), e.UpstreamEDE.Text
	}
	return []string{e.Time.UTC().Format(time.RFC3339Nano), csvCell(e.ClientIP), csvCell(e.ClientName), csvCell(e.QName),
		csvCell(e.QType), csvCell(e.Status), csvCell(e.RCode), csvCell(e.Reason), id(e.ListID), id(e.RuleID),
		csvCell(e.Service), csvCell(e.Upstream), strconv.FormatInt(e.DurationUs, 10), csvCell(e.Answer),
		csvCell(e.UpstreamAnswer), strconv.FormatBool(e.DNSSEC), csvCell(e.Protocol), edeCode, csvCell(edeText), csvCell(e.ECS)}
}

// logsExport streams the query log as NDJSON or CSV.
func (s *Server) logsExport(w http.ResponseWriter, r *http.Request) error {
	format := qString(r, "format")
	if format != "ndjson" && format != "csv" {
		return apperr.Invalid("format", "must be ndjson or csv")
	}
	f, err := queryFilter(r)
	if err != nil {
		return err
	}
	if !s.exporting.CompareAndSwap(false, true) {
		return apperr.TooMany("another query-log export is running; try again when it is done")
	}
	defer s.exporting.Store(false)
	maxRows, maxTime := s.exportMaxRows, s.exportMaxTime
	if maxRows <= 0 {
		maxRows = exportMaxRows
	}
	if maxTime <= 0 {
		maxTime = exportMaxTime
	}
	start := time.Now()
	ctx := r.Context()
	bw := bufio.NewWriterSize(w, 64<<10)
	var cw *csv.Writer
	started := false
	begin := func() {
		started = true
		h := w.Header()
		if format == "csv" {
			h.Set("Content-Type", "text/csv; charset=utf-8")
		} else {
			h.Set("Content-Type", "application/x-ndjson")
		}
		h.Set("Content-Disposition", `attachment; filename="picache-queries-`+exportFileTime(f.From)+"-"+exportFileTime(f.To)+"."+format+`"`)
		w.WriteHeader(http.StatusOK)
		if format == "csv" {
			cw = csv.NewWriter(bw)
			_ = cw.Write(exportCSVHeader)
		}
	}
	rows, truncated := 0, ""
	writeErr := false
	err = s.d.Logs.ExportQueries(ctx, f, func(chunk []logs.QueryEvent) (bool, error) {
		if !started {
			begin()
		}
		extendDeadlines(w, exportPerChunk)
		for i := range chunk {
			switch {
			case ctx.Err() != nil:
				truncated = "disconnected"
			case rows >= maxRows:
				truncated = "rows"
			case time.Since(start) >= maxTime:
				truncated = "time"
			}
			if truncated != "" {
				return false, nil
			}
			if format == "csv" {
				_ = cw.Write(exportCSVRow(&chunk[i]))
			} else {
				if err := json.MarshalWrite(bw, &chunk[i]); err != nil {
					return false, err
				}
				_ = bw.WriteByte('\n')
			}
			rows++
		}
		if cw != nil {
			cw.Flush()
		}
		if err := bw.Flush(); err != nil {
			writeErr, truncated = true, "disconnected"
			return false, nil
		}
		if ctx.Err() != nil {
			truncated = "disconnected"
			return false, nil
		}
		return true, nil
	})
	if err != nil && !started {
		return err // nothing sent yet: an ordinary error answer
	}
	if err != nil {
		// Abort the connection: a cut file must not look complete.
		s.audit(r, "logs.export", "", map[string]any{"format": format, "rows": rows, "truncated": "disconnected"})
		if ctx.Err() == nil {
			s.log.Error("query-log export failed", slog.Any("err", err))
		}
		panic(http.ErrAbortHandler)
	}
	if !started {
		begin()
	}
	if (truncated == "rows" || truncated == "time") && !writeErr {
		if format == "csv" {
			_ = cw.Write([]string{"# truncated: " + truncated})
		} else {
			_, _ = bw.WriteString(`{"truncated":true,"reason":"` + truncated + `"}` + "\n")
		}
	}
	if cw != nil {
		cw.Flush()
	}
	_ = bw.Flush()
	s.audit(r, "logs.export", "", map[string]any{"format": format, "rows": rows, "truncated": truncated})
	return nil
}
