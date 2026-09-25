package settings

// Defaults is the single source of truth for default settings
// (docs/ARCHITECTURE.md section 13).
func Defaults() All {
	return All{
		DNS: DNS{
			Upstreams:           []string{"https://dns.quad9.net/dns-query", "https://cloudflare-dns.com/dns-query"},
			Bootstrap:           []string{"9.9.9.9", "149.112.112.112", "1.1.1.1", "1.0.0.1"},
			UpstreamMode:        "load_balance",
			UpstreamTimeoutMs:   10000,
			LocalPTRUpstreams:   []string{},
			LocalDomain:         "lan",
			ServerNames:         []string{"picache"},
			RouterResolver:      "auto",
			AllowedNetworks:     []string{},
			RateLimitQPS:        50,
			RateLimitBurst:      200,
			RateLimitExempt:     []string{},
			RefuseANY:           true,
			CacheEnabled:        true,
			CacheSize:           10000,
			ServeStale:          true,
			ServeStaleMaxAgeSec: 3600,
		},
		Filter: Filter{
			Enabled:                 true,
			BlockingMode:            "null",
			BlockedTTL:              10,
			CNAMEInspection:         true,
			UpdateIntervalHours:     24,
			BlockMozillaCanary:      true,
			BlockICloudPrivateRelay: true,
		},
		DownloadCache: DownloadCache{
			Enabled:             false,
			CacheIPv4:           []string{},
			CacheIPv6:           []string{},
			DNSTTL:              60,
			DomainsSource:       "https://raw.githubusercontent.com/uklans/cache-domains/master/",
			UpdateIntervalHours: 24,
			DisabledServices:    []string{"test"},
			NocacheClients:      []string{},
		},
		Cache: Cache{
			SliceSizeBytes:     1 << 20,
			MinFreeBytes:       10 << 30,
			MaxAgeDays:         365,
			ReadAheadSlices:    2,
			MaxConcurrentFills: 64,
			MaxFillsPerClient:  32,
			ActiveStoreID:      "local",
		},
		Logs: Logs{
			QueryLogEnabled:        true,
			QueryLogRetentionHours: 168,
			CacheLogRetentionHours: 48,
			SessionRetentionDays:   90,
			StatsRetentionDays:     365,
			MaxDBSizeMiB:           2048,
		},
		Web: Web{
			SessionIdleMinutes: 60,
			SessionMaxHours:    168,
			AllowedHosts:       []string{},
		},
		Updates: Updates{
			CheckEnabled: true,
		},
	}
}
