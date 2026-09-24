package clients

import (
	"cmp"
	"context"
	"database/sql"
	"log/slog"
	"net/netip"
	"slices"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

const (
	maxSeenRows  = 100_000 // clients_seen rows kept (oldest pruned first)
	maxKnownRows = 10_000  // entries returned by Known
)

var seenMigrations = []string{
	`CREATE TABLE clients_seen (
		ip         TEXT    PRIMARY KEY,
		mac        TEXT    NOT NULL DEFAULT '',
		hostname   TEXT    NOT NULL DEFAULT '',
		first_seen INTEGER NOT NULL,
		last_seen  INTEGER NOT NULL,
		queries    INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX clients_seen_last ON clients_seen(last_seen);`,
}

// seenEntry is the in-memory activity of one address.
type seenEntry struct {
	first, last time.Time
	pending     int64 // queries not yet written to logs.db
	total       int64 // queries since start (used when logs.db is unavailable)
	transient   bool  // last recorded by SeenTransient: never written to logs.db
}

// Seen records activity of ip (in memory; flushed to logs.db periodically
// with the address, its MAC and hostname).
func (r *Registry) Seen(ip netip.Addr) { r.recordSeen(ip, true) }

// SeenTransient records activity of ip in memory only: Known lists it while
// the process runs, but nothing about it is written to logs.db, and
// activity of ip not yet written is discarded. The DNS server uses it
// while client addresses are anonymised (logs.anonymizeClientIps).
func (r *Registry) SeenTransient(ip netip.Addr) { r.recordSeen(ip, false) }

func (r *Registry) recordSeen(ip netip.Addr, persist bool) {
	ip = netutil.Canon(ip)
	if !ip.IsValid() {
		return
	}
	now := time.Now()
	r.seenMu.Lock()
	e, ok := r.seen.get(ip)
	if !ok {
		e = &seenEntry{first: now}
		r.seen.put(ip, e)
	}
	e.last = now
	e.total++
	e.transient = !persist
	if persist {
		e.pending++
	} else {
		e.pending = 0
	}
	r.seenMu.Unlock()
	if !ok {
		r.enqueueName(ip)
	}
}

// seenLoop flushes seen data every minute, refreshes hostnames hourly and
// prunes daily. A final flush runs when ctx ends.
func (r *Registry) seenLoop(ctx context.Context) {
	flush := time.NewTicker(seenFlushEvery)
	defer flush.Stop()
	names := time.NewTicker(nameRefreshEvery)
	defer names.Stop()
	prune := time.NewTicker(24 * time.Hour)
	defer prune.Stop()
	r.prune(ctx)
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			r.flush(fctx)
			cancel()
			return
		case <-flush.C:
			r.flush(ctx)
		case <-names.C:
			r.refreshNames()
		case <-prune.C:
			r.prune(ctx)
		}
	}
}

type seenRow struct {
	ip          netip.Addr
	first, last time.Time
	queries     int64
}

