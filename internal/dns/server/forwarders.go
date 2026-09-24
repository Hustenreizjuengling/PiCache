package dnsserver

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Forwarder limits.
const (
	maxForwarders       = 256
	maxForwarderTargets = 8
)

// fwdEntry is one enabled forwarder of the snapshot.
type fwdEntry struct {
	domain    string // as configured ("*.x" for subdomains only)
	upstreams []string
	ips       []netip.Addr // IP-literal targets (loop guard, rate-limit exemption)
}

// fwdTable is the immutable snapshot of enabled forwarders.
type fwdTable struct {
	apex map[string]*fwdEntry // "x": x and its subdomains
	wild map[string]*fwdEntry // "*.x" keyed by "x": subdomains only
	ips  []netip.Addr         // all IP-literal targets
}

func newFwdTable(fwds []Forwarder) *fwdTable {
	t := &fwdTable{apex: map[string]*fwdEntry{}, wild: map[string]*fwdEntry{}}
	for _, f := range fwds {
		if !f.Enabled {
			continue
		}
		e := &fwdEntry{domain: f.Domain, upstreams: f.Upstreams, ips: upstreamIPs(f.Upstreams)}
		t.ips = append(t.ips, e.ips...)
		if base, ok := strings.CutPrefix(f.Domain, "*."); ok {
			t.wild[base] = e
		} else {
			t.apex[f.Domain] = e
		}
	}
	return t
}

// match returns the most specific forwarder for name (nil if none). For the
// same base domain, "*.x" wins over "x" for subdomains.
func (t *fwdTable) match(name string) *fwdEntry {
	if len(t.apex) == 0 && len(t.wild) == 0 {
		return nil
	}
	for n := name; n != ""; n = parent(n) {
		if n != name {
			if e, ok := t.wild[n]; ok {
				return e
			}
		}
		if e, ok := t.apex[n]; ok {
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

// Forwarders lists conditional forwarders.
func (s *Server) Forwarders(ctx context.Context) ([]Forwarder, error) {
	return s.queryForwarders(ctx, 0)
}

func (s *Server) queryForwarders(ctx context.Context, id int64) ([]Forwarder, error) {
	where, args := "", []any{}
	if id != 0 {
		where, args = " WHERE id = ?", []any{id}
	}
	rows, err := s.d.DB.R.QueryContext(ctx, `SELECT id, domain, upstreams, enabled, comment, created_at, updated_at
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
	return out, rows.Err()
}

func (s *Server) forwarder(ctx context.Context, id int64) (Forwarder, error) {
	fs, err := s.queryForwarders(ctx, id)
	if err != nil {
		return Forwarder{}, err
	}
	if len(fs) == 0 {
		return Forwarder{}, apperr.NotFound("forwarder", id)
	}
	return fs[0], nil
}

// validateForwarder normalises and validates a forwarder input.
func validateForwarder(in ForwarderInput) (ForwarderInput, error) {
	in.Domain = normalizeName(in.Domain)
	base, _ := strings.CutPrefix(in.Domain, "*.")
	if !validDomain(base) {
		return in, apperr.Invalid("domain", "must be a valid domain name (e.g. fritz.box, *.corp.example or 178.168.192.in-addr.arpa)")
	}
	if reservedRecordName(in.Domain) {
		return in, apperr.Invalid("domain", "%s is answered by PiCache itself", in.Domain)
	}
	if len(in.Upstreams) == 0 || len(in.Upstreams) > maxForwarderTargets {
		return in, apperr.Invalid("upstreams", "between 1 and %d upstreams are required", maxForwarderTargets)
	}
	ups := make([]string, 0, len(in.Upstreams))
	for i, u := range in.Upstreams {
		u = strings.TrimSpace(u)
		if _, err := settings.ParseUpstream(u); err != nil {
			return in, apperr.Invalid(fmt.Sprintf("upstreams[%d]", i), "%v", err)
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

func domainTaken(ctx context.Context, tx *sql.Tx, domain string, id int64) error {
	var other int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM dns_forwarders WHERE domain = ? AND id != ?`, domain, id).Scan(&other)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	return apperr.Conflict("a forwarder for %s already exists", domain)
}

// CreateForwarder adds a forwarder.
func (s *Server) CreateForwarder(ctx context.Context, in ForwarderInput) (Forwarder, error) {
	in, err := validateForwarder(in)
	if err != nil {
		return Forwarder{}, err
	}
	ups, err := json.Marshal(in.Upstreams)
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
		if err := domainTaken(ctx, tx, in.Domain, 0); err != nil {
			return err
		}
		now := db.NowMs()
		res, err := tx.ExecContext(ctx, `INSERT INTO dns_forwarders (domain, upstreams, enabled, comment, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, in.Domain, string(ups), in.Enabled, in.Comment, now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return Forwarder{}, err
	}
	return s.forwarder(ctx, id)
}

// UpdateForwarder updates a forwarder.
func (s *Server) UpdateForwarder(ctx context.Context, id int64, in ForwarderInput) (Forwarder, error) {
	in, err := validateForwarder(in)
	if err != nil {
		return Forwarder{}, err
	}
	ups, err := json.Marshal(in.Upstreams)
	if err != nil {
		return Forwarder{}, err
	}
	err = s.writeConfig(ctx, func(tx *sql.Tx) error {
		if err := domainTaken(ctx, tx, in.Domain, id); err != nil {
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
		return nil
	})
	if err != nil {
		return Forwarder{}, err
	}
	return s.forwarder(ctx, id)
}

// DeleteForwarder deletes a forwarder.
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
