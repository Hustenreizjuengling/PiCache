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

// SafeDialer dials TCP connections to hostnames resolved with Resolve and
// refuses destinations that are not public unicast addresses (unless
// AllowPrivate returns true) or that belong to this machine.
//
// Use it as http.Transport.DialContext for every outbound fetch triggered by
// client traffic (proxy, SNI) so a client cannot make PiCache connect to
// internal services.
type SafeDialer struct {
	Resolve      Resolver
	AllowPrivate func() bool // nil = never
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
		addrs = []netip.Addr{ip.Unmap()}
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
	allowed, err := d.Filter(addrs)
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
func (d *SafeDialer) Filter(addrs []netip.Addr) ([]netip.Addr, error) {
	allowPrivate := d.AllowPrivate != nil && d.AllowPrivate()
	own := d.OwnAddrs
	if own == nil {
		own = LocalAddrs
	}
	mine := own()
	var out []netip.Addr
	for _, ip := range addrs {
		ip = ip.Unmap()
		if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || slices.Contains(mine, ip) || ip.IsLoopback() {
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