// flush writes pending activity to logs.db.
func (r *Registry) flush(ctx context.Context) {
	if r.ldb == nil {
		return
	}
	var rows []seenRow
	r.seenMu.Lock()
	r.seen.each(func(ip netip.Addr, e *seenEntry) bool {
		if e.pending > 0 {
			rows = append(rows, seenRow{ip: ip, first: e.first, last: e.last, queries: e.pending})
			e.pending = 0
		}
		return true
	})
	r.seenMu.Unlock()
	if len(rows) == 0 {
		return
	}
	arp := *r.arp.Load()
	err := r.ldb.Tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO clients_seen (ip, mac, hostname, first_seen, last_seen, queries)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(ip) DO UPDATE SET
				mac        = CASE WHEN excluded.mac != '' THEN excluded.mac ELSE clients_seen.mac END,
				hostname   = CASE WHEN excluded.hostname != '' THEN excluded.hostname ELSE clients_seen.hostname END,
				first_seen = MIN(clients_seen.first_seen, excluded.first_seen),
				last_seen  = MAX(clients_seen.last_seen, excluded.last_seen),
				queries    = clients_seen.queries + excluded.queries`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, row := range rows {
			if _, err := stmt.ExecContext(ctx, row.ip.String(), arp[row.ip], r.hostname(row.ip),
				db.Ms(row.first), db.Ms(row.last), row.queries); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		r.log.Warn("could not store seen clients", slog.Int("rows", len(rows)), slog.Any("err", err))
	}
}

// prune removes activity older than 30 days (memory and logs.db) and keeps
// logs.db below maxSeenRows.
func (r *Registry) prune(ctx context.Context) {
	cutoff := time.Now().Add(-seenRetention)
	r.seenMu.Lock()
	var old []netip.Addr
	r.seen.each(func(ip netip.Addr, e *seenEntry) bool {
		if e.last.Before(cutoff) && e.pending == 0 {
			old = append(old, ip)
		}
		return true
	})
	for _, ip := range old {
		r.seen.delete(ip)
	}
	r.seenMu.Unlock()
	if r.ldb == nil {
		return
	}
	if _, err := r.ldb.W.ExecContext(ctx, `DELETE FROM clients_seen WHERE last_seen < ?`, db.Ms(cutoff)); err != nil {
		r.log.Warn("could not prune seen clients", slog.Any("err", err))
		return
	}
	if _, err := r.ldb.W.ExecContext(ctx, `DELETE FROM clients_seen WHERE ip IN
		(SELECT ip FROM clients_seen ORDER BY last_seen DESC LIMIT -1 OFFSET ?)`, maxSeenRows); err != nil {
		r.log.Warn("could not prune seen clients", slog.Any("err", err))
	}
}

// Known lists addresses seen within the last `within` (0 = 30 days), most
// recent first (at most 10 000).
func (r *Registry) Known(ctx context.Context, within time.Duration) ([]Known, error) {
	if within <= 0 || within > seenRetention {
		within = seenRetention
	}
	since := time.Now().Add(-within)
	byIP := map[netip.Addr]*Known{}
	if r.ldb != nil {
		rows, err := r.ldb.R.QueryContext(ctx, `SELECT ip, mac, hostname, first_seen, last_seen, queries
			FROM clients_seen WHERE last_seen >= ? ORDER BY last_seen DESC LIMIT ?`, db.Ms(since), maxKnownRows)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k Known
			var first, last int64
			if err := rows.Scan(&k.IP, &k.MAC, &k.Hostname, &first, &last, &k.Queries); err != nil {
				rows.Close()
				return nil, err
			}
			ip, err := netip.ParseAddr(k.IP)
			if err != nil {
				continue
			}
			k.FirstSeen, k.LastSeen = db.Time(first), db.Time(last)
			byIP[ip] = &k
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	r.seenMu.Lock()
	r.seen.each(func(ip netip.Addr, e *seenEntry) bool {
		if e.last.Before(since) {
			return true
		}
		k, ok := byIP[ip]
		if !ok {
			q := e.total
			if r.ldb != nil && !e.transient {
				q = e.pending
			}
			byIP[ip] = &Known{IP: ip.String(), FirstSeen: e.first.UTC(), LastSeen: e.last.UTC(), Queries: q}
			return true
		}
		if e.last.After(k.LastSeen) {
			k.LastSeen = e.last.UTC()
		}
		if e.first.Before(k.FirstSeen) {
			k.FirstSeen = e.first.UTC()
		}
		k.Queries += e.pending
		return true
	})
	r.seenMu.Unlock()

	snap := r.snap.Load()
	arp := *r.arp.Load()
	out := make([]Known, 0, len(byIP))
	for ip, k := range byIP {
		if mac := arp[ip]; mac != "" {
			k.MAC = mac
		}
		if h := r.hostname(ip); h != "" {
			k.Hostname = h
		}
		if c := snap.match(ip, k.MAC); c != nil {
			k.ClientID, k.Name = c.id, c.name
		}
		out = append(out, *k)
	}
	slices.SortFunc(out, func(a, b Known) int {
		if c := b.LastSeen.Compare(a.LastSeen); c != 0 {
			return c
		}
		return cmp.Compare(a.IP, b.IP)
	})
	if len(out) > maxKnownRows {
		out = out[:maxKnownRows]
	}
	return out, nil
}
