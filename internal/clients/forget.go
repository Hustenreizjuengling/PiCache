package clients

import (
	"context"
	"database/sql"
	"net/netip"
	"slices"

	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// Forgetting seen data (DELETE /clients/known, POST /clients/known/flush):
// always through the registry, never a plain DELETE from an API goroutine.
// The registry's flushMu is held by flush from the moment it copies the
// pending counts until its write returned; forgetting holds it for the
// whole operation, so no pending flush can re-create a row. Under seenMu it
// removes the in-memory entries (the seen entries with their pending counts
// and last ClientID, and the rows of the ClientID list whose address is one
// of them) and releases seenMu again before it deletes the clients_seen
// rows: every DNS query takes seenMu (Seen), and a busy logs.db writer must
// never stall DNS answering. A query in between is new activity and may
// re-create its device. Then the cached names and identities of the
// addresses are dropped. A forgotten device reappears with its next query;
// the kernel's neighbour table is not touched.

// ForgetKnown forgets the seen data of ip, or with mac (ip invalid) of
// every address whose current neighbour-table MAC or stored seen MAC is
// mac. It returns the number of addresses forgotten (0 is no error).
func (r *Registry) ForgetKnown(ctx context.Context, ip netip.Addr, mac string) (int, error) {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	var addrs []netip.Addr
	if ip.IsValid() {
		addrs = []netip.Addr{netutil.Canon(ip)}
	} else {
		for a, m := range *r.arp.Load() {
			if m == mac {
				addrs = append(addrs, a)
			}
		}
		if r.ldb != nil {
			rows, err := r.ldb.R.QueryContext(ctx, `SELECT ip FROM clients_seen WHERE mac = ?`, mac)
			if err != nil {
				return 0, err
			}
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					rows.Close()
					return 0, err
				}
				if a, err := netip.ParseAddr(s); err == nil && !slices.Contains(addrs, a) {
					addrs = append(addrs, a)
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return 0, err
			}
		}
	}
	if len(addrs) == 0 {
		return 0, nil
	}
	gone := map[netip.Addr]bool{}
	r.seenMu.Lock()
	for _, a := range addrs {
		if _, ok := r.seen.peek(a); ok {
			r.seen.delete(a)
			gone[a] = true
		}
	}
	var ids []string
	r.dnsIDs.each(func(id string, e *seenClientID) bool {
		if slices.Contains(addrs, e.addr) {
			ids = append(ids, id)
		}
		return true
	})
	for _, id := range ids {
		r.dnsIDs.delete(id)
	}
	r.seenMu.Unlock()
	var err error
	if r.ldb != nil {
		err = r.ldb.Tx(ctx, func(tx *sql.Tx) error {
			for _, a := range addrs {
				res, err := tx.ExecContext(ctx, `DELETE FROM clients_seen WHERE ip = ?`, a.String())
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n > 0 {
					gone[a] = true
				}
			}
			return nil
		})
	}
	r.forgetNames(addrs)
	if err != nil {
		return 0, err
	}
	return len(gone), nil
}

// FlushKnown forgets all seen data and returns the number of addresses
// forgotten.
func (r *Registry) FlushKnown(ctx context.Context) (int, error) {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	gone := map[netip.Addr]bool{}
	r.seenMu.Lock()
	r.seen.each(func(a netip.Addr, _ *seenEntry) bool { gone[a] = true; return true })
	r.seen.clear()
	r.dnsIDs.clear()
	r.seenMu.Unlock()
	var err error
	if r.ldb != nil {
		err = r.ldb.Tx(ctx, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, `SELECT ip FROM clients_seen`)
			if err != nil {
				return err
			}
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					rows.Close()
					return err
				}
				if a, err := netip.ParseAddr(s); err == nil {
					gone[a] = true
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `DELETE FROM clients_seen`)
			return err
		})
	}
	if err != nil {
		return 0, err
	}
	r.namesMu.Lock()
	r.names.clear()
	r.namesMu.Unlock()
	r.whois.reset()
	r.invalidate()
	return len(gone), nil
}

// forgetNames drops the cached names and identities of addrs.
func (r *Registry) forgetNames(addrs []netip.Addr) {
	r.namesMu.Lock()
	for _, a := range addrs {
		r.names.delete(a)
	}
	r.namesMu.Unlock()
	for _, a := range addrs {
		r.invalidateIP(a)
	}
}
