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
	"slices"
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
	// Upstreams: "https://host/dns-query", "tls://host[:port]",
	// "udp://host[:port]", "tcp://host[:port]" or plain "host[:port]"; plain
	// DNS upstreams given by name must be public names (PublicUpstreamName).
	Upstreams []string `json:"upstreams"`
	// FallbackUpstreams are asked only when every default upstream failed
	// to reply at all (transport errors, timeouts; never after a reply of
	// any rcode). Same syntax as Upstreams; empty = no fallback.
	FallbackUpstreams []string `json:"fallbackUpstreams"`
	Bootstrap         []string `json:"bootstrap"`    // plain IPs, used only to resolve upstream hostnames
	UpstreamMode      string   `json:"upstreamMode"` // load_balance | parallel | strict | fastest_addr
	UpstreamTimeoutMs int      `json:"upstreamTimeoutMs"`
	// UpstreamBlockedTTL is the cache lifetime (seconds) of answers the
	// default upstreams blocked themselves (EDE 15–17, 0.0.0.0/::, block
	// pages, Quad9's NXDOMAIN without RA).
	UpstreamBlockedTTL int `json:"upstreamBlockedTtl"`
	// BootstrapPreferIPv6 dials the resolved addresses of DoT, DoH and
	// named plain upstreams IPv6 first (IPv4 first otherwise).
	BootstrapPreferIPv6 bool `json:"bootstrapPreferIpv6"`
	// ECS sends an EDNS client subnet with the client queries answered by
	// the default upstreams (off by default).
	ECS               ECS      `json:"ecs"`
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
	TrustConnectedNetworks bool `json:"trustConnectedNetworks"`
	// BlockedClients are IP addresses, CIDRs or MAC addresses whose DNS
	// queries are dropped (DNS only; the download cache is not affected).
	BlockedClients []string `json:"blockedClients"`
	RateLimitQPS   int      `json:"rateLimitQps"` // per rate-limit key (netutil.RateKey); 0 disables
	RateLimitBurst int      `json:"rateLimitBurst"`
	// RateLimitIPv4Prefix and RateLimitIPv6Prefix are the prefix lengths
	// that public sources share one rate-limit bucket by (LAN sources are
	// always limited per address).
	RateLimitIPv4Prefix int      `json:"rateLimitIpv4Prefix"`
	RateLimitIPv6Prefix int      `json:"rateLimitIpv6Prefix"`
	RateLimitExempt     []string `json:"rateLimitExempt"` // CIDRs (loopback, router, forwarder targets and trusted EDNS sources are exempt automatically)
	RefuseANY           bool     `json:"refuseAny"`
	// EDNSClientTrusted are forwarders (single IP addresses) whose queries
	// may carry the client's address (ECS) and MAC (option 65001).
	EDNSClientTrusted []string `json:"ednsClientTrusted"`

	// RebindProtection blocks answers of the default upstreams that point
	// names at private, loopback or link-local addresses (DNS rebinding);
	// RebindAllow lists domains (with their subdomains) that may do so.
	RebindProtection bool     `json:"rebindProtection"`
	RebindAllow      []string `json:"rebindAllow"`
	// DomainNeeded answers A/AAAA/HTTPS/SVCB/ANY queries of single-label
	// names as <name>.<localDomain> and never sends them to the default
	// upstreams.
	DomainNeeded bool `json:"domainNeeded"`
	// PrivateReverseNetworks are networks whose reverse zones are served
	// locally like the RFC 6303 zones (IPv4 /8, /16, /24; IPv6 /16–/124 in
	// steps of 4).
	PrivateReverseNetworks []string `json:"privateReverseNetworks"`
	// DroppedDomains ("domain" or "domain:TYPE", subtree) get no answer at
	// all and are not logged.
	DroppedDomains []string `json:"droppedDomains"`
	// BogusNXDomain are addresses (IPs or CIDRs) that turn an answer of the
	// default upstreams into NXDOMAIN.
	BogusNXDomain []string `json:"bogusNxdomain"`

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

// ECS configures the EDNS client subnet (RFC 7871) sent to the default
// upstreams: "off"; "client" = the /24 (IPv4) or /56 (IPv6) of the source
// address when it is public; "custom" = CustomSubnet.
type ECS struct {
	Mode         string `json:"mode"`
	CustomSubnet string `json:"customSubnet"` // a public IPv4 /8–/24 or IPv6 /32–/56 network (host bits masked)
}

// ECS modes.
const (
	ECSOff    = "off"
	ECSClient = "client"
	ECSCustom = "custom"
)

// Limits of the DNS lists (validation errors name them).
const (
	MaxFallbackUpstreams      = 4
	MaxRebindAllow            = 256
	MaxPrivateReverseNetworks = 32
	MaxBlockedClients         = 256
	MaxDroppedDomains         = 256
	MaxBogusNXDomain          = 64
	MaxEDNSClientTrusted      = 16
)

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
// serves while Enabled is true, PICACHE_DHCP is not off and no safety gate
// blocks it. Validate checks the form of the values; the interface, the
// range and the router are checked against the live interface
// (dhcp.Service.CheckSettings) when Enabled is true. It holds slices: compare
// with Equal.
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
	// GenerateNames (with RegisterHostnames and a domain) answers
	// <a>-<b>-<c>-<d>.<domain> for active leases without a usable host
	// name or whose name another client holds (DNS answers only).
	GenerateNames bool `json:"generateNames"`
	// IgnoreOtherServers serves although another DHCP server was detected
	// (DANGEROUS: two servers hand out conflicting addresses).
	IgnoreOtherServers bool `json:"ignoreOtherServers"`
	// OnlyReserved ignores DHCPv4 clients without a reservation (a
	// convenience, not an access control: MAC addresses can be forged).
	OnlyReserved bool `json:"onlyReserved"`
	// RapidCommit answers a DISCOVER that carries option 80 with an ACK
	// (RFC 4039) while no other DHCP server counts.
	RapidCommit bool        `json:"rapidCommit"`
	Options     DHCPOptions `json:"options"`
	IPv6        DHCPIPv6    `json:"ipv6"`
}

