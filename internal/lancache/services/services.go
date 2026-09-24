// Package services manages the LanCache service catalogue: the
// uklans/cache-domains source, enable/disable state, custom services and
// hosts, the anchored host matchers used by DNS/proxy/SNI, the special
// pass-through path rules, and content grouping/labels
// (docs/ARCHITECTURE.md 8.1, 8.2 step 6, 8.4).
package services

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

var errNotImplemented = errors.New("services: not implemented")

// SteamUserAgentSuffix identifies Steam CDN requests regardless of Host.
const SteamUserAgentSuffix = "Valve/Steam HTTP Client 1.0"

// SteamTrigger is the name Steam resolves to detect a LanCache.
const SteamTrigger = "lancache.steamcontent.com"

// Service is one LanCache service.
type Service struct {
	ID           string   `json:"id"` // cache-domains "name", or "custom-<slug>"
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Notes        string   `json:"notes,omitempty"`
	MixedContent bool     `json:"mixedContent"`
	Custom       bool     `json:"custom"`
	Enabled      bool     `json:"enabled"`
	Domains      []string `json:"domains"`      // effective host patterns (source + extra)
	ExtraDomains []string `json:"extraDomains"` // user-added host patterns
	DomainCount  int      `json:"domainCount"`
}

// ServiceInput creates or updates a custom service.
type ServiceInput struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Domains     []string `json:"domains"` // exact hosts or *.suffix
}

// SourceStatus describes the cache-domains source.
type SourceStatus struct {
	Source       string    `json:"source"`
	LastFetched  time.Time `json:"lastFetched,omitzero"` // last successful fetch
	LastAttempt  time.Time `json:"lastAttempt,omitzero"`
	Error        string    `json:"error,omitempty"`
	ServiceCount int       `json:"serviceCount"`
	DomainCount  int       `json:"domainCount"`
	Ready        bool      `json:"ready"` // a snapshot is loaded
}

// Group is the content group of a request (what was downloaded).
type Group struct {
	Key     string `json:"key"`     // e.g. "steam:depot:228990", "blizzard:wow"
	Label   string `json:"label"`   // default display label
	Version string `json:"version"` // e.g. Steam manifest id, package version
}

// Registry is safe for concurrent use; matchers are immutable snapshots.
type Registry struct {
	db  *db.DB
	set *settings.Store
	log *slog.Logger
}

// New creates the registry. fetch downloads cache-domains (bypass resolver);
// dir holds the snapshot.
func New(ctx context.Context, d *db.DB, set *settings.Store, fetch *http.Client, dir string, log *slog.Logger) (*Registry, error) {
	return &Registry{db: d, set: set, log: log}, nil
}

// Start loads the snapshot, fetches if missing/stale and refreshes periodically.
func (r *Registry) Start(ctx context.Context) {}

// MatchDNS returns the enabled service whose host patterns match qname
// (lower-case, no trailing dot). The Steam trigger matches when steam is enabled.
func (r *Registry) MatchDNS(qname string) (serviceID string, ok bool) { return "", false }

// Classify maps an HTTP request to a service. User-Agent ending in
// SteamUserAgentSuffix → steam. known=false means the host belongs to no
// service (the proxy must refuse it).
func (r *Registry) Classify(host, userAgent string) (serviceID string, enabled, known bool) {
	return "", false, false
}

// SNIAllowed reports whether sni belongs to an enabled service.
func (r *Registry) SNIAllowed(sni string) (serviceID string, ok bool) { return "", false }

// Services lists all services with effective state.
func (r *Registry) Services(ctx context.Context) ([]Service, error) { return nil, errNotImplemented }

// Service returns one service.
func (r *Registry) Service(ctx context.Context, id string) (Service, error) {
	return Service{}, errNotImplemented
}

// SetEnabled enables or disables a service (persisted in settings.LanCache.DisabledServices).
func (r *Registry) SetEnabled(ctx context.Context, id string, enabled bool) error {
	return errNotImplemented
}

// SetExtraDomains replaces the user-added hosts of a service.
func (r *Registry) SetExtraDomains(ctx context.Context, id string, domains []string) error {
	return errNotImplemented
}

// CreateCustom adds a custom service.
func (r *Registry) CreateCustom(ctx context.Context, in ServiceInput) (Service, error) {
	return Service{}, errNotImplemented
}

// UpdateCustom updates a custom service.
func (r *Registry) UpdateCustom(ctx context.Context, id string, in ServiceInput) (Service, error) {
	return Service{}, errNotImplemented
}

// DeleteCustom deletes a custom service.
func (r *Registry) DeleteCustom(ctx context.Context, id string) error { return errNotImplemented }

// Refresh fetches the cache-domains source now.
func (r *Registry) Refresh(ctx context.Context) error { return errNotImplemented }

// Status returns the source status.
func (r *Registry) Status() SourceStatus { return SourceStatus{} }

// Label returns the display label for a group key: user override, cached
// name (e.g. Steam app name if online lookup is enabled), else the rule
// default. Never blocks on the network; unknown Steam depots trigger an
// asynchronous lookup when settings.LanCache.SteamNameLookup is on.
func (r *Registry) Label(groupKey string) string { return groupKey }

// Labels resolves several keys at once.
func (r *Registry) Labels(keys []string) map[string]string {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = r.Label(k)
	}
	return out
}

// SetLabel stores a user label override for a group key ("" removes it).
func (r *Registry) SetLabel(ctx context.Context, groupKey, label string) error {
	return errNotImplemented
}

// GroupFor derives the content group of a request (pure function, rules in
// ARCHITECTURE 8.4). path must not contain the query string.
func GroupFor(service, host, path string) Group {
	return Group{Key: service + ":" + host, Label: service + " · " + host}
}

// IsBypassPath reports whether a request path must be passed through
// uncached (ARCHITECTURE 8.2 step 6).
func IsBypassPath(path string) bool { return false }
