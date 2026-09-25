// Package settings holds all runtime settings as one typed document stored in
// picache.db. Components read an immutable snapshot with Get and subscribe to
// changes; the API edits settings with Update, which validates, persists and
// notifies atomically.
package settings

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
)

// All is the complete settings document. Treat values returned by Get as
// read-only: slices and maps are shared between snapshots.
type All struct {
	DNS           DNS           `json:"dns"`
	Filter        Filter        `json:"filter"`
	DownloadCache DownloadCache `json:"downloadCache"`
	Cache         Cache         `json:"cache"`
	Logs          Logs          `json:"logs"`
	Web           Web           `json:"web"`
	Updates       Updates       `json:"updates"`
	Backups       Backups       `json:"backups"`
	DHCP          DHCP          `json:"dhcp"`
}

// DNS configures the resolver side.
type DNS struct {
	// Upstreams: "https://host/dns-query", "tls://host[:port]", "udp://ip[:port]",
	// "tcp://ip[:port]" or plain "ip[:port]".
	Upstreams         []string `json:"upstreams"`
	Bootstrap         []string `json:"bootstrap"`    // plain IPs, used only to resolve upstream hostnames
	UpstreamMode      string   `json:"upstreamMode"` // load_balance | parallel | strict
	UpstreamTimeoutMs int      `json:"upstreamTimeoutMs"`
	LocalPTRUpstreams []string `json:"localPtrUpstreams"` // resolvers for private reverse zones (e.g. the router)
	LocalDomain       string   `json:"localDomain"`       // e.g. "lan" or "fritz.box"; never sent to public upstreams
	ServerNames       []string `json:"serverNames"`       // names answered with this server's addresses
	// RouterResolver answers private reverse zones, the local domain,
	// home.arpa and resolv.conf search domains when no forwarder or local PTR
	// upstream covers them: "auto" = the IPv4 default gateway, else the
	// IPv6 one (only if it answers DNS), "" = off, or an explicit IP.
	RouterResolver string `json:"routerResolver"`

	AllowedNetworks  []string `json:"allowedNetworks"`  // extra client CIDRs beyond the private defaults
	AllowAllNetworks bool     `json:"allowAllNetworks"` // DANGEROUS: open resolver
	// TrustConnectedNetworks also allows every network this machine is
	// connected to, public ones included (netutil.ConnectedSubnets; follows
	// prefix changes). Off by default: on a cloud server the on-link
	// network can contain other tenants.
	TrustConnectedNetworks bool     `json:"trustConnectedNetworks"`
	RateLimitQPS           int      `json:"rateLimitQps"` // per client key (netutil.ClientKey: a device address; public IPv6 per /64); 0 disables
	RateLimitBurst         int      `json:"rateLimitBurst"`
	RateLimitExempt        []string `json:"rateLimitExempt"` // CIDRs (loopback, router and forwarder targets are exempt automatically)
	RefuseANY              bool     `json:"refuseAny"`

	CacheEnabled        bool   `json:"cacheEnabled"`
	CacheSize           int    `json:"cacheSize"` // entries
	CacheMinTTL         uint32 `json:"cacheMinTtl"`
	CacheMaxTTL         uint32 `json:"cacheMaxTtl"` // 0 = no cap
	ServeStale          bool   `json:"serveStale"`
	ServeStaleMaxAgeSec int    `json:"serveStaleMaxAgeSec"`
	DNSSEC              bool   `json:"dnssec"` // set DO upstream and pass AD through (no local validation)

	// DisableAAAA answers AAAA queries that would be forwarded with NODATA
	// and removes ipv6hint from forwarded HTTPS/SVCB answers (networks with
	// broken IPv6). Local records and this server's names are not affected.
	DisableAAAA bool `json:"disableAAAA"`
	// DNS64 synthesises AAAA answers for IPv4-only names (NAT64 networks).
	// It cannot be combined with DisableAAAA.
	DNS64 DNS64 `json:"dns64"`
}

// DNS64 configures AAAA synthesis (RFC 6147) for NAT64 networks.
type DNS64 struct {
	Enabled bool   `json:"enabled"`
	Prefix  string `json:"prefix"` // an IPv6 /96 (default the well-known prefix 64:ff9b::/96, RFC 6052)
}

// DefaultDNS64Prefix is the NAT64 well-known prefix (RFC 6052).
const DefaultDNS64Prefix = "64:ff9b::/96"

// DNS64Prefix returns the configured DNS64 prefix (invalid if it does not
// parse; Validate rejects that earlier).
func (d *DNS) DNS64Prefix() netip.Prefix {
	p, err := netip.ParsePrefix(d.DNS64.Prefix)
	if err != nil {
		return netip.Prefix{}
	}
	return p.Masked()
}

