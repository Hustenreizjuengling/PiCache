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
)

// Record limits.
const (
	maxRecords       = 10000
	maxTXTLen        = 1024
	maxCommentLen    = 512
	defaultRecordTTL = 300
	maxRecordTTL     = 86400
	maxCNAMEHops     = 8
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
}

// localRR is one enabled record of the in-memory zone.
type localRR struct {
	typ   uint16
	value string // A/AAAA: address; CNAME/PTR: target name (normalised); TXT: text
	ip    netip.Addr
	ttl   uint32
	lease bool // the name of a DHCP lease, not a configured record
}

// zone is the immutable snapshot of enabled local records. Wildcards are
// keyed by their base ("*.example.lan" → "example.lan") and match
// subdomains only. Auto-PTR records are part of exact.
type zone struct {
	exact map[string][]localRR
	wild  map[string][]localRR
}

// lookup returns the records for name: exact names beat wildcards; among
// wildcards the most specific one wins.
func (z *zone) lookup(name string) ([]localRR, bool) {
	if rrs, ok := z.exact[name]; ok {
		return rrs, true
	}
	if len(z.wild) == 0 {
		return nil, false
	}
	for p := parent(name); p != ""; p = parent(p) {
		if rrs, ok := z.wild[p]; ok {
			return rrs, true
		}
	}
	return nil, false
}

func newZone(records []Record) *zone {
	z := &zone{exact: map[string][]localRR{}, wild: map[string][]localRR{}}
	for _, r := range records {
		if !r.Enabled {
			continue
		}
		rr := localRR{typ: dns.StringToType[r.Type], value: r.Value, ttl: r.TTL}
		if rr.typ == dns.TypeA || rr.typ == dns.TypeAAAA {
			rr.ip, _ = netip.ParseAddr(r.Value)
		}
		if base, ok := strings.CutPrefix(r.Name, "*."); ok {
			z.wild[base] = append(z.wild[base], rr)
			continue
		}
		z.exact[r.Name] = append(z.exact[r.Name], rr)
		if rr.ip.IsValid() {
			rev := reverseName(rr.ip)
			z.exact[rev] = append(z.exact[rev], localRR{typ: dns.TypePTR, value: r.Name, ttl: r.TTL})
		}
	}
	return z
}

// --- CRUD ---

// Records lists local records.
func (s *Server) Records(ctx context.Context) ([]Record, error) {
	return s.queryRecords(ctx, 0)
}

