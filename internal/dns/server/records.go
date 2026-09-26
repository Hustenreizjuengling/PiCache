package dnsserver

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Record limits.
const (
	maxRecords        = 10000
	maxTXTLen         = 1024
	maxCommentLen     = 512
	defaultRecordTTL  = 300
	maxRecordTTL      = 86400
	maxCNAMEHops      = 8
	maxRecordGroups   = 64
	maxLoopCheckNames = 64
)

// Record scopes and other-family modes (Record.Scope, Record.OtherFamily).
const (
	ScopeAll      = "all"
	ScopeGroups   = "groups"
	FamilyNoData  = "nodata"
	FamilyForward = "forward"
)

var migrations = []string{
	`CREATE TABLE dns_records (
		id         INTEGER PRIMARY KEY,
		name       TEXT    NOT NULL,
		type       TEXT    NOT NULL,
		value      TEXT    NOT NULL,
		ttl        INTEGER NOT NULL,
		enabled    INTEGER NOT NULL DEFAULT 1,
		comment    TEXT    NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		UNIQUE (name, type, value)
	);
	CREATE TABLE dns_forwarders (
		id         INTEGER PRIMARY KEY,
		domain     TEXT    NOT NULL UNIQUE,
		upstreams  TEXT    NOT NULL,
		enabled    INTEGER NOT NULL DEFAULT 1,
		comment    TEXT    NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);`,
	// v2 (0.9.0): every domain of a forwarder in its order (position 0 is
	// dns_forwarders.domain), so one forwarder serves several domains; a
	// domain belongs to at most one forwarder. Back-filled from
	// dns_forwarders, so a restored older backup is converted at the start
	// that applies it; dns_forwarders and its indexes stay unchanged.
	`CREATE TABLE dns_forwarder_domains (
		forwarder_id INTEGER NOT NULL REFERENCES dns_forwarders(id) ON DELETE CASCADE,
		position     INTEGER NOT NULL,
		domain       TEXT    NOT NULL UNIQUE,
		PRIMARY KEY (forwarder_id, position)
	);
	INSERT INTO dns_forwarder_domains (forwarder_id, position, domain) SELECT id, 0, domain FROM dns_forwarders;`,
	// v3 (0.13.0): records per group (split horizon) and the other address
	// family of A/AAAA records. Every existing record keeps serving
	// everyone (scope 'all'); a record of scope 'groups' whose groups were
	// all deleted (the links cascade) serves nobody. Columns, a table and an
	// index only: dns_records keeps its rows and its UNIQUE key.
	`ALTER TABLE dns_records ADD COLUMN scope        TEXT NOT NULL DEFAULT 'all';    -- all | groups
	ALTER TABLE dns_records ADD COLUMN other_family TEXT NOT NULL DEFAULT 'nodata'; -- nodata | forward
	CREATE TABLE dns_record_groups (
		record_id INTEGER NOT NULL REFERENCES dns_records(id) ON DELETE CASCADE,
		group_id  INTEGER NOT NULL REFERENCES client_groups(id) ON DELETE CASCADE,
		PRIMARY KEY (record_id, group_id)
	) WITHOUT ROWID;
	CREATE INDEX dns_record_groups_group ON dns_record_groups(group_id);`,
}

// localRR is one enabled record of the in-memory zone.
type localRR struct {
	typ   uint16
	value string // A/AAAA: address; CNAME/PTR: target name (normalised); TXT: text; SRV, MX, HTTPS, SVCB: presentation form
	ip    netip.Addr
	ttl   uint32
	lease bool // the name of a DHCP lease, not a configured record
	// forward: an A or AAAA record whose other family is asked upstream
	// (Record.OtherFamily "forward").
	forward bool
	auto    bool // the automatic PTR record of an A or AAAA record
}

// zoneSet holds the records of one name (or wildcard base): those of scope
// all and those of each group (sorted by group ID; a record of several
// groups is in each).
type zoneSet struct {
	all    []localRR
	groups []groupRRs
}

type groupRRs struct {
	group int64
	rrs   []localRR
}

// pick returns the records of the lowest-id group of the client that has
// records here, else the records of scope all; scopes are never merged.
func (e *zoneSet) pick(groups []int64) ([]localRR, bool) {
	for i := range e.groups {
		if slices.Contains(groups, e.groups[i].group) {
			return e.groups[i].rrs, true
		}
	}
	return e.all, len(e.all) > 0
}

// add appends rr to the scope of a record (all, or each of its groups).
func (e *zoneSet) add(rr localRR, all bool, groups []int64) {
	if all {
		e.all = append(e.all, rr)
		return
	}
	for _, g := range groups {
		i, found := slices.BinarySearchFunc(e.groups, g, func(x groupRRs, g int64) int { return cmp.Compare(x.group, g) })
		if !found {
			e.groups = slices.Insert(e.groups, i, groupRRs{group: g})
		}
		e.groups[i].rrs = append(e.groups[i].rrs, rr)
	}
}

