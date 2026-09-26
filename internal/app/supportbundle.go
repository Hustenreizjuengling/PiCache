package app

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/applog"
	"github.com/hustenreizjuengling/picache/internal/dhcp"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/version"
)

// The support bundle (POST /system/support-bundle, docs/SECURITY.md): a zip
// of an allowlist of files built in memory from cached data (never a
// network scan), at most 16 MiB (log.ndjson is cut oldest first to fit).
// settings.json has a rule for every leaf (bundlescrub.go); every text of
// the other files goes through the text scrubber; without
// includeClientNames private addresses and MAC addresses are replaced too.
// Nothing else is ever added: no accounts, sessions, tokens, audit log,
// query log, cache or SNI events, leases, reservations, clients, groups,
// filter lists or rules, storage targets, notification channels, backups
// or keys.

// supportBundleMax bounds the bundle (the uncompressed files together;
// tests lower it).
var supportBundleMax = 16<<20 - 64<<10

// Bundle file names in the order they are written.
const (
	bundleManifest = "MANIFEST.txt"
	bundleLog      = "log.ndjson"
)

// logKeysAlways are log attributes whose values are always redacted in the
// bundle; logKeysPrivate are redacted without includeClientNames.
var (
	logKeysAlways  = []string{"username", "user", "by"}
	logKeysPrivate = []string{"name", "hostname", "clientName", "sni", "host", "qname", "domain", "groups", "mac"}
)

// bundleVersion is version.json.
type bundleVersion struct {
	Version    any       `json:"version"`
	StartedAt  time.Time `json:"startedAt"`
	UptimeSec  int64     `json:"uptimeSec"`
	Deployment string    `json:"deployment"` // systemd | docker | other
}

// bundleFile is a file of the bundle with its redaction counts.
type bundleFile struct {
	name   string
	data   []byte
	counts map[string]int
}

