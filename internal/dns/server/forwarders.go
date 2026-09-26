package dnsserver

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Forwarder limits.
const (
	maxForwarders       = 256
	maxForwarderTargets = 8
	maxForwarderDomains = 16
)

// Special forwarder values: the domain Unqualified matches single-label
// names (A, AAAA, HTTPS, SVCB and ANY queries; never the root), the target
// DefaultTarget sends a domain to the default upstreams (the step-13 path),
// e.g. corp.example → the router with public.corp.example → default.
const (
	Unqualified   = "(unqualified)"
	DefaultTarget = "default"
)

// fwdEntry is one enabled forwarder domain of the snapshot.
type fwdEntry struct {
	domain    string // as configured ("*.x" for subdomains only; Unqualified)
	upstreams []string
	ips       []netip.Addr // IP-literal targets (loop guard, rate-limit exemption)
	def       bool         // target DefaultTarget: the default upstreams
}

// fwdTable is the immutable snapshot of enabled forwarders.
type fwdTable struct {
	apex        map[string]*fwdEntry // "x": x and its subdomains
	wild        map[string]*fwdEntry // "*.x" keyed by "x": subdomains only
	unqualified *fwdEntry            // Unqualified (nil if none)
	ips         []netip.Addr         // all IP-literal targets
}

func newFwdTable(fwds []Forwarder) *fwdTable {
	t := &fwdTable{apex: map[string]*fwdEntry{}, wild: map[string]*fwdEntry{}}
	for _, f := range fwds {
		if !f.Enabled {
			continue
		}
		domains := f.Domains
		if len(domains) == 0 {
			domains = []string{f.Domain}
		}
		ips := upstreamIPs(f.Upstreams)
		t.ips = append(t.ips, ips...)
		def := len(f.Upstreams) == 1 && f.Upstreams[0] == DefaultTarget
		for _, d := range domains {
			e := &fwdEntry{domain: d, upstreams: f.Upstreams, ips: ips, def: def}
			switch base, wild := strings.CutPrefix(d, "*."); {
			case d == Unqualified:
				t.unqualified = e
			case wild:
				t.wild[base] = e
			default:
				t.apex[d] = e
			}
		}
	}
	return t
}

// match returns the most specific forwarder for name (nil if none). For the
// same base domain, "*.x" wins over "x" for subdomains. explicitOnly skips
// forwarders with the target DefaultTarget as if they were absent (steps 6
// and 11a: a later settings change must never send private names to the
// default upstreams).
func (t *fwdTable) match(name string, explicitOnly bool) *fwdEntry {
	if len(t.apex) == 0 && len(t.wild) == 0 {
		return nil
	}
	usable := func(e *fwdEntry) bool { return !explicitOnly || !e.def }
	for n := name; n != ""; n = parent(n) {
		if n != name {
			if e, ok := t.wild[n]; ok && usable(e) {
				return e
			}
		}
		if e, ok := t.apex[n]; ok && usable(e) {
			return e
		}
	}
	return nil
}

// upstreamIPs returns the IP literals among upstream specs.
func upstreamIPs(specs []string) []netip.Addr {
	var out []netip.Addr
	for _, u := range specs {
		spec, err := settings.ParseUpstream(u)
		if err != nil || !spec.IsIPLit {
			continue
		}
		if ip, err := netip.ParseAddr(spec.Host); err == nil {
			out = append(out, ip.Unmap().WithZone(""))
		}
	}
	return out
}

// Forwarders lists conditional forwarders (by domain, then id).
func (s *Server) Forwarders(ctx context.Context) ([]Forwarder, error) {
	return queryForwarders(ctx, s.d.DB.R, 0)
}

