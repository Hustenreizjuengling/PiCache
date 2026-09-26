package api

import (
	"cmp"
	"context"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Top lists grouped by device: maxGroupedTop rows are grouped before the
// requested limit applies (the hourly top tables keep 1000 keys);
// defaultTopLimit is the limit when none is given (as logs.Top).
const (
	maxGroupedTop   = 1000
	defaultTopLimit = 10
)

// statsGroup reads ?group: "" (per address) or "device".
func statsGroup(r *http.Request) (bool, error) {
	switch qString(r, "group") {
	case "":
		return false, nil
	case "device":
		return true, nil
	}
	return false, apperr.Invalid("group", "must be device or empty")
}

// devices returns the device of each address (docs/ARCHITECTURE.md 11), or
// nil when devices cannot be told apart: without a clients registry, or
// while client addresses are anonymised (the statistics are keyed by
// masked addresses, and names are not shown then).
func (s *Server) devices(ctx context.Context, addrs []string) (map[string]clients.Device, error) {
	if s.d.Clients == nil || (s.d.Settings != nil && s.d.Settings.Get().Logs.AnonymizeClientIPs) {
		return nil, nil
	}
	return s.d.Clients.Devices(ctx, addrs)
}

// deviceKey returns the grouping key of an address.
func deviceKey(devs map[string]clients.Device, addr string) string {
	if d, ok := devs[addr]; ok && d.Key != "" {
		return d.Key
	}
	return "ip:" + addr
}

// deviceName picks the name of a device from its members (most relevant
// first): the configured client name, else the name of an IPv4 member
// (typically the router's DHCPv4 name), else the first member's name.
func deviceName(devs map[string]clients.Device, members []string) string {
	first := ""
	for _, a := range members {
		d := devs[a]
		if d.ClientID != 0 && d.Name != "" {
			return d.Name
		}
		if ip, err := netip.ParseAddr(a); err == nil && ip.Unmap().Is4() && d.Name != "" {
			return d.Name
		}
		if first == "" {
			first = d.Name
		}
	}
	return first
}

// annotateClientStats sets the client ID and MAC of every row.
func annotateClientStats(stats []logs.ClientStat, devs map[string]clients.Device) {
	for i := range stats {
		d := devs[stats[i].ClientIP]
		stats[i].ClientID, stats[i].MAC = d.ClientID, d.MAC
	}
}

// groupClientStats merges the rows of the addresses of one device: the most
// recently active address is ClientIP, counters are summed, LastSeen is the
// latest, Addresses lists all addresses (most recent first) and ClientName
// is the configured name or the device name (else the most recent row's
// name). MAC is set when all addresses with a known MAC share it.
func groupClientStats(stats []logs.ClientStat, devs map[string]clients.Device) []logs.ClientStat {
	byKey := map[string][]logs.ClientStat{}
	var keys []string
	for _, st := range stats {
		k := deviceKey(devs, st.ClientIP)
		if _, ok := byKey[k]; !ok {
			keys = append(keys, k)
		}
		byKey[k] = append(byKey[k], st)
	}
	out := make([]logs.ClientStat, 0, len(keys))
	for _, k := range keys {
		rows := byKey[k]
		slices.SortStableFunc(rows, func(a, b logs.ClientStat) int {
			return cmp.Or(b.LastSeen.Compare(a.LastSeen), cmp.Compare(a.ClientIP, b.ClientIP))
		})
		g := logs.ClientStat{ClientIP: rows[0].ClientIP, LastSeen: rows[0].LastSeen, Addresses: make([]string, 0, len(rows))}
		macs := map[string]bool{}
		for _, st := range rows {
			g.Addresses = append(g.Addresses, st.ClientIP)
			g.Queries += st.Queries
			g.Blocked += st.Blocked
			g.CacheBytes += st.CacheBytes
			g.CacheHitBytes += st.CacheHitBytes
			d := devs[st.ClientIP]
			if d.ClientID != 0 {
				g.ClientID = d.ClientID
			}
			if d.MAC != "" {
				macs[d.MAC] = true
			}
		}
		if len(macs) == 1 {
			for m := range macs {
				g.MAC = m
			}
		}
		if g.ClientName = deviceName(devs, g.Addresses); g.ClientName == "" {
			for _, st := range rows {
				if st.ClientName != "" {
					g.ClientName = st.ClientName
					break
				}
			}
		}
		out = append(out, g)
	}
	slices.SortFunc(out, func(a, b logs.ClientStat) int {
		return cmp.Or(cmp.Compare(b.Queries, a.Queries), cmp.Compare(b.CacheBytes, a.CacheBytes),
			cmp.Compare(a.ClientIP, b.ClientIP))
	})
	return out
}

// groupTop merges the top-list rows (kinds clients and cache-clients) of
// the addresses of one device: counts and bytes are summed, Key is the most
// active address, Label the device name (else the most active row's
// label) and Addresses lists all addresses, most active first. The groups
// are ordered like the list (clients by count, cache-clients by bytes) and
// cut to limit.
func groupTop(items []logs.TopItem, devs map[string]clients.Device, byBytes bool, limit int) []logs.TopItem {
	more := func(a, b logs.TopItem) int {
		if byBytes {
			return cmp.Or(cmp.Compare(b.Bytes, a.Bytes), cmp.Compare(b.Count, a.Count), cmp.Compare(a.Key, b.Key))
		}
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Key, b.Key))
	}
	byKey := map[string][]logs.TopItem{}
	var keys []string
	for _, it := range items {
		k := deviceKey(devs, it.Key)
		if _, ok := byKey[k]; !ok {
			keys = append(keys, k)
		}
		byKey[k] = append(byKey[k], it)
	}
	out := make([]logs.TopItem, 0, len(keys))
	for _, k := range keys {
		rows := byKey[k]
		slices.SortStableFunc(rows, more)
		g := logs.TopItem{Key: rows[0].Key, Addresses: make([]string, 0, len(rows))}
		for _, it := range rows {
			g.Addresses = append(g.Addresses, it.Key)
			g.Count += it.Count
			g.Bytes += it.Bytes
		}
		if g.Label = deviceName(devs, g.Addresses); g.Label == "" {
			for _, it := range rows {
				if it.Label != "" {
					g.Label = it.Label
					break
				}
			}
		}
		out = append(out, g)
	}
	slices.SortFunc(out, more)
	return out[:min(len(out), limit)]
}