// hasPTR reports whether the scope (all, or group g) has an explicit PTR
// record.
func (e *zoneSet) hasPTR(all bool, g int64) bool {
	isPTR := func(rr localRR) bool { return rr.typ == dns.TypePTR && !rr.auto }
	if all {
		return slices.ContainsFunc(e.all, isPTR)
	}
	for _, x := range e.groups {
		if x.group == g {
			return slices.ContainsFunc(x.rrs, isPTR)
		}
	}
	return false
}

// zone is the immutable snapshot of enabled local records. Wildcards are
// keyed by their base ("*.example.lan" → "example.lan") and match
// subdomains only. Automatic PTR records are part of exact, in the scope of
// their record.
type zone struct {
	exact map[string]*zoneSet
	wild  map[string]*zoneSet
}

// lookup returns the records a client with groups finds for name
// (ARCHITECTURE 7.1 step 7): the exact name (its lowest-id group of the
// client that has records, then scope all), then the wildcards from the
// most specific base upwards (at each base the same order). The records of
// the first hit are the answer set.
func (z *zone) lookup(name string, groups []int64) ([]localRR, bool) {
	if e, ok := z.exact[name]; ok {
		if rrs, ok := e.pick(groups); ok {
			return rrs, true
		}
	}
	if len(z.wild) == 0 {
		return nil, false
	}
	for p := parent(name); p != ""; p = parent(p) {
		if e, ok := z.wild[p]; ok {
			if rrs, ok := e.pick(groups); ok {
				return rrs, true
			}
		}
	}
	return nil, false
}

func newZone(records []Record) *zone {
	z := &zone{exact: map[string]*zoneSet{}, wild: map[string]*zoneSet{}}
	set := func(m map[string]*zoneSet, key string) *zoneSet {
		e, ok := m[key]
		if !ok {
			e = &zoneSet{}
			m[key] = e
		}
		return e
	}
	type autoPTR struct {
		rev    string
		rr     localRR
		all    bool
		groups []int64
	}
	var auto []autoPTR
	for _, r := range records {
		if !r.Enabled || (r.Scope == ScopeGroups && len(r.GroupIDs) == 0) {
			continue // disabled, or scoped to no group: applies to nobody
		}
		all := r.Scope != ScopeGroups
		rr := localRR{typ: dns.StringToType[r.Type], value: r.Value, ttl: r.TTL}
		if rr.typ == dns.TypeA || rr.typ == dns.TypeAAAA {
			rr.ip, _ = netip.ParseAddr(r.Value)
			rr.forward = r.OtherFamily == FamilyForward
		}
		if base, ok := strings.CutPrefix(r.Name, "*."); ok {
			set(z.wild, base).add(rr, all, r.GroupIDs)
			continue
		}
		set(z.exact, r.Name).add(rr, all, r.GroupIDs)
		if rr.ip.IsValid() {
			auto = append(auto, autoPTR{rev: reverseName(rr.ip), rr: localRR{typ: dns.TypePTR, value: r.Name, ttl: r.TTL, auto: true},
				all: all, groups: r.GroupIDs})
		}
	}
	// An explicit PTR record beats the automatic one of the same reverse
	// name in the same scope.
	for _, a := range auto {
		e := set(z.exact, a.rev)
		if a.all {
			if !e.hasPTR(true, 0) {
				e.add(a.rr, true, nil)
			}
			continue
		}
		for _, g := range a.groups {
			if !e.hasPTR(false, g) {
				e.add(a.rr, false, []int64{g})
			}
		}
	}
	return z
}

// --- CRUD ---

const recordColumns = `id, name, type, value, ttl, enabled, comment, created_at, updated_at, scope, other_family`

// Records lists local records.
func (s *Server) Records(ctx context.Context) ([]Record, error) {
	return queryRecords(ctx, s.d.DB.R, 0)
}

