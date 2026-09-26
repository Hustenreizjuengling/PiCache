package app

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/publicsuffix"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Redaction of the support bundle (docs/SECURITY.md "Support bundle"): a
// rule for every leaf of the settings, and a text scrubber for every text
// value of the other files. Names become placeholders name-<n> and MAC
// addresses mac-<n>, the same placeholder for the same value in every
// file; public addresses are masked to /16 or /48; upstreams and URLs are
// reduced in the text as in settings.json.

// settingRule is how a settings leaf is written to settings.json.
type settingRule int

const (
	ruleKeep          settingRule = iota // booleans, numbers, enums, times, ids
	ruleUpstream                         // upstream-like string (scrubUpstream)
	ruleAddr                             // address or CIDR (scrubAddrValue)
	ruleName                             // a name: placeholder unless a default value
	ruleHost                             // a name or an IP literal (web.allowedHosts)
	ruleBlockedClient                    // an address, CIDR or MAC (MACs dropped)
	ruleDroppedDomain                    // "domain[:TYPE]" (the domain as a name)
	ruleRouter                           // "", "auto" or an address
)

// settingRules has a decision for every leaf of settings.All (dotted JSON
// path; array elements share their array's path). A leaf without a rule is
// written as "[not included]"; a test fails for it.
var settingRules = map[string]settingRule{
	"backups.destination": ruleKeep, "backups.enabled": ruleKeep, "backups.includeSecrets": ruleKeep, "backups.keep": ruleKeep,
	"backups.schedule": ruleKeep, "backups.time": ruleKeep, "backups.weekday": ruleKeep,

	"cache.activeStoreId": ruleKeep, "cache.maxAgeDays": ruleKeep, "cache.maxConcurrentFills": ruleKeep,
	"cache.maxFillsPerClient": ruleKeep, "cache.maxSizeBytes": ruleKeep, "cache.minFreeBytes": ruleKeep,
	"cache.readAheadSlices": ruleKeep, "cache.sliceSizeBytes": ruleKeep,

	"dhcp.dnsServer": ruleAddr, "dhcp.domain": ruleName, "dhcp.enabled": ruleKeep, "dhcp.generateNames": ruleKeep,
	"dhcp.ignoreOtherServers": ruleKeep, "dhcp.interface": ruleKeep, "dhcp.ipv6.dhcpv6": ruleKeep,
	"dhcp.ipv6.routerAdvertisements": ruleKeep, "dhcp.leaseSeconds": ruleKeep, "dhcp.onlyReserved": ruleKeep,
	"dhcp.options.extraSearchDomains": ruleName, "dhcp.options.mtu": ruleKeep, "dhcp.options.ntpServers": ruleAddr,
	"dhcp.options.wpadUrl": ruleUpstream, "dhcp.rangeEnd": ruleAddr, "dhcp.rangeStart": ruleAddr, "dhcp.rapidCommit": ruleKeep,
	"dhcp.registerHostnames": ruleKeep, "dhcp.router": ruleAddr,

	"dns.allowAllNetworks": ruleKeep, "dns.allowedNetworks": ruleAddr, "dns.blockedClients": ruleBlockedClient,
	"dns.bogusNxdomain": ruleAddr, "dns.bootstrap": ruleUpstream, "dns.bootstrapPreferIpv6": ruleKeep,
	"dns.cacheEnabled": ruleKeep, "dns.cacheMaxTtl": ruleKeep, "dns.cacheMinTtl": ruleKeep, "dns.cacheSize": ruleKeep,
	"dns.disableAAAA": ruleKeep, "dns.dns64.enabled": ruleKeep, "dns.dns64.prefix": ruleAddr, "dns.dnssec": ruleKeep,
	"dns.domainNeeded": ruleKeep, "dns.droppedDomains": ruleDroppedDomain, "dns.ecs.customSubnet": ruleAddr,
	"dns.ecs.mode": ruleKeep, "dns.ednsClientTrusted": ruleAddr, "dns.fallbackUpstreams": ruleUpstream,
	"dns.localDomain": ruleName, "dns.localPtrUpstreams": ruleUpstream, "dns.privateReverseNetworks": ruleAddr,
	"dns.rateLimitBurst": ruleKeep, "dns.rateLimitExempt": ruleAddr, "dns.rateLimitIpv4Prefix": ruleKeep,
	"dns.rateLimitIpv6Prefix": ruleKeep, "dns.rateLimitQps": ruleKeep, "dns.rebindAllow": ruleName,
	"dns.rebindProtection": ruleKeep, "dns.refuseAny": ruleKeep, "dns.routerResolver": ruleRouter, "dns.serveStale": ruleKeep,
	"dns.serveStaleMaxAgeSec": ruleKeep, "dns.serverNames": ruleName, "dns.trustConnectedNetworks": ruleKeep,
	"dns.upstreamBlockedTtl": ruleKeep, "dns.upstreamMode": ruleKeep, "dns.upstreamTimeoutMs": ruleKeep,
	"dns.upstreams": ruleUpstream,

	"downloadCache.allowPrivateUpstreams": ruleKeep, "downloadCache.cacheIpv4": ruleAddr, "downloadCache.cacheIpv6": ruleAddr,
	"downloadCache.disabledServices": ruleKeep, "downloadCache.dnsTtl": ruleKeep, "downloadCache.domainsSource": ruleUpstream,
	"downloadCache.enabled": ruleKeep, "downloadCache.nocacheClients": ruleAddr, "downloadCache.updateIntervalHours": ruleKeep,

	"filter.blockIcloudPrivateRelay": ruleKeep, "filter.blockMozillaCanary": ruleKeep, "filter.blockedTtl": ruleKeep,
	"filter.blockingIpv4": ruleAddr, "filter.blockingIpv6": ruleAddr, "filter.blockingMode": ruleKeep,
	"filter.cnameInspection": ruleKeep, "filter.enabled": ruleKeep, "filter.pausedUntil": ruleKeep,
	"filter.updateIntervalHours": ruleKeep,

	"health.loadPerCpuMax": ruleKeep, "health.memoryAvailableMinPercent": ruleKeep, "health.temperatureMaxCelsius": ruleKeep,

	"logs.anonymizeClientIps": ruleKeep, "logs.cacheLogRetentionHours": ruleKeep, "logs.flushSeconds": ruleKeep,
	"logs.hideDomains": ruleKeep, "logs.ignoredDomains": ruleName, "logs.maxDbSizeMiB": ruleKeep, "logs.privacyLevel": ruleKeep,
	"logs.queryLogEnabled": ruleKeep, "logs.queryLogRetentionHours": ruleKeep, "logs.sessionRetentionDays": ruleKeep,
	"logs.statsEnabled": ruleKeep, "logs.statsOnlyAddressQueries": ruleKeep, "logs.statsRetentionDays": ruleKeep,

	"updates.checkEnabled": ruleKeep, "updates.includePrereleases": ruleKeep,

	"web.allowedHosts": ruleHost, "web.allowedNetworks": ruleAddr, "web.language": ruleKeep, "web.metricsEnabled": ruleKeep,
	"web.redirectToHttps": ruleKeep, "web.restrictToNetworks": ruleKeep, "web.sessionIdleMinutes": ruleKeep,
	"web.sessionMaxHours": ruleKeep, "web.tlsMinVersion": ruleKeep, "web.trustedProxies": ruleAddr,
}