// clientSeriesRange is the default range of GET /stats/clients/{key}/series.
const clientSeriesRange = 7 * 24 * time.Hour

// seriesKey parses the key of GET /stats/clients/{key}/series: a canonical
// IP address without zone, "ip:<address>", "client:<positive int>" or
// "mac:<canonical MAC>" (lower-case, colons). It returns the address of an
// address key or the device key.
func seriesKey(key string) (addr netip.Addr, device string, err error) {
	bad := apperr.Invalid("key", "must be an IP address, ip:<address>, client:<id> or mac:<mac>")
	switch {
	case strings.HasPrefix(key, "client:"):
		id := strings.TrimPrefix(key, "client:")
		n, perr := strconv.ParseInt(id, 10, 64)
		if perr != nil || n <= 0 || strconv.FormatInt(n, 10) != id {
			return addr, "", bad
		}
		return addr, key, nil
	case strings.HasPrefix(key, "mac:"):
		mac := strings.TrimPrefix(key, "mac:")
		if m, ok := settings.NormalizeMAC(mac); !ok || m != mac {
			return addr, "", bad
		}
		return addr, key, nil
	}
	s := strings.TrimPrefix(key, "ip:")
	a, perr := netip.ParseAddr(s)
	if perr != nil || a.Zone() != "" || netutil.Canon(a).String() != s {
		return addr, "", bad
	}
	return a, "", nil
}

// logsClientSeries serves the activity of one client per step: an address,
// or the addresses a device had in the range (as ?group=device groups
// them; at most 256, the most recently seen).
func (s *Server) logsClientSeries(w http.ResponseWriter, r *http.Request) error {
	addr, device, err := seriesKey(r.PathValue("key"))
	if err != nil {
		return err
	}
	from, to, err := qRange(r, clientSeriesRange)
	if err != nil {
		return err
	}
	step, err := logsStep(r)
	if err != nil {
		return err
	}
	addrs := []string{}
	if addr.IsValid() {
		addrs = append(addrs, addr.String())
	} else if addrs, err = s.deviceAddresses(r.Context(), device, from, to); err != nil {
		return err
	}
	ser, err := s.d.Logs.ClientSeries(r.Context(), addrs, from, to, step)
	if err != nil {
		return err
	}
	return ok(w, ser)
}

// deviceAddresses returns the addresses of the range that belong to a
// device key, most recently seen first (at most logs.MaxSeriesAddresses).
func (s *Server) deviceAddresses(ctx context.Context, key string, from, to time.Time) ([]string, error) {
	if s.d.Settings != nil && s.d.Settings.Get().Logs.AnonymizeClientIPs {
		return nil, apperr.Invalid("key", "devices cannot be resolved while client addresses are anonymised")
	}
	stats, err := s.d.Logs.ClientStats(ctx, from, to)
	if err != nil {
		return nil, err
	}
	all := make([]string, len(stats))
	for i, st := range stats {
		all[i] = st.ClientIP
	}
	devs, err := s.devices(ctx, all)
	if err != nil || devs == nil {
		return []string{}, err
	}
	slices.SortStableFunc(stats, func(a, b logs.ClientStat) int {
		return cmp.Or(b.LastSeen.Compare(a.LastSeen), cmp.Compare(a.ClientIP, b.ClientIP))
	})
	out := []string{}
	for _, st := range stats {
		if deviceKey(devs, st.ClientIP) == key && len(out) < logs.MaxSeriesAddresses {
			out = append(out, st.ClientIP)
		}
	}
	return out, nil
}