// queryRecords reads the records (or record id) with their groups (q: the
// read pool or a transaction).
func queryRecords(ctx context.Context, q queryer, id int64) ([]Record, error) {
	where, args := "", []any{}
	if id != 0 {
		where, args = " WHERE id = ?", []any{id}
	}
	rows, err := q.QueryContext(ctx, `SELECT `+recordColumns+` FROM dns_records`+where+` ORDER BY name, type, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("dns: list records: %w", err)
	}
	defer rows.Close()
	out := []Record{}
	index := map[int64]int{}
	for rows.Next() {
		var r Record
		var created, updated int64
		if err := rows.Scan(&r.ID, &r.Name, &r.Type, &r.Value, &r.TTL, &r.Enabled, &r.Comment, &created, &updated,
			&r.Scope, &r.OtherFamily); err != nil {
			return nil, fmt.Errorf("dns: scan record: %w", err)
		}
		r.CreatedAt, r.UpdatedAt = db.Time(created), db.Time(updated)
		r.Data, r.GroupIDs = recordData(r.Type, r.Value), []int64{}
		index[r.ID] = len(out)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	where, args = "", []any{}
	if id != 0 {
		where, args = " WHERE record_id = ?", []any{id}
	}
	grows, err := q.QueryContext(ctx, `SELECT record_id, group_id FROM dns_record_groups`+where+` ORDER BY record_id, group_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("dns: list record groups: %w", err)
	}
	defer grows.Close()
	for grows.Next() {
		var rid, gid int64
		if err := grows.Scan(&rid, &gid); err != nil {
			return nil, fmt.Errorf("dns: scan record group: %w", err)
		}
		if i, ok := index[rid]; ok {
			out[i].GroupIDs = append(out[i].GroupIDs, gid)
		}
	}
	return out, grows.Err()
}

func (s *Server) record(ctx context.Context, id int64) (Record, error) {
	rs, err := queryRecords(ctx, s.d.DB.R, id)
	if err != nil {
		return Record{}, err
	}
	if len(rs) == 0 {
		return Record{}, apperr.NotFound("record", id)
	}
	return rs[0], nil
}

// recordSpec is a validated record: the input merged with the stored
// record (on update).
type recordSpec struct {
	Name, Type, Value string
	TTL               uint32
	Enabled           bool
	Comment           string
	Scope             string
	GroupIDs          []int64
	OtherFamily       string
}

// validateRecord normalises and validates a record input merged with the
// stored record old (nil on create). Scope, GroupIDs and OtherFamily left
// out keep their stored values (the defaults on create: all, none,
// nodata); a stored value that does not fit the merged record (groups of a
// record whose scope became all, forward on a record that is no longer A
// or AAAA) is reset when the input does not name it and refused when it
// does.
func validateRecord(in RecordInput, old *Record) (recordSpec, error) {
	sp := recordSpec{Name: normalizeName(in.Name), TTL: in.TTL, Enabled: in.Enabled,
		Scope: ScopeAll, GroupIDs: []int64{}, OtherFamily: FamilyNoData}
	if old != nil {
		sp.Scope, sp.GroupIDs, sp.OtherFamily = old.Scope, slices.Clone(old.GroupIDs), old.OtherFamily
	}
	base, wildcard := strings.CutPrefix(sp.Name, "*.")
	if !validDomain(base) || strings.Contains(base, "*") {
		return sp, apperr.Invalid("name", "must be a valid domain name (A-labels), optionally starting with \"*.\" for all subdomains")
	}
	if reservedRecordName(sp.Name) {
		return sp, apperr.Invalid("name", "%s is answered by PiCache itself and cannot have records", sp.Name)
	}
	sp.Type = strings.ToUpper(strings.TrimSpace(in.Type))
	if dataPresent(in.Data) && !typedRecord(sp.Type) {
		return sp, apperr.Invalid("data", "only SRV, MX, PTR, HTTPS and SVCB records have data; give the value")
	}
	switch sp.Type {
	case "A":
		ip, err := netip.ParseAddr(strings.TrimSpace(in.Value))
		if err != nil || !ip.Unmap().Is4() {
			return sp, apperr.Invalid("value", "must be an IPv4 address")
		}
		sp.Value = ip.Unmap().String()
	case "AAAA":
		ip, err := netip.ParseAddr(strings.TrimSpace(in.Value))
		if err != nil || !ip.Is6() || ip.Is4In6() || ip.Zone() != "" {
			return sp, apperr.Invalid("value", "must be an IPv6 address")
		}
		sp.Value = ip.String()
	case "CNAME":
		sp.Value = normalizeName(in.Value)
		if !validDomain(sp.Value) {
			return sp, apperr.Invalid("value", "must be a valid domain name")
		}
		if sp.Value == sp.Name || (wildcard && inZone(sp.Value, base) && sp.Value != base) {
			return sp, apperr.Invalid("value", "a CNAME must not point to itself")
		}
	case "TXT":
		sp.Value = strings.TrimSpace(in.Value)
		if sp.Value == "" || len(sp.Value) > maxTXTLen || !utf8.ValidString(sp.Value) || strings.ContainsFunc(sp.Value, unicode.IsControl) {
			return sp, apperr.Invalid("value", "must be 1–%d bytes of text without control characters", maxTXTLen)
		}
	case "PTR":
		if wildcard {
			return sp, apperr.Invalid("name", "a PTR record needs a complete in-addr.arpa (4 labels) or ip6.arpa (32 nibbles) name")
		}
		fallthrough
	case "SRV", "MX", "HTTPS", "SVCB":
		v, err := normalizeTyped(sp.Type, sp.Name, in.Value, in.Data)
		if err != nil {
			return sp, err
		}
		sp.Value = v
	default:
		return sp, apperr.Invalid("type", "must be A, AAAA, CNAME, TXT, SRV, MX, PTR, HTTPS or SVCB")
	}
	if sp.TTL == 0 {
		sp.TTL = defaultRecordTTL
	}
	if sp.TTL > maxRecordTTL {
		return sp, apperr.Invalid("ttl", "must be at most %d seconds", maxRecordTTL)
	}
	var err error
	if sp.Comment, err = cleanComment(in.Comment); err != nil {
		return sp, err
	}
	if in.Scope != nil {
		sp.Scope = strings.ToLower(strings.TrimSpace(*in.Scope))
		if sp.Scope != ScopeAll && sp.Scope != ScopeGroups {
			return sp, apperr.Invalid("scope", "must be all or groups")
		}
	}
	if in.GroupIDs != nil {
		if len(in.GroupIDs) > maxRecordGroups {
			return sp, apperr.Invalid("groupIds", "at most %d groups", maxRecordGroups)
		}
		groups := slices.Clone(in.GroupIDs)
		for _, g := range groups {
			if g <= 0 {
				return sp, apperr.Invalid("groupIds", "invalid group id %d", g)
			}
		}
		slices.Sort(groups)
		sp.GroupIDs = slices.Compact(groups)
	}
	if sp.Scope == ScopeAll && len(sp.GroupIDs) > 0 {
		if in.GroupIDs != nil {
			return sp, apperr.Invalid("groupIds", "a record of scope all has no groups")
		}
		sp.GroupIDs = []int64{}
	}
	if in.OtherFamily != nil {
		sp.OtherFamily = strings.ToLower(strings.TrimSpace(*in.OtherFamily))
		if sp.OtherFamily != FamilyNoData && sp.OtherFamily != FamilyForward {
			return sp, apperr.Invalid("otherFamily", "must be nodata or forward")
		}
	}
	if sp.OtherFamily == FamilyForward && sp.Type != "A" && sp.Type != "AAAA" {
		if in.OtherFamily != nil {
			return sp, apperr.Invalid("otherFamily", "forward is only for A and AAAA records")
		}
		sp.OtherFamily = FamilyNoData
	}
	return sp, nil
}