// Redaction counters of a bundle file (MANIFEST.txt).
const (
	countAddresses = "addresses masked"
	countNames     = "names replaced"
	countMACs      = "MAC addresses replaced"
	countRedacted  = "values redacted"
	countUpstreams = "upstreams and URLs reduced"
	countDropped   = "entries dropped"
	countUnknown   = "members not included"
)

const notIncluded = "[not included]"

// scrubber holds the placeholders of one bundle.
type scrubber struct {
	keepPrivate bool // "include client names": private addresses and MACs kept
	defaults    map[string][]string
	defaultHost map[string]bool   // host names of the default upstream-like values
	names       map[string]string // name → placeholder
	labels      map[string]string // text replacement by whole labels: names and upstream hosts
	nameOrder   []string          // keys of labels, longest first
	upstreams   map[string]string // configured upstream URLs → their settings.json form
	upOrder     []string          // keys of upstreams, longest first
	macs        map[string]string
	counts      map[string]int // of the file being written
}

func newScrubber(keepPrivate bool) *scrubber {
	sc := &scrubber{keepPrivate: keepPrivate, names: map[string]string{}, labels: map[string]string{},
		upstreams: map[string]string{}, macs: map[string]string{}, defaults: map[string][]string{},
		defaultHost: map[string]bool{}, counts: map[string]int{}}
	d := settings.Defaults()
	if raw, err := jsonMarshal(&d); err == nil {
		_, _ = rewriteJSON(raw, func(path string, tok jsontext.Token) (jsontext.Token, bool) {
			if tok.Kind() == '"' {
				sc.defaults[path] = append(sc.defaults[path], tok.String())
				if settingRules[path] == ruleUpstream {
					if h := upstreamHost(tok.String()); h != "" {
						sc.defaultHost[h] = true
					}
				}
			}
			return tok, true
		})
	}
	return sc
}

