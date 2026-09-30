// Package clients manages groups and clients and resolves the identity of a
// querying IP (docs/ARCHITECTURE.md 7.1 step 5 and 7.2 "Groups").
//
// Identification order: exact IP → CIDR (longest prefix; ties → highest ID)
// → interface (iface:, only for a transport source: never an address a
// trusted forwarder named or a client a trusted proxy forwarded; for an
// IPv6 link-local source the zone the kernel reported with it) → MAC
// identifier (from the ARP/neighbour table) → learned MAC → (a ClientID,
// decided by the DNS server) → host name (host:, the address's own cached
// name; Identity.ByHost) → default group. Learned MACs carry a client configured by IP or CIDR over to the
// other addresses of the same device: after every neighbour-table read and
// configuration change, every MAC whose neighbour addresses identify
// exactly one configured client by IP or CIDR maps to that client (never
// the default gateway's MAC, and not a MAC with several IPv4 addresses of
// which not all identify that client: proxy ARP or a MAC-rewriting
// repeater), so a device configured by its IPv4 address is recognised when
// it queries over IPv6. Group 1 "Default" always exists and cannot be
// deleted. Only enabled groups are returned in an Identity. Identify is on
// the DNS hot path and must be cheap (cached per IP, invalidated on any
// change). A new on-link address (e.g. a temporary IPv6 address) is not in
// the neighbour table before this machine sends something to it: when
// clients are configured, Identify first makes the kernel resolve it (one
// empty datagram) and waits up to about 30 ms for the entry (prime; at most
// 8 at a time, 20 per second). An on-link source whose MAC is still unknown
// is cached for at most 2 s and asks for an early neighbour-table read (at
// most one extra read per second).
//
// Names (each source only while its switch in clients.nameSources is on):
// the name of PiCache's DHCP lease of the address (SetLeaseNames), else its
// PTR name, else its name in /etc/hosts, else the name of another address
// with the same MAC from these sources (a lease name first, then an IPv4
// address's name, then the most recently resolved one), e.g. the router's
// DHCPv4 name for a device's IPv6 addresses.
//
// Schema (picache.db, component "clients"): table
// client_groups(id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, comment,
// enabled, created_at); migration v1 inserts (1, 'Default'). Other packages
// reference client_groups(id) ON DELETE CASCADE. Clients: client_clients,
// client_identifiers, client_memberships.
// Seen/known addresses are runtime data and live in logs.db (component
// "clients-seen", table clients_seen), pruned after logs.seenRetentionDays
// (default 30); they can be forgotten (ForgetKnown, FlushKnown). Entries are
// keyed by IP address; the MAC column is filled from the neighbour table
// (IPv4 ARP and IPv6 NDP). SeenTransient keeps activity in memory only
// (used while client addresses are anonymised).
//
// Bounds: the identity cache and the seen map hold at most 65 536 entries
// (LRU); PTR lookups for names run in one worker with a de-duplicated queue
// of ≤ 1024 (dropped when full); the name fallback considers at most 64
// addresses per MAC. Seen is called only after ACL and rate-limit checks.
// Group changes do not require a filter recompile (the filter gets group
// IDs per query).
package clients

import (
	"context"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/oui"
)

// DefaultGroupID is the ID of the built-in "Default" group.
const DefaultGroupID int64 = 1

// Bounds and intervals.
const (
	maxCacheEntries  = 65536
	maxSeenEntries   = 65536
	maxNameEntries   = 65536
	maxPTRQueue      = 1024
	maxAddrsPerMAC   = 64 // neighbour addresses per MAC kept for the name fallback
	arpInterval      = 30 * time.Second
	arpEarlyGap      = time.Second     // at most one early neighbour-table read per second
	primeParallel    = 8               // neighbour resolutions a query may wait for at the same time
	primePerSecond   = 20              // neighbour resolutions started per second (spoofed sources)
	shortIdentityTTL = 2 * time.Second // identities of on-link sources without a MAC yet
	seenFlushEvery   = time.Minute
	seenRetention    = 30 * 24 * time.Hour // the default of logs.seenRetentionDays
	nameRefreshEvery = time.Hour
	ptrTimeout       = 3 * time.Second
)

