package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/dhcp"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/version"
)

// registerMetrics registers GET /metrics (Prometheus text format). It is 404
// unless settings.Web.MetricsEnabled and requires an admin API token.
func (s *Server) registerMetrics() {
	s.mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !s.d.Settings.Get().Web.MetricsEnabled {
			writeError(w, r, s.log, errNotFoundRoute)
			return
		}
		p, err := s.d.Auth.Authenticate(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="picache"`)
			writeError(w, r, s.log, err)
			return
		}
		if p.TokenID == 0 || p.Scope != auth.ScopeAdmin {
			writeError(w, r, s.log, apperr.Forbidden("metrics require an admin API token (Authorization: Bearer pc_…)"))
			return
		}
		var buf bytes.Buffer
		writeMetrics(&buf, s.collectMetrics())
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write(buf.Bytes())
	})
}

// metricsSnapshot holds the values exported by /metrics.
type metricsSnapshot struct {
	Version        string
	DNSQueries     int64
	DNSRateLimited int64
	DNSBlocked     int64 // queries of dns.blockedClients (no answer)
	DNSDropped     int64 // queries of dns.droppedDomains (no answer)
	CacheBytesHit  int64
	CacheBytesWAN  int64
	StoreOnline    bool
	StoreBytes     int64
	StoreFreeBytes uint64
	Upstreams      []upstream.UpstreamStat
	LogDropped     uint64
	DNSCache       upstream.CacheStat
	DHCP           *dhcpMetrics // nil without a DHCP service
}

// dhcpMetrics are the DHCPv4 counters and the active leases.
type dhcpMetrics struct {
	Counters     dhcp.Counters
	ActiveLeases int
}

func (s *Server) collectMetrics() metricsSnapshot {
	dns, px, st := s.d.DNS.Stats(), s.d.Proxy.Stats(), s.d.Runtime.StoreState()
	m := metricsSnapshot{
		Version:        version.Version,
		DNSQueries:     dns.Queries,
		DNSRateLimited: dns.RateLimited,
		DNSBlocked:     dns.BlockedClients,
		DNSDropped:     dns.Dropped,
		CacheBytesHit:  px.BytesHit,
		CacheBytesWAN:  px.BytesWAN,
		StoreOnline:    st.Online,
		StoreFreeBytes: st.FreeBytes,
		Upstreams:      s.d.Upstream.Stats(),
		LogDropped:     s.d.Logs.Metrics().Dropped,
		DNSCache:       s.d.Upstream.CacheStats(),
	}
	if s.d.DHCP != nil {
		dm := &dhcpMetrics{Counters: s.d.DHCP.Status().Counters}
		for _, l := range s.d.DHCP.Leases() {
			if l.Active {
				dm.ActiveLeases++
			}
		}
		m.DHCP = dm
	}
	if st.Usage != nil {
		m.StoreBytes = st.Usage.CachedBytes
	}
	return m
}

// writeMetrics renders m in the Prometheus text exposition format 0.0.4.
func writeMetrics(w io.Writer, m metricsSnapshot) {
	family := func(name, typ, help string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	family("picache_build_info", "gauge", "Build information of the running PiCache binary.")
	fmt.Fprintf(w, "picache_build_info{version=\"%s\"} 1\n", promEscapeLabel(m.Version))

	family("picache_dns_queries_total", "counter", "DNS queries received since start.")
	fmt.Fprintf(w, "picache_dns_queries_total %d\n", m.DNSQueries)
	family("picache_dns_rate_limited_total", "counter", "DNS queries dropped or refused by the per-client rate limit since start.")
	fmt.Fprintf(w, "picache_dns_rate_limited_total %d\n", m.DNSRateLimited)
	family("picache_dns_blocked_clients_total", "counter", "DNS queries of blocked clients (dns.blockedClients) that got no answer since start.")
	fmt.Fprintf(w, "picache_dns_blocked_clients_total %d\n", m.DNSBlocked)
	family("picache_dns_dropped_total", "counter", "DNS queries for dropped domains (dns.droppedDomains) that got no answer since start.")
	fmt.Fprintf(w, "picache_dns_dropped_total %d\n", m.DNSDropped)

	family("picache_cache_bytes_hit_total", "counter", "Bytes served from the download cache store since start.")
	fmt.Fprintf(w, "picache_cache_bytes_hit_total %d\n", m.CacheBytesHit)
	family("picache_cache_bytes_wan_total", "counter", "Bytes fetched from upstream CDNs by the download cache proxy since start.")
	fmt.Fprintf(w, "picache_cache_bytes_wan_total %d\n", m.CacheBytesWAN)
	if m.StoreOnline {
		family("picache_cache_store_bytes", "gauge", "Bytes of cached content in the active store.")
		fmt.Fprintf(w, "picache_cache_store_bytes %d\n", m.StoreBytes)
		family("picache_cache_store_free_bytes", "gauge", "Free bytes on the filesystem of the active store.")
		fmt.Fprintf(w, "picache_cache_store_free_bytes %d\n", m.StoreFreeBytes)
	}

	if len(m.Upstreams) > 0 {
		family("picache_upstream_rtt_ms", "gauge", "Smoothed round-trip time of each DNS upstream in milliseconds.")
		for _, u := range m.Upstreams {
			fmt.Fprintf(w, "picache_upstream_rtt_ms{upstream=\"%s\"} %s\n", promEscapeLabel(u.Upstream), promFloat(u.AvgRTTMs))
		}
	}

	family("picache_log_events_dropped_total", "counter", "Log events dropped because the log writer could not keep up.")
	fmt.Fprintf(w, "picache_log_events_dropped_total %d\n", m.LogDropped)

	c := m.DNSCache
	family("picache_dns_cache_entries", "gauge", "Answers in the DNS response cache.")
	fmt.Fprintf(w, "picache_dns_cache_entries %d\n", c.Entries)
	for _, x := range []struct {
		name, help string
		v          int64
	}{
		{"hits", "DNS answers served from the response cache (stale ones included) since start.", c.Hits},
		{"misses", "DNS response cache lookups without a usable answer since start.", c.Misses},
		{"stale_hits", "Stale DNS answers served from the response cache since start.", c.StaleHits},
		{"insertions", "Answers stored in the DNS response cache since start.", c.Insertions},
		{"evictions", "DNS response cache entries removed for the capacity since start.", c.Evictions},
		{"expired", "DNS response cache entries removed after their TTL and the serve-stale window since start.", c.Expired},
	} {
		family("picache_dns_cache_"+x.name+"_total", "counter", x.help)
		fmt.Fprintf(w, "picache_dns_cache_%s_total %d\n", x.name, x.v)
	}

	if d := m.DHCP; d != nil {
		for _, x := range []struct {
			name, help string
			v          int64
		}{
			{"received", "DHCPv4 packets received since start.", d.Counters.Received},
			{"offers", "DHCPOFFER messages sent since start.", d.Counters.Offers},
			{"acks", "DHCPACK messages sent since start.", d.Counters.Acks},
			{"naks", "DHCPNAK messages sent since start.", d.Counters.Naks},
			{"declines", "DHCPDECLINE messages received since start.", d.Counters.Declines},
			{"releases", "DHCPRELEASE messages received since start.", d.Counters.Releases},
			{"informs", "DHCPINFORM messages answered since start.", d.Counters.Informs},
			{"dropped", "DHCPv4 packets dropped (malformed, rate limited or not for this server) since start.", d.Counters.Dropped},
		} {
			family("picache_dhcp_"+x.name+"_total", "counter", x.help)
			fmt.Fprintf(w, "picache_dhcp_%s_total %d\n", x.name, x.v)
		}
		family("picache_dhcp_leases_active", "gauge", "Active DHCPv4 leases.")
		fmt.Fprintf(w, "picache_dhcp_leases_active %d\n", d.ActiveLeases)
	}
}

var promLabelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func promEscapeLabel(s string) string { return promLabelEscaper.Replace(s) }

func promFloat(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