// jsonMarshal encodes v for the bundle (deterministic member order).
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v, json.Deterministic(true)) }

// upstreamHost returns the lower-case host of an upstream-like value.
func upstreamHost(v string) string {
	if strings.Contains(v, "://") {
		if u, err := url.Parse(v); err == nil {
			return strings.ToLower(u.Hostname())
		}
		return ""
	}
	if h, _, err := net.SplitHostPort(v); err == nil {
		return strings.ToLower(h)
	}
	return strings.ToLower(strings.Trim(v, "[]"))
}

// placeholder returns the placeholder of a name ("localhost" and "picache"
// are kept).
func (sc *scrubber) placeholder(name string) string {
	n := strings.ToLower(strings.TrimSuffix(name, "."))
	if n == "" || n == "localhost" || n == "picache" {
		return name
	}
	if p, ok := sc.names[n]; ok {
		return p
	}
	p := "name-" + strconv.Itoa(len(sc.names)+1)
	sc.names[n] = p
	sc.addLabel(n, p)
	return p
}

// addName registers a configured name (text scrubbing).
func (sc *scrubber) addName(name string) { sc.placeholder(name) }

// addLabel makes the text scrubber replace name (lower-case) where it
// stands as whole labels; the first replacement registered for a name
// stays.
func (sc *scrubber) addLabel(name, with string) {
	if _, ok := sc.labels[name]; ok {
		return
	}
	sc.labels[name] = with
	sc.nameOrder = append(sc.nameOrder, name)
	slices.SortStableFunc(sc.nameOrder, func(a, b string) int { return len(b) - len(a) })
}

// keptAddr reports addresses the settings rules keep: private, ULA,
// loopback, link-local and unspecified.
func keptAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified()
}

// maskPrefix masks an address to /16 (IPv4) or /48 (IPv6), or a prefix to
// at most that length.
func maskPrefix(a netip.Addr, bits int) netip.Prefix {
	a = a.Unmap().WithZone("")
	keep := 16
	if a.Is6() {
		keep = 48
	}
	p, _ := a.Prefix(min(bits, keep))
	return p
}

