package clients

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// maxDeviceNameLen bounds the name of a device group (the room for " (99)"
// within maxNameLen).
const maxDeviceNameLen = 59

// DeviceRequest asks for the group of one device ("Only for this device",
// ARCHITECTURE 7.2): its address, its neighbour-table MAC ("" when unknown
// or protected: the caller leaves out the router's, this machine's and the
// trusted forwarders' MACs) and its host name ("" = the address).
type DeviceRequest struct {
	IP   netip.Addr
	MAC  string
	Name string
}

// DeviceGroup is what EnsureDeviceGroup did: the device's client and group
// (as they are now) and whether it created them (what RevertDeviceGroup
// undoes). A reused group has the client as its only member already.
type DeviceGroup struct {
	Client        Client
	Group         Group
	CreatedClient bool
	CreatedGroup  bool
}

// DeviceName returns the host name of an address as GET /clients/known
// reports it: its DHCP lease name, its PTR name or the name of another
// address with the same MAC ("" if none is known; never a configured
// client's name).
func (r *Registry) DeviceName(ip netip.Addr, mac string) string {
	return r.name(netutil.Canon(ip), mac)
}

// CleanDeviceName makes untrusted text (a device's host name) a group
// name: invalid UTF-8 and every character of the Unicode categories Cc and
// Cf removed, white space collapsed, trimmed and cut to 59 characters at a
// rune boundary.
func CleanDeviceName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Cf) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > maxDeviceNameLen {
		s = strings.TrimSpace(string([]rune(s)[:maxDeviceNameLen]))
	}
	return s
}

// EnsureDeviceGroup gives the device at req.IP a group that holds only it,
// in one transaction:
//
//   - the client: the configured client that identifies the address when
//     none of its identifiers is a CIDR (it describes devices, not a
//     network); otherwise a new client named after the device (the
//     address, and its MAC unless another client holds it) in the groups
//     the address has now (the matching CIDR client's, else Default) with
//     the CIDR client's flags. An address that is itself an identifier of a
//     client that also covers a network is a conflict.
//   - the group: the group created for this client earlier while the
//     client is its only member; otherwise a new enabled group named after
//     the client (" (2)" … " (99)" appended on a name collision) marked as
//     the client's device group. The client keeps all its groups and gains
//     this one.
//
// The limits of the single routes apply (apperr.Conflict).
func (r *Registry) EnsureDeviceGroup(ctx context.Context, req DeviceRequest) (DeviceGroup, error) {
	ip := netutil.Canon(req.IP)
	if !ip.IsValid() {
		return DeviceGroup{}, apperr.Invalid("clientIp", "must be an IP address")
	}
	var out DeviceGroup
	var clientID, groupID int64
	err := r.write(ctx, func(tx *sql.Tx) error {
		out = DeviceGroup{}
		var err error
		if clientID, err = r.deviceClient(ctx, tx, ip, req, &out); err != nil {
			return err
		}
		groupID, err = deviceGroupOf(ctx, tx, clientID, &out)
		return err
	})
	if err != nil {
		return DeviceGroup{}, err
	}
	if out.Client, err = r.client(ctx, clientID); err != nil {
		return DeviceGroup{}, err
	}
	if out.Group, err = r.group(ctx, groupID); err != nil {
		return DeviceGroup{}, err
	}
	return out, nil
}

