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
	CacheBytesHit  int64
	CacheBytesWAN  int64
	StoreOnline    bool
	StoreBytes     int64
	StoreFreeBytes uint64
	Upstreams      []upstream.UpstreamStat
	LogDropped     uint64
}

func (s *Server) collectMetrics() metricsSnapshot {
	dns, px, st := s.d.DNS.Stats(), s.d.Proxy.Stats(), s.d.Runtime.StoreState()
	m := metricsSnapshot{
		Version:        version.Version,
		DNSQueries:     dns.Queries,
		DNSRateLimited: dns.RateLimited,
		CacheBytesHit:  px.BytesHit,
		CacheBytesWAN:  px.BytesWAN,
		StoreOnline:    st.Online,
		StoreFreeBytes: st.FreeBytes,
		Upstreams:      s.d.Upstream.Stats(),
		LogDropped:     s.d.Logs.Metrics().Dropped,
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
}

var promLabelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func promEscapeLabel(s string) string { return promLabelEscaper.Replace(s) }

func promFloat(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
