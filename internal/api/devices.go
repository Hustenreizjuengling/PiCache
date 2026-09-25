package api

import (
	"cmp"
	"context"
	"net/http"
	"net/netip"
	"slices"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/logs"
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
