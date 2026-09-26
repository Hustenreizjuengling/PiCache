package dnsserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// Limits of POST /dns/records/import (the body is bounded by the API's
// 1 MiB).
const (
	maxRecordImportLines  = 10000
	maxRecordImportErrors = 1000
)

// hostsJunk are the names of hosts-file headers that never become records
// (the junk host names of the list parser).
var hostsJunk = map[string]bool{
	"localhost": true, "localhost.localdomain": true, "local": true, "broadcasthost": true,
	"ip6-localhost": true, "ip6-loopback": true, "ip6-localnet": true, "ip6-mcastprefix": true,
	"ip6-allnodes": true, "ip6-allrouters": true, "ip6-allhosts": true, "0.0.0.0": true,
}

// RecordImport is the body of POST /dns/records/import: a hosts file
// ("<address> <name> [<alias> …]", inline " #" comments) whose names
// become A or AAAA records of Scope (default all) and GroupIDs.
type RecordImport struct {
	Format   string  `json:"format"` // hosts
	Text     string  `json:"text"`
	Scope    *string `json:"scope"`
	GroupIDs []int64 `json:"groupIds"`
	DryRun   bool    `json:"dryRun"`
}

// RecordImportResult says what an import did (Applied) or, after an error
// or a dry run, what the valid lines would do. Errors holds the first error
// of each line, sorted by line, at most 1 000 (never null); ErrorCount
// counts every line with an error.
type RecordImportResult struct {
	Applied    bool                   `json:"applied"`
	Added      int                    `json:"added"`
	Unchanged  int                    `json:"unchanged"`
	Skipped    int                    `json:"skipped"`
	Errors     []ForwarderImportError `json:"errors"`
	ErrorCount int                    `json:"errorCount"`
}

