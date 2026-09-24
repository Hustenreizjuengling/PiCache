package app

import (
	"fmt"
	"net"
)

// listeners are bound before privileges are dropped.
type listeners struct {
	dnsUDP []net.PacketConn
	dnsTCP []net.Listener
	cache  []net.Listener
	sni    []net.Listener
	web    []net.Listener
	webTLS []net.Listener
}

func (a *App) bindListeners() error {
	l := &a.ln
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
	tcp := func(role string, addrs []string, dst *[]net.Listener) error {
		for _, addr := range addrs {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return bindErr(role, addr, err)
			}
			*dst = append(*dst, ln)
		}
		return nil
	}
	if err := tcp("cache (http)", a.cfg.CacheListen, &l.cache); err != nil {
		return err
	}
	if err := tcp("SNI pass-through", a.cfg.SNIListen, &l.sni); err != nil {
		return err
	}
	if err := tcp("web UI", a.cfg.WebListen, &l.web); err != nil {
		return err
	}
	return tcp("web UI (https)", a.cfg.WebTLSListen, &l.webTLS)
}

func bindErr(role, addr string, err error) error {
	hint := ""
	if isAddrInUse(err) && (role == "DNS (udp)" || role == "DNS (tcp)") {
		hint = " (port 53 is in use; is systemd-resolved's stub listener or another DNS server running? See docs/DEPLOYMENT.md)"
	} else if isPermission(err) {
		hint = " (binding ports below 1024 needs CAP_NET_BIND_SERVICE; use the systemd unit or Docker setup from deploy/)"
	}
	return fmt.Errorf("bind %s on %s: %w%s", role, addr, err, hint)
}

func (l *listeners) closeAll() {
	for _, pc := range l.dnsUDP {
		_ = pc.Close()
	}
	for _, group := range [][]net.Listener{l.dnsTCP, l.cache, l.sni, l.web, l.webTLS} {
		for _, ln := range group {
			_ = ln.Close()
		}
	}
}
