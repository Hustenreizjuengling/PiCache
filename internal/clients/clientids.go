package clients

import (
	"cmp"
	"net/netip"
	"slices"
	"time"

	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// ClientIDs of encrypted DNS (docs/ARCHITECTURE.md 19): a device sends one
// in the SNI of DoT (<ClientID>.<serverName>) or the path of DoH
// (/dns-query/<ClientID>). A configured client lists it as the identifier
// clientid:<ClientID>. It identifies a device only when the device's
// address and MAC identify no configured client (the DNS server decides);
// it never authenticates.

// maxSeenClientIDs bounds the ClientIDs remembered since the start (LRU,
// in memory only).
const maxSeenClientIDs = 1024

// SeenDNSClientID is a ClientID a device sent since the start (GET
// /clients/dns-client-ids). ClientID and Name name the configured client
// that has the ClientID (absent for an unknown one); Address is the last
// source that sent it.
type SeenDNSClientID struct {
	DNSClientID string    `json:"dnsClientId"`
	Address     string    `json:"address"`
	ClientID    int64     `json:"clientId,omitzero"`
	Name        string    `json:"name,omitempty"`
	FirstSeen   time.Time `json:"firstSeen"`
	LastSeen    time.Time `json:"lastSeen"`
	Queries     int64     `json:"queries"`
}

// seenClientID is the in-memory activity of one ClientID.
type seenClientID struct {
	addr        netip.Addr
	first, last time.Time
	queries     int64
}

// IdentifyDNSClientID returns the identity of the configured client that
// has the identifier clientid:<id> (id normalised, without the prefix):
// its enabled groups, name and flags, without an address or MAC. false
// when no client has it. Nothing is cached per ClientID.
func (r *Registry) IdentifyDNSClientID(id string) (*Identity, bool) {
	snap := r.snap.Load()
	c, ok := snap.byClientID[id]
	if !ok || id == "" {
		return nil, false
	}
	return &Identity{ClientID: c.id, Name: c.name, GroupIDs: snap.enabledGroups(c.groups),
		DownloadCacheBypass: c.downloadCacheBypass, IgnoreLogs: c.ignoreLogs, IgnoreStats: c.ignoreStats}, true
}

// SeenDNSClientID records that ip sent the ClientID id (normalised) with a
// query, in memory only (never written to logs.db): the seen entry of ip
// remembers it as its last ClientID, and the ClientID list (at most 1024,
// the least recently seen evicted) counts it. The DNS server calls it
// after Seen or SeenTransient, never for identities with ignoreLogs.
func (r *Registry) SeenDNSClientID(ip netip.Addr, id string) {
	ip = netutil.Canon(ip)
	if !ip.IsValid() || id == "" {
		return
	}
	now := time.Now()
	r.seenMu.Lock()
	defer r.seenMu.Unlock()
	if e, ok := r.seen.peek(ip); ok {
		e.dnsClientID = id
	}
	e, ok := r.dnsIDs.get(id)
	if !ok {
		e = &seenClientID{first: now}
		r.dnsIDs.put(id, e)
	}
	e.addr, e.last = ip, now
	e.queries++
}

// DNSClientIDs lists the ClientIDs seen since the start, newest first (at
// most 1024), with the configured client that has each.
func (r *Registry) DNSClientIDs() []SeenDNSClientID {
	snap := r.snap.Load()
	r.seenMu.Lock()
	out := make([]SeenDNSClientID, 0, r.dnsIDs.len())
	r.dnsIDs.each(func(id string, e *seenClientID) bool {
		row := SeenDNSClientID{DNSClientID: id, Address: e.addr.String(), FirstSeen: e.first.UTC(), LastSeen: e.last.UTC(), Queries: e.queries}
		if c, ok := snap.byClientID[id]; ok {
			row.ClientID, row.Name = c.id, c.name
		}
		out = append(out, row)
		return true
	})
	r.seenMu.Unlock()
	slices.SortStableFunc(out, func(a, b SeenDNSClientID) int {
		if c := b.LastSeen.Compare(a.LastSeen); c != 0 {
			return c
		}
		return cmp.Compare(a.DNSClientID, b.DNSClientID)
	})
	return out
}

// lastClientID returns the last ClientID ip sent since the start ("" if
// none).
func (r *Registry) lastClientID(ip netip.Addr) string {
	r.seenMu.Lock()
	defer r.seenMu.Unlock()
	if e, ok := r.seen.peek(ip); ok {
		return e.dnsClientID
	}
	return ""
}

// clientOfClientID returns the configured client of a ClientID (nil if
// none).
func (snap *snapshot) clientOfClientID(id string) *clientEntry {
	if id == "" {
		return nil
	}
	return snap.byClientID[id]
}