// SupportBundle builds the support bundle.
func (a *App) SupportBundle(ctx context.Context, includeClientNames bool) ([]byte, error) {
	sc := newScrubber(includeClientNames)
	set := a.set.Get()
	sc.registerSettingNames(set)
	for _, h := range a.cfg.WebHosts {
		if _, err := netip.ParseAddr(h); err != nil {
			sc.addName(h)
		}
	}
	if host, err := os.Hostname(); err == nil {
		sc.addName(host)
	}
	if a.clients != nil {
		groups, err := a.clients.GroupUpstreamConfigs(ctx)
		if err != nil {
			return nil, err
		}
		sc.registerGroups(groups)
	}
	var files []bundleFile
	add := func(name string, data []byte) {
		files = append(files, bundleFile{name: name, data: data, counts: sc.counts})
		sc.counts = map[string]int{}
	}
	addJSON := func(name string, v any, scrub bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := jsonMarshal(v)
		if err != nil {
			return err
		}
		if scrub {
			raw, err = sc.scrubJSON(raw)
		} else {
			raw, err = rewriteJSON(raw, func(_ string, t jsonToken) (jsonToken, bool) { return t, true })
		}
		if err != nil {
			return err
		}
		add(name, append(raw, '\n'))
		return nil
	}

	started := a.StartedAt()
	if err := addJSON("version.json", bundleVersion{Version: version.Get(), StartedAt: started.UTC(),
		UptimeSec: int64(time.Since(started).Seconds()), Deployment: a.deployment()}, false); err != nil {
		return nil, err
	}
	settingsJSON, err := sc.scrubSettings(set)
	if err != nil {
		return nil, err
	}
	add("settings.json", append(settingsJSON, '\n'))
	if err := addJSON("health.json", a.Health(ctx), true); err != nil {
		return nil, err
	}
	if err := addJSON("listeners.json", a.Listeners(), true); err != nil {
		return nil, err
	}
	var nc any = map[string]any{"available": false}
	if a.network != nil {
		c := a.network.Check(ctx)
		if includeClientNames {
			nc = c
		} else {
			nc = reducedNetworkCheck(&c)
		}
	}
	if err := addJSON("network-check.json", nc, true); err != nil {
		return nil, err
	}
	var dh any = map[string]any{"available": false}
	if a.dhcp != nil {
		st := a.dhcp.Status()
		if includeClientNames {
			dh = st
		} else {
			dh = reducedDHCP(&st)
		}
	}
	if err := addJSON("dhcp.json", dh, true); err != nil {
		return nil, err
	}
	var lm logs.Metrics
	if a.logs != nil {
		lm = a.logs.Metrics()
	}
	if err := addJSON("databases.json", map[string]any{"databases": a.Databases(), "logs": lm}, true); err != nil {
		return nil, err
	}
	if err := addJSON("host.json", a.HostInfo(), true); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// log.ndjson: oldest first; the oldest records are cut to fit.
	used := 8 << 10 // the manifest
	for _, f := range files {
		used += len(f.data)
	}
	var recs []applog.Record
	if a.appLog != nil {
		recs = a.appLog.Records(slog.LevelDebug, "", applog.Capacity)
	}
	lines := make([][]byte, 0, len(recs))
	for i := len(recs) - 1; i >= 0; i-- { // newest first → oldest first
		b, err := jsonMarshal(sc.scrubRecord(recs[i]))
		if err != nil {
			return nil, err
		}
		lines = append(lines, append(b, '\n'))
	}
	size := 0
	for _, l := range lines {
		size += len(l)
	}
	cut := 0
	for cut < len(lines) && used+size > supportBundleMax {
		size -= len(lines[cut])
		cut++
	}
	add(bundleLog, bytes.Join(lines[cut:], nil))

	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	now := time.Now().UTC()
	write := func(name string, data []byte) error {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: now})
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	if err := write(bundleManifest, bundleManifestText(files, includeClientNames, cut, now)); err != nil {
		return nil, err
	}
	for _, f := range files {
		if err := write(f.name, f.data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return zbuf.Bytes(), nil
}

// bundleManifestText is MANIFEST.txt.
func bundleManifestText(files []bundleFile, includeClientNames bool, cut int, now time.Time) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "PiCache support bundle\n\nversion: %s\ncreated: %s\nincludeClientNames: %v\n\n", version.Version,
		now.Format(time.RFC3339), includeClientNames)
	b.WriteString("Review these files before sharing them.\n\nFiles:\n")
	b.WriteString("  " + bundleManifest + "\n")
	for _, f := range files {
		fmt.Fprintf(&b, "  %s (%d bytes)\n", f.name, len(f.data))
	}
	b.WriteString(`
Redaction rules:
  settings.json: every member has a rule. Upstreams and URLs are reduced to
    scheme://host[:port] (a path as /…); host names other than the defaults
    keep their registrable domain only (*.example.net). Public addresses and
    networks are masked to /16 (IPv4) or /48 (IPv6); private, loopback,
    link-local and ULA ones are kept. MAC addresses of dns.blockedClients
    are dropped. Configured names become name-<n> unless they are defaults.
  Other files: the configured names, PICACHE_WEB_HOSTS and the host name
    become the same name-<n> placeholders; the configured upstreams (also
    those of client groups) and their host names are reduced as in
    settings.json, and every URL to scheme://host[:port] (a path as /…, no
    query); public addresses are masked to /16 or /48, private ones too, MAC
    addresses become mac-<n> and the names of groups with their own
    upstreams name-<n> unless client names are included. log.ndjson: the
    values of username, user and by are redacted, without client names also
    name, hostname, clientName, sni, host, qname, domain, groups and mac;
    secrets were never logged.
  Never included: accounts, sessions, tokens, the audit log, the query log,
    cache and SNI events, leases, reservations, clients, groups, filter lists
    and rules, storage targets, notification channels, backups and keys.

Applied:
`)
	for _, f := range files {
		keys := make([]string, 0, len(f.counts))
		for k, n := range f.counts {
			if n > 0 {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%d %s", f.counts[k], k))
		}
		if f.name == bundleLog && cut > 0 {
			parts = append(parts, fmt.Sprintf("%d oldest records left out (size limit)", cut))
		}
		if len(parts) == 0 {
			parts = append(parts, "nothing")
		}
		fmt.Fprintf(&b, "  %s: %s\n", f.name, strings.Join(parts, ", "))
	}
	return []byte(b.String())
}

