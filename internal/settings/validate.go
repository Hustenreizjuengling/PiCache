package settings

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// UpstreamSpec is a parsed upstream resolver address.
type UpstreamSpec struct {
	Raw     string // original string
	Proto   string // udp | tcp | tls | https
	Host    string // hostname or IP literal (lower-case, without brackets)
	Port    int
	URL     string // full URL for https
	IsIPLit bool   // Host is an IP literal
}

// Addr returns host:port.
func (u UpstreamSpec) Addr() string { return net.JoinHostPort(u.Host, strconv.Itoa(u.Port)) }

// ParseUpstream parses and validates an upstream string. Plain DNS
// upstreams (udp/tcp) may be given by name; whether such a name may be used
// (PublicUpstreamName) depends on the local domain and is checked by the
// callers.
//
//	9.9.9.9 | 9.9.9.9:53 | [2620:fe::fe]:53 | dns.example | udp://… | tcp://… | tls://dns.quad9.net[:853] | https://dns.quad9.net/dns-query
func ParseUpstream(s string) (UpstreamSpec, error) {
	s = strings.TrimSpace(s)
	spec := UpstreamSpec{Raw: s}
	if s == "" {
		return spec, errors.New("empty upstream")
	}
	if !strings.Contains(s, "://") {
		s = "udp://" + s
	}
	s = escapeZone(s)
	u, err := url.Parse(s)
	if err != nil {
		return spec, err
	}
	spec.Proto = strings.ToLower(u.Scheme)
	spec.Host = strings.ToLower(u.Hostname())
	if spec.Host == "" {
		return spec, errors.New("missing host")
	}
	_, ipErr := netip.ParseAddr(spec.Host)
	spec.IsIPLit = ipErr == nil
	defPort := map[string]int{"udp": 53, "tcp": 53, "tls": 853, "https": 443}
	p, ok := defPort[spec.Proto]
	if !ok {
		return spec, errors.New("scheme must be udp, tcp, tls or https")
	}
	spec.Port = p
	if ps := u.Port(); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil || n < 1 || n > 65535 {
			return spec, errors.New("invalid port")
		}
		spec.Port = n
	}
	switch spec.Proto {
	case "udp", "tcp":
		if u.Path != "" && u.Path != "/" {
			return spec, errors.New("plain DNS upstreams take no path")
		}
		if !spec.IsIPLit && numericTLD(spec.Host) {
			// "192.168.1781", "10.0.0": a mistyped address, never a name.
			return spec, errors.New("not a valid IP address")
		}
	case "tls":
		if u.Path != "" && u.Path != "/" {
			return spec, errors.New("DoT upstreams take no path")
		}
	case "https":
		if u.Path == "" || u.Path == "/" {
			u.Path = "/dns-query"
		}
		if u.User != nil || u.Fragment != "" {
			return spec, errors.New("DoH URL must not contain credentials or fragment")
		}
		spec.URL = u.String()
	}
	if !spec.IsIPLit && !validHostname(spec.Host) {
		return spec, errors.New("invalid hostname")
	}
	return spec, nil
}

// escapeZone percent-encodes the zone of a bracketed IPv6 literal
// ("[fe80::1%eth0]:53", e.g. a router resolver on a link-local address) as
// URLs require ("%25eth0").
func escapeZone(s string) string {
	i := strings.IndexByte(s, '[')
	j := strings.IndexByte(s, ']')
	if i < 0 || j < i {
		return s
	}
	if k := strings.IndexByte(s[i:j], '%'); k >= 0 && !strings.HasPrefix(s[i+k:], "%25") {
		return s[:i+k] + "%25" + s[i+k+1:]
	}
	return s
}

var hostnameRE = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func validHostname(h string) bool {
	h = strings.TrimSuffix(h, ".")
	return len(h) > 0 && len(h) <= 253 && hostnameRE.MatchString(h)
}

// ValidHostname reports whether h is a syntactically valid DNS hostname (A-labels).
func ValidHostname(h string) bool { return validHostname(strings.ToLower(h)) }