// scrubAddrValue applies the address rule to an address or CIDR (false
// for anything else).
func (sc *scrubber) scrubAddrValue(v string) (string, bool) {
	if a, err := netip.ParseAddr(v); err == nil {
		if keptAddr(a) {
			return v, true
		}
		sc.counts[countAddresses]++
		return maskPrefix(a, 128).Addr().String(), true
	}
	if p, err := netip.ParsePrefix(v); err == nil {
		if keptAddr(p.Addr()) {
			return v, true
		}
		sc.counts[countAddresses]++
		bits := p.Bits()
		if p.Addr().Is4In6() {
			bits = max(bits-96, 0)
		}
		return maskPrefix(p.Addr(), bits).String(), true
	}
	return "", false
}

// scrubHostName reduces a host name to its eTLD+1 with the labels left of
// it as "*" (abc123.dns.nextdns.io → *.nextdns.io) unless it is a host of
// the defaults; an IP literal gets the address rule.
func (sc *scrubber) scrubHostName(h string) string {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if a, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil {
		v, _ := sc.scrubAddrValue(a.String())
		if strings.HasPrefix(h, "[") {
			return "[" + v + "]"
		}
		return v
	}
	if sc.defaultHost[h] {
		return h
	}
	base, err := publicsuffix.EffectiveTLDPlusOne(h)
	if err != nil {
		sc.counts[countNames]++
		return "*"
	}
	if base == h {
		return h
	}
	sc.counts[countNames]++
	return "*." + base
}