// scrubRecord scrubs an application log record for the bundle.
func (sc *scrubber) scrubRecord(r applog.Record) applog.Record {
	r.Msg = sc.scrubText(r.Msg)
	attrs := make([]applog.Attr, len(r.Attrs))
	for i, at := range r.Attrs {
		key := at.Key
		if j := strings.LastIndexByte(key, '.'); j >= 0 {
			key = key[j+1:]
		}
		switch {
		case slices.Contains(logKeysAlways, key), !sc.keepPrivate && slices.Contains(logKeysPrivate, key):
			if at.Value != "" {
				sc.counts[countRedacted]++
				at.Value = "[redacted]"
			}
		default:
			at.Value = sc.scrubText(at.Value)
		}
		attrs[i] = at
	}
	r.Attrs = attrs
	return r
}

// reducedNetworkCheck is network-check.json without client names: counts
// and statuses only.
func reducedNetworkCheck(c *api.NetworkCheck) map[string]any {
	prefixes := []string{}
	for _, g := range c.Self.Global {
		if a, err := netip.ParseAddr(g); err == nil {
			if p := maskPrefix(a, 48).String(); !slices.Contains(prefixes, p) {
				prefixes = append(prefixes, p)
			}
		}
	}
	checks := make([]map[string]string, 0, len(c.Checks))
	for _, it := range c.Checks {
		checks = append(checks, map[string]string{"id": it.ID, "status": it.Status})
	}
	devices := map[string]int{"total": len(c.Devices), "active": 0, "inactive": 0, "never": 0}
	for _, d := range c.Devices {
		if _, ok := devices[d.Status]; ok {
			devices[d.Status]++
		}
	}
	routerKind := ""
	if c.Router != nil {
		routerKind = c.Router.Kind
	}
	return map[string]any{
		"checkedAt": c.CheckedAt, "mode": c.Mode, "statsAvailable": c.StatsAvailable, "routerKind": routerKind,
		"self": map[string]any{"ipv4": len(c.Self.IPv4), "ula": len(c.Self.ULA), "global": len(c.Self.Global),
			"globalPrefixes": prefixes, "dnsIpv6": c.Self.DNSIPv6},
		"queries24h": c.Queries24h, "checks": checks, "devices": devices, "dhcp": c.DHCP,
	}
}

// reducedDHCP is dhcp.json without client names: state, counts and
// blockers (never leases or reservations).
func reducedDHCP(st *dhcp.Status) map[string]any {
	out := map[string]any{
		"available": st.Available, "state": st.State, "deployment": st.Deployment, "blockers": st.Blockers,
		"counters": st.Counters, "otherServers": len(st.OtherServers),
	}
	if st.ReasonCode != "" {
		out["reasonCode"] = st.ReasonCode
	}
	if p := st.Pool; p != nil {
		out["pool"] = map[string]int{"size": p.Size, "used": p.Used, "static": p.Static}
	}
	ra := map[string]any{"enabled": st.IPv6.RouterAdvertisements.Enabled, "available": st.IPv6.RouterAdvertisements.Available,
		"state": st.IPv6.RouterAdvertisements.State, "blockers": st.IPv6.RouterAdvertisements.Blockers}
	if rc := st.IPv6.RouterAdvertisements.ReasonCode; rc != "" {
		ra["reasonCode"] = rc
	}
	out["ipv6"] = map[string]any{
		"routerAdvertisements": ra,
		"dhcpv6": map[string]any{"enabled": st.IPv6.DHCPv6.Enabled, "state": st.IPv6.DHCPv6.State,
			"blockers": st.IPv6.DHCPv6.Blockers},
		"otherAnnouncers": len(st.IPv6.OtherAnnouncers),
	}
	return out
}