// normalize trims and lower-cases list entries and removes duplicates.
func (a *All) normalize() {
	clean := func(in []string, lower bool) []string {
		out := make([]string, 0, len(in))
		for _, s := range in {
			s = strings.TrimSpace(s)
			if lower {
				s = strings.ToLower(s)
			}
			if s != "" && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
		return out
	}
	d := &a.DNS
	d.Upstreams = clean(d.Upstreams, false)
	d.FallbackUpstreams = clean(d.FallbackUpstreams, false)
	d.Bootstrap = clean(d.Bootstrap, true)
	d.RebindAllow = normalizeList(d.RebindAllow, func(s string) string { return strings.Trim(strings.ToLower(s), ".") })
	d.PrivateReverseNetworks = normalizeList(d.PrivateReverseNetworks, normalizePrefix)
	d.BlockedClients = NormalizeBlockedClients(d.BlockedClients)
	d.DroppedDomains = normalizeList(d.DroppedDomains, func(s string) string {
		if e, ok := parseDroppedDomain(s); ok {
			return e.String()
		}
		return s
	})
	d.BogusNXDomain = normalizeList(d.BogusNXDomain, normalizeAddrOrPrefix)
	d.EDNSClientTrusted = normalizeList(d.EDNSClientTrusted, normalizeAddrOrPrefix)
	d.ECS.Mode = strings.ToLower(strings.TrimSpace(d.ECS.Mode))
	if d.ECS.Mode == "" {
		d.ECS.Mode = ECSOff
	}
	d.ECS.CustomSubnet = normalizePrefix(strings.TrimSpace(d.ECS.CustomSubnet))
	d.LocalPTRUpstreams = clean(d.LocalPTRUpstreams, false)
	d.ServerNames = clean(d.ServerNames, true)
	d.AllowedNetworks = clean(d.AllowedNetworks, true)
	d.RateLimitExempt = clean(d.RateLimitExempt, true)
	d.LocalDomain = strings.Trim(strings.ToLower(strings.TrimSpace(d.LocalDomain)), ".")
	d.RouterResolver = strings.ToLower(strings.TrimSpace(d.RouterResolver))
	d.DNS64.Prefix = strings.ToLower(strings.TrimSpace(d.DNS64.Prefix))
	if d.DNS64.Prefix == "" {
		d.DNS64.Prefix = DefaultDNS64Prefix
	}
	if p, err := netip.ParsePrefix(d.DNS64.Prefix); err == nil {
		d.DNS64.Prefix = p.Masked().String()
	}
	l := &a.DownloadCache
	l.CacheIPv4 = clean(l.CacheIPv4, true)
	l.CacheIPv6 = clean(l.CacheIPv6, true)
	l.DisabledServices = clean(l.DisabledServices, true)
	l.NocacheClients = clean(l.NocacheClients, true)
	a.Web.AllowedHosts = clean(a.Web.AllowedHosts, true)
	a.Web.AllowedNetworks = normalizeList(a.Web.AllowedNetworks, normalizeAddrOrPrefix)
	a.Web.TrustedProxies = normalizeList(a.Web.TrustedProxies, normalizeAddrOrPrefix)
	a.Web.TLSMinVersion = strings.TrimSpace(a.Web.TLSMinVersion)
	b := &a.Backups
	b.Schedule = strings.ToLower(strings.TrimSpace(b.Schedule))
	b.Time = strings.TrimSpace(b.Time)
	b.Destination = strings.ToLower(strings.TrimSpace(b.Destination))
	h := &a.DHCP
	h.Interface = strings.TrimSpace(h.Interface)
	for _, s := range []*string{&h.RangeStart, &h.RangeEnd, &h.Router, &h.DNSServer} {
		*s = strings.TrimSpace(*s)
		if ip, err := netip.ParseAddr(*s); err == nil && ip.Unmap().Is4() {
			*s = ip.Unmap().String()
		}
	}
	h.Domain = strings.Trim(strings.ToLower(strings.TrimSpace(h.Domain)), ".")
	// The option lists keep their order and duplicates (Validate names the
	// duplicate entry); empty entries are dropped.
	o := &h.Options
	ntp := make([]string, 0, len(o.NTPServers))
	for _, s := range o.NTPServers {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if ip, err := netip.ParseAddr(s); err == nil && ip.Unmap().Is4() {
			s = ip.Unmap().String()
		}
		ntp = append(ntp, s)
	}
	o.NTPServers = ntp
	search := make([]string, 0, len(o.ExtraSearchDomains))
	for _, s := range o.ExtraSearchDomains {
		if s = strings.Trim(strings.ToLower(strings.TrimSpace(s)), "."); s != "" {
			search = append(search, s)
		}
	}
	o.ExtraSearchDomains = search
	o.WPADURL = strings.TrimSpace(o.WPADURL)
}

// Validate checks all sections and returns an apperr.Invalid error naming
// the first offending field (dotted path, e.g. "dns.upstreams[1]").
func (a *All) Validate() error {
	d := a.DNS
	if len(d.Upstreams) == 0 {
		return apperr.Invalid("dns.upstreams", "at least one upstream is required")
	}
	for field, n := range map[string]int{"dns.upstreams": len(d.Upstreams), "dns.bootstrap": len(d.Bootstrap),
		"dns.localPtrUpstreams": len(d.LocalPTRUpstreams), "dns.serverNames": len(d.ServerNames)} {
		if n > 16 {
			return apperr.Invalid(field, "at most 16 entries")
		}
	}
	for field, n := range map[string]int{"dns.allowedNetworks": len(d.AllowedNetworks), "dns.rateLimitExempt": len(d.RateLimitExempt),
		"downloadCache.cacheIpv4": len(a.DownloadCache.CacheIPv4), "downloadCache.cacheIpv6": len(a.DownloadCache.CacheIPv6),
		"downloadCache.nocacheClients": len(a.DownloadCache.NocacheClients), "web.allowedHosts": len(a.Web.AllowedHosts)} {
		if n > 256 {
			return apperr.Invalid(field, "at most 256 entries")
		}
	}
	for field, lim := range map[string][2]int{
		"dns.fallbackUpstreams":      {len(d.FallbackUpstreams), MaxFallbackUpstreams},
		"dns.rebindAllow":            {len(d.RebindAllow), MaxRebindAllow},
		"dns.privateReverseNetworks": {len(d.PrivateReverseNetworks), MaxPrivateReverseNetworks},
		"dns.blockedClients":         {len(d.BlockedClients), MaxBlockedClients},
		"dns.droppedDomains":         {len(d.DroppedDomains), MaxDroppedDomains},
		"dns.bogusNxdomain":          {len(d.BogusNXDomain), MaxBogusNXDomain},
		"dns.ednsClientTrusted":      {len(d.EDNSClientTrusted), MaxEDNSClientTrusted},
	} {
		if lim[0] > lim[1] {
			return apperr.Invalid(field, "at most %d entries", lim[1])
		}
	}
	needBootstrap := false
	for _, list := range []struct {
		field string
		ups   []string
	}{{"dns.upstreams", d.Upstreams}, {"dns.fallbackUpstreams", d.FallbackUpstreams}} {
		for i, u := range list.ups {
			field := list.field + "[" + strconv.Itoa(i) + "]"
			spec, err := ParseUpstream(u)
			if err != nil {
				return apperr.Invalid(field, "%v", err)
			}
			if spec.IsIPLit {
				continue
			}
			needBootstrap = true
			if (spec.Proto == "udp" || spec.Proto == "tcp") && !PublicUpstreamName(spec.Host, d.LocalDomain) {
				return apperr.Invalid(field, "%s", ErrPlainUpstreamName)
			}
		}
	}
	for i, b := range d.Bootstrap {
		if _, err := netip.ParseAddr(b); err != nil {
			return apperr.Invalid("dns.bootstrap["+strconv.Itoa(i)+"]", "must be an IP address")
		}
	}
	if needBootstrap && len(d.Bootstrap) == 0 {
		return apperr.Invalid("dns.bootstrap", "required when an upstream or fallback is given by host name")
	}
	if err := d.validateLists(); err != nil {
		return err
	}
	for i, u := range d.LocalPTRUpstreams {
		spec, err := ParseUpstream(u)
		if err != nil || !spec.IsIPLit || (spec.Proto != "udp" && spec.Proto != "tcp") {
			return apperr.Invalid("dns.localPtrUpstreams["+strconv.Itoa(i)+"]", "must be a plain DNS server IP")
		}
	}
	switch d.UpstreamMode {
	case "load_balance", "parallel", "strict", "fastest_addr":
	default:
		return apperr.Invalid("dns.upstreamMode", "must be load_balance, parallel, strict or fastest_addr")
	}
	if d.UpstreamTimeoutMs < 500 || d.UpstreamTimeoutMs > 60000 {
		return apperr.Invalid("dns.upstreamTimeoutMs", "must be between 500 and 60000")
	}
	if d.UpstreamBlockedTTL < 10 || d.UpstreamBlockedTTL > 86400 {
		return apperr.Invalid("dns.upstreamBlockedTtl", "must be between 10 and 86400")
	}
	if d.RateLimitIPv4Prefix < 8 || d.RateLimitIPv4Prefix > 32 {
		return apperr.Invalid("dns.rateLimitIpv4Prefix", "must be between 8 and 32")
	}
	if d.RateLimitIPv6Prefix < 32 || d.RateLimitIPv6Prefix > 64 {
		return apperr.Invalid("dns.rateLimitIpv6Prefix", "must be between 32 and 64")
	}
	if d.LocalDomain != "" && !validHostname(d.LocalDomain) {
		return apperr.Invalid("dns.localDomain", "invalid domain")
	}
	for i, n := range d.ServerNames {
		if !validHostname(n) {
			return apperr.Invalid("dns.serverNames["+strconv.Itoa(i)+"]", "invalid hostname")
		}
	}
	if err := validPrefixes("dns.allowedNetworks", d.AllowedNetworks); err != nil {
		return err
	}
	for i, s := range d.AllowedNetworks {
		p, _ := ParsePrefix(s)
		if (p.Addr().Is4() && p.Bits() < 8) || (p.Addr().Is6() && p.Bits() < 32) {
			return apperr.Invalid("dns.allowedNetworks["+strconv.Itoa(i)+"]", "network is too broad; use dns.allowAllNetworks if you really want an open resolver")
		}
	}
	switch d.RouterResolver {
	case "", "auto":
	default:
		if ip, err := netip.ParseAddr(d.RouterResolver); err != nil || ip.Zone() != "" {
			return apperr.Invalid("dns.routerResolver", "must be empty, auto or an IP address")
		}
	}
	if err := validPrefixes("dns.rateLimitExempt", d.RateLimitExempt); err != nil {
		return err
	}
	if d.RateLimitQPS < 0 || d.RateLimitQPS > 100000 {
		return apperr.Invalid("dns.rateLimitQps", "must be between 0 and 100000")
	}
	if d.RateLimitQPS > 0 && d.RateLimitBurst < d.RateLimitQPS {
		return apperr.Invalid("dns.rateLimitBurst", "must be at least rateLimitQps")
	}
	if d.CacheSize < 0 || d.CacheSize > 10_000_000 {
		return apperr.Invalid("dns.cacheSize", "must be between 0 and 10000000")
	}
	if d.CacheMaxTTL != 0 && d.CacheMinTTL > d.CacheMaxTTL {
		return apperr.Invalid("dns.cacheMinTtl", "must not exceed cacheMaxTtl")
	}
	if d.ServeStaleMaxAgeSec < 0 || d.ServeStaleMaxAgeSec > 7*86400 {
		return apperr.Invalid("dns.serveStaleMaxAgeSec", "must be between 0 and 604800")
	}
	if p, err := netip.ParsePrefix(d.DNS64.Prefix); err != nil || !validDNS64Prefix(p) {
		return apperr.Invalid("dns.dns64.prefix", "must be an IPv6 /96 prefix such as %s", DefaultDNS64Prefix)
	}
	if d.DisableAAAA && d.DNS64.Enabled {
		return apperr.Invalid("dns.disableAAAA", "cannot be on together with DNS64 (dns.dns64.enabled)")
	}

	f := a.Filter
	if f.BlockingIPv4 != "" {
		if ip, err := netip.ParseAddr(f.BlockingIPv4); err != nil || !ip.Is4() {
			return apperr.Invalid("filter.blockingIpv4", "must be an IPv4 address")
		}
	}
	if f.BlockingIPv6 != "" {
		if ip, err := netip.ParseAddr(f.BlockingIPv6); err != nil || !ip.Is6() || ip.Is4In6() {
			return apperr.Invalid("filter.blockingIpv6", "must be an IPv6 address")
		}
	}
	switch f.BlockingMode {
	case "null", "nxdomain", "nodata", "refused":
	case "custom_ip":
		ip4, err := netip.ParseAddr(f.BlockingIPv4)
		if err != nil || !ip4.Is4() {
			return apperr.Invalid("filter.blockingIpv4", "must be an IPv4 address in custom_ip mode")
		}
		if f.BlockingIPv6 != "" {
			ip6, err := netip.ParseAddr(f.BlockingIPv6)
			if err != nil || !ip6.Is6() {
				return apperr.Invalid("filter.blockingIpv6", "must be an IPv6 address")
			}
		}
	default:
		return apperr.Invalid("filter.blockingMode", "must be null, nxdomain, nodata, refused or custom_ip")
	}
	if f.BlockedTTL > 86400 {
		return apperr.Invalid("filter.blockedTtl", "must be at most 86400")
	}
	if f.UpdateIntervalHours < 0 || f.UpdateIntervalHours > 24*30 {
		return apperr.Invalid("filter.updateIntervalHours", "must be between 0 and 720")
	}

	l := a.DownloadCache
	for i, s := range l.CacheIPv4 {
		ip, err := netip.ParseAddr(s)
		if err != nil || !ip.Is4() || !inPrefixes(ip, "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16") {
			return apperr.Invalid("downloadCache.cacheIpv4["+strconv.Itoa(i)+"]", "must be a private IPv4 address (10/8, 172.16/12, 192.168/16): Steam, Riot and Origin ignore other cache addresses")
		}
	}
	for i, s := range l.CacheIPv6 {
		ip, err := netip.ParseAddr(s)
		if err != nil || !ip.Is6() || ip.Is4In6() || ip.Zone() != "" || !inPrefixes(ip, "fc00::/7") {
			return apperr.Invalid("downloadCache.cacheIpv6["+strconv.Itoa(i)+"]", "must be a unique local IPv6 address (fc00::/7): clients ignore other IPv6 cache addresses")
		}
	}
	if err := validPrefixes("downloadCache.nocacheClients", l.NocacheClients); err != nil {
		return err
	}
	if l.DNSTTL < 1 || l.DNSTTL > 86400 {
		return apperr.Invalid("downloadCache.dnsTtl", "must be between 1 and 86400")
	}
	if u, err := url.Parse(l.DomainsSource); err != nil || u.Scheme != "https" || u.Host == "" {
		return apperr.Invalid("downloadCache.domainsSource", "must be an https URL")
	}
	if l.UpdateIntervalHours < 0 || l.UpdateIntervalHours > 24*30 {
		return apperr.Invalid("downloadCache.updateIntervalHours", "must be between 0 and 720")
	}

	c := a.Cache
	if c.SliceSizeBytes < 256<<10 || c.SliceSizeBytes > 64<<20 || c.SliceSizeBytes&(c.SliceSizeBytes-1) != 0 {
		return apperr.Invalid("cache.sliceSizeBytes", "must be a power of two between 256 KiB and 64 MiB")
	}
	if c.MaxSizeBytes < 0 {
		return apperr.Invalid("cache.maxSizeBytes", "must not be negative")
	}
	if c.MinFreeBytes < 0 {
		return apperr.Invalid("cache.minFreeBytes", "must not be negative")
	}
	if c.MaxAgeDays < 1 || c.MaxAgeDays > 3650 {
		return apperr.Invalid("cache.maxAgeDays", "must be between 1 and 3650")
	}
	if c.ReadAheadSlices < 0 || c.ReadAheadSlices > 16 {
		return apperr.Invalid("cache.readAheadSlices", "must be between 0 and 16")
	}
	if c.MaxConcurrentFills < 1 || c.MaxConcurrentFills > 1024 {
		return apperr.Invalid("cache.maxConcurrentFills", "must be between 1 and 1024")
	}
	if int64(c.MaxConcurrentFills)*c.SliceSizeBytes > 1<<30 {
		return apperr.Invalid("cache.maxConcurrentFills", "concurrent fills × slice size must not exceed 1 GiB of memory")
	}
	if c.MaxFillsPerClient < 1 || c.MaxFillsPerClient > c.MaxConcurrentFills {
		return apperr.Invalid("cache.maxFillsPerClient", "must be between 1 and maxConcurrentFills")
	}
	if c.ActiveStoreID == "" {
		return apperr.Invalid("cache.activeStoreId", "required")
	}

	g := a.Logs
	if g.QueryLogRetentionHours < 1 || g.QueryLogRetentionHours > 24*366 {
		return apperr.Invalid("logs.queryLogRetentionHours", "must be between 1 and 8784")
	}
	if g.CacheLogRetentionHours < 1 || g.CacheLogRetentionHours > 24*366 {
		return apperr.Invalid("logs.cacheLogRetentionHours", "must be between 1 and 8784")
	}
	if g.SessionRetentionDays < 1 || g.SessionRetentionDays > 3650 {
		return apperr.Invalid("logs.sessionRetentionDays", "must be between 1 and 3650")
	}
	if g.StatsRetentionDays < 1 || g.StatsRetentionDays > 3650 {
		return apperr.Invalid("logs.statsRetentionDays", "must be between 1 and 3650")
	}
	if g.MaxDBSizeMiB < 64 || g.MaxDBSizeMiB > 1<<20 {
		return apperr.Invalid("logs.maxDbSizeMiB", "must be between 64 and 1048576")
	}

	w := a.Web
	if w.SessionIdleMinutes < 5 || w.SessionIdleMinutes > 7*24*60 {
		return apperr.Invalid("web.sessionIdleMinutes", "must be between 5 and 10080")
	}
	if w.SessionMaxHours < 1 || w.SessionMaxHours > 24*90 {
		return apperr.Invalid("web.sessionMaxHours", "must be between 1 and 2160")
	}
	for i, h := range w.AllowedHosts {
		if !validHostname(h) {
			if _, err := netip.ParseAddr(h); err != nil {
				return apperr.Invalid("web.allowedHosts["+strconv.Itoa(i)+"]", "invalid hostname")
			}
		}
	}
	switch w.Language {
	case "", "en", "de":
	default:
		return apperr.Invalid("web.language", "must be empty, en or de")
	}
	if err := w.validateAccess(); err != nil {
		return err
	}
	// updates: two switches, any combination is valid.

	b := a.Backups
	switch b.Schedule {
	case "daily", "weekly":
	default:
		return apperr.Invalid("backups.schedule", "must be daily or weekly")
	}
	if _, _, ok := ParseClock(b.Time); !ok {
		return apperr.Invalid("backups.time", "must be a time of day as HH:MM (00:00 to 23:59)")
	}
	if b.Weekday < 0 || b.Weekday > 6 {
		return apperr.Invalid("backups.weekday", "must be between 0 (Sunday) and 6 (Saturday)")
	}
	if b.Keep < 1 || b.Keep > 90 {
		return apperr.Invalid("backups.keep", "must be between 1 and 90")
	}
	if b.Destination != BackupsLocal && !targetIDRE.MatchString(b.Destination) {
		return apperr.Invalid("backups.destination", "must be local or the id of a storage target")
	}
	return a.DHCP.validate()
}

// validate checks the form of the DHCP settings. The interface, the range,
// the router and PiCache's own address are checked against the live
// interface elsewhere (dhcp.Service.CheckSettings), and only while the
// server is enabled: the interface may be absent while it is off.
func (h *DHCP) validate() error {
	if h.Interface != "" && !validIfaceName(h.Interface) {
		return apperr.Invalid("dhcp.interface", "must be the name of a network interface")
	}
	if h.Enabled && h.Interface == "" {
		return apperr.Invalid("dhcp.interface", "choose the interface to serve")
	}
	var start, end netip.Addr
	for _, f := range []struct {
		field string
		v     string
		dst   *netip.Addr
	}{{"dhcp.rangeStart", h.RangeStart, &start}, {"dhcp.rangeEnd", h.RangeEnd, &end}} {
		if f.v == "" {
			if h.Enabled {
				return apperr.Invalid(f.field, "required while the DHCP server is enabled")
			}
			continue
		}
		ip, err := netip.ParseAddr(f.v)
		if err != nil || !ip.Is4() || !inPrefixes(ip, "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16") {
			return apperr.Invalid(f.field, "must be a private IPv4 address (10/8, 172.16/12, 192.168/16)")
		}
		*f.dst = ip
	}
	if start.IsValid() && end.IsValid() {
		if end.Less(start) {
			return apperr.Invalid("dhcp.rangeEnd", "must not be lower than the range start")
		}
		if n := IPv4Distance(start, end) + 1; n > DHCPMaxPoolSize {
			return apperr.Invalid("dhcp.rangeEnd", "the range may hold at most %d addresses (it holds %d)", DHCPMaxPoolSize, n)
		}
	}
	if h.LeaseSeconds < DHCPMinLeaseSeconds || h.LeaseSeconds > DHCPMaxLeaseSeconds {
		return apperr.Invalid("dhcp.leaseSeconds", "must be between %d (5 minutes) and %d (7 days)", DHCPMinLeaseSeconds, DHCPMaxLeaseSeconds)
	}
	for _, f := range []struct{ field, v string }{{"dhcp.router", h.Router}, {"dhcp.dnsServer", h.DNSServer}} {
		if f.v == "" {
			continue
		}
		ip, err := netip.ParseAddr(f.v)
		if err != nil || !ip.Is4() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
			return apperr.Invalid(f.field, "must be empty or a unicast IPv4 address")
		}
	}
	if h.Domain != "" && !validHostname(h.Domain) {
		return apperr.Invalid("dhcp.domain", "must be empty or a domain name")
	}
	return h.Options.validate()
}

