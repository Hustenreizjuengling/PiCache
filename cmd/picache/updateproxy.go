package main

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/update"
)

// updateHTTPClient returns the HTTP client of the root helper and `sudo
// picache update`: nil (the update package's own client) without
// PICACHE_UPDATE_PROXY, else one whose connections are tunnelled through
// that proxy to the checked addresses (netutil.SafeDialer with a Tunnel;
// the host resolver resolves the targets and the proxy).
func updateHTTPClient(proxyVar string) (*http.Client, error) {
	p, err := update.ParseUpdateProxy(proxyVar)
	if err != nil || p == nil {
		return nil, err
	}
	resolve := func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	tun := &netutil.Tunnel{Scheme: p.Scheme, Host: p.Host, Port: p.Port, Resolve: resolve, Timeout: 15 * time.Second}
	d := &netutil.SafeDialer{Resolve: resolve, Timeout: 15 * time.Second, Proxy: func(context.Context) *netutil.Tunnel { return tun }}
	c := update.NewHTTPClient()
	c.Transport.(*http.Transport).DialContext = d.DialContext
	return c, nil
}

// failingHTTPClient fails every request with err (an invalid
// PICACHE_UPDATE_PROXY stops the run with the variable named).
func failingHTTPClient(err error) *http.Client {
	return &http.Client{Transport: failingTransport{err}}
}

type failingTransport struct{ err error }

func (t failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, t.err }