// scrubUpstream reduces an upstream-like value: scheme://host[:port], a
// path other than "", "/" or "/dns-query" as "/…", no user information,
// query or fragment; plain host[:port] values keep their form.
func (sc *scrubber) scrubUpstream(v string) string {
	if !strings.Contains(v, "://") {
		if h, p, err := net.SplitHostPort(v); err == nil {
			return net.JoinHostPort(strings.Trim(sc.scrubHostName(h), "[]"), p)
		}
		return sc.scrubHostName(v)
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		sc.counts[countUnknown]++
		return notIncluded
	}
	host := sc.scrubHostName(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if p := u.Port(); p != "" {
		host += ":" + p
	}
	path := u.EscapedPath()
	if path != "" && path != "/" && path != "/dns-query" {
		path = "/…"
	}
	out := u.Scheme + "://" + host + path
	if out != v {
		sc.counts[countUpstreams]++
	}
	return out
}

// settingValue applies the rule of path to a string value (false: drop the
// array element).
func (sc *scrubber) settingValue(path, v string) (string, bool) {
	rule, ok := settingRules[path]
	if !ok {
		sc.counts[countUnknown]++
		return notIncluded, true
	}
	if v == "" {
		return v, true
	}
	isDefault := slices.Contains(sc.defaults[path], v)
	switch rule {
	case ruleKeep:
		return v, true
	case ruleUpstream:
		return sc.scrubUpstream(v), true
	case ruleRouter:
		if v == "auto" {
			return v, true
		}
		fallthrough
	case ruleAddr:
		if out, ok := sc.scrubAddrValue(v); ok {
			return out, true
		}
		sc.counts[countUnknown]++
		return notIncluded, true
	case ruleName:
		if isDefault {
			return v, true
		}
		sc.counts[countNames]++
		return sc.placeholder(v), true
	case ruleHost:
		if out, ok := sc.scrubAddrValue(v); ok {
			return out, true
		}
		if isDefault {
			return v, true
		}
		sc.counts[countNames]++
		return sc.placeholder(v), true
	case ruleBlockedClient:
		if out, ok := sc.scrubAddrValue(v); ok {
			return out, true
		}
		sc.counts[countDropped]++
		return "", false // MAC entries are dropped
	case ruleDroppedDomain:
		if isDefault {
			return v, true
		}
		domain, typ, hasType := strings.Cut(v, ":")
		sc.counts[countNames]++
		out := sc.placeholder(domain)
		if hasType {
			out += ":" + typ
		}
		return out, true
	}
	sc.counts[countUnknown]++
	return notIncluded, true
}

// registerSettingNames adds the names the settings rules replace (for the
// text scrubber of the other files).
func (sc *scrubber) registerSettingNames(a *settings.All) {
	add := func(path string, vals ...string) {
		for _, v := range vals {
			if v != "" && !slices.Contains(sc.defaults[path], v) {
				if _, err := netip.ParseAddr(v); err != nil {
					sc.addName(v)
				}
			}
		}
	}
	add("web.allowedHosts", a.Web.AllowedHosts...)
	add("dns.serverNames", a.DNS.ServerNames...)
	add("dns.localDomain", a.DNS.LocalDomain)
	add("dhcp.domain", a.DHCP.Domain)
	add("dhcp.options.extraSearchDomains", a.DHCP.Options.ExtraSearchDomains...)
	add("dns.rebindAllow", a.DNS.RebindAllow...)
	for _, d := range a.DNS.DroppedDomains {
		if !slices.Contains(sc.defaults["dns.droppedDomains"], d) {
			domain, _, _ := strings.Cut(d, ":")
			sc.addName(domain)
		}
	}
	add("logs.ignoredDomains", a.Logs.IgnoredDomains...)
	sc.registerUpstreams(a)
}

// registerUpstreams makes the text scrubber apply the upstream rule of
// settings.json (the log names upstreams by their configured value, and
// errors quote DoH URLs): a configured URL is replaced by its reduced form,
// and a host name the rule shortens by that form (abc123.dns.nextdns.io →
// *.nextdns.io; a name without a registrable domain by its name-<n>
// placeholder). Other URLs in the text are reduced by reduceTextURL.
func (sc *scrubber) registerUpstreams(a *settings.All) {
	raw, err := jsonMarshal(a)
	if err != nil {
		return
	}
	counts := sc.counts
	sc.counts = map[string]int{} // registering counts nothing
	defer func() { sc.counts = counts }()
	_, _ = rewriteJSON(raw, func(path string, tok jsontext.Token) (jsontext.Token, bool) {
		v := tok.String()
		if tok.Kind() != '"' || settingRules[path] != ruleUpstream || v == "" {
			return tok, true
		}
		// Only URLs as a whole: a plain address such as 1.1.1.1 could be
		// part of another one; plain values are left to the name and
		// address rules.
		if strings.Contains(v, "://") {
			if _, ok := sc.upstreams[v]; !ok {
				if out := sc.scrubUpstream(v); out != v {
					sc.upstreams[v] = out
					sc.upOrder = append(sc.upOrder, v)
				}
			}
		}
		h := upstreamHost(v)
		if _, err := netip.ParseAddr(h); h == "" || err == nil {
			return tok, true
		}
		switch out := sc.scrubHostName(h); {
		case out == "*":
			sc.addName(h)
		case out != h:
			sc.addLabel(h, out)
		}
		return tok, true
	})
	slices.SortStableFunc(sc.upOrder, func(a, b string) int { return len(b) - len(a) })
}

// scrubSettings writes the settings with the rule of every leaf.
func (sc *scrubber) scrubSettings(a *settings.All) ([]byte, error) {
	raw, err := jsonMarshal(a)
	if err != nil {
		return nil, err
	}
	return rewriteJSON(raw, func(path string, tok jsontext.Token) (jsontext.Token, bool) {
		if tok.Kind() != '"' {
			if _, ok := settingRules[path]; !ok {
				sc.counts[countUnknown]++
				return jsontext.String(notIncluded), true
			}
			return tok, true
		}
		v, keep := sc.settingValue(path, tok.String())
		return jsontext.String(v), keep
	})
}

var (
	macRE = regexp.MustCompile(`(?i)\b[0-9a-f]{2}(?:[:-][0-9a-f]{2}){5}\b`)
	// Candidates for addresses in text: IPv4 (with a prefix length) and
	// IPv6 (with an embedded IPv4 address, a zone or a prefix length).
	ipv4RE = regexp.MustCompile(`[0-9]{1,3}(?:\.[0-9]{1,3}){3}(?:/[0-9]{1,2})?`)
	ipv6RE = regexp.MustCompile(`[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}(?:\.[0-9]{1,3}){0,3}(?:%[0-9A-Za-z._-]+)?(?:/[0-9]{1,3})?`)
	// A URL in text: scheme://, then up to a space, a quote, an angle
	// bracket or another character a URL does not contain unescaped.
	urlRE = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]{0,15}://[^\s"'<>\x60{}|\\^]+`)
)

