package dhcp

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Leases returns the active and recently expired leases, newest first
// (GET /dhcp/leases). ClientName is left to the caller.
func (s *Service) Leases() []Lease {
	now := s.now()
	nm := s.names.Load()
	s.mu.Lock()
	out := make([]Lease, 0, len(s.t.leases))
	for _, l := range s.t.leases {
		host := s.t.effectiveName(l)
		e := Lease{MAC: l.mac, IP: l.ip.String(), Hostname: host, ClientID: l.clientID, Expires: l.expires.UTC(),
			Active: l.active(now)}
		if st := s.t.reservationOf(l); st != nil && st.ip == l.ip {
			e.Static = true
		}
		if r, ok := nm.byIP[l.ip]; ok && e.Active {
			e.DNSName, e.NameGenerated = r.name, r.generated
		}
		if host != "" && e.Active && s.holders[host] != l.mac {
			e.NameConflict = true
		}
		out = append(out, e)
	}
	s.mu.Unlock()
	slices.SortFunc(out, func(a, b Lease) int {
		if c := b.Expires.Compare(a.Expires); c != 0 {
			return c
		}
		return strings.Compare(a.MAC, b.MAC)
	})
	return out
}

// DeleteLeases ends every lease (DELETE /dhcp/leases): all rows of
// dhcp_leases (dynamic and on reserved addresses) in one transaction, the
// pending offers and the quarantine. While serving, devices keep their
// addresses until they renew; the neighbour check keeps protecting the
// addresses still in use. It returns the number of rows deleted.
func (s *Service) DeleteLeases(ctx context.Context) (int, error) {
	s.mu.Lock()
	n, err := s.wipe(ctx, false)
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	s.refreshNames()
	return n.leases, nil
}

// wiped counts the rows a wipe deleted.
type wiped struct{ leases, statics int }

// wipe empties dhcp_leases (and with statics dhcp_static) in one
// transaction and then the in-memory table: leases, offers, quarantine
// (and reservations). s.mu held.
func (s *Service) wipe(ctx context.Context, statics bool) (wiped, error) {
	var n wiped
	wctx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	err := s.d.DB.Tx(wctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(wctx, `DELETE FROM dhcp_leases`)
		if err != nil {
			return err
		}
		c, _ := res.RowsAffected()
		n.leases = int(c)
		if statics {
			if res, err = tx.ExecContext(wctx, `DELETE FROM dhcp_static`); err != nil {
				return err
			}
			c, _ = res.RowsAffected()
			n.statics = int(c)
		}
		return nil
	})
	if err != nil {
		return wiped{}, err
	}
	keep := s.t.statics
	s.t = newTable()
	if !statics {
		for _, st := range keep {
			s.t.putStatic(st)
		}
	}
	return n, nil
}

// Reset puts the DHCP server back to its defaults (POST /dhcp/reset): the
// section dhcp through settings.Update, so the normal switch-off path runs
// (announcements withdrawn, sockets closed, markers removed); if that
// fails nothing else changes. Then dhcp_static and dhcp_leases are emptied
// in one transaction and the in-memory table, offers, quarantine and names
// cleared. Other-server detections, the last probe, the exchange log and
// the other IPv6 announcers are kept: they are observations of the
// network, and the gate keeps protecting after a reset.
//
// The result says what was done also when err is not nil: Settings is
// true once the settings were reset, even if emptying the tables failed
// afterwards (the caller audits that change).
func (s *Service) Reset(ctx context.Context) (ResetResult, error) {
	if _, err := s.d.Settings.Update(ctx, func(a *settings.All) error {
		a.DHCP = settings.Defaults().DHCP
		return nil
	}); err != nil {
		return ResetResult{}, err
	}
	res := ResetResult{Settings: true}
	s.mu.Lock()
	n, err := s.wipe(ctx, true)
	s.mu.Unlock()
	s.Kick()
	if err != nil {
		return res, err
	}
	s.refreshNames()
	res.Leases, res.Statics = n.leases, n.statics
	return res, nil
}