// validate checks the form of the typed DHCP options. The rules that
// depend on the effective domain are in checkSearchList.
func (o *DHCPOptions) validate() error {
	if len(o.NTPServers) > DHCPMaxNTPServers {
		return apperr.Invalid("dhcp.options.ntpServers", "at most %d NTP servers", DHCPMaxNTPServers)
	}
	for i, s := range o.NTPServers {
		field := "dhcp.options.ntpServers[" + strconv.Itoa(i) + "]"
		ip, err := netip.ParseAddr(s)
		if err != nil || !ip.Is4() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
			return apperr.Invalid(field, "must be a unicast IPv4 address")
		}
		if slices.Index(o.NTPServers, s) < i {
			return apperr.Invalid(field, "%s is listed twice", s)
		}
	}
	if o.MTU != 0 && (o.MTU < DHCPMinMTU || o.MTU > DHCPMaxMTU) {
		return apperr.Invalid("dhcp.options.mtu", "must be 0 (none) or between %d and %d", DHCPMinMTU, DHCPMaxMTU)
	}
	if o.WPADURL != "" && !validWPADURL(o.WPADURL) {
		return apperr.Invalid("dhcp.options.wpadUrl",
			"must be empty or an http or https URL of at most %d characters (printable ASCII, international names in punycode, no user name or password)", DHCPMaxWPADURL)
	}
	if len(o.ExtraSearchDomains) > DHCPMaxSearch {
		return apperr.Invalid("dhcp.options.extraSearchDomains", "at most %d extra search domains", DHCPMaxSearch)
	}
	for i, d := range o.ExtraSearchDomains {
		field := "dhcp.options.extraSearchDomains[" + strconv.Itoa(i) + "]"
		if !validHostname(d) {
			return apperr.Invalid(field, "must be a domain name")
		}
		if slices.Index(o.ExtraSearchDomains, d) < i {
			return apperr.Invalid(field, "%s is listed twice", d)
		}
	}
	return nil
}

