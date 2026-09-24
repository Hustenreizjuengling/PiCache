package netutil

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"time"
)

// ErrForbiddenDestination is returned when the SSRF guard refuses an address.
var ErrForbiddenDestination = errors.New("destination address not allowed")

type allowPrivateKey struct{}

// WithAllowPrivate marks ctx so that SafeDialer (with AllowPrivate == nil)
// may dial private (RFC 1918/ULA/CGNAT) destinations for this request. Use it
// only when the admin configured an explicit private IP literal (e.g. a
// blocklist hosted on the LAN), never after a redirect to another host.
func WithAllowPrivate(ctx context.Context) context.Context {
	return context.WithValue(ctx, allowPrivateKey{}, true)
}

func privateAllowedByCtx(ctx context.Context) bool {
	v, _ := ctx.Value(allowPrivateKey{}).(bool)
	return v
}

// SafeDialer dials TCP connections to hostnames resolved with Resolve and
// refuses destinations that are not public unicast addresses (unless
// allowed), that are link-local/multicast (always), or that belong to this
// machine.
//
// Use it as http.Transport.DialContext for every outbound fetch triggered by
// client traffic or external data (proxy, SNI, list and cache-domains
// downloads) so nobody can make PiCache connect to internal services.
type SafeDialer struct {
	Resolve Resolver
	// AllowPrivate reports whether private destinations are allowed. nil
	// means: allowed only if ctx was marked with WithAllowPrivate.
	AllowPrivate func(ctx context.Context) bool
	Timeout      time.Duration
	// OwnAddrs returns this machine's addresses (loop protection). nil = LocalAddrs.
	OwnAddrs func() []netip.Addr
}

// DialContext implements the http.Transport dial signature.
func (d *SafeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid port %q", portStr)
	}
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{Canon(ip)}
	} else {
		if d.Resolve == nil {
			return nil, errors.New("safe dialer: no resolver")
		}
		addrs, err = d.Resolve(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("no addresses for %s", host)
		}
	}
	allowed, err := d.Filter(ctx, addrs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", host, err)
	}
	timeout := d.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	var lastErr error
	for _, ip := range allowed {
		nd := net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
		conn, err := nd.DialContext(ctx, network, netip.AddrPortFrom(ip, uint16(port)).String())
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

// Filter returns the addresses that may be dialed, in order.
func (d *SafeDialer) Filter(ctx context.Context, addrs []netip.Addr) ([]netip.Addr, error) {
	allowPrivate := false
	if d.AllowPrivate != nil {
		allowPrivate = d.AllowPrivate(ctx)
	} else {
		allowPrivate = privateAllowedByCtx(ctx)
	}
	own := d.OwnAddrs
	if own == nil {
		own = LocalAddrs
	}
	mine := own()
	var out []netip.Addr
	forbidden := func(ip netip.Addr) bool {
		return !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() || inAny(ip, alwaysForbidden) || slices.Contains(mine, ip)
	}
	for _, ip := range addrs {
		ip = Canon(ip)
		if forbidden(ip) {
			continue
		}
		// NAT64/6to4/IPv4-compatible addresses reach their embedded IPv4
		// address, which must pass the same checks.
		if v4, ok := EmbeddedIPv4(ip); ok && forbidden(v4) {
			continue
		}
		if !allowPrivate && !IsPublicUnicast(ip) {
			continue
		}
		out = append(out, ip)
	}
	if len(out) == 0 {
		return nil, ErrForbiddenDestination
	}
	return out, nil
}
