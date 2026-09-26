package dhcp

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
)

// Limits of the stored data.
const (
	maxLeases   = 4096
	maxStatics  = 1024
	retention   = 24 * time.Hour   // expired leases are kept to hand the address back
	quarantine  = 10 * time.Minute // declined or conflicting addresses
	offerHold   = time.Minute      // an offered address is held for its client
	maxComment  = 200
	writeBudget = 5 * time.Second // one database write
)

// migrations of component "dhcp". Append only.
var migrations = []string{
	`CREATE TABLE dhcp_static (
		mac        TEXT    PRIMARY KEY,
		ip         TEXT    NOT NULL UNIQUE,
		hostname   TEXT    NOT NULL DEFAULT '',
		comment    TEXT    NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE TABLE dhcp_leases (
		mac        TEXT    PRIMARY KEY,
		ip         TEXT    NOT NULL UNIQUE,
		hostname   TEXT    NOT NULL DEFAULT '',
		client_id  TEXT    NOT NULL DEFAULT '',
		expires_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);`,
	// v2 (0.8.0): a reservation may also match option 61 and may have its
	// own lease time (0 = dhcp.leaseSeconds).
	`ALTER TABLE dhcp_static ADD COLUMN client_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE dhcp_static ADD COLUMN lease_seconds INTEGER NOT NULL DEFAULT 0;
	CREATE UNIQUE INDEX dhcp_static_client_id ON dhcp_static(client_id) WHERE client_id <> '';`,
}

// lease is a dynamic lease (or the lease of a static entry) as handed out.
type lease struct {
	mac      string
	ip       netip.Addr
	hostname string // sanitised name the client sent ("" = none)
	clientID string // option 61 as colon-separated hex
	expires  time.Time
	updated  time.Time
}

func (l *lease) active(now time.Time) bool { return l.expires.After(now) }

// static is a reservation: a configured address for a MAC (the key), or for
// a client identifier (option 61).
type static struct {
	mac          string
	ip           netip.Addr
	hostname     string
	comment      string
	clientID     string // colon-separated lower-case hex ("" = none)
	leaseSeconds int    // 0 = dhcp.leaseSeconds
	created      time.Time
	updated      time.Time
}

// sameContent reports whether two reservations hand out the same.
func (st *static) sameContent(o *static) bool {
	return st.mac == o.mac && st.ip == o.ip && st.hostname == o.hostname && st.comment == o.comment &&
		st.clientID == o.clientID && st.leaseSeconds == o.leaseSeconds
}

// offer is an address offered to a client and held for it briefly.
type offer struct {
	ip    netip.Addr
	until time.Time
}

// table is the in-memory state of leases, static entries, quarantined
// addresses and pending offers. Guarded by Service.mu.
type table struct {
	leases     map[string]*lease // by MAC
	byIP       map[netip.Addr]*lease
	statics    map[string]*static // by MAC
	staticIP   map[netip.Addr]*static
	staticCID  map[string]*static // by client identifier
	quarantine map[netip.Addr]time.Time
	offers     map[string]offer // by MAC
	offerIP    map[netip.Addr]string
}

func newTable() *table {
	return &table{
		leases: map[string]*lease{}, byIP: map[netip.Addr]*lease{},
		statics: map[string]*static{}, staticIP: map[netip.Addr]*static{}, staticCID: map[string]*static{},
		quarantine: map[netip.Addr]time.Time{}, offers: map[string]offer{}, offerIP: map[netip.Addr]string{},
	}
}

// reservationOf returns the reservation a lease belongs to: the one of its
// MAC (a MAC match always wins), else the one whose client identifier the
// lease carries when the lease is on its address (a client-ID match).
func (t *table) reservationOf(l *lease) *static {
	if st := t.statics[l.mac]; st != nil {
		return st
	}
	if st := t.staticCID[l.clientID]; l.clientID != "" && st != nil && st.ip == l.ip {
		return st
	}
	return nil
}

