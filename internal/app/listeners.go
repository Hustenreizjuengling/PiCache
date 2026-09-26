package app

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/dhcp"
)

type (
	sqlTx       = sql.Tx
	netListener = net.Listener
)

// listeners are bound before privileges are dropped.
type listeners struct {
	dnsUDP []net.PacketConn
	dnsTCP []net.Listener
	cache  []net.Listener
	sni    []net.Listener
	web    []net.Listener
	webTLS []net.Listener
	failed map[string]string // role → error for non-fatal bind failures
	// dhcp are the DHCP sockets the markers ask for (UDP 67 and 547 while
	// DHCP is switched on, the raw ICMPv6 socket while router
	// advertisements are on too); their failures are reported by the DHCP
	// status and the health check "dhcp", never fatal.
	dhcp *dhcp.Sockets

	closeOnce sync.Once
}

// bindListeners binds every configured address. DNS failures and "no web
// listener at all" are fatal; cache, SNI and extra web listeners fail softly
// (logged, reported in health) so a port clash never takes DNS down.
func (a *App) bindListeners() error {
	l := &a.ln
	l.failed = map[string]string{}
	for _, addr := range a.cfg.DNSListen {
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			return bindErr("DNS (udp)", addr, err)
		}
		l.dnsUDP = append(l.dnsUDP, pc)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return bindErr("DNS (tcp)", addr, err)
		}
		l.dnsTCP = append(l.dnsTCP, ln)
	}
	soft := func(role, label string, addrs []string, dst *[]net.Listener) {
		for _, addr := range addrs {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				e := bindErr(label, addr, err)
				a.log.Error("listener not started", slog.String("role", role), slog.Any("err", e))
				l.failed[role] = e.Error()
				continue
			}
			*dst = append(*dst, ln)
		}
	}
	soft("cache", "cache (http)", a.cfg.CacheListen, &l.cache)
	soft("sni", "SNI pass-through", a.cfg.SNIListen, &l.sni)
	soft("web", "web UI", a.cfg.WebListen, &l.web)
	soft("web-tls", "web UI (https)", a.cfg.WebTLSListen, &l.webTLS)
	if len(l.web) == 0 && len(l.webTLS) == 0 {
		return errors.New("no web UI listener could be bound: " + fmt.Sprint(l.failed))
	}
	// The DHCP sockets the markers of the DHCP service ask for (nothing
	// with PICACHE_DHCP=off, everything with the legacy on).
	l.dhcp = dhcp.OpenAtStart(dhcp.StartOptions{OptOut: a.cfg.DHCP == config.DHCPOff, Legacy: a.cfg.DHCP == config.DHCPOn,
		DataDir: a.cfg.DataDir})
	v4, v6, raw := l.dhcp.Errors()
	for _, f := range []struct{ what, err string }{{"DHCPv4", v4}, {"DHCPv6", v6}, {"router advertisements", raw}} {
		if f.err != "" {
			a.log.Warn("DHCP socket not opened", slog.String("for", f.what), slog.String("reason", f.err))
		}
	}
	return nil
}

func bindErr(role, addr string, err error) error {
	hint := ""
	switch {
	case isAddrInUse(err) && (role == "DNS (udp)" || role == "DNS (tcp)"):
		hint = " (port 53 is in use; is systemd-resolved's stub listener or another DNS server running? See docs/DEPLOYMENT.md)"
	case isAddrInUse(err):
		hint = " (the port is used by another program; change the PICACHE_*_LISTEN setting)"
	case isPermission(err):
		hint = " (binding ports below 1024 needs CAP_NET_BIND_SERVICE; use the systemd unit or Docker setup from deploy/)"
	}
	return fmt.Errorf("bind %s on %s: %w%s", role, addr, err, hint)
}

func (l *listeners) info() api.ListenerInfo {
	addrs := func(ls []net.Listener) []string {
		out := make([]string, 0, len(ls))
		for _, x := range ls {
			out = append(out, x.Addr().String())
		}
		return out
	}
	udp := make([]string, 0, len(l.dnsUDP))
	for _, pc := range l.dnsUDP {
		udp = append(udp, pc.LocalAddr().String())
	}
	failed := make(map[string]string, len(l.failed))
	for k, v := range l.failed {
		failed[k] = v
	}
	return api.ListenerInfo{
		Bound: map[string][]string{
			"dns-udp": udp, "dns-tcp": addrs(l.dnsTCP), "cache": addrs(l.cache),
			"sni": addrs(l.sni), "web": addrs(l.web), "web-tls": addrs(l.webTLS),
		},
		Failed: failed,
	}
}

func (l *listeners) closeAll() {
	l.closeOnce.Do(func() {
		l.dhcp.Close()
		for _, pc := range l.dnsUDP {
			_ = pc.Close()
		}
		for _, group := range [][]net.Listener{l.dnsTCP, l.cache, l.sni, l.web, l.webTLS} {
			for _, ln := range group {
				_ = ln.Close()
			}
		}
	})
}