// Filter configures blocking.
type Filter struct {
	Enabled                 bool       `json:"enabled"`              // false = blocking disabled until re-enabled
	PausedUntil             *time.Time `json:"pausedUntil,omitzero"` // timed pause (blocking off until then)
	BlockingMode            string     `json:"blockingMode"`         // null | nxdomain | nodata | refused | custom_ip
	BlockingIPv4            string     `json:"blockingIpv4"`
	BlockingIPv6            string     `json:"blockingIpv6"`
	BlockedTTL              uint32     `json:"blockedTtl"`
	CNAMEInspection         bool       `json:"cnameInspection"`
	UpdateIntervalHours     int        `json:"updateIntervalHours"` // 0 = manual only
	BlockMozillaCanary      bool       `json:"blockMozillaCanary"`
	BlockICloudPrivateRelay bool       `json:"blockIcloudPrivateRelay"`
}

// DownloadCache configures the download cache: its DNS answers and the proxy
// front ends.
type DownloadCache struct {
	Enabled               bool     `json:"enabled"`   // off by default; enabled in the UI after checking IP and storage
	CacheIPv4             []string `json:"cacheIpv4"` // RFC 1918 only; empty = auto-detect
	CacheIPv6             []string `json:"cacheIpv6"` // ULA (fc00::/7) only; empty = AAAA answered with NODATA
	DNSTTL                uint32   `json:"dnsTtl"`
	DomainsSource         string   `json:"domainsSource"` // base URL of cache-domains (raw, https)
	UpdateIntervalHours   int      `json:"updateIntervalHours"`
	DisabledServices      []string `json:"disabledServices"`
	NocacheClients        []string `json:"nocacheClients"`        // CIDRs whose ?nocache=1 is honoured (default: none)
	AllowPrivateUpstreams bool     `json:"allowPrivateUpstreams"` // DANGEROUS: disables the SSRF guard
}

// Cache configures the slice store and retention.
type Cache struct {
	SliceSizeBytes     int64  `json:"sliceSizeBytes"`     // applies to newly created stores
	MaxSizeBytes       int64  `json:"maxSizeBytes"`       // 0 = no limit (bounded by free space)
	MinFreeBytes       int64  `json:"minFreeBytes"`       // effective: min(this, 10 % of the filesystem), ≥ 2 GiB if shared with the data dir
	MaxAgeDays         int    `json:"maxAgeDays"`         // inactive retention
	ReadAheadSlices    int    `json:"readAheadSlices"`    // per request
	MaxConcurrentFills int    `json:"maxConcurrentFills"` // global; × slice size ≤ 1 GiB
	MaxFillsPerClient  int    `json:"maxFillsPerClient"`  // per client key, incl. read-ahead
	ActiveStoreID      string `json:"activeStoreId"`      // storage target id; "local" = built-in (change via the storage API)
}

// Logs configures retention and privacy.
type Logs struct {
	QueryLogEnabled        bool `json:"queryLogEnabled"` // false: no query rows, statistics still counted
	QueryLogRetentionHours int  `json:"queryLogRetentionHours"`
	CacheLogRetentionHours int  `json:"cacheLogRetentionHours"`
	SessionRetentionDays   int  `json:"sessionRetentionDays"`
	StatsRetentionDays     int  `json:"statsRetentionDays"`
	AnonymizeClientIPs     bool `json:"anonymizeClientIps"`
	MaxDBSizeMiB           int  `json:"maxDbSizeMiB"` // logs.db cap; raw events, sessions and hourly top lists are trimmed proportionally
}

// Web configures the UI/API.
type Web struct {
	SessionIdleMinutes int      `json:"sessionIdleMinutes"`
	SessionMaxHours    int      `json:"sessionMaxHours"`
	AllowedHosts       []string `json:"allowedHosts"` // extra Host names (reverse proxy, custom DNS name)
	RedirectToHTTPS    bool     `json:"redirectToHttps"`
	MetricsEnabled     bool     `json:"metricsEnabled"`
	Language           string   `json:"language"` // "" = browser default, "en", "de"
}

// Updates configures the release check (docs/ARCHITECTURE.md 14.3). Installing
// an update always needs an admin action.
type Updates struct {
	CheckEnabled       bool `json:"checkEnabled"`       // check GitHub for a new release every day
	IncludePrereleases bool `json:"includePrereleases"` // offer pre-releases (vX.Y.Z-rc.N) too
}

