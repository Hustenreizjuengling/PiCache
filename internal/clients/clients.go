// Package clients manages groups and clients and resolves the identity of a
// querying IP (docs/ARCHITECTURE.md 7.1 step 5 and 7.2 "Groups").
//
// Identification order: exact IP → CIDR (longest prefix; ties → highest ID)
// → MAC (from the ARP/neighbour table) → default group. Group 1 "Default"
// always exists and cannot be deleted. Only enabled groups are returned in an
// Identity. Identify is on the DNS hot path and must be cheap (cached per IP,
// invalidated on any change).
//
// Schema (picache.db, component "clients"): table
// client_groups(id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, comment,
// enabled, created_at); migration v1 inserts (1, 'Default'). Other packages
// reference client_groups(id) ON DELETE CASCADE. Clients: client_clients,
// client_identifiers, client_memberships.
// Seen/known addresses are runtime data and live in logs.db (component
// "clients-seen", table clients_seen), pruned after 30 days. Entries are
// keyed by IP address; the MAC column is filled from the neighbour table
// (IPv4 ARP and IPv6 NDP). SeenTransient keeps activity in memory only
// (used while client addresses are anonymised).
//
// Bounds: the identity cache and the seen map hold at most 65 536 entries
// (LRU); PTR lookups for names run in one worker with a de-duplicated queue
// of ≤ 1024 (dropped when full). Seen is called only after ACL and rate-limit
// checks. Group changes do not require a filter recompile (the filter gets
// group IDs per query).
package clients

import (
	"context"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// DefaultGroupID is the ID of the built-in "Default" group.
const DefaultGroupID int64 = 1

// Bounds and intervals.
const (
	maxCacheEntries  = 65536
	maxSeenEntries   = 65536
	maxNameEntries   = 65536
	maxPTRQueue      = 1024
	arpInterval      = 30 * time.Second
	seenFlushEvery   = time.Minute
	seenRetention    = 30 * 24 * time.Hour
	nameRefreshEvery = time.Hour
	ptrTimeout       = 3 * time.Second
)

// Group is a policy group. Lists and rules (package filter) reference groups.
type Group struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Comment     string    `json:"comment"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"createdAt"`
	ClientCount int       `json:"clientCount"`
}

// GroupInput creates or updates a group.
type GroupInput struct {
	Name    string `json:"name"`
	Comment string `json:"comment"`
	Enabled bool   `json:"enabled"`
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
	IgnoreLogs          bool      `json:"ignoreLogs"`          // exclude from query log and stats
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

// ClientInput creates or updates a client. An empty GroupIDs puts the client
// into the Default group.
type ClientInput struct {
	Name                string   `json:"name"`
	Identifiers         []string `json:"identifiers"`
	GroupIDs            []int64  `json:"groupIds"`
	Comment             string   `json:"comment"`
	DownloadCacheBypass bool     `json:"downloadCacheBypass"`
	IgnoreLogs          bool     `json:"ignoreLogs"`
}

// Identity is the resolved identity of a querying address. Identities are
// shared between callers and must be treated as read-only.
type Identity struct {
	IP                  netip.Addr
	ClientID            int64   // 0 if no configured client matched
	Name                string  // configured name, else resolved hostname, else ""
	MAC                 string  // if known
	GroupIDs            []int64 // enabled groups only (sorted); may be empty if all its groups are disabled
	DownloadCacheBypass bool
	IgnoreLogs          bool
}

// Known is a client address that has been seen recently.
type Known struct {
	IP        string    `json:"ip"`
	MAC       string    `json:"mac,omitempty"`
	Hostname  string    `json:"hostname,omitempty"` // from PTR
	ClientID  int64     `json:"clientId,omitempty"`
	Name      string    `json:"name,omitempty"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
	Queries   int64     `json:"queries"`
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

	cacheMu  sync.Mutex
	cacheGen uint64
	cache    *lru[netip.Addr, *Identity]

	seenMu sync.Mutex
	seen   *lru[netip.Addr, *seenEntry]

	namesMu sync.Mutex
	names   *lru[netip.Addr, hostName]
	queued  map[netip.Addr]struct{}
	queue   chan netip.Addr
	ptr     atomic.Pointer[PTRResolver]

	cbMu     sync.Mutex
	onChange []func()
}

// New opens the registry: cdb = picache.db (configuration), ldb = logs.db
// (seen data; may be nil when logs are disabled).
func New(ctx context.Context, cdb, ldb *db.DB, log *slog.Logger) (*Registry, error) {
	if log == nil {
		log = slog.Default()
	}
	r := &Registry{
		db:      cdb,
		ldb:     ldb,
		log:     log.With(slog.String("component", "clients")),
		cache:   newLRU[netip.Addr, *Identity](maxCacheEntries),
		seen:    newLRU[netip.Addr, *seenEntry](maxSeenEntries),
		names:   newLRU[netip.Addr, hostName](maxNameEntries),
		queued:  make(map[netip.Addr]struct{}),
		queue:   make(chan netip.Addr, maxPTRQueue),
		readARP: readARP,

		readNeighbours: readNeighbourTable,
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

// Start runs background refreshes (ARP table every 30 s, hostnames hourly,
// flushing "seen" data every minute, daily pruning). Blocks until ctx is done.
func (r *Registry) Start(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { r.arpLoop(ctx) })
	wg.Go(func() { r.ptrWorker(ctx) })
	wg.Go(func() { r.seenLoop(ctx) })
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

// Identify returns the identity of ip. Hot path; never blocks on I/O.
func (r *Registry) Identify(ip netip.Addr) *Identity {
	ip = netutil.Canon(ip)
	r.cacheMu.Lock()
	if id, ok := r.cache.get(ip); ok {
		r.cacheMu.Unlock()
		return id
	}
	gen := r.cacheGen
	r.cacheMu.Unlock()

	id := r.resolve(ip)

	r.cacheMu.Lock()
	if r.cacheGen == gen {
		r.cache.put(ip, id)
	}
	r.cacheMu.Unlock()
	return id
}

// DisplayName returns the best display name for ip ("" if none).
func (r *Registry) DisplayName(ip netip.Addr) string { return r.Identify(ip).Name }

// Describe returns the configured client that ip or mac identifies (0 and
// "" if none) and the resolved host name of ip ("" if unknown). Unlike
// Identify it takes the MAC from the caller (a fresh neighbour table read)
// and caches nothing.
func (r *Registry) Describe(ip netip.Addr, mac string) (clientID int64, name, hostname string) {
	ip = netutil.Canon(ip)
	if c := r.snap.Load().match(ip, mac); c != nil {
		clientID, name = c.id, c.name
	}
	return clientID, name, r.hostname(ip)
}

// resolve computes an identity from the current snapshot, ARP table and
// hostname cache.
func (r *Registry) resolve(ip netip.Addr) *Identity {
	snap := r.snap.Load()
	mac := (*r.arp.Load())[ip]
	id := &Identity{IP: ip, MAC: mac}
	c := snap.match(ip, mac)
	if c != nil {
		id.ClientID = c.id
		id.Name = c.name
		id.GroupIDs = snap.enabledGroups(c.groups)
		id.DownloadCacheBypass = c.downloadCacheBypass
		id.IgnoreLogs = c.ignoreLogs
	} else {
		id.GroupIDs = snap.defaultGroups
	}
	if id.Name == "" {
		id.Name = r.hostname(ip)
	}
	return id
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