// deviceClient finds or creates the client of the device (see
// EnsureDeviceGroup).
func (r *Registry) deviceClient(ctx context.Context, tx *sql.Tx, ip netip.Addr, req DeviceRequest, out *DeviceGroup) (int64, error) {
	snap := r.snap.Load()
	mac := (*r.arp.Load())[ip]
	groups := []int64{DefaultGroupID}
	var flags struct{ bypass, logs, stats bool }
	if c := r.match(snap, ip, mac); c != nil {
		idents, err := clientIdentifiers(ctx, tx, c.id)
		if err != nil {
			return 0, err
		}
		network := slices.ContainsFunc(idents, func(v string) bool { return strings.Contains(v, "/") })
		if !network {
			return c.id, nil
		}
		if slices.Contains(idents, ip.String()) {
			return 0, apperr.Conflict("the address belongs to client %s, which also covers a network; configure the device on Clients & groups", c.name)
		}
		groups = slices.Clone(c.groups)
		flags.bypass, flags.logs, flags.stats = c.downloadCacheBypass, c.ignoreLogs, c.ignoreStats
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM client_clients`).Scan(&n); err != nil {
		return 0, err
	}
	if n >= maxClients {
		return 0, apperr.Conflict("at most %d clients are allowed", maxClients)
	}
	name := CleanDeviceName(req.Name)
	if name == "" {
		name = ip.String()
	}
	idents := []identifier{{value: ip.String(), kind: kindIP, ip: ip}}
	if m, ok := normalizeMAC(req.MAC); ok {
		var owner int64
		switch err := tx.QueryRowContext(ctx, `SELECT client_id FROM client_identifiers WHERE value = ?`, m).Scan(&owner); {
		case errors.Is(err, sql.ErrNoRows):
			idents = append(idents, identifier{value: m, kind: kindMAC})
		case err != nil:
			return 0, err
		}
	}
	now := db.NowMs()
	res, err := tx.ExecContext(ctx, `INSERT INTO client_clients (name, comment, download_cache_bypass, ignore_logs, ignore_stats,
			created_at, updated_at) VALUES (?, '', ?, ?, ?, ?, ?)`, name, flags.bypass, flags.logs, flags.stats, now, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	in := ClientInput{GroupIDs: groups}
	if err := saveClientLinks(ctx, tx, id, in, idents); err != nil {
		return 0, err
	}
	out.CreatedClient = true
	return id, nil
}

// clientIdentifiers returns the identifiers of client id.
func clientIdentifiers(ctx context.Context, tx *sql.Tx, id int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT value FROM client_identifiers WHERE client_id = ? ORDER BY pos`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// deviceGroupOf finds the device group of client id (marked for it and the
// client its only member) or creates one, and makes the client a member.
func deviceGroupOf(ctx context.Context, tx *sql.Tx, clientID int64, out *DeviceGroup) (int64, error) {
	var gid int64
	err := tx.QueryRowContext(ctx, `SELECT g.id FROM client_groups g WHERE g.device_client_id = ?
		AND (SELECT COUNT(*) FROM client_memberships m WHERE m.group_id = g.id) = 1
		AND EXISTS (SELECT 1 FROM client_memberships m WHERE m.group_id = g.id AND m.client_id = ?)
		ORDER BY g.id LIMIT 1`, clientID, clientID).Scan(&gid)
	switch {
	case err == nil:
		return gid, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, err
	}
	var clientName string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM client_clients WHERE id = ?`, clientID).Scan(&clientName); err != nil {
		return 0, err
	}
	var n, memberships int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM client_groups), (SELECT COUNT(*) FROM client_memberships WHERE client_id = ?)`,
		clientID).Scan(&n, &memberships); err != nil {
		return 0, err
	}
	if n >= maxGroups {
		return 0, apperr.Conflict("at most %d groups are allowed", maxGroups)
	}
	if memberships >= maxGroupsPerClient {
		return 0, apperr.Conflict("at most %d groups per client are allowed", maxGroupsPerClient)
	}
	base := CleanDeviceName(clientName)
	if base == "" {
		base = "Device" // a name of format characters only
	}
	name := ""
	for i := 1; i <= 99 && name == ""; i++ {
		cand := base
		if i > 1 {
			cand = fmt.Sprintf("%s (%d)", base, i)
		}
		var other int64
		switch err := tx.QueryRowContext(ctx, `SELECT id FROM client_groups WHERE name = ? COLLATE NOCASE`, cand).Scan(&other); {
		case errors.Is(err, sql.ErrNoRows):
			name = cand
		case err != nil:
			return 0, err
		}
	}
	if name == "" {
		return 0, apperr.Conflict("the groups %s … %s (99) exist already; rename the device's client on Clients & groups", base, base)
	}
	comment := "Only for " + clientName
	res, err := tx.ExecContext(ctx, `INSERT INTO client_groups (name, comment, enabled, created_at, device_client_id) VALUES (?, ?, 1, ?, ?)`,
		name, comment, db.NowMs(), clientID)
	if err != nil {
		return 0, err
	}
	if gid, err = res.LastInsertId(); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO client_memberships (client_id, group_id) VALUES (?, ?)`, clientID, gid); err != nil {
		return 0, err
	}
	out.CreatedGroup = true
	return gid, nil
}

// RevertDeviceGroup undoes exactly what EnsureDeviceGroup did (the rule of
// "Only for this device" could not be saved): a created group (with the
// client's membership) and a created client are deleted; a reused client
// keeps every other group.
func (r *Registry) RevertDeviceGroup(ctx context.Context, d DeviceGroup) error {
	if !d.CreatedClient && !d.CreatedGroup {
		return nil
	}
	return r.write(ctx, func(tx *sql.Tx) error {
		if d.CreatedGroup {
			if _, err := tx.ExecContext(ctx, `DELETE FROM client_groups WHERE id = ?`, d.Group.ID); err != nil {
				return err
			}
		}
		if d.CreatedClient {
			if _, err := tx.ExecContext(ctx, `DELETE FROM client_clients WHERE id = ?`, d.Client.ID); err != nil {
				return err
			}
		}
		return nil
	})
}
