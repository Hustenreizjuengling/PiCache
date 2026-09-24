package upstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

const (
	ipCacheMax = 1024
	testName   = "example.com." // IANA-reserved, always resolvable
	maxPTRName = 253
)

// LookupIP resolves host to addresses via the default upstreams, bypassing
// all local data. IPv4 only unless want6 (IPv4 addresses first). Cached by
// TTL. Errors are *net.DNSError (IsNotFound for NXDOMAIN or no address).
func (r *Resolver) LookupIP(ctx context.Context, host string, want6 bool) ([]netip.Addr, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if ip.Zone() != "" || (ip.Is6() && !want6) {
			return nil, &net.DNSError{Err: "no suitable address", Name: host, IsNotFound: true}
		}
		return []netip.Addr{ip}, nil
	}
	if !settings.ValidHostname(host) {
		return nil, &net.DNSError{Err: "invalid host name", Name: host, IsNotFound: true}
	}
	if addrs, ok := r.ips.get(host, want6, time.Now()); ok {
		return addrs, nil
	}
	set := r.defaultSet()
	type result struct {
		addrs []netip.Addr
		ttl   uint32
		err   error
	}
	var v6 result
	var wg sync.WaitGroup
	if want6 {
		wg.Go(func() { v6.addrs, v6.ttl, v6.err = r.lookupType(ctx, set, host, dns.TypeAAAA) })
	}
	var v4 result
	v4.addrs, v4.ttl, v4.err = r.lookupType(ctx, set, host, dns.TypeA)
	wg.Wait()

	addrs := append(v4.addrs, v6.addrs...)
	if len(addrs) == 0 {
		for _, err := range []error{v4.err, v6.err} {
			if err != nil {
				return nil, err
			}
		}
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	ttl := ^uint32(0)
	for _, res := range []result{v4, v6} {
		if len(res.addrs) > 0 {
			ttl = min(ttl, res.ttl)
		}
	}
	r.ips.put(host, want6, addrs, time.Now().Add(time.Duration(ttl)*time.Second))
	return slices.Clone(addrs), nil
}

func (r *Resolver) lookupType(ctx context.Context, set *upstreamSet, host string, qtype uint16) ([]netip.Addr, uint32, error) {
	name := dns.Fqdn(host)
	req := new(dns.Msg)
	req.SetQuestion(name, qtype)
	m, _, err := r.resolve(ctx, req, set)
	if err != nil {
		return nil, 0, &net.DNSError{Err: err.Error(), Name: host, IsTemporary: true,
			IsTimeout: errors.Is(err, errTimeout) || errors.Is(err, context.DeadlineExceeded), UnwrapErr: err}
	}
	switch m.Rcode {
	case dns.RcodeSuccess:
		addrs, ttl := addrsFromAnswer(m, name, qtype)
		return addrs, ttl, nil
	case dns.RcodeNameError:
		return nil, 0, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	default:
		return nil, 0, &net.DNSError{Err: "server misbehaving: " + dns.RcodeToString[m.Rcode], Name: host, IsTemporary: true}
	}
}

// ipCache holds LookupIP results independently of the response cache
// setting (the proxy and SNI server depend on it).
type ipCache struct {
	mu sync.Mutex
	m  map[ipKey]ipEntry
}

type ipKey struct {
	host  string
	want6 bool
}

type ipEntry struct {
	addrs   []netip.Addr
	expires time.Time
}

func (c *ipCache) get(host string, want6 bool, now time.Time) ([]netip.Addr, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[ipKey{host, want6}]
	if !ok || !now.Before(e.expires) {
		return nil, false
	}
	return slices.Clone(e.addrs), true
}

func (c *ipCache) put(host string, want6 bool, addrs []netip.Addr, expires time.Time) {
	now := time.Now()
	if !now.Before(expires) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := ipKey{host, want6}
	if _, ok := c.m[k]; !ok && len(c.m) >= ipCacheMax {
		for key, e := range c.m {
			if !now.Before(e.expires) {
				delete(c.m, key)
			}
		}
		for key := range c.m {
			if len(c.m) < ipCacheMax {
				break
			}
			delete(c.m, key)
		}
	}
	c.m[k] = ipEntry{addrs: slices.Clone(addrs), expires: expires}
}

func (c *ipCache) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.m)
}