// putLease stores l, replacing the client's previous lease and any
// (expired) lease of another client on the same address.
func (t *table) putLease(l *lease) {
	if old := t.leases[l.mac]; old != nil && t.byIP[old.ip] == old {
		delete(t.byIP, old.ip)
	}
	if other := t.byIP[l.ip]; other != nil && other.mac != l.mac {
		delete(t.leases, other.mac)
	}
	t.leases[l.mac] = l
	t.byIP[l.ip] = l
	t.dropOffer(l.mac)
}

// dropLease removes the lease of mac.
func (t *table) dropLease(mac string) *lease {
	l := t.leases[mac]
	if l == nil {
		return nil
	}
	delete(t.leases, mac)
	if t.byIP[l.ip] == l {
		delete(t.byIP, l.ip)
	}
	return l
}

func (t *table) putStatic(st *static) {
	t.dropStatic(st.mac)
	t.statics[st.mac] = st
	t.staticIP[st.ip] = st
	if st.clientID != "" {
		t.staticCID[st.clientID] = st
	}
}

func (t *table) dropStatic(mac string) {
	if st := t.statics[mac]; st != nil {
		delete(t.statics, mac)
		if t.staticIP[st.ip] == st {
			delete(t.staticIP, st.ip)
		}
		if t.staticCID[st.clientID] == st {
			delete(t.staticCID, st.clientID)
		}
	}
}

func (t *table) putOffer(mac string, ip netip.Addr, until time.Time) {
	t.dropOffer(mac)
	if other, ok := t.offerIP[ip]; ok {
		delete(t.offers, other)
	}
	t.offers[mac] = offer{ip: ip, until: until}
	t.offerIP[ip] = mac
}

func (t *table) dropOffer(mac string) {
	if o, ok := t.offers[mac]; ok {
		delete(t.offers, mac)
		if t.offerIP[o.ip] == mac {
			delete(t.offerIP, o.ip)
		}
	}
}

// quarantined reports whether ip is quarantined at now.
func (t *table) quarantined(ip netip.Addr, now time.Time) bool {
	until, ok := t.quarantine[ip]
	return ok && now.Before(until)
}

// expire drops pending offers and quarantine entries that ended, and
// leases that expired more than retention ago (returned for the database).
func (t *table) expire(now time.Time) (purged []string) {
	for mac, o := range t.offers {
		if !now.Before(o.until) {
			t.dropOffer(mac)
		}
	}
	for ip, until := range t.quarantine {
		if !now.Before(until) {
			delete(t.quarantine, ip)
		}
	}
	for mac, l := range t.leases {
		if now.Sub(l.expires) > retention {
			t.dropLease(mac)
			purged = append(purged, mac)
		}
	}
	return purged
}

// makeRoom drops the lease that expired first when the table is full (a
// new client needs a row); false when every lease is active.
func (t *table) makeRoom(now time.Time) (dropped string, ok bool) {
	if len(t.leases) < maxLeases {
		return "", true
	}
	var oldest *lease
	for _, l := range t.leases {
		if !l.active(now) && (oldest == nil || l.expires.Before(oldest.expires)) {
			oldest = l
		}
	}
	if oldest == nil {
		return "", false
	}
	t.dropLease(oldest.mac)
	return oldest.mac, true
}

// sortedLeases returns the leases ordered by MAC (deterministic walks).
func (t *table) sortedLeases() []*lease {
	out := make([]*lease, 0, len(t.leases))
	for _, l := range t.leases {
		out = append(out, l)
	}
	slices.SortFunc(out, func(a, b *lease) int { return cmp.Compare(a.mac, b.mac) })
	return out
}

// --- database ---

