package clients

import (
	"cmp"
	"net/netip"
	"slices"
)

// snapshot is the immutable identification state built from picache.db.
type snapshot struct {
	groupEnabled  map[int64]bool
	defaultGroups []int64 // groups of unknown clients: [1] if Default is enabled, else []
	byIP          map[netip.Addr]*clientEntry
	cidrs         []cidrEntry // longest prefix first, then highest client ID
	byMAC         map[string]*clientEntry
}

type clientEntry struct {
	id                  int64
	name                string
	groups              []int64 // all memberships (sorted)
	downloadCacheBypass bool
	ignoreLogs          bool
	ignoreStats         bool
}

type cidrEntry struct {
	prefix netip.Prefix
	client *clientEntry
}

func newSnapshot(groups []Group, clients []Client) *snapshot {
	s := &snapshot{
		groupEnabled: make(map[int64]bool, len(groups)),
		byIP:         map[netip.Addr]*clientEntry{},
		byMAC:        map[string]*clientEntry{},
	}
	for _, g := range groups {
		s.groupEnabled[g.ID] = g.Enabled
	}
	s.defaultGroups = []int64{}
	if s.groupEnabled[DefaultGroupID] {
		s.defaultGroups = []int64{DefaultGroupID}
	}
	for _, c := range clients {
		e := &clientEntry{id: c.ID, name: c.Name, groups: c.GroupIDs,
			downloadCacheBypass: c.DownloadCacheBypass, ignoreLogs: c.IgnoreLogs, ignoreStats: c.IgnoreStats}
		for _, raw := range c.Identifiers {
			id, ok := parseIdentifier(raw)
			if !ok {
				continue
			}
			switch id.kind {
			case kindIP:
				s.byIP[id.ip] = e
			case kindCIDR:
				s.cidrs = append(s.cidrs, cidrEntry{prefix: id.prefix, client: e})
			case kindMAC:
				s.byMAC[id.value] = e
			}
		}
	}
	slices.SortFunc(s.cidrs, func(a, b cidrEntry) int {
		if c := cmp.Compare(b.prefix.Bits(), a.prefix.Bits()); c != 0 {
			return c
		}
		return cmp.Compare(b.client.id, a.client.id)
	})
	return s
}

// match finds the configured client for ip: exact IP, then the longest
// matching CIDR (ties: highest client ID), then the MAC address (learned
// MACs: Registry.match).
func (s *snapshot) match(ip netip.Addr, mac string) *clientEntry {
	if c := s.matchIP(ip); c != nil {
		return c
	}
	if mac != "" {
		if c, ok := s.byMAC[mac]; ok {
			return c
		}
	}
	return nil
}

// matchIP finds the configured client for ip by its address alone: exact
// IP, then the longest matching CIDR (ties: highest client ID).
func (s *snapshot) matchIP(ip netip.Addr) *clientEntry {
	if c, ok := s.byIP[ip]; ok {
		return c
	}
	for _, e := range s.cidrs {
		if e.prefix.Contains(ip) {
			return e.client
		}
	}
	return nil
}

// enabledGroups returns the enabled subset of groups (sorted, never nil).
func (s *snapshot) enabledGroups(groups []int64) []int64 {
	out := make([]int64, 0, len(groups))
	for _, g := range groups {
		if s.groupEnabled[g] {
			out = append(out, g)
		}
	}
	return out
}