// scrubText scrubs a text value: configured upstreams and URLs reduced as
// in settings.json, configured names and the host name → placeholders
// (whole labels, case-insensitive), public addresses masked, private ones
// too and MAC addresses as placeholders unless client names are included.
func (sc *scrubber) scrubText(s string) string {
	if s == "" {
		return s
	}
	if !sc.keepPrivate {
		s = macRE.ReplaceAllStringFunc(s, func(m string) string {
			hw, err := net.ParseMAC(m)
			if err != nil {
				return m
			}
			key := hw.String()
			p, ok := sc.macs[key]
			if !ok {
				p = "mac-" + strconv.Itoa(len(sc.macs)+1)
				sc.macs[key] = p
			}
			sc.counts[countMACs]++
			return p
		})
	}
	for _, u := range sc.upOrder {
		if n := strings.Count(s, u); n > 0 {
			s = strings.ReplaceAll(s, u, sc.upstreams[u])
			sc.counts[countUpstreams] += n
		}
	}
	if strings.Contains(s, "://") {
		s = urlRE.ReplaceAllStringFunc(s, sc.reduceTextURL)
	}
	s = replaceBounded(s, ipv6RE, func(c byte) bool { return isHex(c) || c == ':' }, sc.scrubAddrToken)
	s = replaceBounded(s, ipv4RE, func(c byte) bool { return c >= '0' && c <= '9' || c == '.' }, sc.scrubAddrToken)
	for _, n := range sc.nameOrder {
		s = replaceLabel(s, n, sc.labels[n], sc.counts)
	}
	return s
}

// reduceTextURL reduces a URL found in text like an upstream of
// settings.json: scheme://host[:port], a path other than "", "/" or
// "/dns-query" as "/…", no user information, query or fragment (DoH paths
// and query strings can hold secrets). The host is left to the name and
// address rules; punctuation at the end is kept as text.
func (sc *scrubber) reduceTextURL(m string) string {
	end := len(m)
	for end > 0 && strings.IndexByte(".,;:!?)]'", m[end-1]) >= 0 {
		end--
	}
	u, tail := m[:end], m[end:]
	scheme, rest, _ := strings.Cut(u, "://")
	host, path := rest, ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		host, path = rest[:i], rest[i:]
	}
	if i := strings.LastIndexByte(host, '@'); i >= 0 {
		host = host[i+1:]
	}
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	if path != "" && path != "/" && path != "/dns-query" {
		path = "/…"
	}
	out := scheme + "://" + host + path
	if out != u {
		sc.counts[countUpstreams]++
	}
	return out + tail
}

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

