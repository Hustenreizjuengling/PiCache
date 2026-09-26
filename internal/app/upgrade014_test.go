package app

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// dns013 is a copy of the DNS section type of 0.13 (the downgrade
// direction: its store decodes a newer document on top of its defaults and
// ignores unknown members).
type dns013 struct {
	Upstreams           []string     `json:"upstreams"`
	FallbackUpstreams   []string     `json:"fallbackUpstreams"`
	Bootstrap           []string     `json:"bootstrap"`    // plain IPs, used only to resolve upstream hostnames
	UpstreamMode        string       `json:"upstreamMode"` // load_balance | parallel | strict | fastest_addr
	UpstreamTimeoutMs   int          `json:"upstreamTimeoutMs"`
	UpstreamBlockedTTL  int          `json:"upstreamBlockedTtl"`
	BootstrapPreferIPv6 bool         `json:"bootstrapPreferIpv6"`
	ECS                 settings.ECS `json:"ecs"`
	LocalPTRUpstreams   []string     `json:"localPtrUpstreams"` // resolvers for private reverse zones (e.g. the router)
	LocalDomain         string       `json:"localDomain"`       // e.g. "lan" or "fritz.box"; never sent to public upstreams
	ServerNames         []string     `json:"serverNames"`       // names answered with this server's addresses
	RouterResolver      string       `json:"routerResolver"`

	AllowedNetworks        []string `json:"allowedNetworks"`  // extra client CIDRs beyond the private defaults
	AllowAllNetworks       bool     `json:"allowAllNetworks"` // DANGEROUS: open resolver
	TrustConnectedNetworks bool     `json:"trustConnectedNetworks"`
	BlockedClients         []string `json:"blockedClients"`
	RateLimitQPS           int      `json:"rateLimitQps"` // per rate-limit key (netutil.RateKey); 0 disables
	RateLimitBurst         int      `json:"rateLimitBurst"`
	RateLimitIPv4Prefix    int      `json:"rateLimitIpv4Prefix"`
	RateLimitIPv6Prefix    int      `json:"rateLimitIpv6Prefix"`
	RateLimitExempt        []string `json:"rateLimitExempt"` // CIDRs (loopback, router, forwarder targets and trusted EDNS sources are exempt automatically)
	RefuseANY              bool     `json:"refuseAny"`
	EDNSClientTrusted      []string `json:"ednsClientTrusted"`

	RebindProtection       bool     `json:"rebindProtection"`
	RebindAllow            []string `json:"rebindAllow"`
	DomainNeeded           bool     `json:"domainNeeded"`
	PrivateReverseNetworks []string `json:"privateReverseNetworks"`
	DroppedDomains         []string `json:"droppedDomains"`
	BogusNXDomain          []string `json:"bogusNxdomain"`

	CacheEnabled        bool   `json:"cacheEnabled"`
	CacheSize           int    `json:"cacheSize"` // entries
	CacheMinTTL         uint32 `json:"cacheMinTtl"`
	CacheMaxTTL         uint32 `json:"cacheMaxTtl"` // 0 = no cap
	ServeStale          bool   `json:"serveStale"`
	ServeStaleMaxAgeSec int    `json:"serveStaleMaxAgeSec"`
	DNSSEC              bool   `json:"dnssec"` // set DO upstream and pass AD through (no local validation)

	DisableAAAA bool           `json:"disableAAAA"`
	DNS64       settings.DNS64 `json:"dns64"`

	LocalRecordsEnabled bool                         `json:"localRecordsEnabled"`
	LocalizeRecords     string                       `json:"localizeRecords"`
	ServerNameAddresses settings.ServerNameAddresses `json:"serverNameAddresses"`
}

// parseIdentifier013 is a copy of 0.13's identifier rule: an IP address, a
// CIDR or a MAC address; anything else is skipped.
func parseIdentifier013(s string) (kind string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64 {
		return "", false
	}
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil || p.Addr().Zone() != "" {
			return "", false
		}
		return "cidr", true
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return "ip", true
	}
	hw, err := net.ParseMAC(s)
	if err != nil || len(hw) != 6 {
		return "", false
	}
	return "mac", true
}

// A picache.db and logs.db of 0.13 start under 0.14: the settings decode
// with plain DNS on and encrypted DNS off, logs v5 keeps the stored rows,
// and a clientid: identifier stored afterwards identifies. The other way,
// 0.13 decodes a 0.14 document and skips clientid: identifiers.
func TestUpgradeFrom013(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	cfgPath, logsPath := filepath.Join(dir, "picache.db"), filepath.Join(dir, "logs.db")
	migrateConfig(t, cfgPath, nil) // 0.14 has no picache.db schema step

	// The settings document of 0.13: no dns.plainDns, no dns.encrypted.
	raw, err := json.Marshal(settings.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc["dns"], "plainDns")
	delete(doc["dns"], "encrypted")
	raw, _ = json.Marshal(doc)
	cdb, err := db.Open(cfgPath, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	if _, err := cdb.W.Exec(`INSERT INTO settings (id, doc, updated_at) VALUES (1, ?, 0)`, string(raw)); err != nil {
		t.Fatal(err)
	}
	set, err := settings.Open(ctx, cdb, log)
	if err != nil {
		t.Fatal(err)
	}
	if d := set.Get().DNS; !d.PlainDNS || d.Encrypted != (settings.EncryptedDNS{}) {
		t.Fatalf("decoded %v %+v", d.PlainDNS, d.Encrypted)
	}

	// logs.db of 0.13 (logs v4) with a query row.
	ldb, err := db.Open(logsPath, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer ldb.Close()
	if err := ldb.Migrate(ctx, "logs", logs.Migrations()[:4]); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := ldb.W.Exec(`INSERT INTO logs_queries (ts, client_ip, qname, qtype, status, rcode, upstream_ede_code, ecs, upstream_answer)
		VALUES (?, '10.0.0.1', 'old.example', 'A', 'forwarded', 'NOERROR', -1, '', '')`, now.Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	st, err := logs.New(ctx, ldb, set, log)
	if err != nil {
		t.Fatal(err)
	}
	page, err := st.QueryLog(ctx, logs.QueryFilter{From: now.Add(-time.Hour), To: now})
	if err != nil || len(page.Items) != 1 || page.Items[0].QName != "old.example" {
		t.Fatalf("stored rows %+v %v", page.Items, err)
	}
	_ = st.Close()

	reg, err := clients.New(ctx, cdb, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	c, err := reg.CreateClient(ctx, clients.ClientInput{Name: "Tablet", Identifiers: []string{"ClientID:Tablet", "192.168.1.50"}})
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := reg.IdentifyDNSClientID("tablet"); !ok || id.ClientID != c.ID {
		t.Fatalf("clientid identifier: %+v %v", id, ok)
	}

	// Downgrade: 0.13 decodes a document of 0.14 without error, and its
	// identifier rule skips clientid: values.
	next := settings.Defaults()
	next.DNS.PlainDNS = false
	next.DNS.Encrypted = settings.EncryptedDNS{DoT: true, ServerName: "dns.lan"}
	raw, _ = json.Marshal(next)
	var old struct {
		DNS dns013 `json:"dns"`
	}
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatalf("0.13 decode: %v", err)
	}
	if len(old.DNS.Upstreams) == 0 {
		t.Fatal("0.13 decode lost the upstreams")
	}
	for _, v := range c.Identifiers {
		kind, ok := parseIdentifier013(v)
		if strings.HasPrefix(v, "clientid:") == ok {
			t.Fatalf("0.13 identifier rule on %q: %s %v", v, kind, ok)
		}
	}
}
