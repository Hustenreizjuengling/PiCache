package app

import (
	"context"
	"runtime"
	"slices"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/hostinfo"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// maxIfaceNetworks bounds the routes shown per interface.
const maxIfaceNetworks = 64

// networkInterfaces builds GET /network/interfaces: the sysfs view of each
// interface (hostinfo, loopback excluded, at most 64, sorted by name) with
// its addresses, the non-default routes through it (the route snapshot of
// iface: identifiers) and the default gateways that use it. None on
// systems other than Linux; in a container bridge network the
// container's own interfaces with mode "bridge".
func (a *App) networkInterfaces(context.Context) api.NetworkInterfaces {
	out := api.NetworkInterfaces{Mode: "host", Interfaces: []api.NetworkInterface{}}
	if a.dns != nil && a.dns.BridgeNetwork() {
		out.Mode = "bridge"
	}
	if runtime.GOOS != "linux" {
		return out
	}
	sampler := a.hostSampler
	if sampler == nil {
		sampler = &hostinfo.Sampler{}
	}
	addrs := map[string][]string{}
	for _, h := range netutil.HostAddrs() {
		addrs[h.Iface] = append(addrs[h.Iface], h.Prefix.String())
	}
	table := netutil.Routes()
	for _, in := range sampler.Interfaces() {
		ni := api.NetworkInterface{Name: in.Name, Index: in.Index, MAC: in.MAC, Up: in.Up, OperState: in.OperState, MTU: in.MTU,
			SpeedMbps: in.SpeedMbps, Duplex: in.Duplex, Virtual: in.Virtual, RxBytes: in.RxBytes, TxBytes: in.TxBytes,
			RxErrors: in.RxErrors, TxErrors: in.TxErrors, Addresses: []string{}, Networks: []string{},
			DefaultGateways: []api.NetworkInterfaceGate{}}
		if a := addrs[in.Name]; a != nil {
			ni.Addresses = slices.Clone(a)
		}
		for _, r := range table.Routes() {
			if r.Iface == in.Name && len(ni.Networks) < maxIfaceNetworks {
				ni.Networks = append(ni.Networks, r.Prefix.String())
			}
		}
		ni.DefaultGateways = interfaceGateways(table.DefaultRoutes(), in.Name)
		out.Interfaces = append(out.Interfaces, ni)
	}
	return out
}

// interfaceGateways returns the default gateways of the default routes
// through iface, each once (two default routes via the same gateway, e.g.
// DHCP plus a static route with another metric, are one gateway).
func interfaceGateways(defaults []netutil.Route, iface string) []api.NetworkInterfaceGate {
	out := []api.NetworkInterfaceGate{}
	for _, r := range defaults {
		if r.Iface != iface || !r.Gateway.IsValid() {
			continue
		}
		fam := "ipv6"
		if r.Gateway.Is4() {
			fam = "ipv4"
		}
		if g := (api.NetworkInterfaceGate{Family: fam, Gateway: r.Gateway.String()}); !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}
