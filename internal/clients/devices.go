package clients

import (
	"context"
	"net/netip"
	"strconv"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// maxDeviceAddrs bounds one Devices call; seenIPsPerStmt bounds the
// parameters of one clients_seen lookup.
const (
	maxDeviceAddrs = 4096
	seenIPsPerStmt = 200
)

// Device describes the device behind a client address, for statistics
// grouped by device (docs/ARCHITECTURE.md 11).
type Device struct {
	// Key groups the addresses of one device: "client:<id>" for a
	// configured client (any rule, learned MACs included), else "mac:<mac>"
	// when the MAC is known, else "ip:<address>".
	Key      string
	ClientID int64  // 0 if no configured client matches
	MAC      string // from the neighbour table, else the MAC stored with the seen data ("" if unknown)
	// Name is the configured client name, else the PTR name of the
	// address or of another address with the same MAC, else the host name
	// stored with the seen data ("" if none).
	Name string
}

// Devices returns the device of each address (at most 4096; addresses are
// given and keyed in their string form, which need not be valid: invalid
// ones get the key "ip:<string>"). MACs missing from the neighbour table
// are looked up in the seen data of logs.db.
func (r *Registry) Devices(ctx context.Context, addrs []string) (map[string]Device, error) {
	addrs = addrs[:min(len(addrs), maxDeviceAddrs)]
	arp := *r.arp.Load()
	ips := make(map[string]netip.Addr, len(addrs))
	var missing []string // valid addresses without a current MAC
	for _, s := range addrs {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			continue
		}
		ip = netutil.Canon(ip)
		ips[s] = ip
		if arp[ip] == "" {
			missing = append(missing, ip.String())
		}
	}
	stored, err := r.seenMACs(ctx, missing)
	if err != nil {
		return nil, err
	}
	snap := r.snap.Load()
	out := make(map[string]Device, len(addrs))
	for _, s := range addrs {
		ip, ok := ips[s]
		if !ok {
			out[s] = Device{Key: "ip:" + s}
			continue
		}
		d := Device{MAC: arp[ip]}
		st := stored[ip.String()]
		if d.MAC == "" {
			d.MAC = st.mac
		}
		if c := r.match(snap, ip, d.MAC); c != nil {
			d.ClientID, d.Name = c.id, c.name
		}
		if d.Name == "" {
			d.Name = r.name(ip, d.MAC)
		}
		if d.Name == "" {
			d.Name = st.hostname
		}
		switch {
		case d.ClientID != 0:
			d.Key = "client:" + strconv.FormatInt(d.ClientID, 10)
		case d.MAC != "":
			d.Key = "mac:" + d.MAC
		default:
			d.Key = "ip:" + ip.String()
		}
		out[s] = d
	}
	return out, nil
}

// seenInfo is the stored MAC and host name of an address.
type seenInfo struct{ mac, hostname string }

// seenMACs reads the stored MAC and host name of addresses from
// clients_seen (nothing without logs.db).
func (r *Registry) seenMACs(ctx context.Context, ips []string) (map[string]seenInfo, error) {
	out := map[string]seenInfo{}
	if r.ldb == nil {
		return out, nil
	}
	for len(ips) > 0 {
		chunk := ips[:min(len(ips), seenIPsPerStmt)]
		ips = ips[len(chunk):]
		args := make([]any, len(chunk))
		for i, s := range chunk {
			args[i] = s
		}
		rows, err := r.ldb.R.QueryContext(ctx, `SELECT ip, mac, hostname FROM clients_seen WHERE ip IN (`+
			strings.Repeat("?, ", len(chunk)-1)+`?)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var ip string
			var info seenInfo
			if err := rows.Scan(&ip, &info.mac, &info.hostname); err != nil {
				rows.Close()
				return nil, err
			}
			out[ip] = info
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