// load reads the static entries and leases. Rows an edited database made
// invalid (MAC, address, host name, client identifier, lease time) are
// skipped; at most 1024 static entries and 4096 leases are read.
func load(ctx context.Context, d *db.DB, t *table) (skipped int, err error) {
	rows, err := d.R.QueryContext(ctx, `SELECT mac, ip, hostname, comment, client_id, lease_seconds, created_at, updated_at
		FROM dhcp_static ORDER BY mac LIMIT ?`, maxStatics)
	if err != nil {
		return 0, fmt.Errorf("dhcp: load static leases: %w", err)
	}
	for rows.Next() {
		var st static
		var ip string
		var created, updated int64
		if err := rows.Scan(&st.mac, &ip, &st.hostname, &st.comment, &st.clientID, &st.leaseSeconds, &created, &updated); err != nil {
			rows.Close()
			return skipped, fmt.Errorf("dhcp: scan static lease: %w", err)
		}
		mac, ok := NormalizeMAC(st.mac)
		addr, aerr := netip.ParseAddr(ip)
		cid, cidOK := normalizeClientID(st.clientID)
		if !ok || aerr != nil || !addr.Is4() || (st.hostname != "" && !validLabel(st.hostname)) || t.staticIP[addr] != nil ||
			(st.clientID != "" && (!cidOK || t.staticCID[cid] != nil)) || !validLeaseSeconds(st.leaseSeconds) {
			skipped++
			continue
		}
		st.mac, st.ip, st.clientID, st.created, st.updated = mac, addr, cid, db.Time(created), db.Time(updated)
		t.putStatic(&st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return skipped, err
	}
	rows, err = d.R.QueryContext(ctx, `SELECT mac, ip, hostname, client_id, expires_at, updated_at
		FROM dhcp_leases ORDER BY expires_at DESC LIMIT ?`, maxLeases)
	if err != nil {
		return skipped, fmt.Errorf("dhcp: load leases: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l lease
		var ip string
		var expires, updated int64
		if err := rows.Scan(&l.mac, &ip, &l.hostname, &l.clientID, &expires, &updated); err != nil {
			return skipped, fmt.Errorf("dhcp: scan lease: %w", err)
		}
		mac, ok := NormalizeMAC(l.mac)
		addr, aerr := netip.ParseAddr(ip)
		if !ok || aerr != nil || !addr.Is4() || (l.hostname != "" && !validLabel(l.hostname)) || len(l.clientID) > 3*maxClientID-1 ||
			t.byIP[addr] != nil || t.leases[mac] != nil {
			skipped++
			continue
		}
		// A name a client may no longer take (stored by an older version)
		// counts as none.
		l.mac, l.ip, l.hostname, l.expires, l.updated = mac, addr, clientName(l.hostname), db.Time(expires), db.Time(updated)
		t.putLease(&l)
	}
	return skipped, rows.Err()
}

// saveLease writes l, removing another client's row for the same address
// (an expired lease that was reclaimed).
func saveLease(ctx context.Context, d *db.DB, l *lease) error {
	ctx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	return d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM dhcp_leases WHERE ip = ? AND mac <> ?`, l.ip.String(), l.mac); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO dhcp_leases (mac, ip, hostname, client_id, expires_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (mac) DO UPDATE SET ip = excluded.ip, hostname = excluded.hostname, client_id = excluded.client_id,
				expires_at = excluded.expires_at, updated_at = excluded.updated_at`,
			l.mac, l.ip.String(), l.hostname, l.clientID, db.Ms(l.expires), db.Ms(l.updated))
		return err
	})
}

// deleteLeases removes the rows of the given MACs.
func deleteLeases(ctx context.Context, d *db.DB, macs ...string) error {
	if len(macs) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	return d.Tx(ctx, func(tx *sql.Tx) error {
		for _, mac := range macs {
			if _, err := tx.ExecContext(ctx, `DELETE FROM dhcp_leases WHERE mac = ?`, mac); err != nil {
				return err
			}
		}
		return nil
	})
}

// Migrations returns the schema steps of component "dhcp" in picache.db
// (`picache db salvage` builds a fresh schema with them).
func Migrations() []string { return slices.Clone(migrations) }