// ResetResult says what POST /dhcp/reset did.
type ResetResult struct {
	Settings        bool // the section dhcp is back to its defaults
	Leases, Statics int  // rows deleted from dhcp_leases and dhcp_static
}

// DeleteLease ends the lease of mac (DELETE /dhcp/leases/{mac}); the
// device gets a new one when it renews.
func (s *Service) DeleteLease(ctx context.Context, mac string) error {
	m, ok := NormalizeMAC(mac)
	if !ok {
		return apperr.Invalid("mac", "must be a MAC address such as aa:bb:cc:dd:ee:ff")
	}
	s.mu.Lock()
	if s.t.leases[m] == nil {
		s.mu.Unlock()
		return apperr.NotFound("lease", m)
	}
	if err := deleteLeases(ctx, s.d.DB, m); err != nil {
		s.mu.Unlock()
		return err
	}
	s.t.dropLease(m)
	s.t.dropOffer(m)
	s.mu.Unlock()
	s.refreshNames()
	return nil
}

// Statics returns the static leases ordered by address (GET /dhcp/static).
func (s *Service) Statics() []StaticLease {
	now := s.now()
	s.mu.Lock()
	out := make([]StaticLease, 0, len(s.t.statics))
	for _, st := range s.t.statics {
		out = append(out, s.staticOut(st, now))
	}
	s.mu.Unlock()
	slices.SortFunc(out, func(a, b StaticLease) int {
		x, _ := netip.ParseAddr(a.IP)
		y, _ := netip.ParseAddr(b.IP)
		return x.Compare(y)
	})
	return out
}

// staticOut describes a reservation (s.mu held): active while the lease
// on its address belongs to it (by MAC or client identifier).
func (s *Service) staticOut(st *static, now time.Time) StaticLease {
	l := s.t.byIP[st.ip]
	return StaticLease{MAC: st.mac, IP: st.ip.String(), Hostname: st.hostname, Comment: st.comment, ClientID: st.clientID,
		LeaseSeconds: st.leaseSeconds, CreatedAt: st.created.UTC(), UpdatedAt: st.updated.UTC(),
		Active: l != nil && l.active(now) && s.t.reservationOf(l) == st}
}

// CreateStatic adds a static lease (POST /dhcp/static).
func (s *Service) CreateStatic(ctx context.Context, in StaticInput) (StaticLease, error) {
	mac, ok := NormalizeMAC(in.MAC)
	if !ok {
		return StaticLease{}, errMAC
	}
	return s.putStatic(ctx, mac, StaticUpdate{IP: in.IP, Hostname: in.Hostname, Comment: in.Comment, ClientID: in.ClientID,
		LeaseSeconds: in.LeaseSeconds}, true)
}

// msgMAC explains an invalid MAC address (errMAC, rows of an import).
const msgMAC = "must be a unicast MAC address such as aa:bb:cc:dd:ee:ff"

var errMAC = apperr.Invalid("mac", msgMAC)

// UpdateStatic changes the static lease of mac (PUT /dhcp/static/{mac}).
func (s *Service) UpdateStatic(ctx context.Context, mac string, in StaticUpdate) (StaticLease, error) {
	m, ok := NormalizeMAC(mac)
	if !ok {
		return StaticLease{}, errMAC
	}
	return s.putStatic(ctx, m, in, false)
}