// Group is a policy group. Lists and rules (package filter) reference groups.
// Upstreams or UpstreamPreset (a key of settings.UpstreamPresets; never
// both, never for the Default group) replace the DNS upstreams for the
// group's clients (ARCHITECTURE 7.4, group sets). DeviceClientID marks the
// group of "Only for this device": the client it was created for.
type Group struct {
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	Comment        string    `json:"comment"`
	Enabled        bool      `json:"enabled"`
	CreatedAt      time.Time `json:"createdAt"`
	ClientCount    int       `json:"clientCount"`
	Upstreams      []string  `json:"upstreams"`
	UpstreamPreset string    `json:"upstreamPreset"`
	DeviceClientID *int64    `json:"deviceClientId,omitempty"`
	// legacy are the stored upstreams read with the meaning of a version
	// before 1.0.0 (settings.LegacyUpstream): stored, used.
	legacy [][2]string
}

// GroupInput creates or updates a group. Upstreams nil (absent or null)
// and UpstreamPreset nil keep the stored values on update and mean none on
// create.
type GroupInput struct {
	Name           string   `json:"name"`
	Comment        string   `json:"comment"`
	Enabled        bool     `json:"enabled"`
	Upstreams      []string `json:"upstreams"`
	UpstreamPreset *string  `json:"upstreamPreset"`
}

