package app

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// The outbound proxy (docs/ARCHITECTURE.md 6.1 "Outbound proxy"):
// network.proxy for the traffic network.proxyFor switches on (list and
// cache-domains downloads, the release check, notifications). The dialers
// resolve and check the targets as without a proxy and tunnel to the
// checked IP literal (netutil.Tunnel); the proxy itself is resolved by the
// host resolver and never one of PiCache's own listeners.

// proxyUse selects the switch of network.proxyFor of a kind of traffic.
type proxyUse func(settings.ProxyFor) bool

var (
	proxyLists         proxyUse = func(p settings.ProxyFor) bool { return p.Lists }
	proxyUpdateCheck   proxyUse = func(p settings.ProxyFor) bool { return p.UpdateCheck }
	proxyNotifications proxyUse = func(p settings.ProxyFor) bool { return p.Notifications }
)

// outboundProxy caches the tunnel of the current settings (the password
// is unsealed once per settings document).
type outboundProxy struct {
	a   *App
	mu  sync.Mutex
	at  *settings.All
	tun *netutil.Tunnel
}

// proxyFor returns the tunnel function of a kind of traffic (nil tunnel:
// direct).
func (a *App) proxyFor(use proxyUse) func(context.Context) *netutil.Tunnel {
	return func(ctx context.Context) *netutil.Tunnel {
		if a.set == nil {
			return nil
		}
		s := a.set.Get()
		if s.Network.Proxy.URL == "" || !use(s.Network.ProxyFor) {
			return nil
		}
		return a.outbound.tunnel(ctx, s)
	}
}

func (o *outboundProxy) tunnel(ctx context.Context, s *settings.All) *netutil.Tunnel {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.at == s && o.tun != nil {
		return o.tun
	}
	p, msg := settings.ParseProxyURL(s.Network.Proxy.URL)
	if msg != "" {
		return nil // Validate refuses such a URL; nothing to tunnel through
	}
	t := &netutil.Tunnel{Scheme: p.Scheme, Host: p.Host, Port: p.Port, Username: s.Network.Proxy.Username,
		Resolve: hostResolve, Refuse: o.a.refuseOwnListener, Timeout: 10 * time.Second}
	if s.Network.Proxy.PasswordSet {
		pw, err := o.a.set.Secret(ctx, settings.SecretProxyPassword)
		if err != nil {
			o.a.log.Warn("the password of the outbound proxy cannot be read", slog.Any("err", err))
		}
		t.Password = pw
	}
	o.at, o.tun = s, t
	return t
}

// proxyChanged reports whether the outbound proxy or the traffic that uses
// it changed.
func proxyChanged(o, n *settings.All) bool {
	op, np := o.Network.Proxy, n.Network.Proxy
	return op.URL != np.URL || op.Username != np.Username || op.PasswordSet != np.PasswordSet ||
		o.Network.ProxyFor != n.Network.ProxyFor
}

// hostResolve resolves a name with the host resolver (/etc/resolv.conf).
func hostResolve(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// refuseOwnListener refuses a proxy address that is one of PiCache's own
// listeners (loopback or this machine's address and a bound port).
func (a *App) refuseOwnListener(ip netip.Addr, port int) string {
	if role := a.ownListener(ip, port); role != "" {
		return "this is PiCache's own " + role + " listener"
	}
	return ""
}

// ownListener returns the role of a bound TCP listener that ip:port
// reaches ("" if none).
func (a *App) ownListener(ip netip.Addr, port int) string {
	ip = ip.Unmap()
	if !ip.IsLoopback() && !ip.IsUnspecified() && !slices.Contains(netutil.LocalAddrs(), ip) {
		return ""
	}
	for name, addrs := range a.Listeners().Bound {
		role, tcp := config.BoundRole(name)
		if !tcp {
			continue // a proxy is dialed over TCP
		}
		for _, s := range addrs {
			host, p, err := net.SplitHostPort(s)
			if err != nil || p != strconv.Itoa(port) {
				continue
			}
			h, err := netip.ParseAddr(host)
			if err != nil || h.IsUnspecified() || h.Unmap() == ip || ip.IsUnspecified() {
				return role
			}
		}
	}
	return ""
}
