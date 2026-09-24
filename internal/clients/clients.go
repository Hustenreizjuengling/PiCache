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
// "clients-seen", table clients_seen), pruned after 30 days; IPv6 entries are
// keyed by MAC when the neighbour table knows it.
//
// Bounds: the identity cache and the seen map hold at most 65 536 entries
// (LRU); PTR lookups for names run in one worker with a de-duplicated queue
// of ≤ 1024 (dropped when full). Seen is called only after ACL and rate-limit
// checks. Group changes do not require a filter recompile (the filter gets
// group IDs per query).
package clients

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
)

var errNotImplemented = errors.New("clients: not implemented")

// DefaultGroupID is the ID of the built-in "Default" group.
const DefaultGroupID int64 = 1

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
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	Identifiers    []string  `json:"identifiers"`
	GroupIDs       []int64   `json:"groupIds"`
	Comment        string    `json:"comment"`
	LanCacheBypass bool      `json:"lanCacheBypass"` // never answer LanCache overrides for this client
	IgnoreLogs     bool      `json:"ignoreLogs"`     // exclude from query log and stats
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// ClientInput creates or updates a client.
type ClientInput struct {
	Name           string   `json:"name"`
	Identifiers    []string `json:"identifiers"`
	GroupIDs       []int64  `json:"groupIds"`
	Comment        string   `json:"comment"`
	LanCacheBypass bool     `json:"lanCacheBypass"`
	IgnoreLogs     bool     `json:"ignoreLogs"`
}

// Identity is the resolved identity of a querying address.
type Identity struct {
	IP             netip.Addr
	ClientID       int64   // 0 if no configured client matched
	Name           string  // configured name, else resolved hostname, else ""
	MAC            string  // if known
	GroupIDs       []int64 // enabled groups only (sorted); may be empty if all its groups are disabled
	LanCacheBypass bool
	IgnoreLogs     bool
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
	log *slog.Logger
}

// New opens the registry: cdb = picache.db (configuration), ldb = logs.db
// (seen data; may be nil when logs are disabled).
func New(ctx context.Context, cdb, ldb *db.DB, log *slog.Logger) (*Registry, error) {
	return &Registry{db: cdb, log: log}, nil
}

// Start runs background refreshes (ARP table every 30 s, hostnames hourly,
// flushing "seen" data every minute, daily pruning). Blocks until ctx is done.
func (r *Registry) Start(ctx context.Context) { <-ctx.Done() }

// SetPTRResolver sets the resolver used for client hostnames.
func (r *Registry) SetPTRResolver(fn PTRResolver) {}

// Identify returns the identity of ip. Hot path; never blocks on I/O.
func (r *Registry) Identify(ip netip.Addr) *Identity {
	return &Identity{IP: ip, GroupIDs: []int64{DefaultGroupID}}
}

// Seen records activity of ip (in memory; flushed periodically).
func (r *Registry) Seen(ip netip.Addr) {}

// DisplayName returns the best display name for ip ("" if none).
func (r *Registry) DisplayName(ip netip.Addr) string { return "" }

// OnChange registers a callback invoked after groups or clients change.
func (r *Registry) OnChange(fn func()) {}

// Groups lists all groups.
func (r *Registry) Groups(ctx context.Context) ([]Group, error) { return nil, errNotImplemented }

// CreateGroup creates a group.
func (r *Registry) CreateGroup(ctx context.Context, in GroupInput) (Group, error) {
	return Group{}, errNotImplemented
}

// UpdateGroup updates a group.
func (r *Registry) UpdateGroup(ctx context.Context, id int64, in GroupInput) (Group, error) {
	return Group{}, errNotImplemented
}

// DeleteGroup deletes a group (not the Default group).
func (r *Registry) DeleteGroup(ctx context.Context, id int64) error { return errNotImplemented }

// Clients lists configured clients.
func (r *Registry) Clients(ctx context.Context) ([]Client, error) { return nil, errNotImplemented }

// CreateClient creates a client.
func (r *Registry) CreateClient(ctx context.Context, in ClientInput) (Client, error) {
	return Client{}, errNotImplemented
}

// UpdateClient updates a client.
func (r *Registry) UpdateClient(ctx context.Context, id int64, in ClientInput) (Client, error) {
	return Client{}, errNotImplemented
}

// DeleteClient deletes a client.
func (r *Registry) DeleteClient(ctx context.Context, id int64) error { return errNotImplemented }

// Known lists addresses seen within the last `within` (0 = 30 days).
func (r *Registry) Known(ctx context.Context, within time.Duration) ([]Known, error) {
	return nil, errNotImplemented
}
