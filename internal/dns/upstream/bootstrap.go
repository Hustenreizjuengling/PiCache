package upstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/miekg/dns"
)

const (
	bootServerTimeout = 1500 * time.Millisecond // per bootstrap server and query type
	bootMinTTL        = time.Minute
	bootMaxTTL        = time.Hour
	bootMaxEntries    = 64
)

var errNoAddrs = errors.New("no addresses")

// bootstrap resolves the hostnames of DoT, DoH and named plain upstreams
// over plain DNS to the configured bootstrap IPs only (never the system
// resolver, never PiCache itself). Results are cached by TTL (1 min to
// 1 h); an expired entry is still used when the bootstrap servers cannot be
// reached.
type bootstrap struct {
	servers []string // "ip:port"
	prefer6 bool     // dns.bootstrapPreferIpv6: IPv6 addresses first

	mu    sync.Mutex
	cache map[string]bootEntry
}

type bootEntry struct {
	addrs   []netip.Addr // IPv4 first
	expires time.Time
}

func newBootstrap(ips []string, port int, prefer6 bool) *bootstrap {
	b := &bootstrap{cache: map[string]bootEntry{}, prefer6: prefer6}
	for _, s := range ips {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			continue // rejected by settings validation
		}
		b.servers = append(b.servers, net.JoinHostPort(ip.String(), strconv.Itoa(port)))
	}
	return b
}

// lookup returns the IPv4 and IPv6 addresses of host in the order they are
// dialled: IPv4 first, or IPv6 first with dns.bootstrapPreferIpv6.
func (b *bootstrap) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	addrs, err := b.lookupV4First(ctx, host)
	if err != nil || !b.prefer6 {
		return addrs, err
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, family6 := range []bool{true, false} {
		for _, a := range addrs {
			if is6 := a.Is6() && !a.Is4In6(); is6 == family6 {
				out = append(out, a)
			}
		}
	}
	return out, nil
}

// lookupV4First returns the IPv4 and IPv6 addresses of host (IPv4 first).
func (b *bootstrap) lookupV4First(ctx context.Context, host string) ([]netip.Addr, error) {
	now := time.Now()
	b.mu.Lock()
	e, cached := b.cache[host]
	b.mu.Unlock()
	if cached && now.Before(e.expires) {
		return e.addrs, nil
	}
	addrs, ttl, err := b.resolve(ctx, host)
	if err != nil {
		if cached {
			return e.addrs, nil
		}
		return nil, fmt.Errorf("bootstrap lookup of %s: %w", host, err)
	}
	ttl = min(max(ttl, bootMinTTL), bootMaxTTL)
	b.mu.Lock()
	if _, ok := b.cache[host]; !ok && len(b.cache) >= bootMaxEntries {
		for k := range b.cache { // evict an arbitrary entry
			delete(b.cache, k)
			break
		}
	}
	b.cache[host] = bootEntry{addrs: addrs, expires: now.Add(ttl)}
	b.mu.Unlock()
	return addrs, nil
}

func (b *bootstrap) resolve(ctx context.Context, host string) ([]netip.Addr, time.Duration, error) {
	if len(b.servers) == 0 {
		return nil, 0, errors.New("no bootstrap servers configured")
	}
	name := dns.Fqdn(host)
	v4, ttl4, err4 := b.query(ctx, name, dns.TypeA)
	if errors.Is(err4, errNXDomain) {
		return nil, 0, err4
	}
	v6, ttl6, err6 := b.query(ctx, name, dns.TypeAAAA)
	var ttl uint32
	switch {
	case len(v4) > 0 && len(v6) > 0:
		ttl = min(ttl4, ttl6)
	case len(v4) > 0:
		ttl = ttl4
	case len(v6) > 0:
		ttl = ttl6
	default:
		if err := errors.Join(err4, err6); err != nil {
			return nil, 0, err
		}
		return nil, 0, errNoAddrs
	}
	return append(v4, v6...), time.Duration(ttl) * time.Second, nil
}

var errNXDomain = errors.New("no such host")

// query asks the bootstrap servers in order until one answers.
func (b *bootstrap) query(ctx context.Context, name string, qtype uint16) ([]netip.Addr, uint32, error) {
	var errs []error
	for _, srv := range b.servers {
		if ctx.Err() != nil {
			break
		}
		q := newQuery(name, qtype, dns.ClassINET, false)
		wire, err := q.Pack()
		if err != nil {
			return nil, 0, err
		}
		qctx, cancel := context.WithTimeout(ctx, bootServerTimeout)
		m, err := exchangePlain(qctx, srv, q, wire, false)
		cancel()
		if err == nil {
			err = checkReply(q, m)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", srv, err))
			continue
		}
		switch m.Rcode {
		case dns.RcodeSuccess:
			addrs, ttl := addrsFromAnswer(m, name, qtype)
			return addrs, ttl, nil
		case dns.RcodeNameError:
			return nil, 0, errNXDomain
		default:
			errs = append(errs, fmt.Errorf("%s: %s", srv, dns.RcodeToString[m.Rcode]))
		}
	}
	if len(errs) == 0 {
		return nil, 0, ctxErr(ctx, ctx.Err())
	}
	return nil, 0, errors.Join(errs...)
}

// addrsFromAnswer returns the A/AAAA addresses for name in m, following the
// CNAME chain (at most 8 hops), and the smallest TTL along the way.
func addrsFromAnswer(m *dns.Msg, name string, qtype uint16) ([]netip.Addr, uint32) {
	chain := map[string]bool{lowerASCII(name): true}
	target := lowerASCII(name)
	minTTL := ^uint32(0)
	for range 8 {
		next := ""
		for _, rr := range m.Answer {
			if c, ok := rr.(*dns.CNAME); ok && lowerASCII(c.Hdr.Name) == target {
				next = lowerASCII(c.Target)
				minTTL = min(minTTL, c.Hdr.Ttl)
				break
			}
		}
		if next == "" || chain[next] {
			break
		}
		chain[next] = true
		target = next
	}
	var out []netip.Addr
	for _, rr := range m.Answer {
		h := rr.Header()
		if h.Rrtype != qtype || !chain[lowerASCII(h.Name)] {
			continue
		}
		var ip netip.Addr
		switch v := rr.(type) {
		case *dns.A:
			ip, _ = netip.AddrFromSlice(v.A.To4())
		case *dns.AAAA:
			ip, _ = netip.AddrFromSlice(v.AAAA.To16())
		}
		if ip.IsValid() {
			out = append(out, ip)
			minTTL = min(minTTL, h.Ttl)
		}
	}
	if len(out) == 0 {
		return nil, 0
	}
	return out, minTTL
}
