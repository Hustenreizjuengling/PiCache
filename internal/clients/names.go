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

// LeaseNameFunc returns the name of the active DHCP lease of an address
// ("" if none).
type LeaseNameFunc func(ip netip.Addr) string

// SetLeaseNames sets the source of DHCP lease names, the first name source
// of an address (before its PTR name). Call LeaseNamesChanged when names
// change.
func (r *Registry) SetLeaseNames(fn LeaseNameFunc) {
	if fn == nil {
		r.leaseName.Store(nil)
		return
	}
	r.leaseName.Store(&fn)
}

// LeaseNamesChanged drops the cached identities of addresses whose lease
// name changed, and of the other addresses of their devices (their name
// fallback).
func (r *Registry) LeaseNamesChanged(ips []netip.Addr) {
	arp := *r.arp.Load()
	for _, ip := range ips {
		ip = netutil.Canon(ip)
		r.invalidateIP(ip)
		if mac := arp[ip]; mac != "" {
			r.invalidateMAC(mac)
		}
	}
}

// lease returns the lease name of ip ("" if none or no source is set).
func (r *Registry) lease(ip netip.Addr) string {
	if fn := r.leaseName.Load(); fn != nil {
		return sanitizeHostname((*fn)(ip))
	}
	return ""
}

// hostname returns the lease name of ip, else its cached PTR name ("" if
// unknown).
func (r *Registry) hostname(ip netip.Addr) string {
	if n := r.lease(ip); n != "" {
		return n
	}
	r.namesMu.Lock()
	h, _ := r.names.peek(ip)
	r.namesMu.Unlock()
	return h.name
}

// name returns the PTR name of ip, else the name of another neighbour
// address with the same MAC ("" if none is known).
func (r *Registry) name(ip netip.Addr, mac string) string {
	if n := r.hostname(ip); n != "" || mac == "" {
		return n
	}
	if l := r.learned.Load(); l != nil {
		return r.macName(l.addrs[mac], ip)
	}
	return ""
}

// macName picks the name of one of addrs (except the address except): the
// name of a DHCP lease of PiCache first, then an IPv4 address's name
// (typically the name its DHCPv4 lease gave the device), then the most
// recently resolved one.
func (r *Registry) macName(addrs []netip.Addr, except netip.Addr) string {
	for _, a := range addrs {
		if a != except {
			if n := r.lease(a); n != "" {
				return n
			}
		}
	}
	var best hostName
	bestV4 := false
	r.namesMu.Lock()
	defer r.namesMu.Unlock()
	for _, a := range addrs {
		if a == except {
			continue
		}
		h, ok := r.names.peek(a)
		if !ok || h.name == "" {
			continue
		}
		if v4 := a.Is4(); best.name == "" || (v4 && !bestV4) || (v4 == bestV4 && h.at.After(best.at)) {
			best, bestV4 = h, v4
		}
	}
	return best.name
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
		if mac := (*r.arp.Load())[ip]; mac != "" {
			r.invalidateMAC(mac) // their name fallback
		}
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

// invalidateMAC drops the cached identities of the neighbour addresses of
// mac.
func (r *Registry) invalidateMAC(mac string) {
	l := r.learned.Load()
	if l == nil || len(l.addrs[mac]) == 0 {
		return
	}
	r.cacheMu.Lock()
	r.cacheGen++
	for _, ip := range l.addrs[mac] {
		r.cache.delete(ip)
	}
	r.cacheMu.Unlock()
}