// DHCPOptions are the typed extra DHCPv4 options. NTP servers (42), the
// MTU (26) and the WPAD URL (252) are sent only when the client asks for
// them; the extra search domains follow the domain in option 119.
type DHCPOptions struct {
	NTPServers         []string `json:"ntpServers"`         // unicast IPv4 addresses, at most 4
	MTU                int      `json:"mtu"`                // 0 = none, else 576–9000
	WPADURL            string   `json:"wpadUrl"`            // "" = none, else an http(s) URL of at most 255 bytes
	ExtraSearchDomains []string `json:"extraSearchDomains"` // at most 4, after the domain in option 119 (IPv4 only)
}

// Equal reports whether two DHCP sections are the same.
func (d DHCP) Equal(o DHCP) bool {
	return d.Enabled == o.Enabled && d.Interface == o.Interface && d.RangeStart == o.RangeStart && d.RangeEnd == o.RangeEnd &&
		d.LeaseSeconds == o.LeaseSeconds && d.Router == o.Router && d.DNSServer == o.DNSServer && d.Domain == o.Domain &&
		d.RegisterHostnames == o.RegisterHostnames && d.GenerateNames == o.GenerateNames &&
		d.IgnoreOtherServers == o.IgnoreOtherServers && d.OnlyReserved == o.OnlyReserved && d.RapidCommit == o.RapidCommit &&
		d.Options.MTU == o.Options.MTU && d.Options.WPADURL == o.Options.WPADURL &&
		slices.Equal(d.Options.NTPServers, o.Options.NTPServers) &&
		slices.Equal(d.Options.ExtraSearchDomains, o.Options.ExtraSearchDomains) && d.IPv6 == o.IPv6
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
	DHCPMaxNTPServers   = 4
	DHCPMaxSearch       = 4   // extra search domains
	DHCPMinMTU          = 576 // RFC 2132 5.1
	DHCPMaxMTU          = 9000
	DHCPMaxWPADURL      = 255 // one option
	DHCPMaxSearchWire   = 255 // the encoded search list in one option 119
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
	// v4 (0.9.0): the default upstreams are Quad9 only (it filters malware;
	// load-balancing it with an unfiltered resolver made blocking random),
	// with a fallback of another operator. A stored list equal to the old
	// default becomes the new one. A document without fallbacks whose own
	// upstream list differs from the new default gets none, so an admin's
	// own resolver never starts sending queries to another operator; only
	// installations on the defaults get the default fallback (by decoding
	// on top of Defaults). Like v3 it also converts a document restored
	// from an older backup.
	`UPDATE settings SET doc = json_set(doc, '$.dns.upstreams', json('` + upstreamsV4 + `'))
	WHERE CASE WHEN json_valid(doc) AND json_type(doc, '$.dns.upstreams') = 'array'
		THEN json(json_extract(doc, '$.dns.upstreams')) = json('` + upstreamsV3 + `')
		ELSE 0 END;
	UPDATE settings SET doc = json_set(doc, '$.dns.fallbackUpstreams', json('[]'))
	WHERE CASE WHEN json_valid(doc) AND json_type(doc, '$.dns.upstreams') = 'array'
			AND json_type(doc, '$.dns.fallbackUpstreams') IS NULL
		THEN json(json_extract(doc, '$.dns.upstreams')) != json('` + upstreamsV4 + `')
		ELSE 0 END;`,
}

// Default bootstrap lists: bootstrapV2 until 0.5.x, bootstrapV3 since 0.6.0
// (settings migration v3; Defaults uses the same addresses). Default
// upstream lists: upstreamsV3 until 0.8.x, upstreamsV4 since 0.9.0
// (settings migration v4).
const (
	bootstrapV2 = `["9.9.9.9","149.112.112.112","1.1.1.1","1.0.0.1"]`
	bootstrapV3 = `["9.9.9.9","149.112.112.112","1.1.1.1","1.0.0.1","2620:fe::fe","2606:4700:4700::1111"]`
	upstreamsV3 = `["https://dns.quad9.net/dns-query","https://cloudflare-dns.com/dns-query"]`
	upstreamsV4 = `["https://dns.quad9.net/dns-query"]`
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
	// The search list depends on dns.localDomain when dhcp.domain is empty;
	// it is checked only when its own inputs (dhcp.domain, the extra
	// domains) change, so neither a later change of the local domain nor
	// any other DHCP change (switching it off, say) is refused because of
	// extras the local domain broke (the DHCP server drops extra domains
	// that no longer fit).
	if old.DHCP.Domain != next.DHCP.Domain ||
		!slices.Equal(old.DHCP.Options.ExtraSearchDomains, next.DHCP.Options.ExtraSearchDomains) {
		if err := next.DHCP.checkSearchList(next.DNS.LocalDomain); err != nil {
			return nil, err
		}
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