func cleanComment(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxCommentLen || strings.ContainsFunc(s, unicode.IsControl) {
		return "", apperr.Invalid("comment", "must be at most %d characters without control characters", maxCommentLen)
	}
	return s, nil
}

// scopesOverlap reports whether two records can reach the same client:
// both of scope all, or both of scope groups with a common group (a scoped
// record without groups reaches nobody; scope all and a group scope are
// different answer sets, never merged).
func scopesOverlap(aScope string, aGroups []int64, bScope string, bGroups []int64) bool {
	if aScope != ScopeGroups || bScope != ScopeGroups {
		return aScope != ScopeGroups && bScope != ScopeGroups
	}
	for _, g := range aGroups {
		if slices.Contains(bGroups, g) {
			return true
		}
	}
	return false
}

// checkGroupIDs refuses group IDs that do not exist (field groupIds).
func checkGroupIDs(ctx context.Context, tx *sql.Tx, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM client_groups WHERE id IN (?`+strings.Repeat(",?", len(ids)-1)+`)`,
		args...).Scan(&n); err != nil {
		return fmt.Errorf("dns: check groups: %w", err)
	}
	if n != len(ids) {
		return apperr.Invalid("groupIds", "unknown group")
	}
	return nil
}

// checkRecordConflicts enforces uniqueness (name, type and value exist
// once, whatever the scope), CNAME exclusivity within each scope and the
// absence of CNAME loops across every scope for record sp (id 0 = new
// record).
func checkRecordConflicts(ctx context.Context, tx *sql.Tx, id int64, sp recordSpec) error {
	var other int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM dns_records WHERE name = ? AND type = ? AND value = ? AND id != ?`,
		sp.Name, sp.Type, sp.Value, id).Scan(&other)
	switch {
	case err == nil:
		return apperr.Conflict("this record already exists")
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	same, err := queryRecordsByName(ctx, tx, sp.Name, id)
	if err != nil {
		return err
	}
	for _, o := range same {
		if (sp.Type == "CNAME" || o.Type == "CNAME") && scopesOverlap(sp.Scope, sp.GroupIDs, o.Scope, o.GroupIDs) {
			return apperr.Conflict("a CNAME cannot share its name (%s) with other records of the same scope", sp.Name)
		}
	}
	if sp.Type != "CNAME" {
		return nil
	}
	// Follow the CNAME records of every scope from the new target: a way
	// back to the name is a loop some client could reach.
	level, seen := []string{sp.Value}, map[string]bool{sp.Value: true}
	for range maxCNAMEHops {
		var next []string
		for _, name := range level {
			rows, err := tx.QueryContext(ctx, `SELECT value FROM dns_records WHERE name = ? AND type = 'CNAME' AND id != ?`, name, id)
			if err != nil {
				return err
			}
			var targets []string
			for rows.Next() {
				var t string
				if err := rows.Scan(&t); err != nil {
					rows.Close()
					return err
				}
				targets = append(targets, t)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			for _, t := range targets {
				if t == sp.Name {
					return apperr.Conflict("this CNAME would create a loop (%s → %s → … → %s)", sp.Name, sp.Value, sp.Name)
				}
				if !seen[t] && len(seen) < maxLoopCheckNames {
					seen[t] = true
					next = append(next, t)
				}
			}
		}
		if len(next) == 0 {
			return nil
		}
		level = next
	}
	return apperr.Conflict("CNAME chains may have at most %d hops", maxCNAMEHops)
}

// queryRecordsByName returns the records of name (not id) with their
// scopes and groups.
func queryRecordsByName(ctx context.Context, tx *sql.Tx, name string, id int64) ([]Record, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM dns_records WHERE name = ? AND id != ?`, name, id)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, rid)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(ids))
	for _, rid := range ids {
		rs, err := queryRecords(ctx, tx, rid)
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	return out, nil
}

// writeRecordGroups replaces the groups of record id.
func writeRecordGroups(ctx context.Context, tx *sql.Tx, id int64, groups []int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM dns_record_groups WHERE record_id = ?`, id); err != nil {
		return err
	}
	for _, g := range groups {
		if _, err := tx.ExecContext(ctx, `INSERT INTO dns_record_groups (record_id, group_id) VALUES (?, ?)`, id, g); err != nil {
			return err
		}
	}
	return nil
}