// Backups configures scheduled backups of picache.db (docs/ARCHITECTURE.md
// 15.2). The files hold the same content as a backup downloaded in the UI.
type Backups struct {
	Enabled  bool   `json:"enabled"`
	Schedule string `json:"schedule"` // daily | weekly
	Time     string `json:"time"`     // HH:MM, local time of the host
	Weekday  int    `json:"weekday"`  // weekly only: 0 = Sunday … 6 = Saturday
	Keep     int    `json:"keep"`     // scheduled backups kept in the destination (1..90)
	// Destination is "local" (<data>/backups/scheduled) or the id of a
	// storage target (<its store root>/picache-backups); the API checks
	// that the target exists.
	Destination    string `json:"destination"`
	IncludeSecrets bool   `json:"includeSecrets"` // keep sealed NAS and notification secrets
}

// BackupsLocal is the Backups.Destination of the data directory.
const BackupsLocal = "local"

// DHCP configures the optional DHCP server (docs/ARCHITECTURE.md 18). It
// serves only when the process opened the DHCP sockets (PICACHE_DHCP) and
// no safety gate blocks it. Validate checks the form of the values; the
// interface, the range and the router are checked against the live
// interface (dhcp.Service.CheckSettings) when Enabled is true.
type DHCP struct {
	Enabled bool `json:"enabled"`
	// Interface is the one interface served: not virtual, with exactly one
	// RFC 1918 IPv4 address (the subnet served).
	Interface  string `json:"interface"`
	RangeStart string `json:"rangeStart"` // first address of the pool (IPv4 in the subnet)
	RangeEnd   string `json:"rangeEnd"`   // last address (≥ RangeStart; at most 4096 addresses)
	// LeaseSeconds is the lease time (300 to 604800).
	LeaseSeconds int `json:"leaseSeconds"`
	// Router is the router option: "" = the IPv4 default gateway (it must
	// be in the subnet), or an IPv4 address in the subnet.
	Router string `json:"router"`
	// DNSServer is the DNS server option: "" = PiCache's own address on the
	// interface, or an IPv4 address.
	DNSServer string `json:"dnsServer"`
	// Domain is the domain name option: "" = dns.localDomain.
	Domain string `json:"domain"`
	// RegisterHostnames answers <host>.<domain> (and the PTR of the lease
	// address) for leases with a host name.
	RegisterHostnames bool `json:"registerHostnames"`
	// IgnoreOtherServers serves although another DHCP server was detected
	// (DANGEROUS: two servers hand out conflicting addresses).
	IgnoreOtherServers bool     `json:"ignoreOtherServers"`
	IPv6               DHCPIPv6 `json:"ipv6"`
}

// DHCPIPv6 configures PiCache's IPv6 DNS announcements on the DHCP
// interface: router advertisements with RDNSS/DNSSL only (router lifetime
// 0, no prefixes) and stateless DHCPv6.
type DHCPIPv6 struct {
	RouterAdvertisements bool `json:"routerAdvertisements"`
	DHCPv6               bool `json:"dhcpv6"`
}

// DHCP limits.
const (
	DHCPMinLeaseSeconds = 300
	DHCPMaxLeaseSeconds = 604800
	DHCPMaxPoolSize     = 4096
)

// BlockingActive reports whether blocking is effective at t.
func (f *Filter) BlockingActive(t time.Time) bool {
	if !f.Enabled {
		return false
	}
	if f.PausedUntil != nil && t.Before(*f.PausedUntil) {
		return false
	}
	return true
}

// Clone returns a deep copy.
func (a *All) Clone() *All {
	b, err := json.Marshal(a)
	if err != nil {
		panic(fmt.Sprintf("settings: clone marshal: %v", err))
	}
	var c All
	if err := json.Unmarshal(b, &c); err != nil {
		panic(fmt.Sprintf("settings: clone unmarshal: %v", err))
	}
	return &c
}

// Listener is called after a successful Update with the previous and the new
// snapshot. It runs synchronously in the updating goroutine and must not call
// Update itself.
type Listener func(old, new *All)

// Store persists settings and fans out changes.
type Store struct {
	db  *db.DB
	log *slog.Logger

	cur atomic.Pointer[All]

	mu        sync.Mutex // serialises updates and listener registration
	listeners map[int]Listener
	nextID    int
	created   bool
}

