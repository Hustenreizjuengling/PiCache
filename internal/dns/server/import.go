package dnsserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// Import limits (POST /dns/forwarders/import).
const (
	maxImportBytes  = 256 << 10
	maxImportLines  = 1024
	maxImportErrors = 256
)

// ForwarderImport is the body of POST /dns/forwarders/import: one
// forwarder per line, "[/d1/d2/…/]t1 t2 …" (an empty domain as in "[//]"
// is Unqualified, the target "#" is DefaultTarget); empty lines and lines
// starting with "#" are skipped.
type ForwarderImport struct {
	Text   string `json:"text"`
	DryRun bool   `json:"dryRun"`
}

// ForwarderImportError is the first error of one line (line 0: the whole
// import). Field is syntax, domains, upstreams or text.
type ForwarderImportError struct {
	Line    int    `json:"line"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ForwarderImportResult says what the import did (Applied) or, after an
// error or a dry run, what the valid lines would do. Errors is never null.
type ForwarderImportResult struct {
	Applied   bool                   `json:"applied"`
	Added     int                    `json:"added"`
	Updated   int                    `json:"updated"`
	Unchanged int                    `json:"unchanged"`
	Errors    []ForwarderImportError `json:"errors"`
}

// errImportNotApplied ends the import transaction without writing.
var errImportNotApplied = errors.New("import not applied")

// importLine is one parsed line.
type importLine struct {
	n  int
	in ForwarderInput
}

// ImportForwarders imports forwarders from text, all or nothing: a line
// whose first domain is an existing forwarder's domain updates it (domains
// and targets replaced, enabled and comment kept), other lines add enabled
// forwarders. Lines are validated like POST /dns/forwarders; a domain may
// appear once in the import and belong to no other existing forwarder, and
// at most 256 forwarders may exist afterwards. Request errors (size, line
// count) are apperr.Invalid with field text.
func (s *Server) ImportForwarders(ctx context.Context, in ForwarderImport) (ForwarderImportResult, error) {
	res := ForwarderImportResult{Errors: []ForwarderImportError{}}
	if len(in.Text) > maxImportBytes {
		return res, apperr.Invalid("text", "at most %d KiB", maxImportBytes>>10)
	}
	raw := strings.Split(strings.TrimSuffix(in.Text, "\n"), "\n")
	if len(raw) > maxImportLines {
		return res, apperr.Invalid("text", "at most %d lines", maxImportLines)
	}
	rules := s.forwarderRules()
	fail := func(line int, field, format string, args ...any) {
		if len(res.Errors) < maxImportErrors {
			res.Errors = append(res.Errors, ForwarderImportError{Line: line, Field: field, Message: fmt.Sprintf(format, args...)})
		}
	}
	var lines []importLine
	seen := map[string]int{} // domain → line
	for i, text := range raw {
		n := i + 1
		text = strings.TrimSpace(strings.TrimSuffix(text, "\r"))
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fin, err := parseImportLine(text)
		if err != nil {
			fail(n, "syntax", "%s", err)
			continue
		}
		fin, err = rules.validateForwarder(fin)
		if err != nil {
			field, msg := "domains", err.Error()
			if ae, ok := apperr.As(err); ok {
				msg = ae.Message
				if strings.HasPrefix(ae.Field, "upstreams") {
					field = "upstreams"
				}
			}
			fail(n, field, "%s", msg)
			continue
		}
		dup := ""
		for _, d := range fin.Domains {
			if prev, ok := seen[d]; ok {
				dup = fmt.Sprintf("%s is on line %d already", d, prev)
				break
			}
		}
		if dup != "" {
			fail(n, "domains", "%s", dup)
			continue
		}
		for _, d := range fin.Domains {
			seen[d] = n
		}
		lines = append(lines, importLine{n: n, in: fin})
	}
	err := s.writeConfig(ctx, func(tx *sql.Tx) error {
		existing, err := queryForwarders(ctx, tx, 0)
		if err != nil {
			return err
		}
		byDomain := map[string]*Forwarder{} // first domain → forwarder
		owner := map[string]*Forwarder{}    // any domain → forwarder
		for i := range existing {
			f := &existing[i]
			byDomain[f.Domain] = f
			for _, d := range f.Domains {
				owner[d] = f
			}
		}
		type change struct {
			id int64 // 0 = add
			in ForwarderInput
		}
		var changes []change
		for _, l := range lines {
			target := byDomain[l.in.Domains[0]]
			conflict := ""
			for _, d := range l.in.Domains {
				if o := owner[d]; o != nil && o != target {
					conflict = fmt.Sprintf("%s belongs to the forwarder for %s", d, o.Domain)
					break
				}
			}
			switch {
			case conflict != "":
				fail(l.n, "domains", "%s", conflict)
			case target == nil:
				res.Added++
				changes = append(changes, change{in: l.in})
			case slices.Equal(target.Domains, l.in.Domains) && slices.Equal(target.Upstreams, l.in.Upstreams):
				res.Unchanged++
			default:
				res.Updated++
				l.in.Enabled, l.in.Comment = target.Enabled, target.Comment
				changes = append(changes, change{id: target.ID, in: l.in})
			}
		}
		if len(existing)+res.Added > maxForwarders {
			fail(0, "text", "at most %d forwarders are allowed (%d exist, %d would be added)", maxForwarders, len(existing), res.Added)
		}
		if len(res.Errors) > 0 || in.DryRun {
			return errImportNotApplied
		}
		for _, c := range changes {
			if c.id != 0 {
				if err := updateForwarder(ctx, tx, c.id, c.in); err != nil {
					return err
				}
			}
		}
		for _, c := range changes {
			if c.id == 0 {
				c.in.Enabled = true
				if _, err := insertForwarder(ctx, tx, c.in); err != nil {
					return err
				}
			}
		}
		return nil
	})
	slices.SortStableFunc(res.Errors, func(a, b ForwarderImportError) int { return a.Line - b.Line })
	switch {
	case errors.Is(err, errImportNotApplied):
		return res, nil
	case err != nil:
		return ForwarderImportResult{Errors: []ForwarderImportError{}}, err
	}
	res.Applied = true
	return res, nil
}

// parseImportLine parses "[/d1/d2/…/]t1 t2 …": the domains between the
// slashes (an empty one is Unqualified), then the whitespace-separated
// targets ("#" is DefaultTarget).
func parseImportLine(line string) (ForwarderInput, error) {
	var in ForwarderInput
	if !strings.HasPrefix(line, "[/") {
		return in, errors.New(`a line must start with the domains, e.g. [/corp.example/]192.168.1.1 or [/example.lan/example.net/]# (# = the default upstreams)`)
	}
	end := strings.Index(line, "/]")
	if end < 2 {
		return in, errors.New(`the domains must end with "/]"`)
	}
	for _, d := range strings.Split(line[2:end], "/") {
		if strings.TrimSpace(d) == "" {
			d = Unqualified
		}
		in.Domains = append(in.Domains, d)
	}
	for _, t := range strings.Fields(line[end+2:]) {
		if t == "#" {
			t = DefaultTarget
		}
		in.Upstreams = append(in.Upstreams, t)
	}
	in.Enabled = true
	return in, nil
}
