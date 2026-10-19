package app

import (
	"context"
	"log/slog"
	"net/netip"
	"runtime"
	"slices"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/portcheck"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// The port checks (clients.portChecks, docs/ARCHITECTURE.md 17 "Port
// checks") run in internal/portcheck; the app chooses the devices: the IPv4
// entries of the kernel's neighbour table inside a private network of this
// machine, one per MAC, never this machine's own addresses.

func (a *App) newPortChecker(ctx context.Context) (*portcheck.Checker, error) {
	c, err := portcheck.New(ctx, portcheck.Deps{
		DB: a.ldb,
		Config: func() portcheck.Config {
			pc := a.set.Get().Clients.PortChecks
			return portcheck.Config{Enabled: pc.Enabled, Interval: time.Duration(pc.IntervalHours) * time.Hour,
				ExtraPorts: pc.ExtraPorts}
		},
		Targets: a.portTargets,
		Name: func(ip netip.Addr, mac string) string {
			_, name, host := a.clients.Describe(ip, mac)
			if name != "" {
				return name
			}
			return host
		},
		Emit: a.emit,
		Log:  a.log.With(slog.String("component", "portcheck")),
	})
	if err != nil {
		return nil, err
	}
	a.set.Subscribe(func(_, _ *settings.All) { c.SettingsChanged() })
	return c, nil
}

// portTargets returns the devices to check.
func (a *App) portTargets(ctx context.Context) ([]portcheck.Target, error) {
	if runtime.GOOS != "linux" {
		return nil, apperr.Unavailable("port checks need Linux (PiCache reads the kernel's neighbour table)")
	}
	host := a.dns.HostNetwork()
	if host.Bridge {
		return nil, apperr.Unavailable("port checks are not available in a container bridge network; run PiCache with host networking")
	}
	return portTargetsOf(ctx, host.Prefixes, a.clients.Neighbours)
}

// portTargetsOf picks the IPv4 neighbours inside the private networks of
// prefixes (this machine's addresses), one per MAC, sorted by address.
func portTargetsOf(ctx context.Context, prefixes []netip.Prefix,
	neighbours func(context.Context) ([]clients.Neighbour, error)) ([]portcheck.Target, error) {
	own := map[netip.Addr]bool{}
	var nets []netip.Prefix
	for _, p := range prefixes {
		own[p.Addr().Unmap()] = true
		if p.Addr().Is4() && p.Addr().IsPrivate() {
			nets = append(nets, p.Masked())
		}
	}
	if len(nets) == 0 {
		return nil, apperr.Unavailable("no private IPv4 network of this machine to check")
	}
	nbs, err := neighbours(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []portcheck.Target
	for _, n := range nbs {
		ip := n.IP.Unmap()
		if !ip.Is4() || own[ip] || !slices.ContainsFunc(nets, func(p netip.Prefix) bool { return p.Contains(ip) }) {
			continue
		}
		key := n.MAC
		if key == "" {
			key = ip.String()
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, portcheck.Target{IP: ip, MAC: n.MAC})
	}
	slices.SortFunc(out, func(x, y portcheck.Target) int { return x.IP.Compare(y.IP) })
	if len(out) > portcheck.MaxTargets {
		out = out[:portcheck.MaxTargets]
	}
	return out, nil
}

// portLoop runs the scheduled port checks (none without logs.db).
func (a *App) portLoop(ctx context.Context) {
	if a.ports != nil {
		a.ports.Start(ctx)
	}
}

// portsAPI is the checker for the API (a nil interface without one).
func (a *App) portsAPI() api.PortChecks {
	if a.ports == nil {
		return nil
	}
	return a.ports
}