// insertRecord stores a validated record and returns its id.
func insertRecord(ctx context.Context, tx *sql.Tx, sp recordSpec) (int64, error) {
	now := db.NowMs()
	res, err := tx.ExecContext(ctx, `INSERT INTO dns_records (name, type, value, ttl, enabled, comment, created_at, updated_at, scope, other_family)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, sp.Name, sp.Type, sp.Value, sp.TTL, sp.Enabled, sp.Comment, now, now, sp.Scope, sp.OtherFamily)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, writeRecordGroups(ctx, tx, id, sp.GroupIDs)
}

// CreateRecord adds a record.
func (s *Server) CreateRecord(ctx context.Context, in RecordInput) (Record, error) {
	sp, err := validateRecord(in, nil)
	if err != nil {
		return Record{}, err
	}
	var id int64
	err = s.writeConfig(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dns_records`).Scan(&n); err != nil {
			return err
		}
		if n >= maxRecords {
			return apperr.Conflict("at most %d local records are allowed", maxRecords)
		}
		if err := checkGroupIDs(ctx, tx, sp.GroupIDs); err != nil {
			return err
		}
		if err := checkRecordConflicts(ctx, tx, 0, sp); err != nil {
			return err
		}
		id, err = insertRecord(ctx, tx, sp)
		return err
	})
	if err != nil {
		return Record{}, err
	}
	return s.record(ctx, id)
}

