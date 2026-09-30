package settings

import (
	"bytes"
	"crypto/x509"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// Sync configures the follower sync (docs/ARCHITECTURE.md 15.4): a follower
// pulls the exportable sections from a primary's GET /system/export with a
// sync token. Token is input only (absent or null keeps the stored token,
// "" removes it, a value replaces it; it is sealed in settings_secrets and
// never part of the document); TokenSet reports whether one is stored and
// is ignored in requests.
type Sync struct {
	Mode            string   `json:"mode"`   // off | follower
	Source          string   `json:"source"` // https://host[:port] of the primary
	Token           *string  `json:"token,omitzero"`
	TokenSet        bool     `json:"tokenSet"`
	CAPEM           string   `json:"caPem"` // trust anchors (1–4 PEM certificates); "" = the system roots
	IntervalMinutes int      `json:"intervalMinutes"`
	Sections        []string `json:"sections"`
}

// Values and limits of the sync section.
const (
	SyncOff             = "off"
	SyncFollower        = "follower"
	MinSyncInterval     = 5
	MaxSyncInterval     = 1440
	MaxSyncSource       = 2048
	MaxSyncCAPEM        = 16 << 10
	MaxSyncCACerts      = 4
	MaxSyncTokenLen     = 256
	syncSourceForm      = "must be an https URL of the primary PiCache (https://host[:port])"
	syncForbiddenAddrs  = "link-local, multicast, unspecified and loopback addresses are not allowed"
	sectionsChooseOne   = "choose at least one section"
	sectionsUnknownFmt  = "unknown section %s"
	clientsGroupsDepFmt = "clients-and-groups replaces the groups that lists, rules, local records and parental controls refer to: also %s lists-and-rules, local-dns and parental"
)

// Sections of the configuration (restore, export and sync; ARCHITECTURE
// 15.3 has the tables of each).
const (
	SectionSettings      = "settings"
	SectionClientsGroups = "clients-and-groups"
	SectionListsRules    = "lists-and-rules"
	SectionLocalDNS      = "local-dns"
	SectionParental      = "parental"
	SectionDHCP          = "dhcp"
	SectionDownloadCache = "download-cache"
	SectionNotifications = "notifications"
	SectionStorage       = "storage"
	SectionDNSSettings   = "dns-settings"
)

// RestoreSections are the sections a partial restore can select, in this
// order.
var RestoreSections = []string{SectionSettings, SectionClientsGroups, SectionListsRules, SectionLocalDNS,
	SectionParental, SectionDHCP, SectionDownloadCache, SectionNotifications, SectionStorage}

// ExportSections are the sections the export and the follower sync carry,
// in this order.
var ExportSections = []string{SectionClientsGroups, SectionListsRules, SectionLocalDNS, SectionParental, SectionDNSSettings}

// GroupLinkedSections refer to client groups (their links are the only
// references between sections).
var GroupLinkedSections = []string{SectionListsRules, SectionLocalDNS, SectionParental}

// allSections are every section name.
var allSections = append(slices.Clone(RestoreSections), SectionDNSSettings)

// CheckSections validates a selection of sections for an operation (verb
// "restore", "export" or "sync"): at least one, every name known and
// allowed (message "<x> cannot be exported|synced|restored"), and for
// restore and sync the dependency rule: clients-and-groups needs the three
// group-linked sections. It returns the names in canonical order without
// duplicates.
func CheckSections(field string, in []string, allowed []string, verb string) ([]string, error) {
	var out []string
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if !slices.Contains(allSections, s) {
			return nil, apperr.Invalid(field, sectionsUnknownFmt, s)
		}
		if !slices.Contains(allowed, s) {
			what := map[string]string{"restore": "restored", "export": "exported", "sync": "synced"}[verb]
			if s == SectionDNSSettings && verb == "restore" {
				return nil, apperr.Invalid(field, "dns-settings cannot be restored: it is part of settings")
			}
			return nil, apperr.Invalid(field, "%s cannot be %s", s, what)
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, apperr.Invalid(field, sectionsChooseOne)
	}
	slices.SortFunc(out, func(a, b string) int { return slices.Index(allSections, a) - slices.Index(allSections, b) })
	if verb != "export" && slices.Contains(out, SectionClientsGroups) {
		for _, s := range GroupLinkedSections {
			if !slices.Contains(out, s) {
				return nil, apperr.Invalid(field, clientsGroupsDepFmt, verb)
			}
		}
	}
	return out, nil
}

// SyncOrigin returns the origin (https://host[:port], lower-case, no
// trailing slash) of a sync source and the field message when it is not a
// valid source: https, a host, an empty or "/" path, no user info, query
// or fragment, at most 2048 characters, no link-local, multicast,
// unspecified or loopback IP literal.
func SyncOrigin(source string) (string, string) {
	if len(source) > MaxSyncSource || !printableASCII(source) {
		return "", syncSourceForm
	}
	u, err := url.Parse(source)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.Opaque != "" {
		return "", syncSourceForm
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(source, "#") {
		return "", "must not contain a user name, password, query or fragment"
	}
	if u.Path != "" && u.Path != "/" {
		return "", syncSourceForm
	}
	host := strings.ToLower(u.Hostname())
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "", syncSourceForm
		}
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return "", syncSourceForm
		}
		ip = ip.Unmap()
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || ip.IsLoopback() {
			return "", syncForbiddenAddrs
		}
		host = ip.String()
		if ip.Is6() {
			host = "[" + host + "]"
		}
	} else if !validHostname(host) {
		return "", syncSourceForm
	}
	origin := "https://" + host
	if p := u.Port(); p != "" && p != "443" {
		origin += ":" + p
	}
	return origin, ""
}

