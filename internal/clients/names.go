package clients

import (
	"context"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// hostName is a cached PTR result ("" = no name).
type hostName struct {
	name string
	at   time.Time
}

// hostname returns the cached PTR name of ip ("" if unknown).
func (r *Registry) hostname(ip netip.Addr) string {
	r.namesMu.Lock()
	h, _ := r.names.peek(ip)
	r.namesMu.Unlock()
	return h.name
}

// enqueueName schedules a PTR lookup for ip (de-duplicated; dropped when
// the queue is full).
func (r *Registry) enqueueName(ip netip.Addr) {
	if !ip.IsValid() || ip.IsLoopback() || ip.IsUnspecified() {
		return
	}
	r.namesMu.Lock()
	defer r.namesMu.Unlock()
	if _, ok := r.queued[ip]; ok {
		return
	}
	select {
	case r.queue <- ip:
		r.queued[ip] = struct{}{}
	default:
	}
}

// LookupNames schedules PTR lookups for addresses whose name is unknown or
// older than nameRefreshEvery, so the network check can name devices that
// never asked PiCache. Link-local addresses are skipped; the bounded queue
// limits the work.
func (r *Registry) LookupNames(ips []netip.Addr) {
	cutoff := time.Now().Add(-nameRefreshEvery)
	for _, ip := range ips {
		ip = netutil.Canon(ip)
		if ip.IsLinkLocalUnicast() {
			continue
		}
		r.namesMu.Lock()
		h, ok := r.names.peek(ip)
		r.namesMu.Unlock()
		if !ok || h.at.Before(cutoff) {
			r.enqueueName(ip)
		}
	}
}

// ptrWorker resolves queued addresses one at a time.
func (r *Registry) ptrWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ip := <-r.queue:
			r.namesMu.Lock()
			delete(r.queued, ip)
			r.namesMu.Unlock()
			r.lookupName(ctx, ip)
		}
	}
}

// lookupName resolves and caches the hostname of ip. On errors the previous
// name is kept.
func (r *Registry) lookupName(ctx context.Context, ip netip.Addr) {
	fn := r.ptr.Load()
	if fn == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, ptrTimeout)
	name, err := (*fn)(cctx, ip)
	cancel()
	if ctx.Err() != nil {
		return
	}
	name = sanitizeHostname(name)
	r.namesMu.Lock()
	old, _ := r.names.peek(ip)
	if err != nil {
		r.log.Debug("client name lookup failed", slog.String("ip", ip.String()), slog.Any("err", err))
		name = old.name
	}
	r.names.put(ip, hostName{name: name, at: time.Now()})
	r.namesMu.Unlock()
	if name != old.name {
		r.invalidateIP(ip)
	}
}

// refreshNames re-resolves the names of addresses active within the last
// refresh interval whose name is older than that interval.
func (r *Registry) refreshNames() {
	cutoff := time.Now().Add(-nameRefreshEvery)
	var ips []netip.Addr
	r.seenMu.Lock()
	r.seen.each(func(ip netip.Addr, e *seenEntry) bool {
		if e.last.Before(cutoff) {
			return true
		}
		ips = append(ips, ip)
		return len(ips) < maxPTRQueue
	})
	r.seenMu.Unlock()
	for _, ip := range ips {
		r.namesMu.Lock()
		h, ok := r.names.peek(ip)
		r.namesMu.Unlock()
		if !ok || h.at.Before(cutoff) {
			r.enqueueName(ip)
		}
	}
}

// sanitizeHostname lower-cases a PTR result and returns "" unless it is a
// plausible host name (external data: never trusted for display as-is).
func sanitizeHostname(s string) string {
	s = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
	if s == "" || len(s) > 253 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_') {
			return ""
		}
	}
	if strings.HasPrefix(s, ".") || strings.Contains(s, "..") {
		return ""
	}
	return s
}

// invalidateIP drops the cached identity of ip.
func (r *Registry) invalidateIP(ip netip.Addr) {
	r.cacheMu.Lock()
	r.cacheGen++
	r.cache.delete(ip)
	r.cacheMu.Unlock()
}