// Client is a configured client. Identifiers are IPs, CIDRs or MAC addresses
// (normalised: IPs/CIDRs canonical, MACs lower-case colon-separated).
type Client struct {
	ID                  int64     `json:"id"`
	Name                string    `json:"name"`
	Identifiers         []string  `json:"identifiers"`
	GroupIDs            []int64   `json:"groupIds"`
	Comment             string    `json:"comment"`
	DownloadCacheBypass bool      `json:"downloadCacheBypass"` // never give this client the download cache DNS answers
	IgnoreLogs          bool      `json:"ignoreLogs"`          // exclude its raw data: query log, cache requests, SNI events, download sessions, seen
	IgnoreStats         bool      `json:"ignoreStats"`         // exclude it from the DNS and cache statistics
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

// ClientInput creates or updates a client. An empty GroupIDs puts the client
// into the Default group. IgnoreStats absent or null (on create and update)
// takes the value of IgnoreLogs: the meaning of the single flag of 0.11 for
// API clients that do not know the member.
type ClientInput struct {
	Name                string   `json:"name"`
	Identifiers         []string `json:"identifiers"`
	GroupIDs            []int64  `json:"groupIds"`
	Comment             string   `json:"comment"`
	DownloadCacheBypass bool     `json:"downloadCacheBypass"`
	IgnoreLogs          bool     `json:"ignoreLogs"`
	IgnoreStats         *bool    `json:"ignoreStats"`
}

// ignoreStats returns the effective IgnoreStats of the input.
func (in *ClientInput) ignoreStats() bool {
	if in.IgnoreStats == nil {
		return in.IgnoreLogs
	}
	return *in.IgnoreStats
}

// Identity is the resolved identity of a querying address. Identities are
// shared between callers and must be treated as read-only.
type Identity struct {
	IP                  netip.Addr
	ClientID            int64   // 0 if no configured client matched
	Name                string  // configured name, else resolved hostname (of this or another address with the same MAC), else ""
	MAC                 string  // if known
	GroupIDs            []int64 // enabled groups only (sorted); may be empty if all its groups are disabled
	DownloadCacheBypass bool
	IgnoreLogs          bool // no raw data (query log, cache requests, SNI events, sessions, seen)
	IgnoreStats         bool // not counted in the statistics
	// ByHost: the client was found by a host: identifier (the last rule of
	// identification): a known ClientID wins over it.
	ByHost bool
}

// Known is a client address that has been seen recently. DNSClientID is
// the last ClientID the address sent since the start (DoT, DoH); for an
// address that identifies no configured client by itself, ClientID and
// Name come from the client that has that ClientID. Interface, Vendor,
// MACRandomized and WHOIS are computed when the row is read (never
// stored).
type Known struct {
	IP            string     `json:"ip"`
	MAC           string     `json:"mac,omitempty"`
	Hostname      string     `json:"hostname,omitempty"` // from the enabled name sources (of this address, else of another one with the same MAC)
	ClientID      int64      `json:"clientId,omitempty"`
	Name          string     `json:"name,omitempty"`
	DNSClientID   string     `json:"dnsClientId,omitempty"`
	Interface     string     `json:"interface,omitempty"`    // the interface a reply to the address leaves by
	Vendor        string     `json:"vendor,omitempty"`       // of the MAC (IEEE registries)
	MACRandomized bool       `json:"macRandomized,omitzero"` // a locally administered ("private") MAC
	WHOIS         *WhoisInfo `json:"whois,omitempty"`        // the owner of a public source's network (RDAP)
	FirstSeen     time.Time  `json:"firstSeen"`
	LastSeen      time.Time  `json:"lastSeen"`
	Queries       int64      `json:"queries"`
}

// VendorOf returns the vendor of a MAC and whether it is locally
// administered (package oui).
func VendorOf(mac string) (vendor string, randomized bool) {
	if mac == "" {
		return "", false
	}
	return oui.Lookup(mac)
}

// PTRResolver resolves the hostname of a client address (router / local PTR upstreams).
type PTRResolver func(ctx context.Context, ip netip.Addr) (string, error)

// Registry holds groups, clients and the identity cache.
type Registry struct {
	db  *db.DB
	ldb *db.DB // logs.db (seen data); nil = memory only
	log *slog.Logger

	writeMu sync.Mutex // serialises configuration writes with their snapshot reload
	snap    atomic.Pointer[snapshot]
	arp     atomic.Pointer[map[netip.Addr]string] // neighbour IPv4/IPv6 → MAC
	readARP func() map[netip.Addr]string          // neighbour table source (replaced in tests)
	// readNeighbours reads the neighbour table for Neighbours (replaced in tests).
	readNeighbours func() ([]Neighbour, error)
	// gateways returns the default gateways, whose MACs are never learned
	// (replaced in tests).
	gateways func() []netip.Addr
	// onLink reports whether an address is a neighbour that the neighbour
	// table will list once it talked to this machine (nil: no neighbour
	// table on this system).
	onLink  func(netip.Addr) bool
	arpKick chan struct{} // requests an early neighbour-table read
	arpMu   sync.Mutex    // serialises neighbour-table reads that are applied

	// probe makes the kernel resolve the link-layer address of a neighbour
	// (nil: never). See prime.
	probe       func(netip.Addr)
	primeSem    chan struct{}
	primeSecond atomic.Int64 // unix second of primeCount
	primeCount  atomic.Int32

	learnMu sync.Mutex // serialises rebuilds of learned
	learned atomic.Pointer[learnedState]

	cacheMu  sync.Mutex
	cacheGen uint64
	cache    *lru[cacheKey, cachedIdentity]

	// ifaceOf returns the interface a reply to an address leaves by
	// (netutil.InterfaceOf; replaced in tests).
	ifaceOf func(netip.Addr) string
	// zoneIface returns the interface name of the zone of an IPv6
	// link-local source ("" if unknown; nil: iface: never applies to
	// link-local sources, as on systems other than Linux).
	zoneIface func(zone string) string
	// cfg is what the registry reads from the settings (ApplyConfig).
	cfg atomic.Pointer[Config]
	// legacyLogged are the group upstreams read with their pre-1.0.0
	// meaning that are logged already (group name, stored; under writeMu).
	legacyLogged map[[2]string]bool
	// hosts are the names of /etc/hosts (clients.nameSources.hostsFile).
	hosts     atomic.Pointer[hostsTable]
	hostsPath string
	hostsKick chan struct{}
	// whois annotates public sources with their network's owner (RDAP).
	whois *whoisLookup
	// flushMu is held by flush from copying the pending counts until its
	// write returned, and by the forgetting of seen data, so a pending
	// flush never re-creates a forgotten row.
	flushMu   sync.Mutex
	pruneKick chan struct{}
	// namesKick asks seenLoop to drop the host names stored with the seen
	// data after a name source was switched off (clearStoredNames);
	// configured is set by the first ApplyConfig (the start), which never
	// asks for it.
	namesKick  chan struct{}
	configured atomic.Bool
	flushHook  func() // called by flush between copying and writing (tests)

	seenMu    sync.Mutex
	seen      *lru[netip.Addr, *seenEntry]
	dnsIDs    *lru[string, *seenClientID]          // ClientIDs seen since the start (under seenMu)
	seenEvery atomic.Pointer[func() time.Duration] // logs.flushSeconds (nil: seenFlushEvery)

	namesMu sync.Mutex
	names   *lru[netip.Addr, hostName]
	queued  map[netip.Addr]struct{}
	queue   chan netip.Addr
	ptr     atomic.Pointer[PTRResolver]
	// leaseName returns the DHCP lease name of an address (nil: none).
	leaseName atomic.Pointer[LeaseNameFunc]

	cbMu     sync.Mutex
	onChange []func()
}

// cacheKey is an identity cache key: the address and whether iface:
// identifiers apply (a transport source) or not (a client a trusted proxy
// forwarded).
type cacheKey struct {
	ip    netip.Addr
	iface bool
}

// cachedIdentity is an identity cache entry. expires (unix nanoseconds) is
// set for on-link sources whose MAC is not known yet; 0 = until
// invalidated. zone is the zone of the IPv6 link-local source it was
// computed for: the same address on another interface is another device.
type cachedIdentity struct {
	id      *Identity
	expires int64
	zone    string
}

// learnedState is derived from one snapshot and one neighbour table: the
// learned MACs and the neighbour addresses per MAC (name fallback).
type learnedState struct {
	snap  *snapshot               // byMAC is valid for this snapshot only
	byMAC map[string]*clientEntry // learned MAC → client
	addrs map[string][]netip.Addr // MAC → neighbour addresses (≤ maxAddrsPerMAC)
}

// New opens the registry: cdb = picache.db (configuration), ldb = logs.db
// (seen data; may be nil when logs are disabled).
func New(ctx context.Context, cdb, ldb *db.DB, log *slog.Logger) (*Registry, error) {
	if log == nil {
		log = slog.Default()
	}
	r := &Registry{
		db:       cdb,
		ldb:      ldb,
		log:      log.With(slog.String("component", "clients")),
		cache:    newLRU[cacheKey, cachedIdentity](maxCacheEntries),
		seen:     newLRU[netip.Addr, *seenEntry](maxSeenEntries),
		dnsIDs:   newLRU[string, *seenClientID](maxSeenClientIDs),
		names:    newLRU[netip.Addr, hostName](maxNameEntries),
		queued:   make(map[netip.Addr]struct{}),
		queue:    make(chan netip.Addr, maxPTRQueue),
		readARP:  readARP,
		gateways: defaultGateways,
		arpKick:  make(chan struct{}, 1),
		primeSem: make(chan struct{}, primeParallel),

		readNeighbours: readNeighbourTable,
		ifaceOf:        netutil.InterfaceOf,
		hostsPath:      "/etc/hosts",
		hostsKick:      make(chan struct{}, 1),
		pruneKick:      make(chan struct{}, 1),
		namesKick:      make(chan struct{}, 1),
	}
	cfg := DefaultConfig()
	r.cfg.Store(&cfg)
	r.hosts.Store(&hostsTable{})
	r.whois = newWhoisLookup(r)
	if runtime.GOOS == "linux" {
		r.onLink = netutil.OnLink
		r.probe = probeNeighbour
		r.zoneIface = zoneInterface
	}
	empty := map[netip.Addr]string{}
	r.arp.Store(&empty)
	if err := cdb.Migrate(ctx, "clients", migrations); err != nil {
		return nil, err
	}
	if ldb != nil {
		if err := ldb.Migrate(ctx, "clients-seen", seenMigrations); err != nil {
			return nil, err
		}
	}
	if err := r.reload(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// defaultGateways returns the IPv4 and IPv6 default gateways (canonical).
func defaultGateways() []netip.Addr {
	var out []netip.Addr
	if ip, err := netutil.DefaultGatewayIPv4(); err == nil {
		out = append(out, netutil.Canon(ip))
	}
	if ip, err := netutil.DefaultGatewayIPv6(); err == nil {
		out = append(out, netutil.Canon(ip))
	}
	return out
}

// Start runs background refreshes (ARP table every 30 s and on request,
// hostnames hourly, flushing "seen" data every max(1 minute,
// logs.flushSeconds), daily pruning).
// Blocks until ctx is done.
func (r *Registry) Start(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { r.arpLoop(ctx) })
	wg.Go(func() { r.ptrWorker(ctx) })
	wg.Go(func() { r.seenLoop(ctx) })
	wg.Go(func() { r.hostsLoop(ctx) })
	wg.Go(func() { r.whois.run(ctx) })
	wg.Wait()
}

// SetPTRResolver sets the resolver used for client hostnames.
func (r *Registry) SetPTRResolver(fn PTRResolver) {
	if fn == nil {
		r.ptr.Store(nil)
		return
	}
	r.ptr.Store(&fn)
}

// OnChange registers a callback invoked after groups or clients change. It
// runs synchronously in the writing goroutine and must not modify groups or
// clients itself.
func (r *Registry) OnChange(fn func()) {
	r.cbMu.Lock()
	defer r.cbMu.Unlock()
	r.onChange = append(r.onChange, fn)
}

// Identify returns the identity of a transport source ip (iface:
// identifiers apply). An IPv6 link-local ip carries the zone the kernel
// reported with it (UDPAddr/TCPAddr.Zone: the interface it arrived on),
// which names its interface. Hot path: cached; only the first query of a
// new on-link address may wait (about 30 ms at most) for its MAC (prime).
func (r *Registry) Identify(ip netip.Addr) *Identity { return r.identify(ip, true) }

// IdentifyForwarded returns the identity of a client a trusted reverse
// proxy forwarded (DoH on the web listeners): like Identify, but iface:
// identifiers never apply (the address did not arrive by its interface).
func (r *Registry) IdentifyForwarded(ip netip.Addr) *Identity { return r.identify(ip, false) }

func (r *Registry) identify(ip netip.Addr, iface bool) *Identity {
	zone := ""
	if iface {
		zone = netutil.LinkLocalZone(ip)
	}
	ip = netutil.Canon(ip)
	key := cacheKey{ip: ip, iface: iface}
	r.cacheMu.Lock()
	if e, ok := r.cache.get(key); ok && e.zone == zone && (e.expires == 0 || time.Now().UnixNano() < e.expires) {
		r.cacheMu.Unlock()
		return e.id
	}
	gen := r.cacheGen
	r.cacheMu.Unlock()

	id := r.resolve(ip, iface, zone)
	e := cachedIdentity{id: id, zone: zone}
	if id.MAC == "" && r.onLink != nil && r.onLink(ip) {
		if r.prime(ip) {
			id = r.resolve(ip, iface, zone)
			e = cachedIdentity{id: id, zone: zone}
		}
		if id.MAC == "" {
			// Its MAC (and with it a MAC or learned-MAC client) appears
			// with the next neighbour-table read, requested now.
			e.expires = time.Now().Add(shortIdentityTTL).UnixNano()
			r.kickARP()
		}
	}

	r.cacheMu.Lock()
	if r.cacheGen == gen {
		r.cache.put(key, e)
	}
	r.cacheMu.Unlock()
	return id
}

// InterfacesChanged drops the cached identities after the route snapshot
// changed (the interfaces of the addresses may have).
func (r *Registry) InterfacesChanged() { r.invalidate() }

// IdentifyDerived returns the identity of a client behind a trusted
// forwarder (dns.ednsClientTrusted) whose address (ip; invalid if none)
// and/or MAC (mac; "" if none) came from the forwarder's EDNS options.
// With an address, the usual order applies to it (exact IP → CIDR → MAC
// identifier → learned MAC → Default), the MAC being mac, else the
// neighbour table's MAC of the address (read only: no probe, no early
// read). With only a MAC the address steps are skipped (the source is the
// forwarder, not the client): MAC identifier → learned MAC → Default, and
// the identity has no address. iface: never applies (the address did not
// arrive by its interface). Nothing is cached or learned: EDNS MACs never
// enter the neighbour table, the learned MACs or the device data.
func (r *Registry) IdentifyDerived(ip netip.Addr, mac string) *Identity {
	snap := r.snap.Load()
	id := &Identity{}
	var c *clientEntry
	if ip.IsValid() {
		ip = netutil.Canon(ip)
		if mac == "" {
			mac = (*r.arp.Load())[ip]
		}
		id.IP, id.MAC = ip, mac
		c = r.matchWith(snap, ip, mac, false)
	} else if mac != "" {
		id.MAC = mac
		if c = snap.byMAC[mac]; c == nil {
			if l := r.learned.Load(); l != nil && l.snap == snap {
				c = l.byMAC[mac]
			}
		}
	}
	if c != nil {
		id.ClientID, id.Name = c.id, c.name
		id.GroupIDs = snap.enabledGroups(c.groups)
		id.DownloadCacheBypass, id.IgnoreLogs, id.IgnoreStats = c.downloadCacheBypass, c.ignoreLogs, c.ignoreStats
	} else {
		id.GroupIDs = snap.defaultGroups
	}
	if id.Name == "" {
		if ip.IsValid() {
			id.Name = r.name(ip, mac)
		} else if l := r.learned.Load(); l != nil {
			id.Name = r.macName(l.addrs[mac], netip.Addr{})
		}
	}
	return id
}

// DisplayName returns the best display name for ip ("" if none).
func (r *Registry) DisplayName(ip netip.Addr) string { return r.Identify(ip).Name }

// Describe returns the configured client that ip or mac identifies (0 and
// "" if none; learned MACs included) and the resolved host name of ip,
// else of another address with the same MAC ("" if unknown). Unlike
// Identify it takes the MAC from the caller (a fresh neighbour table read)
// and caches nothing.
func (r *Registry) Describe(ip netip.Addr, mac string) (clientID int64, name, hostname string) {
	ip = netutil.Canon(ip)
	if c := r.match(r.snap.Load(), ip, mac); c != nil {
		clientID, name = c.id, c.name
	}
	return clientID, name, r.name(ip, mac)
}

// resolve computes an identity from the current snapshot, ARP table and
// hostname cache (iface: whether iface: identifiers apply; zone: the zone
// of an IPv6 link-local transport source). host: identifiers come last
// (ByHost).
func (r *Registry) resolve(ip netip.Addr, iface bool, zone string) *Identity {
	snap := r.snap.Load()
	mac := (*r.arp.Load())[ip]
	id := &Identity{IP: ip, MAC: mac}
	var c *clientEntry
	if iface && len(snap.byIface) > 0 && ip.Is6() && ip.IsLinkLocalUnicast() {
		c = r.matchLinkLocal(snap, ip, mac, zone)
	} else {
		c = r.matchWith(snap, ip, mac, iface)
	}
	if c == nil {
		if c = r.matchHost(snap, ip); c != nil {
			id.ByHost = true
		}
	}
	if c != nil {
		id.ClientID = c.id
		id.Name = c.name
		id.GroupIDs = snap.enabledGroups(c.groups)
		id.DownloadCacheBypass = c.downloadCacheBypass
		id.IgnoreLogs = c.ignoreLogs
		id.IgnoreStats = c.ignoreStats
	} else {
		id.GroupIDs = snap.defaultGroups
	}
	if id.Name == "" {
		id.Name = r.name(ip, mac)
	}
	return id
}

// match finds the configured client of a source address ip: exact IP,
// CIDR, interface, MAC identifier, then learned MAC.
func (r *Registry) match(snap *snapshot, ip netip.Addr, mac string) *clientEntry {
	return r.matchWith(snap, ip, mac, true)
}

// matchWith is match with the interface rule only when iface is true.
func (r *Registry) matchWith(snap *snapshot, ip netip.Addr, mac string, iface bool) *clientEntry {
	name := ""
	if iface && len(snap.byIface) > 0 && r.ifaceOf != nil {
		name = r.ifaceOf(ip)
	}
	return r.matchOn(snap, ip, name, mac)
}

// matchOn finds the client of ip on the interface name ("" = none or not
// applicable) with the MAC mac: exact IP, CIDR, interface, MAC identifier,
// then learned MAC.
func (r *Registry) matchOn(snap *snapshot, ip netip.Addr, name, mac string) *clientEntry {
	if c := snap.match(ip, name, mac); c != nil || mac == "" {
		return c
	}
	if l := r.learned.Load(); l != nil && l.snap == snap {
		return l.byMAC[mac]
	}
	return nil
}

// matchLinkLocal is match for an IPv6 link-local transport source while
// iface: identifiers exist. Every interface has fe80::/64, so the routes
// cannot tell its interface; the zone the kernel reported with the source
// can (the interface it arrived on; a reply can only leave by it). A
// link-local source of unknown interface may be on an iface: network,
// e.g. a guest device that copied a trusted device's MAC: only its address
// identifies it, never its MAC.
func (r *Registry) matchLinkLocal(snap *snapshot, ip netip.Addr, mac, zone string) *clientEntry {
	name := ""
	if r.zoneIface != nil {
		name = r.zoneIface(zone)
	}
	if name == "" {
		return snap.matchIP(ip)
	}
	return r.matchOn(snap, ip, name, mac)
}

// zoneInterface returns the interface name of an IPv6 zone: Go reports the
// name, or the index when its interface list was not current yet ("" if
// unknown).
func zoneInterface(zone string) string {
	if zone == "" {
		return ""
	}
	if n, err := strconv.Atoi(zone); err == nil {
		if ifc, err := net.InterfaceByIndex(n); err == nil {
			return ifc.Name
		}
		return ""
	}
	return zone
}

// rebuildLearned derives the learned MACs and the addresses per MAC from
// the current snapshot and neighbour table. It reports whether the learned
// MACs changed.
func (r *Registry) rebuildLearned() bool {
	r.learnMu.Lock()
	defer r.learnMu.Unlock()
	snap := r.snap.Load()
	arp := *r.arp.Load()
	st := &learnedState{snap: snap, byMAC: map[string]*clientEntry{}, addrs: map[string][]netip.Addr{}}
	never := map[string]bool{} // the gateways' MACs
	for _, gw := range r.gateways() {
		if mac := arp[netutil.Canon(gw)]; mac != "" {
			never[mac] = true
		}
	}
	type macState struct {
		client        *clientEntry
		ambiguous     bool
		v4, v4Matched int
	}
	macs := map[string]*macState{}
	for ip, mac := range arp {
		if len(st.addrs[mac]) < maxAddrsPerMAC {
			st.addrs[mac] = append(st.addrs[mac], ip)
		}
		if never[mac] || snap == nil {
			continue
		}
		ms := macs[mac]
		if ms == nil {
			ms = &macState{}
			macs[mac] = ms
		}
		c := snap.matchIP(ip)
		if ip.Is4() {
			ms.v4++
			if c != nil {
				ms.v4Matched++
			}
		}
		if c == nil {
			continue
		}
		if ms.client != nil && ms.client.id != c.id {
			ms.ambiguous = true
		}
		ms.client = c
	}
	for mac, ms := range macs {
		// Several IPv4 addresses behind one MAC that do not all belong to
		// the client are other devices (proxy ARP, MAC-rewriting repeater).
		if ms.client != nil && !ms.ambiguous && (ms.v4 <= 1 || ms.v4Matched == ms.v4) {
			st.byMAC[mac] = ms.client
		}
	}
	old := r.learned.Swap(st)
	return old == nil || old.snap != snap ||
		!maps.EqualFunc(old.byMAC, st.byMAC, func(a, b *clientEntry) bool { return a.id == b.id })
}

// primeWaits are the pauses before the neighbour-table reads of prime
// (about 30 ms in total; a LAN neighbour answers within a millisecond).
var primeWaits = []time.Duration{2 * time.Millisecond, 4 * time.Millisecond, 8 * time.Millisecond, 16 * time.Millisecond}

// prime resolves the MAC of a new on-link address before its first query
// is answered: a device's new (e.g. temporary IPv6) address is not in the
// neighbour table yet, because this machine never sent anything to it, so
// without this its first query would get the Default group. It sends one
// empty datagram to the address (the kernel then resolves its link-layer
// address) and waits briefly for the entry. Only when a MAC can decide the
// client (some client is configured: by MAC, or by IP or CIDR whose MAC the
// same read learns), not for link-local addresses, at most primeParallel at a time and primePerSecond per
// second. It reports whether ip has a MAC now.
func (r *Registry) prime(ip netip.Addr) bool {
	if r.probe == nil || ip.IsLinkLocalUnicast() || !r.clientsConfigured() || !r.primeAllowed() {
		return false
	}
	select {
	case r.primeSem <- struct{}{}:
		defer func() { <-r.primeSem }()
	default:
		return false
	}
	r.probe(ip)
	for _, d := range primeWaits {
		time.Sleep(d)
		if r.refreshARPFor(ip) {
			return true
		}
	}
	return false
}

// clientsConfigured reports whether any client is configured, so that a
// MAC can decide the client of an address (a MAC identifier, or a learned
// MAC of a client configured by IP or CIDR).
func (r *Registry) clientsConfigured() bool {
	snap := r.snap.Load()
	return snap != nil && (len(snap.byIP) > 0 || len(snap.cidrs) > 0 || len(snap.byMAC) > 0)
}

// primeAllowed counts a neighbour resolution against primePerSecond.
func (r *Registry) primeAllowed() bool {
	now := time.Now().Unix()
	if sec := r.primeSecond.Load(); sec != now && r.primeSecond.CompareAndSwap(sec, now) {
		r.primeCount.Store(0)
	}
	return r.primeCount.Add(1) <= primePerSecond
}

// kickARP requests an early neighbour-table read (non-blocking; requests
// that arrive while one is pending are merged).
func (r *Registry) kickARP() {
	select {
	case r.arpKick <- struct{}{}:
	default:
	}
}

// invalidate drops all cached identities (after configuration, ARP or
// hostname changes).
func (r *Registry) invalidate() {
	r.cacheMu.Lock()
	r.cacheGen++
	r.cache.clear()
	r.cacheMu.Unlock()
}

// changed invalidates the identity cache and notifies the OnChange
// listeners after a committed and reloaded configuration change.
func (r *Registry) changed() {
	r.invalidate()
	r.cbMu.Lock()
	cbs := append([]func(){}, r.onChange...)
	r.cbMu.Unlock()
	for _, fn := range cbs {
		fn()
	}
}
