package settings

import (
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
	Host    string // IP literal (udp/tcp) or hostname/IP (tls/https)
	Port    int
	URL     string // full URL for https
	IsIPLit bool   // Host is an IP literal
}

// Addr returns host:port.
func (u UpstreamSpec) Addr() string { return net.JoinHostPort(u.Host, strconv.Itoa(u.Port)) }

// ParseUpstream parses and validates an upstream string.
//
//	9.9.9.9 | 9.9.9.9:53 | [2620:fe::fe]:53 | udp://… | tcp://… | tls://dns.quad9.net[:853] | https://dns.quad9.net/dns-query
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
		if !spec.IsIPLit {
			return spec, errors.New("plain DNS upstreams must be IP addresses")
		}
		if u.Path != "" && u.Path != "/" {
			return spec, errors.New("plain DNS upstreams take no path")
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
	d.Bootstrap = clean(d.Bootstrap, true)
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
	b := &a.Backups
	b.Schedule = strings.ToLower(strings.TrimSpace(b.Schedule))
	b.Time = strings.TrimSpace(b.Time)
	b.Destination = strings.ToLower(strings.TrimSpace(b.Destination))
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
	needBootstrap := false
	for i, u := range d.Upstreams {
		spec, err := ParseUpstream(u)
		if err != nil {
			return apperr.Invalid("dns.upstreams["+strconv.Itoa(i)+"]", "%v", err)
		}
		if !spec.IsIPLit {
			needBootstrap = true
		}
	}
	for i, b := range d.Bootstrap {
		if _, err := netip.ParseAddr(b); err != nil {
			return apperr.Invalid("dns.bootstrap["+strconv.Itoa(i)+"]", "must be an IP address")
		}
	}
	if needBootstrap && len(d.Bootstrap) == 0 {
		return apperr.Invalid("dns.bootstrap", "required when an upstream is given by hostname")
	}
	for i, u := range d.LocalPTRUpstreams {
		spec, err := ParseUpstream(u)
		if err != nil || !spec.IsIPLit || (spec.Proto != "udp" && spec.Proto != "tcp") {
			return apperr.Invalid("dns.localPtrUpstreams["+strconv.Itoa(i)+"]", "must be a plain DNS server IP")
		}
	}
	switch d.UpstreamMode {
	case "load_balance", "parallel", "strict":
	default:
		return apperr.Invalid("dns.upstreamMode", "must be load_balance, parallel or strict")
	}
	if d.UpstreamTimeoutMs < 500 || d.UpstreamTimeoutMs > 60000 {
		return apperr.Invalid("dns.upstreamTimeoutMs", "must be between 500 and 60000")
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
	return nil
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