func (s *Server) queryRecords(ctx context.Context, id int64) ([]Record, error) {
	where, args := "", []any{}
	if id != 0 {
		where, args = " WHERE id = ?", []any{id}
	}
	rows, err := s.d.DB.R.QueryContext(ctx, `SELECT id, name, type, value, ttl, enabled, comment, created_at, updated_at
		FROM dns_records`+where+` ORDER BY name, type, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("dns: list records: %w", err)
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var r Record
		var created, updated int64
		if err := rows.Scan(&r.ID, &r.Name, &r.Type, &r.Value, &r.TTL, &r.Enabled, &r.Comment, &created, &updated); err != nil {
			return nil, fmt.Errorf("dns: scan record: %w", err)
		}
		r.CreatedAt, r.UpdatedAt = db.Time(created), db.Time(updated)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Server) record(ctx context.Context, id int64) (Record, error) {
	rs, err := s.queryRecords(ctx, id)
	if err != nil {
		return Record{}, err
	}
	if len(rs) == 0 {
		return Record{}, apperr.NotFound("record", id)
	}
	return rs[0], nil
}

// validateRecord normalises and validates a record input.
func validateRecord(in RecordInput) (RecordInput, error) {
	in.Name = normalizeName(in.Name)
	base, wildcard := strings.CutPrefix(in.Name, "*.")
	if !validDomain(base) || strings.Contains(base, "*") {
		return in, apperr.Invalid("name", "must be a valid domain name (A-labels), optionally starting with \"*.\" for all subdomains")
	}
	if reservedRecordName(in.Name) {
		return in, apperr.Invalid("name", "%s is answered by PiCache itself and cannot have records", in.Name)
	}
	in.Type = strings.ToUpper(strings.TrimSpace(in.Type))
	switch in.Type {
	case "A":
		ip, err := netip.ParseAddr(strings.TrimSpace(in.Value))
		if err != nil || !ip.Unmap().Is4() {
			return in, apperr.Invalid("value", "must be an IPv4 address")
		}
		in.Value = ip.Unmap().String()
	case "AAAA":
		ip, err := netip.ParseAddr(strings.TrimSpace(in.Value))
		if err != nil || !ip.Is6() || ip.Is4In6() || ip.Zone() != "" {
			return in, apperr.Invalid("value", "must be an IPv6 address")
		}
		in.Value = ip.String()
	case "CNAME":
		in.Value = normalizeName(in.Value)
		if !validDomain(in.Value) {
			return in, apperr.Invalid("value", "must be a valid domain name")
		}
		if in.Value == in.Name || (wildcard && inZone(in.Value, base) && in.Value != base) {
			return in, apperr.Invalid("value", "a CNAME must not point to itself")
		}
	case "TXT":
		in.Value = strings.TrimSpace(in.Value)
		if in.Value == "" || len(in.Value) > maxTXTLen || !utf8.ValidString(in.Value) || strings.ContainsFunc(in.Value, unicode.IsControl) {
			return in, apperr.Invalid("value", "must be 1–%d bytes of text without control characters", maxTXTLen)
		}
	default:
		return in, apperr.Invalid("type", "must be A, AAAA, CNAME or TXT")
	}
	if in.TTL == 0 {
		in.TTL = defaultRecordTTL
	}
	if in.TTL > maxRecordTTL {
		return in, apperr.Invalid("ttl", "must be at most %d seconds", maxRecordTTL)
	}
	var err error
	if in.Comment, err = cleanComment(in.Comment); err != nil {
		return in, err
	}
	return in, nil
}

func cleanComment(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxCommentLen || strings.ContainsFunc(s, unicode.IsControl) {
		return "", apperr.Invalid("comment", "must be at most %d characters without control characters", maxCommentLen)
	}
	return s, nil
}

// checkRecordConflicts enforces uniqueness, CNAME exclusivity and the
// absence of CNAME loops for record in (id 0 = new record).
func checkRecordConflicts(ctx context.Context, tx *sql.Tx, id int64, in RecordInput) error {
	var other int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM dns_records WHERE name = ? AND type = ? AND value = ? AND id != ?`,
		in.Name, in.Type, in.Value, id).Scan(&other)
	switch {
	case err == nil:
		return apperr.Conflict("this record already exists")
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	cond := `type = 'CNAME'`
	if in.Type == "CNAME" {
		cond = `1 = 1`
	}
	err = tx.QueryRowContext(ctx, `SELECT id FROM dns_records WHERE name = ? AND id != ? AND `+cond+` LIMIT 1`, in.Name, id).Scan(&other)
	switch {
	case err == nil:
		return apperr.Conflict("a CNAME cannot share its name (%s) with other records", in.Name)
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	if in.Type != "CNAME" {
		return nil
	}
	// Follow the chain through existing exact CNAME records.
	target := in.Value
	for range maxCNAMEHops {
		var next string
		err := tx.QueryRowContext(ctx, `SELECT value FROM dns_records WHERE name = ? AND type = 'CNAME' AND id != ? LIMIT 1`,
			target, id).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if next == in.Name {
			return apperr.Conflict("this CNAME would create a loop (%s → %s → … → %s)", in.Name, in.Value, in.Name)
		}
		target = next
	}
	return apperr.Conflict("CNAME chains may have at most %d hops", maxCNAMEHops)
}

// CreateRecord adds a record.
func (s *Server) CreateRecord(ctx context.Context, in RecordInput) (Record, error) {
	in, err := validateRecord(in)
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
		if err := checkRecordConflicts(ctx, tx, 0, in); err != nil {
			return err
		}
		now := db.NowMs()
		res, err := tx.ExecContext(ctx, `INSERT INTO dns_records (name, type, value, ttl, enabled, comment, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, in.Name, in.Type, in.Value, in.TTL, in.Enabled, in.Comment, now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return Record{}, err
	}
	return s.record(ctx, id)
}

// UpdateRecord updates a record.
func (s *Server) UpdateRecord(ctx context.Context, id int64, in RecordInput) (Record, error) {
	in, err := validateRecord(in)
	if err != nil {
		return Record{}, err
	}
	err = s.writeConfig(ctx, func(tx *sql.Tx) error {
		if err := checkRecordConflicts(ctx, tx, id, in); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE dns_records SET name = ?, type = ?, value = ?, ttl = ?, enabled = ?, comment = ?, updated_at = ?
			WHERE id = ?`, in.Name, in.Type, in.Value, in.TTL, in.Enabled, in.Comment, db.NowMs(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return apperr.NotFound("record", id)
		}
		return nil
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

// reloadConfig rebuilds the record zone and forwarder table from the DB.
func (s *Server) reloadConfig(ctx context.Context) error {
	recs, err := s.queryRecords(ctx, 0)
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
// authoritative NODATA. ok is false when nothing matches.
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
			for _, rr := range rrs {
				if rr.typ == qc.qtype || qc.qtype == dns.TypeANY {
					m.Answer = append(m.Answer, localToRR(owner, rr))
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

// lookupLocal returns the local records of name, else the name of a DHCP
// lease: <host>.<domain> → A, or the PTR of a lease address (TTL
// min(300 s, remaining lease)). Local records win over lease names.
func (s *Server) lookupLocal(qc *qctx, z *zone, name string) ([]localRR, bool) {
	if rrs, ok := z.lookup(name); ok {
		return rrs, true
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

// localToRR converts a local record into an RR owned by owner.
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
	}
	return &dns.CNAME{Hdr: h, Target: fqdn(rr.value)}
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

// Migrations returns the schema steps of component "dns" in picache.db
// (`picache db salvage` builds a fresh schema with them).
func Migrations() []string { return slices.Clone(migrations) }
