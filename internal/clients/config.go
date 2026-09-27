package clients

import (
	"net/netip"
	"slices"
	"time"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Config is what the registry reads from the settings (the app applies it
// at start and on every change): the name sources, the local domain of
// host: identifiers, the retention of the seen data and the inputs of the
// WHOIS eligibility.
type Config struct {
	Sources       settings.NameSources
	LocalDomain   string
	SeenRetention time.Duration // logs.seenRetentionDays
	Anonymize     bool          // logs.anonymizeClientIps: no WHOIS lookups
	// AllowedNetworks (dns.allowedNetworks) of /24 or longer (IPv4) or /48
	// or longer (IPv6) are the admin's own networks: no WHOIS lookups.
	AllowedNetworks []netip.Prefix
	// TrustedForwarders (dns.ednsClientTrusted) are never looked up.
	TrustedForwarders []netip.Addr
}

// DefaultConfig is the configuration of the defaults of the settings.
func DefaultConfig() Config {
	d := settings.Defaults()
	return ConfigFrom(&d)
}

// ConfigFrom derives the registry's configuration from the settings.
func ConfigFrom(a *settings.All) Config {
	c := Config{Sources: a.Clients.NameSources, LocalDomain: a.DNS.LocalDomain, Anonymize: a.Logs.AnonymizeClientIPs,
		SeenRetention: time.Duration(a.Logs.SeenRetentionDays) * 24 * time.Hour}
	if c.SeenRetention <= 0 {
		c.SeenRetention = seenRetention
	}
	c.AllowedNetworks = settings.ParsePrefixes(a.DNS.AllowedNetworks)
	for _, s := range a.DNS.EDNSClientTrusted {
		if ip, err := netip.ParseAddr(s); err == nil {
			c.TrustedForwarders = append(c.TrustedForwarders, ip.Unmap())
		}
	}
	return c
}

// config returns the current configuration.
func (r *Registry) config() *Config { return r.cfg.Load() }

// ApplyConfig applies a configuration from the settings: a name source
// switched off drops its names (PTR names are no longer looked up, the
// hosts file no longer read; after the start also the host names stored
// with the seen data), a changed source or local domain drops the cached
// identities (host: identifiers and names), a lowered retention is
// applied within a minute, and WHOIS switched off (or client addresses
// anonymised) drops its queue and cache.
func (r *Registry) ApplyConfig(c Config) {
	c.AllowedNetworks = slices.Clone(c.AllowedNetworks)
	c.TrustedForwarders = slices.Clone(c.TrustedForwarders)
	first := !r.configured.Swap(true)
	old := r.cfg.Swap(&c)
	if !first && ((old.Sources.PTR && !c.Sources.PTR) || (old.Sources.DHCP && !c.Sources.DHCP) ||
		(old.Sources.HostsFile && !c.Sources.HostsFile)) {
		select {
		case r.namesKick <- struct{}{}:
		default:
		}
	}
	if old.Sources.PTR && !c.Sources.PTR {
		r.namesMu.Lock()
		r.names.clear()
		clear(r.queued)
		r.namesMu.Unlock()
	}
	if old.Sources.HostsFile != c.Sources.HostsFile {
		if !c.Sources.HostsFile {
			r.hosts.Store(&hostsTable{})
		}
		select {
		case r.hostsKick <- struct{}{}:
		default:
		}
	}
	if (old.Sources.WHOIS && !c.Sources.WHOIS) || (!old.Anonymize && c.Anonymize) {
		r.whois.reset()
	}
	if old.Sources != c.Sources || old.LocalDomain != c.LocalDomain {
		r.invalidate()
	}
	if c.SeenRetention < old.SeenRetention {
		select {
		case r.pruneKick <- struct{}{}:
		default:
		}
	}
}

// retention returns how long seen data is kept.
func (r *Registry) retention() time.Duration {
	if d := r.config().SeenRetention; d > 0 {
		return d
	}
	return seenRetention
}