// migrations of component "settings". Append only.
var migrations = []string{
	// v1
	`CREATE TABLE settings (
		id         INTEGER PRIMARY KEY CHECK (id = 1),
		doc        TEXT    NOT NULL,
		updated_at INTEGER NOT NULL
	);`,
	// v2 (0.2.0): the download cache section is stored as "downloadCache";
	// 0.1.x stored it under its old name. Runs once per database at Open, so it
	// also converts a document restored from an older backup (schema v1) at
	// the start that applies the restore. An existing "downloadCache"
	// section wins; the old member is removed either way. A document that is
	// not valid JSON is left alone (Open reports it).
	`UPDATE settings SET doc = CASE
		WHEN COALESCE(json_type(doc, '$.downloadCache'), 'null') = 'null' AND json_type(doc, '$.lancache') = 'object'
		THEN json_set(json_remove(doc, '$.lancache'), '$.downloadCache', json(json_extract(doc, '$.lancache')))
		ELSE json_remove(doc, '$.lancache')
	END
	WHERE CASE WHEN json_valid(doc) THEN json_type(doc, '$.lancache') IS NOT NULL ELSE 0 END;`,
	// v3 (0.6.0): the default bootstrap list has the IPv6 addresses of Quad9
	// and Cloudflare too. A stored list that equals the old default exactly
	// gets them appended; an edited list is left alone. Like v2 it also
	// converts a document restored from an older backup.
	`UPDATE settings SET doc = json_set(doc, '$.dns.bootstrap', json('` + bootstrapV3 + `'))
	WHERE CASE WHEN json_valid(doc) AND json_type(doc, '$.dns.bootstrap') = 'array'
		THEN json(json_extract(doc, '$.dns.bootstrap')) = json('` + bootstrapV2 + `')
		ELSE 0 END;`,
}

// Default bootstrap lists: bootstrapV2 until 0.5.x, bootstrapV3 since 0.6.0
// (settings migration v3; Defaults uses the same addresses).
const (
	bootstrapV2 = `["9.9.9.9","149.112.112.112","1.1.1.1","1.0.0.1"]`
	bootstrapV3 = `["9.9.9.9","149.112.112.112","1.1.1.1","1.0.0.1","2620:fe::fe","2606:4700:4700::1111"]`
)

// Open loads the settings document, creating it from Defaults on first start.
// Unknown or missing fields in a stored document are tolerated: missing fields
// take their default value, so new settings get sane values after upgrades.
func Open(ctx context.Context, d *db.DB, log *slog.Logger) (*Store, error) {
	if err := d.Migrate(ctx, "settings", migrations); err != nil {
		return nil, err
	}
	s := &Store{db: d, log: log.With(slog.String("component", "settings")), listeners: map[int]Listener{}}

	cur := Defaults()
	var doc string
	err := d.R.QueryRowContext(ctx, `SELECT doc FROM settings WHERE id = 1`).Scan(&doc)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if err := s.persist(ctx, &cur); err != nil {
			return nil, err
		}
		s.created = true
	case err != nil:
		return nil, fmt.Errorf("settings: load: %w", err)
	default:
		// Decode on top of defaults so absent fields keep their defaults;
		// fields removed in newer versions are ignored.
		if err := json.Unmarshal([]byte(doc), &cur); err != nil {
			return nil, fmt.Errorf("settings: decode stored document: %w", err)
		}
		cur.normalize()
		if err := cur.Validate(); err != nil {
			s.log.Warn("stored settings are invalid; keeping them but fix them in the UI", slog.Any("err", err))
		}
	}
	s.cur.Store(&cur)
	return s, nil
}

// Created reports whether the settings document was created by this Open
// (first start); the app then applies environment-detected defaults.
func (s *Store) Created() bool { return s.created }

// Get returns the current immutable snapshot.
func (s *Store) Get() *All { return s.cur.Load() }

// Update applies fn to a deep copy of the current settings, validates the
// result, persists it and notifies listeners. If fn or validation fails,
// nothing changes.
func (s *Store) Update(ctx context.Context, fn func(*All) error) (*All, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.cur.Load()
	next := old.Clone()
	if err := fn(next); err != nil {
		return nil, err
	}
	next.normalize()
	if err := next.Validate(); err != nil {
		return nil, err
	}
	if err := s.persist(ctx, next); err != nil {
		return nil, err
	}
	s.cur.Store(next)
	for _, l := range s.listeners {
		l(old, next)
	}
	return next, nil
}

// Subscribe registers a listener and returns a function that removes it.
func (s *Store) Subscribe(l Listener) (unsubscribe func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	s.listeners[id] = l
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.listeners, id)
	}
}

func (s *Store) persist(ctx context.Context, a *All) error {
	b, err := json.Marshal(a, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("settings: encode: %w", err)
	}
	_, err = s.db.W.ExecContext(ctx,
		`INSERT INTO settings (id, doc, updated_at) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET doc = excluded.doc, updated_at = excluded.updated_at`,
		string(b), db.NowMs())
	if err != nil {
		return fmt.Errorf("settings: save: %w", err)
	}
	return nil
}