// validWPADURL reports whether u can be sent as option 252: at most 255
// bytes of printable ASCII (0x21–0x7E), an absolute http or https URL with
// a host and without user info.
func validWPADURL(s string) bool {
	if len(s) > DHCPMaxWPADURL {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.User != nil || u.Opaque != "" || u.Hostname() == "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	return scheme == "http" || scheme == "https"
}

// EffectiveDomain returns the domain the DHCP server hands out: dhcp.domain,
// else dns.localDomain.
func EffectiveDomain(a *All) string {
	if a.DHCP.Domain != "" {
		return a.DHCP.Domain
	}
	return a.DNS.LocalDomain
}

// checkSearchList checks the extra search domains against the effective
// domain (dhcp.domain, else localDomain): none may equal it, and the
// search list of option 119 (the domain first, then the extras, DNS wire
// format without compression) must fit into 255 bytes.
func (h *DHCP) checkSearchList(localDomain string) error {
	domain := h.Domain
	if domain == "" {
		domain = localDomain
	}
	n := 0
	if domain != "" {
		n = len(domain) + 2
	}
	for i, d := range h.Options.ExtraSearchDomains {
		if d == domain {
			return apperr.Invalid("dhcp.options.extraSearchDomains["+strconv.Itoa(i)+"]", "%s is the DHCP domain already", d)
		}
		n += len(d) + 2
	}
	if n > DHCPMaxSearchWire {
		return apperr.Invalid("dhcp.options.extraSearchDomains", "the search list (the domain and the extra domains) must fit into %d bytes", DHCPMaxSearchWire)
	}
	return nil
}

// IPv4Distance returns b − a for IPv4 addresses a ≤ b (0 otherwise).
func IPv4Distance(a, b netip.Addr) int64 {
	if !a.Is4() || !b.Is4() || b.Less(a) {
		return 0
	}
	x, y := a.As4(), b.As4()
	return int64(binary.BigEndian.Uint32(y[:])) - int64(binary.BigEndian.Uint32(x[:]))
}

// validIfaceName reports whether s can be a Linux interface name: 1–15
// bytes, printable ASCII without whitespace, "/" or ":" (netutil has the
// same rule for paths; settings cannot import it).
func validIfaceName(s string) bool {
	if s == "" || len(s) > 15 || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f || c == '/' || c == ':' {
			return false
		}
	}
	return true
}