// queryer is a *sql.DB or a *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// queryForwarders reads the forwarders (or forwarder id) with their domains.
func queryForwarders(ctx context.Context, q queryer, id int64) ([]Forwarder, error) {
	where, args := "", []any{}
	if id != 0 {
		where, args = " WHERE id = ?", []any{id}
	}
	rows, err := q.QueryContext(ctx, `SELECT id, domain, upstreams, enabled, comment, created_at, updated_at
		FROM dns_forwarders`+where+` ORDER BY domain, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("dns: list forwarders: %w", err)
	}
	defer rows.Close()
	out := []Forwarder{}
	for rows.Next() {
		var f Forwarder
		var ups string
		var created, updated int64
		if err := rows.Scan(&f.ID, &f.Domain, &ups, &f.Enabled, &f.Comment, &created, &updated); err != nil {
			return nil, fmt.Errorf("dns: scan forwarder: %w", err)
		}
		if err := json.Unmarshal([]byte(ups), &f.Upstreams); err != nil {
			return nil, fmt.Errorf("dns: forwarder %d upstreams: %w", f.ID, err)
		}
		f.CreatedAt, f.UpdatedAt = db.Time(created), db.Time(updated)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	domains, err := forwarderDomains(ctx, q, id)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if ds := domains[out[i].ID]; len(ds) > 0 && ds[0] == out[i].Domain {
			out[i].Domains = ds
		} else {
			out[i].Domains = []string{out[i].Domain} // a row without its domains (edited database)
		}
	}
	return out, nil
}

// forwarderDomains returns the domains of every forwarder (or of id) in
// their order.
func forwarderDomains(ctx context.Context, q queryer, id int64) (map[int64][]string, error) {
	where, args := "", []any{}
	if id != 0 {
		where, args = " WHERE forwarder_id = ?", []any{id}
	}
	rows, err := q.QueryContext(ctx, `SELECT forwarder_id, domain FROM dns_forwarder_domains`+where+
		` ORDER BY forwarder_id, position`, args...)
	if err != nil {
		return nil, fmt.Errorf("dns: list forwarder domains: %w", err)
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var fid int64
		var d string
		if err := rows.Scan(&fid, &d); err != nil {
			return nil, fmt.Errorf("dns: scan forwarder domain: %w", err)
		}
		out[fid] = append(out[fid], d)
	}
	return out, rows.Err()
}

func (s *Server) forwarder(ctx context.Context, id int64) (Forwarder, error) {
	fs, err := queryForwarders(ctx, s.d.DB.R, id)
	if err != nil {
		return Forwarder{}, err
	}
	if len(fs) == 0 {
		return Forwarder{}, apperr.NotFound("forwarder", id)
	}
	return fs[0], nil
}

// forwarderRules is what forwarder validation depends on besides the
// input: the settings and this machine's search domains.
type forwarderRules struct {
	set    *settings.All
	search []string
	rev    map[string]bool // reverse zones of dns.privateReverseNetworks
}

func (s *Server) forwarderRules() forwarderRules {
	return forwarderRules{set: s.d.Settings.Get(), search: s.host.Load().search, rev: s.lists.Load().revZones}
}

// normalizeForwarderDomain returns the stored form of a forwarder domain:
// lower-case without a trailing dot; Unqualified as it is.
func normalizeForwarderDomain(d string) string {
	d = strings.TrimSpace(d)
	if strings.EqualFold(d, Unqualified) {
		return Unqualified
	}
	return normalizeName(d)
}

// validateForwarder normalises and validates a forwarder input: 1–16
// domains (domains wins; domain alone means [domain]; with both, domains[0]
// must be domain), each a valid domain, "*.domain" or Unqualified, listed
// once; 1–8 targets, or the single target DefaultTarget (not for private
// reverse zones, the local domain, home.arpa, search domains, special-use
// zones or Unqualified); plain targets given by name must be public names
// and every name needs bootstrap servers.
func (rules forwarderRules) validateForwarder(in ForwarderInput) (ForwarderInput, error) {
	domainsGiven := len(in.Domains) > 0
	field := func(i int) string {
		if !domainsGiven {
			return "domain"
		}
		return fmt.Sprintf("domains[%d]", i)
	}
	domains := in.Domains
	switch {
	case !domainsGiven && strings.TrimSpace(in.Domain) == "":
		return in, apperr.Invalid("domain", "required")
	case !domainsGiven:
		domains = []string{in.Domain}
	case strings.TrimSpace(in.Domain) != "" && normalizeForwarderDomain(in.Domain) != normalizeForwarderDomain(in.Domains[0]):
		return in, apperr.Invalid("domain", "must be the first of domains (or be left out)")
	}
	if len(domains) > maxForwarderDomains {
		return in, apperr.Invalid("domains", "between 1 and %d domains are allowed", maxForwarderDomains)
	}
	norm := make([]string, 0, len(domains))
	for i, d := range domains {
		d = normalizeForwarderDomain(d)
		base, _ := strings.CutPrefix(d, "*.")
		switch {
		case d == Unqualified:
		case !validDomain(base):
			return in, apperr.Invalid(field(i), "must be a valid domain name (e.g. fritz.box, *.corp.example, 178.168.192.in-addr.arpa or %s)", Unqualified)
		case reservedRecordName(d):
			return in, apperr.Invalid(field(i), "%s is answered by PiCache itself", d)
		}
		if slices.Contains(norm, d) {
			return in, apperr.Invalid(field(i), "%s is listed twice", d)
		}
		norm = append(norm, d)
	}
	in.Domain, in.Domains = norm[0], norm
	if len(in.Upstreams) == 0 || len(in.Upstreams) > maxForwarderTargets {
		return in, apperr.Invalid("upstreams", "between 1 and %d upstreams are required", maxForwarderTargets)
	}
	ups := make([]string, 0, len(in.Upstreams))
	for i, u := range in.Upstreams {
		u = strings.TrimSpace(u)
		if strings.EqualFold(u, DefaultTarget) {
			if len(in.Upstreams) != 1 {
				return in, apperr.Invalid(fmt.Sprintf("upstreams[%d]", i), "default must be the only target")
			}
			if why := rules.defaultRefused(norm); why != "" {
				return in, apperr.Invalid(fmt.Sprintf("upstreams[%d]", i), "default (the default upstreams) cannot be used for %s", why)
			}
			ups = append(ups, DefaultTarget)
			continue
		}
		spec, err := settings.ParseUpstream(u)
		if err != nil {
			return in, apperr.Invalid(fmt.Sprintf("upstreams[%d]", i), "%v", err)
		}
		if !spec.IsIPLit {
			if (spec.Proto == "udp" || spec.Proto == "tcp") && !settings.PublicUpstreamName(spec.Host, rules.set.DNS.LocalDomain, rules.search...) {
				return in, apperr.Invalid(fmt.Sprintf("upstreams[%d]", i), "%s", settings.ErrPlainUpstreamName)
			}
			if len(rules.set.DNS.Bootstrap) == 0 {
				return in, apperr.Invalid(fmt.Sprintf("upstreams[%d]", i), "a host name needs dns.bootstrap servers")
			}
		}
		ups = append(ups, u)
	}
	in.Upstreams = ups
	var err error
	if in.Comment, err = cleanComment(in.Comment); err != nil {
		return in, err
	}
	return in, nil
}

// defaultRefused names the rule a domain breaks for the target
// DefaultTarget ("" if none): names that never reach the default
// upstreams (step 6) and single-label names.
func (rules forwarderRules) defaultRefused(domains []string) string {
	for _, d := range domains {
		if d == Unqualified {
			return "single-label names (" + Unqualified + ")"
		}
		base := strings.TrimPrefix(d, "*.")
		if _, ok := privateReverseZone(base); ok {
			return "private reverse zones (" + d + ")"
		}
		for n := base; n != ""; n = parent(n) {
			if rules.rev[n] {
				return "private reverse zones of dns.privateReverseNetworks (" + d + ")"
			}
		}
		if ld := rules.set.DNS.LocalDomain; ld != "" && inZone(base, ld) {
			return "the local domain (" + d + ")"
		}
		if inZone(base, "home.arpa") {
			return "home.arpa (" + d + ")"
		}
		for _, sd := range rules.search {
			if inZone(base, sd) {
				return "search domains (" + d + ")"
			}
		}
		for _, z := range append(slices.Clone(specialZones), "localhost", "resolver.arpa") {
			if inZone(base, z) {
				return "special-use names (" + d + ")"
			}
		}
	}
	return ""
}

// domainsTaken returns a conflict when one of domains belongs to a
// forwarder other than id.
func domainsTaken(ctx context.Context, tx *sql.Tx, domains []string, id int64) error {
	for _, d := range domains {
		var other int64
		err := tx.QueryRowContext(ctx, `SELECT forwarder_id FROM dns_forwarder_domains WHERE domain = ? AND forwarder_id != ?`, d, id).Scan(&other)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			return apperr.Conflict("a forwarder for %s already exists", d)
		}
		// dns_forwarders.domain holds the first domain of every forwarder
		// (a row whose domains are missing counts too).
		err = tx.QueryRowContext(ctx, `SELECT id FROM dns_forwarders WHERE domain = ? AND id != ?`, d, id).Scan(&other)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			return apperr.Conflict("a forwarder for %s already exists", d)
		}
	}
	return nil
}

// writeDomains replaces the domains of forwarder id.
func writeDomains(ctx context.Context, tx *sql.Tx, id int64, domains []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM dns_forwarder_domains WHERE forwarder_id = ?`, id); err != nil {
		return err
	}
	for i, d := range domains {
		if _, err := tx.ExecContext(ctx, `INSERT INTO dns_forwarder_domains (forwarder_id, position, domain) VALUES (?, ?, ?)`,
			id, i, d); err != nil {
			return err
		}
	}
	return nil
}

