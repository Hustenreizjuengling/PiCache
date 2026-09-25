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
		if st := s.t.statics[l.mac]; st != nil && st.ip == l.ip {
			e.Static = true
		}
		if r, ok := nm.byIP[l.ip]; ok && e.Active {
			e.DNSName = r.name
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

func (s *Service) staticOut(st *static, now time.Time) StaticLease {
	l := s.t.leases[st.mac]
	return StaticLease{MAC: st.mac, IP: st.ip.String(), Hostname: st.hostname, Comment: st.comment,
		CreatedAt: st.created.UTC(), UpdatedAt: st.updated.UTC(), Active: l != nil && l.ip == st.ip && l.active(now)}
}

// CreateStatic adds a static lease (POST /dhcp/static).
func (s *Service) CreateStatic(ctx context.Context, in StaticInput) (StaticLease, error) {
	mac, ok := NormalizeMAC(in.MAC)
	if !ok {
		return StaticLease{}, apperr.Invalid("mac", "must be a unicast MAC address such as aa:bb:cc:dd:ee:ff")
	}
	return s.putStatic(ctx, mac, StaticUpdate{IP: in.IP, Hostname: in.Hostname, Comment: in.Comment}, true)
}

// UpdateStatic changes the static lease of mac (PUT /dhcp/static/{mac}).
func (s *Service) UpdateStatic(ctx context.Context, mac string, in StaticUpdate) (StaticLease, error) {
	m, ok := NormalizeMAC(mac)
	if !ok {
		return StaticLease{}, apperr.Invalid("mac", "must be a unicast MAC address such as aa:bb:cc:dd:ee:ff")
	}
	return s.putStatic(ctx, m, in, false)
}

// DeleteStatic removes the static lease of mac (DELETE /dhcp/static/{mac}).
// A lease the client holds stays until it expires or is renewed.
func (s *Service) DeleteStatic(ctx context.Context, mac string) error {
	m, ok := NormalizeMAC(mac)
	if !ok {
		return apperr.Invalid("mac", "must be a unicast MAC address such as aa:bb:cc:dd:ee:ff")
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
	ip, err := netip.ParseAddr(strings.TrimSpace(in.IP))
	if err != nil || !ip.Unmap().Is4() {
		return StaticLease{}, apperr.Invalid("ip", "must be an IPv4 address")
	}
	ip = ip.Unmap()
	if err := s.checkStaticIP(ip); err != nil {
		return StaticLease{}, err
	}
	host := strings.ToLower(strings.TrimSpace(in.Hostname))
	if host != "" && !validLabel(host) {
		return StaticLease{}, apperr.Invalid("hostname", "must be a host name: letters a-z, digits and '-', at most 63 characters, not starting or ending with '-'")
	}
	comment := strings.TrimSpace(in.Comment)
	if !utf8.ValidString(comment) || utf8.RuneCountInString(comment) > maxComment || strings.ContainsFunc(comment, unicode.IsControl) {
		return StaticLease{}, apperr.Invalid("comment", "must be at most %d characters without control characters", maxComment)
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
	if other := s.t.staticIP[ip]; other != nil && other.mac != mac {
		s.mu.Unlock()
		return StaticLease{}, apperr.Invalid("ip", "%s is the static address of %s", ip, other.mac)
	}
	if host != "" {
		for _, other := range s.t.statics {
			if other.mac != mac && other.hostname == host {
				s.mu.Unlock()
				return StaticLease{}, apperr.Invalid("hostname", "%s is the host name of the static lease of %s", host, other.mac)
			}
		}
	}
	st := &static{mac: mac, ip: ip, hostname: host, comment: comment, created: now, updated: now}
	if old != nil {
		st.created = old.created
	}
	wctx, cancel := context.WithTimeout(ctx, writeBudget)
	err = s.d.DB.Tx(wctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(wctx, `INSERT INTO dhcp_static (mac, ip, hostname, comment, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (mac) DO UPDATE SET ip = excluded.ip, hostname = excluded.hostname, comment = excluded.comment,
				updated_at = excluded.updated_at`,
			mac, ip.String(), host, comment, db.Ms(st.created), db.Ms(st.updated))
		return err
	})
	cancel()
	if err != nil {
		s.mu.Unlock()
		if isUnique(err) {
			return StaticLease{}, apperr.Invalid("ip", "%s is the static address of another client", ip)
		}
		return StaticLease{}, err
	}
	s.t.putStatic(st)
	out := s.staticOut(st, now)
	s.mu.Unlock()
	s.refreshNames()
	return out, nil
}

// checkStaticIP checks a static address against the configured subnet: in
// the subnet, not its network or broadcast address, not PiCache's or the
// router's address. Without a usable interface only a private address is
// required.
func (s *Service) checkStaticIP(ip netip.Addr) error {
	set := s.d.Settings.Get()
	if !netutil.IsRFC1918(ip) {
		return apperr.Invalid("ip", "must be a private IPv4 address (10/8, 172.16/12, 192.168/16)")
	}
	if set.DHCP.Interface == "" {
		return nil
	}
	s.mu.Lock()
	last := s.lastGW
	s.mu.Unlock()
	g := s.env.evalGate(set, last)
	if g.iface.problem != "" {
		return nil // the interface is absent now; checked again when it is enabled
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

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") && !errors.Is(err, context.DeadlineExceeded)
}