// LookupPTR resolves the hostname of ip via the given plain-DNS servers; "" if none.
// Names that are not plain host names ([a-z0-9._-]) are ignored.
func (r *Resolver) LookupPTR(ctx context.Context, ip netip.Addr, servers []string) (string, error) {
	if !ip.IsValid() {
		return "", errInvalidAddr
	}
	var plain []string
	for _, s := range servers {
		if spec, err := settings.ParseUpstream(s); err == nil && spec.IsIPLit && (spec.Proto == "udp" || spec.Proto == "tcp") {
			plain = append(plain, s)
		}
	}
	if len(plain) == 0 {
		return "", errNoPlainPTR
	}
	set, err := r.viaSet(plain)
	if err != nil {
		return "", err
	}
	arpa, err := dns.ReverseAddr(ip.Unmap().WithZone("").String())
	if err != nil {
		return "", err
	}
	req := new(dns.Msg)
	req.SetQuestion(arpa, dns.TypePTR)
	m, _, err := r.resolve(ctx, req, set)
	if err != nil {
		return "", err
	}
	switch m.Rcode {
	case dns.RcodeSuccess:
	case dns.RcodeNameError:
		return "", nil
	default:
		return "", fmt.Errorf("upstream: PTR lookup: %s", dns.RcodeToString[m.Rcode])
	}
	for _, rr := range m.Answer {
		if p, ok := rr.(*dns.PTR); ok && equalFoldASCII(p.Hdr.Name, arpa) {
			if name := cleanPTRName(p.Ptr); name != "" {
				return name, nil
			}
		}
	}
	return "", nil
}

// cleanPTRName lower-cases name, strips the trailing dot and returns "" for
// anything but a plain host name (no escapes, spaces or control bytes).
func cleanPTRName(name string) string {
	name = strings.TrimSuffix(lowerASCII(name), ".")
	if name == "" || len(name) > maxPTRName {
		return ""
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return ""
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
				return ""
			}
		}
	}
	return name
}

// Probe reports whether a plain DNS server answers (used to validate the
// auto-detected router resolver). Timeout 1 s. It asks for the PTR of the
// server's own address, which routers answer locally; NOERROR and NXDOMAIN
// count as answers.
func (r *Resolver) Probe(ctx context.Context, server netip.Addr) bool {
	if !server.IsValid() {
		return false
	}
	server = server.Unmap()
	arpa, err := dns.ReverseAddr(server.WithZone("").String())
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	q := newQuery(arpa, dns.TypePTR, dns.ClassINET, false)
	wire, err := q.Pack()
	if err != nil {
		return false
	}
	m, err := exchangePlain(ctx, netip.AddrPortFrom(server, uint16(r.opts.plainPort)).String(), q, wire, false)
	if err != nil || checkReply(q, m) != nil {
		return false
	}
	return m.Rcode == dns.RcodeSuccess || m.Rcode == dns.RcodeNameError
}

// Test resolves a fixed name (example.com A) through one upstream string,
// bypassing the cache, the clock guard and the statistics.
func (r *Resolver) Test(ctx context.Context, upstream string) TestResult {
	res := TestResult{Upstream: upstream}
	spec, err := settings.ParseUpstream(upstream)
	if err != nil {
		res.Error = "invalid upstream: " + err.Error()
		return res
	}
	boot := r.def.Load().boot
	if !spec.IsIPLit && len(boot.servers) == 0 {
		res.Error = "a bootstrap server is required to resolve " + spec.Host
		return res
	}
	t := r.newTransport(spec, boot)
	defer t.close()
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	q := newQuery(testName, dns.TypeA, dns.ClassINET, r.set.Get().DNS.DNSSEC)
	wire, err := q.Pack()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	start := time.Now()
	m, err := t.exchange(ctx, q, wire)
	res.RTTMs = msFloat(time.Since(start))
	if err == nil {
		err = checkReply(q, m)
	}
	if err != nil {
		res.Error = ctxErr(ctx, err).Error()
		return res
	}
	if m.Rcode != dns.RcodeSuccess {
		res.Error = "upstream answered " + dns.RcodeToString[m.Rcode]
		return res
	}
	res.OK = true
	res.Answer = summarizeAnswer(m)
	return res
}

// summarizeAnswer lists up to four addresses of the test answer.
func summarizeAnswer(m *dns.Msg) string {
	addrs, _ := addrsFromAnswer(m, testName, dns.TypeA)
	if len(addrs) == 0 {
		return "NOERROR (no address)"
	}
	parts := make([]string, 0, 4)
	for _, a := range addrs[:min(len(addrs), 4)] {
		parts = append(parts, a.String())
	}
	s := strings.Join(parts, ", ")
	if len(addrs) > 4 {
		s += ", …"
	}
	return s
}