// insertForwarder stores a validated forwarder and returns its id.
func insertForwarder(ctx context.Context, tx *sql.Tx, in ForwarderInput) (int64, error) {
	ups, err := json.Marshal(in.Upstreams)
	if err != nil {
		return 0, err
	}
	now := db.NowMs()
	res, err := tx.ExecContext(ctx, `INSERT INTO dns_forwarders (domain, upstreams, enabled, comment, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, in.Domain, string(ups), in.Enabled, in.Comment, now, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, writeDomains(ctx, tx, id, in.Domains)
}

// updateForwarder replaces a forwarder with a validated input.
func updateForwarder(ctx context.Context, tx *sql.Tx, id int64, in ForwarderInput) error {
	ups, err := json.Marshal(in.Upstreams)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE dns_forwarders SET domain = ?, upstreams = ?, enabled = ?, comment = ?, updated_at = ?
		WHERE id = ?`, in.Domain, string(ups), in.Enabled, in.Comment, db.NowMs(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.NotFound("forwarder", id)
	}
	return writeDomains(ctx, tx, id, in.Domains)
}

// CreateForwarder adds a forwarder.
func (s *Server) CreateForwarder(ctx context.Context, in ForwarderInput) (Forwarder, error) {
	in, err := s.forwarderRules().validateForwarder(in)
	if err != nil {
		return Forwarder{}, err
	}
	var id int64
	err = s.writeConfig(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dns_forwarders`).Scan(&n); err != nil {
			return err
		}
		if n >= maxForwarders {
			return apperr.Conflict("at most %d forwarders are allowed", maxForwarders)
		}
		if err := domainsTaken(ctx, tx, in.Domains, 0); err != nil {
			return err
		}
		id, err = insertForwarder(ctx, tx, in)
		return err
	})
	if err != nil {
		return Forwarder{}, err
	}
	return s.forwarder(ctx, id)
}

// UpdateForwarder updates a forwarder.
func (s *Server) UpdateForwarder(ctx context.Context, id int64, in ForwarderInput) (Forwarder, error) {
	in, err := s.forwarderRules().validateForwarder(in)
	if err != nil {
		return Forwarder{}, err
	}
	err = s.writeConfig(ctx, func(tx *sql.Tx) error {
		if err := domainsTaken(ctx, tx, in.Domains, id); err != nil {
			return err
		}
		return updateForwarder(ctx, tx, id, in)
	})
	if err != nil {
		return Forwarder{}, err
	}
	return s.forwarder(ctx, id)
}

// DeleteForwarder deletes a forwarder (its domains go with it: ON DELETE
// CASCADE).
func (s *Server) DeleteForwarder(ctx context.Context, id int64) error {
	return s.writeConfig(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM dns_forwarders WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return apperr.NotFound("forwarder", id)
		}
		return nil
	})
}