// ParseCAPEM parses sync.caPem: 1–4 PEM CERTIFICATE blocks, at most
// 16 KiB, nothing but white space around them.
func ParseCAPEM(s string) ([]*x509.Certificate, error) {
	if len(s) > MaxSyncCAPEM {
		return nil, errors.New("too large")
	}
	rest := []byte(s)
	var out []*x509.Certificate
	for {
		rest = bytes.TrimSpace(rest)
		if len(rest) == 0 {
			break
		}
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil || b.Type != "CERTIFICATE" || len(b.Headers) > 0 || len(out) == MaxSyncCACerts {
			return nil, errors.New("not 1 to 4 certificates")
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("no certificate")
	}
	return out, nil
}

// normalize trims the sync members; the source becomes its origin.
func (s *Sync) normalize() {
	s.Mode = strings.ToLower(strings.TrimSpace(s.Mode))
	if s.Mode == "" {
		s.Mode = SyncOff
	}
	s.Source = strings.TrimSpace(s.Source)
	if o, msg := SyncOrigin(s.Source); s.Source != "" && msg == "" {
		s.Source = o
	}
	s.CAPEM = strings.TrimSpace(s.CAPEM)
	if s.CAPEM != "" {
		s.CAPEM += "\n"
	}
	secs := make([]string, 0, len(s.Sections))
	for _, x := range s.Sections {
		if x = strings.ToLower(strings.TrimSpace(x)); x != "" && !slices.Contains(secs, x) {
			secs = append(secs, x)
		}
	}
	s.Sections = secs
}

// validate checks the form of the sync section; the token (secrets,
// checkToken) and the addresses of this machine are checked by their
// callers.
func (s *Sync) validate() error {
	switch s.Mode {
	case SyncOff, SyncFollower:
	default:
		return apperr.Invalid("sync.mode", "must be off or follower")
	}
	if s.Source == "" {
		if s.Mode == SyncFollower {
			return apperr.Invalid("sync.source", "enter the address of the primary")
		}
	} else if _, msg := SyncOrigin(s.Source); msg != "" {
		return apperr.Invalid("sync.source", "%s", msg)
	}
	if s.CAPEM != "" {
		if _, err := ParseCAPEM(s.CAPEM); err != nil {
			return apperr.Invalid("sync.caPem", "must be 1 to 4 PEM certificates")
		}
	}
	if s.IntervalMinutes < MinSyncInterval || s.IntervalMinutes > MaxSyncInterval {
		return apperr.Invalid("sync.intervalMinutes", "must be between %d and %d", MinSyncInterval, MaxSyncInterval)
	}
	if len(s.Sections) > 0 || s.Mode == SyncFollower {
		if _, err := CheckSections("sync.sections", s.Sections, ExportSections, "sync"); err != nil {
			return err
		}
	}
	return nil
}

// checkToken refuses a write from old to s that leaves a follower without
// a sync token and changes the sync section (a token removed included). A
// stored follower that lost its token, e.g. by a restored backup without
// secrets, keeps accepting writes that leave the sync section as it is
// (pausing blocking, the DNS settings), and the health check "sync"
// reports the missing token.
func (s *Sync) checkToken(old *Sync) error {
	if s.Mode != SyncFollower || s.TokenSet {
		return nil
	}
	if old.TokenSet || old.Mode != s.Mode || old.Source != s.Source || old.CAPEM != s.CAPEM ||
		old.IntervalMinutes != s.IntervalMinutes || !slices.Equal(old.Sections, s.Sections) {
		return apperr.Invalid("sync.token", "enter a sync token of the primary")
	}
	return nil
}

// ValidSyncToken reports whether s can be an API token (1–256 printable
// ASCII characters).
func ValidSyncToken(s string) bool {
	return len(s) >= 1 && len(s) <= MaxSyncTokenLen && printableASCII(s)
}

// --- the syncable settings (section dns-settings) ---

// syncRules has a decision for every leaf of All (dotted JSON path; array
// elements share their array's path): true = the member is part of
// dns-settings and synced from the primary. Every member of dns and
// filter is, except the access, listener and name members of this
// machine; no other section ever is (a test fails for a leaf without a
// decision). Since no web or DNS access member is synced, a sync can never
// lock the admin out of a follower.
var syncRules = func() map[string]bool {
	m := map[string]bool{}
	for _, l := range []string{
		"dns.upstreams", "dns.fallbackUpstreams", "dns.bootstrap", "dns.upstreamMode", "dns.upstreamTimeoutMs",
		"dns.upstreamBlockedTtl", "dns.bootstrapPreferIpv6", "dns.ecs.mode", "dns.ecs.customSubnet",
		"dns.localPtrUpstreams", "dns.localDomain", "dns.blockedClients", "dns.rateLimitQps", "dns.rateLimitBurst",
		"dns.rateLimitIpv4Prefix", "dns.rateLimitIpv6Prefix", "dns.rateLimitExempt", "dns.refuseAny",
		"dns.ednsClientTrusted", "dns.rebindProtection", "dns.rebindAllow", "dns.domainNeeded",
		"dns.privateReverseNetworks", "dns.droppedDomains", "dns.bogusNxdomain", "dns.cacheEnabled", "dns.cacheSize",
		"dns.cacheMinTtl", "dns.cacheMaxTtl", "dns.serveStale", "dns.serveStaleMaxAgeSec", "dns.dnssec", "dns.dnssecMode",
		"dns.disableAAAA", "dns.dns64.enabled", "dns.dns64.prefix", "dns.localRecordsEnabled", "dns.localizeRecords",
		"filter.blockingMode", "filter.blockingIpv4", "filter.blockingIpv6", "filter.blockedTtl",
		"filter.cnameInspection", "filter.updateIntervalHours", "filter.blockMozillaCanary",
		"filter.blockIcloudPrivateRelay",
	} {
		m[l] = true
	}
	for _, l := range []string{
		"dns.serverNames", "dns.serverNameAddresses.ipv4", "dns.serverNameAddresses.ipv6", "dns.encrypted.dot",
		"dns.encrypted.doh", "dns.encrypted.serverName", "dns.plainDns", "dns.allowedNetworks", "dns.allowAllNetworks",
		"dns.trustConnectedNetworks", "dns.routerResolver", "filter.enabled", "filter.pausedUntil",

		"downloadCache.allowPrivateUpstreams", "downloadCache.cacheIpv4", "downloadCache.cacheIpv6",
		"downloadCache.disabledServices", "downloadCache.dnsTtl", "downloadCache.domainsSource",
		"downloadCache.enabled", "downloadCache.nocacheClients", "downloadCache.updateIntervalHours",
		"cache.activeStoreId", "cache.maxAgeDays", "cache.maxConcurrentFills", "cache.maxFillsPerClient",
		"cache.maxSizeBytes", "cache.minFreeBytes", "cache.readAheadSlices", "cache.sliceSizeBytes",
		"logs.anonymizeClientIps", "logs.cacheLogRetentionHours", "logs.flushSeconds", "logs.hideDomains",
		"logs.ignoredDomains", "logs.maxDbSizeMiB", "logs.privacyLevel", "logs.queryLogEnabled",
		"logs.queryLogRetentionHours", "logs.sessionRetentionDays", "logs.statsEnabled",
		"logs.statsOnlyAddressQueries", "logs.statsRetentionDays", "logs.seenRetentionDays",
		"web.allowedHosts", "web.allowedNetworks", "web.language", "web.metricsEnabled", "web.onboardingDone", "web.redirectToHttps",
		"web.restrictToNetworks", "web.sessionIdleMinutes", "web.sessionMaxHours", "web.tlsMinVersion",
		"web.trustedProxies",
		"updates.checkEnabled", "updates.includePrereleases", "updates.channel",
		"backups.destination", "backups.enabled", "backups.includeSecrets", "backups.keep", "backups.schedule",
		"backups.time", "backups.weekday",
		"dhcp.dnsServer", "dhcp.domain", "dhcp.enabled", "dhcp.generateNames", "dhcp.ignoreOtherServers",
		"dhcp.interface", "dhcp.ipv6.dhcpv6", "dhcp.ipv6.routerAdvertisements", "dhcp.leaseSeconds",
		"dhcp.onlyReserved", "dhcp.options.extraSearchDomains", "dhcp.options.mtu", "dhcp.options.ntpServers",
		"dhcp.options.wpadUrl", "dhcp.rangeEnd", "dhcp.rangeStart", "dhcp.rapidCommit", "dhcp.registerHostnames",
		"dhcp.router",
		"health.loadPerCpuMax", "health.memoryAvailableMinPercent", "health.temperatureMaxCelsius",
		"clients.nameSources.ptr", "clients.nameSources.dhcp", "clients.nameSources.hostsFile",
		"clients.nameSources.whois",
		"sync.mode", "sync.source", "sync.tokenSet", "sync.caPem", "sync.intervalMinutes", "sync.sections",
		"network.proxy.url", "network.proxy.username", "network.proxy.passwordSet", "network.proxyFor.lists",
		"network.proxyFor.updateCheck", "network.proxyFor.notifications",
		"ntp.enabled", "ntp.stratum",
	} {
		m[l] = false
	}
	return m
}()

// SyncRuleKnown reports whether a leaf has a sync decision (tests).
func SyncRuleKnown(path string) bool {
	_, ok := syncRules[path]
	return ok
}

// Syncable reports whether a settings member (dotted path of a leaf) is
// synced from a primary (section dns-settings).
func Syncable(path string) bool { return syncRules[path] }

// SyncableSettings returns the dns-settings of a document: the syncable
// members of dns and filter as {"dns":{…}, "filter":{…}}.
func SyncableSettings(a *All) (jsontext.Value, error) {
	out := map[string]map[string]jsontext.Value{}
	for _, sec := range []struct {
		name string
		v    any
	}{{"dns", &a.DNS}, {"filter", &a.Filter}} {
		raw, err := json.Marshal(sec.v, json.Deterministic(true))
		if err != nil {
			return nil, err
		}
		var members map[string]jsontext.Value
		if err := json.Unmarshal(raw, &members); err != nil {
			return nil, err
		}
		kept := map[string]jsontext.Value{}
		for k, v := range members {
			if pruned, ok := pruneSyncable(sec.name+"."+k, v); ok {
				kept[k] = pruned
			}
		}
		out[sec.name] = kept
	}
	b, err := json.Marshal(out, json.Deterministic(true))
	return jsontext.Value(b), err
}

// pruneSyncable keeps the syncable leaves below path (objects recursively).
func pruneSyncable(path string, v jsontext.Value) (jsontext.Value, bool) {
	if v.Kind() != '{' {
		return v, syncRules[path]
	}
	var members map[string]jsontext.Value
	if err := json.Unmarshal(v, &members); err != nil {
		return nil, false
	}
	kept := map[string]jsontext.Value{}
	for k, m := range members {
		if p, ok := pruneSyncable(path+"."+k, m); ok {
			kept[k] = p
		}
	}
	if len(kept) == 0 {
		return nil, false
	}
	b, err := json.Marshal(kept, json.Deterministic(true))
	return jsontext.Value(b), err == nil
}

// ApplySyncable replaces the syncable members of a's dns and filter
// sections with those of raw ({"dns":{…}, "filter":{…}}, as
// SyncableSettings builds it); the other members stay a's. A member that
// is not syncable or unknown to this version is an error (a newer
// primary), as is an unknown section.
func ApplySyncable(a *All, raw jsontext.Value) error {
	var in map[string]map[string]jsontext.Value
	if err := json.Unmarshal(raw, &in, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("dns-settings: %w", err)
	}
	for name, members := range in {
		var dst any
		switch name {
		case "dns":
			dst = &a.DNS
		case "filter":
			dst = &a.Filter
		default:
			return fmt.Errorf("dns-settings: unknown section %q", name)
		}
		cur, err := json.Marshal(dst)
		if err != nil {
			return err
		}
		var curMembers map[string]jsontext.Value
		if err := json.Unmarshal(cur, &curMembers); err != nil {
			return err
		}
		for k, v := range members {
			merged, err := overlaySyncable(name+"."+k, curMembers[k], v)
			if err != nil {
				return err
			}
			curMembers[k] = merged
		}
		b, err := json.Marshal(curMembers)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, dst, json.RejectUnknownMembers(true)); err != nil {
			return fmt.Errorf("dns-settings: %s: %w", name, err)
		}
	}
	return nil
}

