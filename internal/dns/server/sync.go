package dnsserver

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// The follower sync (docs/ARCHITECTURE.md 15.4): the local records and the
// conditional forwarders of a primary replace this follower's, IDs
// included, in the app's transaction; every record and forwarder is
// validated like a POST (syntax, scopes, CNAME exclusivity and loops, the
// forwarder domain rules against this follower's settings, the limits).

// SyncedLocalDNS are the validated records and forwarders of a sync.
type SyncedLocalDNS struct {
	records    []syncedRecord
	forwarders []syncedForwarder
}

type syncedRecord struct {
	id  int64
	sp  recordSpec
	rec Record
}

type syncedForwarder struct {
	id  int64
	in  ForwarderInput
	fwd Forwarder
}

// ValidateSync checks the records and forwarders of a primary (the group
// ids of the records are this follower's).
func (s *Server) ValidateSync(records []Record, forwarders []Forwarder) (*SyncedLocalDNS, error) {
	if len(records) > maxRecords {
		return nil, apperr.Conflict("at most %d local records are allowed", maxRecords)
	}
	if len(forwarders) > maxForwarders {
		return nil, apperr.Conflict("at most %d forwarders are allowed", maxForwarders)
	}
	out := &SyncedLocalDNS{}
	ids := map[int64]bool{}
	for _, r := range records {
		in := RecordInput{Name: r.Name, Type: r.Type, Value: r.Value, TTL: r.TTL, Enabled: r.Enabled, Comment: r.Comment,
			GroupIDs: nonNilIDs(r.GroupIDs)}
		// The value is the canonical presentation form (data is derived
		// from it), so the typed records are validated from the value.
		scope, family := r.Scope, r.OtherFamily
		in.Scope, in.OtherFamily = &scope, &family
		sp, err := validateRecord(in, nil)
		if err != nil {
			return nil, fmt.Errorf("record %s %s: %w", r.Name, r.Type, err)
		}
		if r.ID <= 0 || ids[r.ID] {
			return nil, apperr.Conflict("record %d is listed twice", r.ID)
		}
		ids[r.ID] = true
		out.records = append(out.records, syncedRecord{id: r.ID, sp: sp, rec: r})
	}
	rules := s.forwarderRules()
	fids := map[int64]bool{}
	for _, f := range forwarders {
		in, err := rules.validateForwarder(ForwarderInput{Domains: f.Domains, Upstreams: f.Upstreams, Enabled: f.Enabled, Comment: f.Comment})
		if err != nil {
			return nil, fmt.Errorf("forwarder %s: %w", f.Domain, err)
		}
		if f.ID <= 0 || fids[f.ID] {
			return nil, apperr.Conflict("forwarder %d is listed twice", f.ID)
		}
		fids[f.ID] = true
		out.forwarders = append(out.forwarders, syncedForwarder{id: f.ID, in: in, fwd: f})
	}
	return out, nil
}

// ReplaceSynced replaces the records and forwarders in tx (IDs of the
// primary): the conflicts, the CNAME loops and the domains taken are
// checked row by row against the rows written before.
func ReplaceSynced(ctx context.Context, tx *sql.Tx, s *SyncedLocalDNS) error {
	for _, q := range []string{`DELETE FROM dns_records`, `DELETE FROM dns_forwarders`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	now := db.NowMs()
	for _, r := range s.records {
		sp := r.sp
		if err := checkGroupIDs(ctx, tx, sp.GroupIDs); err != nil {
			return fmt.Errorf("record %s: %w", sp.Name, err)
		}
		if err := checkRecordConflicts(ctx, tx, r.id, sp); err != nil {
			return fmt.Errorf("record %s: %w", sp.Name, err)
		}
		created, updated := db.Ms(r.rec.CreatedAt), db.Ms(r.rec.UpdatedAt)
		if created == 0 {
			created, updated = now, now
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dns_records (id, name, type, value, ttl, enabled, comment, created_at, updated_at,
				scope, other_family) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.id, sp.Name, sp.Type, sp.Value, sp.TTL, sp.Enabled,
			sp.Comment, created, updated, sp.Scope, sp.OtherFamily); err != nil {
			return fmt.Errorf("record %s: %w", sp.Name, err)
		}
		if err := writeRecordGroups(ctx, tx, r.id, sp.GroupIDs); err != nil {
			return err
		}
	}
	for _, f := range s.forwarders {
		if err := domainsTaken(ctx, tx, f.in.Domains, f.id); err != nil {
			return err
		}
		ups, err := json.Marshal(f.in.Upstreams)
		if err != nil {
			return err
		}
		created, updated := db.Ms(f.fwd.CreatedAt), db.Ms(f.fwd.UpdatedAt)
		if created == 0 {
			created, updated = now, now
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dns_forwarders (id, domain, upstreams, enabled, comment, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, f.id, f.in.Domain, string(ups), f.in.Enabled, f.in.Comment, created, updated); err != nil {
			return fmt.Errorf("forwarder %s: %w", f.in.Domain, err)
		}
		if err := writeDomains(ctx, tx, f.id, f.in.Domains); err != nil {
			return err
		}
	}
	return nil
}

// nonNilIDs returns ids or an empty list.
func nonNilIDs(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}