// ImportRecords imports a hosts file, all or nothing: any error writes
// nothing; a dry run only validates and counts; otherwise every new record
// is written in one transaction and the zone is reloaded once. Every name
// (aliases too) becomes an A or AAAA record (TTL 300, enabled). Comment and
// empty lines, lines of junk names only and lines with a loopback address
// are skipped; 0.0.0.0 and :: (a blocklist), other non-unicast addresses,
// addresses with a zone, invalid or wildcard names, a name repeated with
// the same address and a conflict with a CNAME of the same scope are
// errors. A record that exists (name, type and value) is unchanged,
// whatever its scope. Request errors are apperr.Invalid.
func (s *Server) ImportRecords(ctx context.Context, in RecordImport) (RecordImportResult, error) {
	res := RecordImportResult{Errors: []ForwarderImportError{}}
	if in.Format != "hosts" {
		return res, apperr.Invalid("format", "must be hosts")
	}
	lines := strings.Split(strings.TrimSuffix(in.Text, "\n"), "\n")
	if len(lines) > maxRecordImportLines {
		return res, apperr.Invalid("text", "at most %d lines", maxRecordImportLines)
	}
	// The scope and groups of every imported record (validated like a
	// record's).
	probe, err := validateRecord(RecordInput{Name: "import.invalid", Type: "A", Value: "192.0.2.1", Scope: in.Scope, GroupIDs: in.GroupIDs}, nil)
	if err != nil {
		return res, err
	}
	failed := map[int]bool{}
	fail := func(line int, field, format string, args ...any) {
		if failed[line] && line != 0 {
			return
		}
		failed[line] = true
		res.ErrorCount++
		if len(res.Errors) < maxRecordImportErrors {
			res.Errors = append(res.Errors, ForwarderImportError{Line: line, Field: field, Message: fmt.Sprintf(format, args...)})
		}
	}
	type pending struct {
		line int
		sp   recordSpec
	}
	var recs []pending
	seen := map[string]int{} // name, type, value → line
	for i, raw := range lines {
		n := i + 1
		specs, skip, lerr := parseHostsLine(raw, probe.Scope, probe.GroupIDs)
		switch {
		case lerr != nil:
			fail(n, lerr.field, "%s", lerr.msg)
			continue
		case skip:
			res.Skipped++
			continue
		}
		dup := false
		for _, sp := range specs {
			if prev, ok := seen[sp.Name+"\x00"+sp.Type+"\x00"+sp.Value]; ok {
				fail(n, "syntax", "%s %s is listed twice (line %d)", sp.Name, sp.Value, prev)
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		for _, sp := range specs {
			seen[sp.Name+"\x00"+sp.Type+"\x00"+sp.Value] = n
			recs = append(recs, pending{line: n, sp: sp})
		}
	}

	err = s.writeConfig(ctx, func(tx *sql.Tx) error {
		if err := checkGroupIDs(ctx, tx, probe.GroupIDs); err != nil {
			return err
		}
		var add []recordSpec
		for _, p := range recs {
			var id int64
			err := tx.QueryRowContext(ctx, `SELECT id FROM dns_records WHERE name = ? AND type = ? AND value = ?`,
				p.sp.Name, p.sp.Type, p.sp.Value).Scan(&id)
			switch {
			case err == nil:
				res.Unchanged++
				continue
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
			same, err := queryRecordsByName(ctx, tx, p.sp.Name, 0)
			if err != nil {
				return err
			}
			if slices.ContainsFunc(same, func(o Record) bool {
				return o.Type == "CNAME" && scopesOverlap(p.sp.Scope, p.sp.GroupIDs, o.Scope, o.GroupIDs)
			}) {
				fail(p.line, "name", "%s has a CNAME record in the same scope", p.sp.Name)
				continue
			}
			add = append(add, p.sp)
		}
		res.Added = len(add)
		var total int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dns_records`).Scan(&total); err != nil {
			return err
		}
		if total+len(add) > maxRecords {
			fail(0, "text", "at most %d local records are allowed (%d exist, %d would be added)", maxRecords, total, len(add))
		}
		if res.ErrorCount > 0 || in.DryRun {
			return errImportNotApplied
		}
		for _, sp := range add {
			if _, err := insertRecord(ctx, tx, sp); err != nil {
				return err
			}
		}
		return nil
	})
	slices.SortStableFunc(res.Errors, func(a, b ForwarderImportError) int { return a.Line - b.Line })
	switch {
	case errors.Is(err, errImportNotApplied):
		return res, nil
	case err != nil:
		return RecordImportResult{Errors: []ForwarderImportError{}}, err
	}
	res.Applied = true
	return res, nil
}

// shortText cuts text quoted in an error message to 60 bytes (at a rune
// boundary).
func shortText(s string) string {
	if len(s) <= 60 {
		return s
	}
	cut := 60
	for cut > 0 && s[cut]&0xc0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// hostsLineError is the error of a hosts line (field syntax unless named).
type hostsLineError struct{ field, msg string }

// parseHostsLine maps one hosts line to A or AAAA records of the scope and
// groups (validated like POST /dns/records): comment and empty lines, lines
// of junk names only (the headers of hosts files: localhost,
// broadcasthost, ip6-allnodes, …) and lines with a loopback address are
// skipped; 0.0.0.0 and :: (a blocklist), other non-unicast addresses,
// addresses with a zone and invalid or wildcard names are errors. An alias
// repeated on its line counts once.
func parseHostsLine(raw, scope string, groups []int64) (specs []recordSpec, skip bool, lerr *hostsLineError) {
	text := strings.TrimSpace(raw)
	if j := strings.Index(text, "#"); j >= 0 && (j == 0 || text[j-1] == ' ' || text[j-1] == '\t') {
		text = strings.TrimSpace(text[:j])
	}
	if text == "" {
		return nil, true, nil
	}
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return nil, false, &hostsLineError{"syntax", "a hosts line is <address> <name> [<alias> …]"}
	}
	ip, err := netip.ParseAddr(fields[0])
	switch {
	case err != nil:
		return nil, false, &hostsLineError{"syntax", shortText(fields[0]) + " is not an IP address"}
	case !slices.ContainsFunc(fields[1:], func(n string) bool { return !hostsJunk[normalizeName(n)] }):
		// A header line of junk names only, whatever its address
		// ("fe80::1%lo0 localhost" of older macOS hosts files).
		return nil, true, nil
	case ip.Zone() != "":
		return nil, false, &hostsLineError{"syntax", "addresses with a zone are not supported"}
	}
	ip = ip.Unmap()
	switch {
	case ip.IsUnspecified():
		return nil, false, &hostsLineError{"syntax", "looks like a blocklist entry: subscribe the file on Filtering → Blocklists"}
	case ip.IsLoopback():
		return nil, true, nil
	case ip.IsMulticast() || ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return nil, false, &hostsLineError{"syntax", ip.String() + " is not a unicast address"}
	}
	typ := "A"
	if ip.Is6() {
		typ = "AAAA"
	}
	for _, name := range fields[1:] {
		name = normalizeName(name)
		if hostsJunk[name] {
			continue
		}
		if strings.Contains(name, "*") {
			return nil, false, &hostsLineError{"name", shortText(name) + ": wildcard names cannot be imported"}
		}
		sp, err := validateRecord(RecordInput{Name: name, Type: typ, Value: ip.String(), Enabled: true, Scope: &scope, GroupIDs: groups}, nil)
		if err != nil {
			field, msg := "syntax", err.Error()
			if ae, ok := apperr.As(err); ok {
				field, msg = ae.Field, shortText(name)+": "+ae.Message
			}
			return nil, false, &hostsLineError{field, msg}
		}
		if !slices.ContainsFunc(specs, func(x recordSpec) bool { return x.Name == sp.Name }) {
			specs = append(specs, sp)
		}
	}
	if len(specs) == 0 {
		return nil, true, nil
	}
	return specs, false, nil
}