// targetIDRE matches storage target ids other than the built-in one
// (storage.ValidTargetID; settings cannot import storage).
var targetIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ParseClock parses a time of day "HH:MM" (two digits each, 00:00–23:59).
func ParseClock(s string) (hour, minute int, ok bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, 0, false
	}
	for _, i := range []int{0, 1, 3, 4} {
		if s[i] < '0' || s[i] > '9' {
			return 0, 0, false
		}
	}
	hour, minute = int(s[0]-'0')*10+int(s[1]-'0'), int(s[3]-'0')*10+int(s[4]-'0')
	return hour, minute, hour <= 23 && minute <= 59
}

// validDNS64Prefix reports whether p can hold synthesised addresses: an
// IPv6 /96 (RFC 6052 allows longer embeddings; PiCache uses the /96 form
// only) outside IPv4-mapped, IPv4-compatible, link-local and multicast
// space.
func validDNS64Prefix(p netip.Prefix) bool {
	a := p.Addr()
	return p.Bits() == 96 && a.Is6() && !a.Is4In6() && !a.IsLinkLocalUnicast() && !a.IsMulticast() &&
		!netip.MustParsePrefix("::/96").Contains(a)
}

func validPrefixes(field string, in []string) error {
	for i, s := range in {
		if _, err := ParsePrefix(s); err != nil {
			return apperr.Invalid(field+"["+strconv.Itoa(i)+"]", "must be an IP address or CIDR")
		}
	}
	return nil
}

func inPrefixes(ip netip.Addr, ps ...string) bool {
	for _, p := range ps {
		if netip.MustParsePrefix(p).Contains(ip.Unmap()) {
			return true
		}
	}
	return false
}

// ParsePrefix accepts "10.0.0.0/8", "192.168.1.5" (→ /32) or "fd00::1" (→ /128).
func ParsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return p, err
		}
		return p.Masked(), nil
	}
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	ip = ip.Unmap()
	return netip.PrefixFrom(ip, ip.BitLen()), nil
}

// ParsePrefixes parses a list (invalid entries are skipped; Validate rejects them earlier).
func ParsePrefixes(in []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		if p, err := ParsePrefix(s); err == nil {
			out = append(out, p)
		}
	}
	return out
}