// UpdateRecord updates a record (RecordInput: members left out keep their
// stored values).
func (s *Server) UpdateRecord(ctx context.Context, id int64, in RecordInput) (Record, error) {
	err := s.writeConfig(ctx, func(tx *sql.Tx) error {
		olds, err := queryRecords(ctx, tx, id)
		if err != nil {
			return err
		}
		if len(olds) == 0 {
			return apperr.NotFound("record", id)
		}
		sp, err := validateRecord(in, &olds[0])
		if err != nil {
			return err
		}
		if err := checkGroupIDs(ctx, tx, sp.GroupIDs); err != nil {
			return err
		}
		if err := checkRecordConflicts(ctx, tx, id, sp); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE dns_records SET name = ?, type = ?, value = ?, ttl = ?, enabled = ?, comment = ?, updated_at = ?,
				scope = ?, other_family = ?
			WHERE id = ?`, sp.Name, sp.Type, sp.Value, sp.TTL, sp.Enabled, sp.Comment, db.NowMs(), sp.Scope, sp.OtherFamily, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return apperr.NotFound("record", id)
		}
		return writeRecordGroups(ctx, tx, id, sp.GroupIDs)
	})
	if err != nil {
		return Record{}, err
	}
	return s.record(ctx, id)
}

// DeleteRecord deletes a record.
func (s *Server) DeleteRecord(ctx context.Context, id int64) error {
	return s.writeConfig(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM dns_records WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return apperr.NotFound("record", id)
		}
		return nil
	})
}

// writeConfig runs fn in a transaction and reloads records and forwarders.
// Writes are serialised so snapshots are never stored out of order.
func (s *Server) writeConfig(ctx context.Context, fn func(*sql.Tx) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.d.DB.Tx(ctx, fn); err != nil {
		return err
	}
	return s.reloadConfig(ctx)
}

// ReloadRecords rebuilds the record zone and the forwarder table from the
// database (app calls it after group changes: a deleted group's record
// links are gone, ON DELETE CASCADE).
func (s *Server) ReloadRecords(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.reloadConfig(ctx)
}

// reloadConfig rebuilds the record zone and forwarder table from the DB.
func (s *Server) reloadConfig(ctx context.Context) error {
	recs, err := queryRecords(ctx, s.d.DB.R, 0)
	if err != nil {
		return err
	}
	fwds, err := queryForwarders(ctx, s.d.DB.R, 0)
	if err != nil {
		return err
	}
	s.zone.Store(newZone(recs))
	s.fwd.Store(newFwdTable(fwds))
	s.reconfigureLimiter()
	return nil
}

// --- answering ---

// localAnswer answers qc from local records (ARCHITECTURE 7.1 step 7),
// else from the names of DHCP leases: a CNAME is answered with its target
// resolved (local records and lease names first, then forwarded, at most
// 8 hops); a name without records of the requested type gets an
// authoritative NODATA. ok is false when nothing matches (also when the
// records ask for the other address family upstream, otherFamily forward).
func (s *Server) localAnswer(qc *qctx) (result, bool) {
	z := s.zone.Load()
	rrs, ok := s.lookupLocal(qc, z, qc.qname)
	if !ok {
		return result{}, false
	}
	m := newReply(qc.req)
	m.Authoritative = true
	res := result{msg: m, status: StatusLocal}
	owner, name := qc.q.Name, qc.qname
	var visited map[string]bool
	for hops := 0; ; hops++ {
		cname := slices.IndexFunc(rrs, func(rr localRR) bool { return rr.typ == dns.TypeCNAME })
		if cname < 0 {
			if qc.tracing() && (len(rrs) == 0 || !rrs[0].lease) {
				qc.note(fmt.Sprintf("local records for %s", name))
			}
			chain := len(m.Answer)
			var answer []localRR
			for _, rr := range rrs {
				if rr.typ == qc.qtype || qc.qtype == dns.TypeANY {
					answer = append(answer, rr)
				}
			}
			for _, rr := range s.localize(qc, answer) {
				if out := localToRR(owner, rr); out != nil {
					m.Answer = append(m.Answer, out)
				}
			}
			if len(m.Answer) == chain {
				qc.note("no local record of the requested type: NODATA")
				m.Ns = []dns.RR{syntheticSOA(qc.q.Name, minTTL(rrs))}
			}
			return res, true
		}
		target := rrs[cname].value
		m.Answer = append(m.Answer, &dns.CNAME{Hdr: rrHeader(owner, dns.TypeCNAME, rrs[cname].ttl), Target: fqdn(target)})
		if qc.tracing() {
			qc.note(fmt.Sprintf("local CNAME %s → %s", name, target))
		}
		if qc.qtype == dns.TypeCNAME {
			return res, true
		}
		if visited == nil {
			visited = map[string]bool{qc.qname: true}
		}
		if hops+1 > maxCNAMEHops || visited[target] {
			qc.note("CNAME chain too long or looping: SERVFAIL")
			return s.servfail(qc, "local CNAME loop or chain longer than 8 hops"), true
		}
		visited[target] = true
		next, ok := s.lookupLocal(qc, z, target)
		if !ok {
			s.resolveCNAMETarget(qc, &res, target, hops+1)
			return res, true
		}
		owner, name, rrs = fqdn(target), target, next
	}
}

// lookupLocal returns the local records the client finds for name (scoped
// by its groups; none while dns.localRecordsEnabled is off), else the name
// of a DHCP lease: <host>.<domain> → A, or the PTR of a lease address (TTL
// min(300 s, remaining lease)). Local records win over lease names; records
// of other groups do not hide a lease name. Records that ask for the other
// address family upstream (otherFamily forward) count as no record for an
// A or AAAA query without a record of its type.
func (s *Server) lookupLocal(qc *qctx, z *zone, name string) ([]localRR, bool) {
	if qc.set.DNS.LocalRecordsEnabled {
		var groups []int64
		if qc.id != nil {
			groups = qc.id.GroupIDs
		}
		if rrs, ok := z.lookup(name, groups); ok {
			if !otherFamilyForward(qc.qtype, rrs) {
				return rrs, true
			}
			if qc.tracing() {
				qc.note(fmt.Sprintf("local records for %s: no %s record, continuing (other family: forward)", name, typeString(qc.qtype)))
			}
		}
	}
	if s.d.Leases == nil || qc.recordsOnly {
		return nil, false
	}
	if ip, ok := parseReverse(name); ok {
		host, ttl, ok := s.d.Leases.LeasePTR(ip)
		if !ok {
			return nil, false
		}
		if qc.tracing() {
			qc.note(fmt.Sprintf("DHCP lease name: %s is %s", ip, host))
		}
		return []localRR{{typ: dns.TypePTR, value: host, ttl: ttl, lease: true}}, true
	}
	ip, ttl, ok := s.d.Leases.LeaseAddr(name)
	if !ok {
		return nil, false
	}
	if qc.tracing() {
		qc.note(fmt.Sprintf("DHCP lease name: %s is %s", name, ip))
	}
	return []localRR{{typ: dns.TypeA, value: ip.String(), ip: ip, ttl: ttl, lease: true}}, true
}

// otherFamilyForward reports whether an A or AAAA query finds an answer set
// without a record of its type and without a CNAME in which a record of
// the other family asks for the query to go upstream (otherFamily
// forward).
func otherFamilyForward(qtype uint16, rrs []localRR) bool {
	var other uint16
	switch qtype {
	case dns.TypeA:
		other = dns.TypeAAAA
	case dns.TypeAAAA:
		other = dns.TypeA
	default:
		return false
	}
	forward := false
	for _, rr := range rrs {
		switch {
		case rr.typ == qtype || rr.typ == dns.TypeCNAME:
			return false
		case rr.typ == other && rr.forward:
			forward = true
		}
	}
	return forward
}

// localize orders the addresses of an A or AAAA answer of configured
// records with at least two addresses by the client's network
// (dns.localizeRecords): an address is local when a prefix of one of this
// machine's interfaces (link-local excluded) contains it and the client;
// an address of the other family when it lies in a prefix of an interface
// that has a prefix containing the client. first: local addresses first
// (each part in stored order); only: only the local ones. When no address
// is local the stored order stays and every address is answered.
func (s *Server) localize(qc *qctx, rrs []localRR) []localRR {
	mode := qc.set.DNS.LocalizeRecords
	if mode == settings.LocalizeOff || len(rrs) < 2 || (qc.qtype != dns.TypeA && qc.qtype != dns.TypeAAAA) ||
		slices.ContainsFunc(rrs, func(rr localRR) bool { return rr.lease || !rr.ip.IsValid() }) {
		return rrs
	}
	client := netutil.Canon(qc.client)
	h := s.host.Load()
	local := func(ip netip.Addr) bool {
		for _, ifc := range h.ifaces {
			clientHere, ipHere, both := false, false, false
			for _, p := range ifc.prefixes {
				if p.Addr().IsLinkLocalUnicast() {
					continue
				}
				m := p.Masked()
				c, a := m.Contains(client), m.Contains(ip)
				clientHere, ipHere, both = clientHere || c, ipHere || a, both || c && a
			}
			if both || (clientHere && ipHere && ip.Is4() != client.Is4()) {
				return true
			}
		}
		return false
	}
	var near, far []localRR
	for _, rr := range rrs {
		if local(netutil.Canon(rr.ip)) {
			near = append(near, rr)
		} else {
			far = append(far, rr)
		}
	}
	switch {
	case len(near) == 0:
		return rrs
	case mode == settings.LocalizeOnly:
		qc.note("local addresses only (dns.localizeRecords)")
		return near
	}
	if len(far) > 0 {
		qc.note("local addresses first (dns.localizeRecords)")
	}
	return append(near, far...)
}

// resolveCNAMETarget appends the answer for a local CNAME target that has no
// local records: special-use names answered by PiCache itself (localhost,
// this server's names, resolver.arpa), else a conditional forwarder, the
// router resolver or the upstreams.
func (s *Server) resolveCNAMETarget(qc *qctx, res *result, target string, hops int) {
	if v4, v6, what, ok := s.specialAddrs(qc, target); ok {
		qc.note("the CNAME target is answered locally (" + what + ")")
		var ips []netip.Addr
		switch qc.qtype {
		case dns.TypeA:
			ips = v4
		case dns.TypeAAAA:
			ips = v6
		}
		rrs := addrRRs(fqdn(target), ips, specialTTL)
		res.msg.Answer = append(res.msg.Answer, rrs...)
		if len(rrs) == 0 {
			res.msg.Ns = []dns.RR{syntheticSOA(qc.q.Name, specialTTL)}
		}
		return
	}
	q := dns.Question{Name: fqdn(target), Qtype: qc.qtype, Qclass: dns.ClassINET}
	resp, info, err := s.routeName(qc, target, q)
	switch {
	case err != nil:
		qc.note("resolving the CNAME target failed: SERVFAIL")
		res.msg.Rcode = dns.RcodeServerFailure
		res.status, res.reason = StatusError, "CNAME target: "+errText(err)
	case resp == nil:
		qc.note("the CNAME target is not resolvable locally: NXDOMAIN")
		res.msg.Rcode = dns.RcodeNameError
		res.msg.Ns = []dns.RR{syntheticSOA(qc.q.Name, specialTTL)}
	default:
		cnames := hops
		for _, rr := range resp.Answer {
			if rr.Header().Rrtype == dns.TypeCNAME {
				cnames++
			}
		}
		if cnames > maxCNAMEHops {
			*res = s.servfail(qc, "CNAME chain longer than 8 hops")
			return
		}
		res.msg.Answer = append(res.msg.Answer, resp.Answer...)
		res.msg.Rcode = resp.Rcode
		if len(resp.Answer) == 0 {
			res.msg.Ns = resp.Ns
		}
		res.upstream = info.Upstream
	}
}

// localToRR converts a local record into an RR owned by owner (nil for a
// stored value that does not parse).
func localToRR(owner string, rr localRR) dns.RR {
	h := rrHeader(owner, rr.typ, rr.ttl)
	switch rr.typ {
	case dns.TypeA:
		return &dns.A{Hdr: h, A: rr.ip.AsSlice()}
	case dns.TypeAAAA:
		return &dns.AAAA{Hdr: h, AAAA: rr.ip.AsSlice()}
	case dns.TypePTR:
		return &dns.PTR{Hdr: h, Ptr: fqdn(rr.value)}
	case dns.TypeTXT:
		return &dns.TXT{Hdr: h, Txt: splitTXT(rr.value)}
	case dns.TypeCNAME:
		return &dns.CNAME{Hdr: h, Target: fqdn(rr.value)}
	}
	return typedRR(owner, rr.typ, rr.value, rr.ttl)
}

// splitTXT splits text into character-strings of at most 255 bytes.
func splitTXT(s string) []string {
	var out []string
	for len(s) > 255 {
		cut := 255
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	return append(out, s)
}

func minTTL(rrs []localRR) uint32 {
	if len(rrs) == 0 {
		return specialTTL
	}
	return slices.MinFunc(rrs, func(a, b localRR) int { return cmp.Compare(a.ttl, b.ttl) }).ttl
}

// BatchRecords deletes, enables or disables the records ids in one
// transaction, all or nothing (an unknown id is apperr.NotFound), and
// reloads the zone once. It returns the number of records whose state
// changed (deleting counts every id).
func (s *Server) BatchRecords(ctx context.Context, action string, ids []int64) (int, error) {
	var changed int
	err := s.writeConfig(ctx, func(tx *sql.Tx) error {
		var err error
		changed, err = batchRows(ctx, tx, "dns_records", "record", action, ids)
		return err
	})
	return changed, err
}

// BatchForwarders deletes, enables or disables the forwarders ids in one
// transaction, all or nothing, and reloads the forwarder table once.
func (s *Server) BatchForwarders(ctx context.Context, action string, ids []int64) (int, error) {
	var changed int
	err := s.writeConfig(ctx, func(tx *sql.Tx) error {
		var err error
		changed, err = batchRows(ctx, tx, "dns_forwarders", "forwarder", action, ids)
		return err
	})
	return changed, err
}

// batchRows applies a batch action to the rows ids of table (a constant).
func batchRows(ctx context.Context, tx *sql.Tx, table, what, action string, ids []int64) (int, error) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	in := " IN (?" + strings.Repeat(",?", len(ids)-1) + ")"
	rows, err := tx.QueryContext(ctx, `SELECT id FROM `+table+` WHERE id`+in, args...)
	if err != nil {
		return 0, err
	}
	have := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		have[id] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if !have[id] {
			return 0, apperr.NotFound(what, id)
		}
	}
	switch action {
	case "delete":
		_, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE id`+in, args...)
		return len(ids), err
	case "enable", "disable":
		on := action == "enable"
		res, err := tx.ExecContext(ctx, `UPDATE `+table+` SET enabled = ?, updated_at = ? WHERE enabled != ? AND id`+in,
			append([]any{on, db.NowMs(), on}, args...)...)
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		return int(n), err
	}
	return 0, apperr.Invalid("action", "must be delete, enable or disable")
}

// Migrations returns the schema steps of component "dns" in picache.db
// (`picache db salvage` builds a fresh schema with them).
func Migrations() []string { return slices.Clone(migrations) }