// DeleteStatic removes the static lease of mac (DELETE /dhcp/static/{mac}).
// A lease the client holds stays until it expires or is renewed.
func (s *Service) DeleteStatic(ctx context.Context, mac string) error {
	m, ok := NormalizeMAC(mac)
	if !ok {
		return errMAC
	}
	s.mu.Lock()
	if s.t.statics[m] == nil {
		s.mu.Unlock()
		return apperr.NotFound("static lease", m)
	}
	wctx, cancel := context.WithTimeout(ctx, writeBudget)
	_, err := s.d.DB.W.ExecContext(wctx, `DELETE FROM dhcp_static WHERE mac = ?`, m)
	cancel()
	if err == nil {
		s.t.dropStatic(m)
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.refreshNames()
	return nil
}

// putStatic validates and stores a static lease.
func (s *Service) putStatic(ctx context.Context, mac string, in StaticUpdate, create bool) (StaticLease, error) {
	st, err := validateStatic(s.staticIPCheck(), mac, in)
	if err != nil {
		return StaticLease{}, err
	}
	now := s.now()
	s.mu.Lock()
	old := s.t.statics[mac]
	switch {
	case create && old != nil:
		s.mu.Unlock()
		return StaticLease{}, apperr.Invalid("mac", "a static lease for this MAC address exists")
	case !create && old == nil:
		s.mu.Unlock()
		return StaticLease{}, apperr.NotFound("static lease", mac)
	case create && len(s.t.statics) >= maxStatics:
		s.mu.Unlock()
		return StaticLease{}, apperr.Conflict("at most %d static leases are allowed", maxStatics)
	}
	for _, other := range s.t.statics {
		if other.mac != mac {
			if err := staticConflict(st, other); err != nil {
				s.mu.Unlock()
				return StaticLease{}, err
			}
		}
	}
	st.created, st.updated = now, now
	if old != nil {
		st.created = old.created
	}
	wctx, cancel := context.WithTimeout(ctx, writeBudget)
	err = s.d.DB.Tx(wctx, func(tx *sql.Tx) error { return insertStatic(wctx, tx, st) })
	cancel()
	if err != nil {
		s.mu.Unlock()
		if isUnique(err) {
			if strings.Contains(err.Error(), "client_id") {
				return StaticLease{}, apperr.Invalid("clientId", "%s is the client identifier of another reservation", st.clientID)
			}
			return StaticLease{}, apperr.Invalid("ip", "%s is the static address of another client", st.ip)
		}
		return StaticLease{}, err
	}
	s.t.putStatic(st)
	out := s.staticOut(st, now)
	s.mu.Unlock()
	s.refreshNames()
	return out, nil
}

// insertStatic writes a reservation (insert, or replace the row of its MAC).
func insertStatic(ctx context.Context, tx *sql.Tx, st *static) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO dhcp_static (mac, ip, hostname, comment, client_id, lease_seconds, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (mac) DO UPDATE SET ip = excluded.ip, hostname = excluded.hostname, comment = excluded.comment,
			client_id = excluded.client_id, lease_seconds = excluded.lease_seconds, updated_at = excluded.updated_at`,
		st.mac, st.ip.String(), st.hostname, st.comment, st.clientID, st.leaseSeconds, db.Ms(st.created), db.Ms(st.updated))
	return err
}

// validateStatic checks and normalises one reservation (POST and PUT
// /dhcp/static and every row of an import): the address (staticIPCheck),
// the host name (a DNS label; one in the form of a generated name must be
// the name of its own address), the comment, the client identifier
// (colon-separated hex, 2–255 bytes, stored lower-case) and the lease time
// (0 = the global one, else 300–604800 s). Uniqueness is checked by the
// caller (staticConflict).
func validateStatic(checkIP func(netip.Addr) error, mac string, in StaticUpdate) (*static, error) {
	ip, err := netip.ParseAddr(strings.TrimSpace(in.IP))
	if err != nil || !ip.Unmap().Is4() {
		return nil, apperr.Invalid("ip", "must be an IPv4 address")
	}
	ip = ip.Unmap()
	if err := checkIP(ip); err != nil {
		return nil, err
	}
	host := strings.ToLower(strings.TrimSpace(in.Hostname))
	switch {
	case host != "" && !validLabel(host):
		return nil, apperr.Invalid("hostname", "must be a host name: letters a-z, digits and '-', at most 63 characters, not starting or ending with '-'")
	case generatedForm(host) && host != generatedName(ip):
		return nil, apperr.Invalid("hostname", "a name of the form a-b-c-d is the generated name of that address; use %s or another name", generatedName(ip))
	}
	comment := strings.TrimSpace(in.Comment)
	if !utf8.ValidString(comment) || utf8.RuneCountInString(comment) > maxComment || strings.ContainsFunc(comment, unicode.IsControl) {
		return nil, apperr.Invalid("comment", "must be at most %d characters without control characters", maxComment)
	}
	var cid string
	if strings.TrimSpace(in.ClientID) != "" {
		var ok bool
		if cid, ok = normalizeClientID(in.ClientID); !ok {
			return nil, apperr.Invalid("clientId", "must be a client identifier (option 61) as colon-separated hex bytes, 2 to 255 bytes, such as 01:aa:bb:cc:dd:ee:ff")
		}
	}
	if !validLeaseSeconds(in.LeaseSeconds) {
		return nil, apperr.Invalid("leaseSeconds", "must be 0 (the global lease time) or between %d (5 minutes) and %d (7 days)",
			settings.DHCPMinLeaseSeconds, settings.DHCPMaxLeaseSeconds)
	}
	return &static{mac: mac, ip: ip, hostname: host, comment: comment, clientID: cid, leaseSeconds: in.LeaseSeconds}, nil
}

// validLeaseSeconds reports whether n is a reservation's lease time.
func validLeaseSeconds(n int) bool {
	return n == 0 || (n >= settings.DHCPMinLeaseSeconds && n <= settings.DHCPMaxLeaseSeconds)
}

// staticConflict reports what st shares with the reservation of another
// MAC: the address, the host name or the client identifier.
func staticConflict(st, other *static) error {
	switch {
	case other.ip == st.ip:
		return apperr.Invalid("ip", "%s is the static address of %s", st.ip, other.mac)
	case st.hostname != "" && other.hostname == st.hostname:
		return apperr.Invalid("hostname", "%s is the host name of the static lease of %s", st.hostname, other.mac)
	case st.clientID != "" && other.clientID == st.clientID:
		return apperr.Invalid("clientId", "%s is the client identifier of the static lease of %s", st.clientID, other.mac)
	}
	return nil
}

// staticIPCheck returns the check of a static address against the
// configured subnet: private, in the subnet, not its network or broadcast
// address, not PiCache's or the router's address. Without a usable
// interface only a private address is required. The interface is read
// once (an import checks many addresses).
func (s *Service) staticIPCheck() func(ip netip.Addr) error {
	set := s.d.Settings.Get()
	var g gate
	usable := false
	if set.DHCP.Interface != "" {
		s.mu.Lock()
		last := s.lastGW
		s.mu.Unlock()
		g = s.env.evalGate(set, last)
		usable = g.iface.problem == "" // absent now: checked again when it is enabled
	}
	return func(ip netip.Addr) error {
		if !netutil.IsRFC1918(ip) {
			return apperr.Invalid("ip", "must be a private IPv4 address (10/8, 172.16/12, 192.168/16)")
		}
		if !usable {
			return nil
		}
		subnet := g.iface.self.Masked()
		switch {
		case !subnet.Contains(ip):
			return apperr.Invalid("ip", "must be an address of %s (the subnet of %s)", subnet, g.iface.name)
		case ip == subnet.Addr() || ip == lastAddr(subnet):
			return apperr.Invalid("ip", "%s is the network or broadcast address of %s", ip, subnet)
		case ip == g.iface.self.Addr():
			return apperr.Invalid("ip", "%s is PiCache's own address", ip)
		case g.router.IsValid() && ip == g.router:
			return apperr.Invalid("ip", "%s is the router's address", ip)
		}
		return nil
	}
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") && !errors.Is(err, context.DeadlineExceeded)
}