// replaceBounded replaces the matches of re in s that are not glued to
// more address characters (glued reports them; a dot that ends a sentence
// is no glue) with fn(match).
func replaceBounded(s string, re *regexp.Regexp, glued func(byte) bool, fn func(string) string) string {
	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringIndex(s, -1) {
		i, j := m[0], m[1]
		if (i > 0 && glued(s[i-1])) || (j < len(s) && glued(s[j]) && !(s[j] == '.' && (j+1 == len(s) || !glued(s[j+1])))) {
			continue
		}
		if r := fn(s[i:j]); r != s[i:j] {
			b.WriteString(s[last:i])
			b.WriteString(r)
			last = j
		}
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// scrubAddrToken masks an address or prefix found in text (loopback and
// unspecified addresses are kept).
func (sc *scrubber) scrubAddrToken(t string) string {
	mask := func(a netip.Addr) bool {
		a = a.Unmap()
		if a.IsLoopback() || a.IsUnspecified() {
			return false
		}
		return !keptAddr(a) || !sc.keepPrivate
	}
	if a, err := netip.ParseAddr(t); err == nil {
		if !mask(a) {
			return t
		}
		sc.counts[countAddresses]++
		return maskPrefix(a, 128).Addr().String()
	}
	if p, err := netip.ParsePrefix(t); err == nil {
		if !mask(p.Addr()) {
			return t
		}
		sc.counts[countAddresses]++
		return maskPrefix(p.Addr(), p.Bits()).String()
	}
	return t
}

// isLabelByte reports the bytes of a DNS label.
func isLabelByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

// replaceLabel replaces name (lower-case) in s where it stands as whole
// labels (case-insensitive): the bytes before and after are no label bytes
// (a dot is none: "nas.smith.home" has the names "nas" and "smith.home").
func replaceLabel(s, name, with string, counts map[string]int) string {
	lower := asciiLower(s) // same length as s: the indexes apply to both
	var b strings.Builder
	last := 0
	for i := 0; i <= len(lower)-len(name); {
		j := strings.Index(lower[i:], name)
		if j < 0 {
			break
		}
		j += i
		end := j + len(name)
		before := j == 0 || !isLabelByte(lower[j-1])
		after := end == len(lower) || !isLabelByte(lower[end])
		if before && after {
			b.WriteString(s[last:j])
			b.WriteString(with)
			last = end
			counts[countNames]++
		}
		i = j + 1
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// scrubJSON scrubs every string value (not the member names) of a JSON
// document with scrubText.
func (sc *scrubber) scrubJSON(raw []byte) ([]byte, error) {
	return rewriteJSON(raw, func(_ string, tok jsontext.Token) (jsontext.Token, bool) {
		if tok.Kind() == '"' {
			return jsontext.String(sc.scrubText(tok.String())), true
		}
		return tok, true
	})
}

// asciiLower lower-cases the ASCII letters of s (unlike strings.ToLower it
// never changes the length).
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// jsonFrame is an open object or array of rewriteJSON.
type jsonFrame struct {
	obj  bool
	name string // the member being read (objects)
}

// rewriteJSON copies a JSON document (indented) and passes every value
// that is not an object or array to fn with its dotted member path (array
// elements have their array's path); fn returns the value to write and
// false to drop an array element.
func rewriteJSON(raw []byte, fn func(path string, tok jsontext.Token) (jsontext.Token, bool)) ([]byte, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(raw))
	var out bytes.Buffer
	enc := jsontext.NewEncoder(&out, jsontext.Multiline(true), jsontext.WithIndent("  "))
	var stack []jsonFrame
	path := func() string {
		var parts []string
		for _, f := range stack {
			if f.obj && f.name != "" {
				parts = append(parts, f.name)
			}
		}
		return strings.Join(parts, ".")
	}
	for {
		tok, err := dec.ReadToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch tok.Kind() {
		case '{', '[':
			if err := enc.WriteToken(tok); err != nil {
				return nil, err
			}
			stack = append(stack, jsonFrame{obj: tok.Kind() == '{'})
			continue
		case '}', ']':
			if err := enc.WriteToken(tok); err != nil {
				return nil, err
			}
			stack = stack[:len(stack)-1]
			continue
		}
		if n := len(stack); n > 0 && stack[n-1].obj {
			if _, length := dec.StackIndex(dec.StackDepth()); length%2 == 1 { // a member name
				stack[n-1].name = tok.String()
				if err := enc.WriteToken(tok); err != nil {
					return nil, err
				}
				continue
			}
		}
		nt, keep := fn(path(), tok)
		if !keep && (len(stack) == 0 || stack[len(stack)-1].obj) {
			nt, keep = jsontext.Null, true // only array elements can be dropped
		}
		if !keep {
			continue
		}
		if err := enc.WriteToken(nt); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

// jsonToken is a token of rewriteJSON.
type jsonToken = jsontext.Token