// overlaySyncable merges the synced value v of the member at path over
// cur (objects member by member).
func overlaySyncable(path string, cur, v jsontext.Value) (jsontext.Value, error) {
	if _, known := syncRules[path]; known {
		if !syncRules[path] {
			return nil, fmt.Errorf("dns-settings: %s is not synced", path)
		}
		return v, nil
	}
	if v.Kind() != '{' || cur.Kind() != '{' {
		return nil, fmt.Errorf("dns-settings: unknown member %s", path)
	}
	var in, base map[string]jsontext.Value
	if err := json.Unmarshal(v, &in); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(cur, &base); err != nil {
		return nil, err
	}
	for k, m := range in {
		merged, err := overlaySyncable(path+"."+k, base[k], m)
		if err != nil {
			return nil, err
		}
		base[k] = merged
	}
	b, err := json.Marshal(base)
	return jsontext.Value(b), err
}

// SyncedChange returns the first syncable member (dotted path, sorted)
// whose value differs between two documents ("" if none).
func SyncedChange(old, next *All) string {
	a, b := Leaves(old), Leaves(next)
	var out []string
	for k, v := range b {
		if syncRules[k] && !bytes.Equal(a[k], v) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	if len(out) == 0 {
		return ""
	}
	return out[0]
}

// Leaves flattens a document into its leaves (dotted paths; arrays are
// leaves).
func Leaves(a *All) map[string]jsontext.Value {
	out := map[string]jsontext.Value{}
	raw, err := json.Marshal(a, json.Deterministic(true))
	if err != nil {
		return out
	}
	var walk func(prefix string, v jsontext.Value)
	walk = func(prefix string, v jsontext.Value) {
		if v.Kind() != '{' {
			out[prefix] = v
			return
		}
		var members map[string]jsontext.Value
		if err := json.Unmarshal(v, &members); err != nil {
			return
		}
		for k, m := range members {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			walk(p, m)
		}
	}
	walk("", jsontext.Value(raw))
	return out
}
