package app

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

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
	dot    []net.Listener    // DNS over TLS (PICACHE_DOT_LISTEN)
	doh    []net.Listener    // DNS over HTTPS only (PICACHE_DOH_LISTEN)
	ntp    []net.PacketConn  // NTP server (PICACHE_NTP_LISTEN, UDP)
	failed map[string]string // role → error for non-fatal bind failures
	// dhcp are the DHCP sockets the markers ask for (UDP 67 and 547 while
	// DHCP is switched on, the raw ICMPv6 socket while router
	// advertisements are on too); their failures are reported by the DHCP
	// status and the health check "dhcp", never fatal.
	dhcp *dhcp.Sockets

	// The saved listeners (listeners.next.json or listeners.json) this
	// start used: candidate is the saved set (nil: none), savedFailed the
	// saved roles that could not be bound (role → error) and fell back to
	// their environment or default values; written by finishListenerFiles.
	candidate   map[string][]string
	fromNext    bool
	savedFailed map[string]string
	savedAt     time.Time

	closeOnce sync.Once
}

// roleKey is the key of a listener role in api.ListenerInfo (dns-udp and
// dns-tcp for dns, web-tls for webTls).
func roleKey(role string) string {
	if role == config.RoleWebTLS {
		return "web-tls"
	}
	return role
}

// roleLabels name the roles in errors.
var roleLabels = map[string]string{
	config.RoleCache: "cache (http)", config.RoleSNI: "SNI pass-through", config.RoleWeb: "web UI",
	config.RoleWebTLS: "web UI (https)", config.RoleDoT: "DNS over TLS", config.RoleDoH: "DNS over HTTPS",
	config.RoleNTP: "NTP server",
}

// configValues returns the addresses of every role from the flags, the
// environment or the defaults (the Config).
func (a *App) configValues() map[string][]string {
	c := a.cfg
	return map[string][]string{config.RoleDNS: c.DNSListen, config.RoleCache: c.CacheListen, config.RoleSNI: c.SNIListen,
		config.RoleWeb: c.WebListen, config.RoleWebTLS: c.WebTLSListen, config.RoleDoT: c.DoTListen, config.RoleDoH: c.DoHListen,
		config.RoleNTP: c.NTPListen}
}

// loadSavedListeners reads the candidate of this start: listeners.next.json
// if it exists, else listeners.json; never with PICACHE_RUN_AS (Docker:
// root binds, and root reads nothing the service can write). A file that
// fails the checks is ignored with a warning that never quotes it.
func (a *App) loadSavedListeners() {
	l := &a.ln
	if a.cfg.RunAs != "" {
		return
	}
	for _, name := range []string{config.ListenersNextFile, config.ListenersFile} {
		roles, err := config.ReadListenersFile(a.cfg.DataDir, name)
		if err != nil {
			a.log.Warn("the saved listeners are ignored", slog.String("file", name), slog.Any("err", err))
			if name == config.ListenersNextFile {
				// A broken next set: fall back to the one that works.
				continue
			}
			return
		}
		if roles != nil {
			l.candidate, l.fromNext = roles, name == config.ListenersNextFile
			return
		}
	}
}

// bindListeners binds every configured address. DNS failures and "no web
// listener at all" are fatal; cache, SNI, extra web, DoT, DoH and NTP
// listeners fail softly (logged, reported in health) so a port clash never
// takes DNS down. DoT, DoH and NTP are bound whether they are switched on
// or not (the switches need no restart). A role of the saved listeners
// (loadSavedListeners) that cannot be bound falls back to its environment
// or default value for this start; a value from the files is never a
// reason to exit.
func (a *App) bindListeners() error {
	l := &a.ln
	l.failed = map[string]string{}
	l.savedFailed = map[string]string{}
	a.loadSavedListeners()
	values := a.configValues()
	saved := func(role string) ([]string, bool) {
		if a.cfg.ListenerLock[role] != "" {
			return nil, false
		}
		v, ok := l.candidate[role]
		return v, ok
	}

	// DNS: the saved addresses first, then the configured ones.
	bindDNS := func(addrs []string) error {
		var udp []net.PacketConn
		var tcp []net.Listener
		for _, addr := range addrs {
			pc, err := net.ListenPacket("udp", addr)
			if err != nil {
				closeAll(udp, tcp)
				return bindErr("DNS (udp)", addr, err)
			}
			udp = append(udp, pc)
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				closeAll(udp, tcp)
				return bindErr("DNS (tcp)", addr, err)
			}
			tcp = append(tcp, ln)
		}
		l.dnsUDP, l.dnsTCP = udp, tcp
		return nil
	}
	if addrs, ok := saved(config.RoleDNS); ok {
		if err := bindDNS(addrs); err != nil {
			a.savedFallback(config.RoleDNS, err, values[config.RoleDNS])
			if err := bindDNS(values[config.RoleDNS]); err != nil {
				return err
			}
		}
	} else if err := bindDNS(values[config.RoleDNS]); err != nil {
		return err
	}

	soft := func(role string, dst *[]net.Listener) {
		label := roleLabels[role]
		try := func(addrs []string, record bool) error {
			var got []net.Listener
			for _, addr := range addrs {
				ln, err := net.Listen("tcp", addr)
				if err != nil {
					e := bindErr(label, addr, err)
					if p := portOf(addr); role == config.RoleDoH && isAddrInUse(err) && p != "" && hasPort(l.sni, p) {
						// The default PICACHE_SNI_LISTEN=:443 is bound first.
						e = fmt.Errorf("bind %s on %s: %w (port %s is used by the SNI pass-through of the download cache: "+
							"set PICACHE_SNI_LISTEN=off, give PICACHE_DOH_LISTEN another port, or bind each to its own address)", label, addr, err, p)
					}
					if !record {
						for _, x := range got {
							_ = x.Close()
						}
						return e
					}
					a.log.Error("listener not started", slog.String("role", roleKey(role)), slog.Any("err", e))
					l.failed[roleKey(role)] = e.Error()
					continue
				}
				got = append(got, ln)
			}
			*dst = got
			return nil
		}
		if addrs, ok := saved(role); ok {
			if err := try(addrs, false); err != nil {
				a.savedFallback(role, err, values[role])
				_ = try(values[role], true)
			}
			return
		}
		_ = try(values[role], true)
	}
	soft(config.RoleCache, &l.cache)
	soft(config.RoleSNI, &l.sni)
	soft(config.RoleWeb, &l.web)
	soft(config.RoleWebTLS, &l.webTLS)
	if len(l.web) == 0 && len(l.webTLS) == 0 {
		return errors.New("no web UI listener could be bound: " + fmt.Sprint(l.failed))
	}
	soft(config.RoleDoT, &l.dot)
	soft(config.RoleDoH, &l.doh)
	a.bindNTP(saved, values)
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

// bindNTP binds the NTP server's UDP addresses (soft: a failure is logged
// and listeners.failed["ntp"]; the health check counts it only while
// ntp.enabled is on).
func (a *App) bindNTP(saved func(string) ([]string, bool), values map[string][]string) {
	l := &a.ln
	try := func(addrs []string, record bool) error {
		var got []net.PacketConn
		for _, addr := range addrs {
			pc, err := net.ListenPacket("udp", addr)
			if err != nil {
				e := bindErr("NTP server", addr, err)
				if !record {
					closeAll(got, nil)
					return e
				}
				a.log.Error("listener not started", slog.String("role", config.RoleNTP), slog.Any("err", e))
				l.failed[config.RoleNTP] = e.Error()
				continue
			}
			got = append(got, pc)
		}
		l.ntp = got
		return nil
	}
	if addrs, ok := saved(config.RoleNTP); ok {
		if err := try(addrs, false); err != nil {
			a.savedFallback(config.RoleNTP, err, values[config.RoleNTP])
			_ = try(values[config.RoleNTP], true)
		}
		return
	}
	_ = try(values[config.RoleNTP], true)
}

// savedFallback records a saved role that could not be bound: it falls
// back to its environment or default value for this start.
func (a *App) savedFallback(role string, err error, fallback []string) {
	fb := strings.Join(fallback, ", ")
	if fb == "" {
		fb = "off"
	}
	msg := fmt.Sprintf("listener %s from the saved listeners could not be bound (%v); using %s", role, err, fb)
	a.log.Error(msg)
	a.ln.savedFailed[role] = err.Error()
	a.ln.failed[roleKey(role)] = msg
}

// closeAll closes partially bound sockets.
func closeAll(pcs []net.PacketConn, lns []net.Listener) {
	for _, pc := range pcs {
		_ = pc.Close()
	}
	for _, ln := range lns {
		_ = ln.Close()
	}
}

// finishListenerFiles writes the result of this start once the data
// directory is usable: listeners.json = the candidate without the roles
// that failed (so the command line tools see what the running process
// bound), listeners.next.json removed, listeners.failed.json = the
// candidate when a role failed (removed when everything was bound).
func (a *App) finishListenerFiles() {
	l := &a.ln
	if a.cfg.RunAs != "" {
		return
	}
	dir := a.cfg.DataDir
	failedPath := filepath.Join(dir, config.ListenersFailedFile)
	if l.candidate == nil {
		_ = os.Remove(failedPath)
		return
	}
	bound := map[string][]string{}
	for role, addrs := range l.candidate {
		if _, failed := l.savedFailed[role]; !failed && a.cfg.ListenerLock[role] == "" {
			bound[role] = addrs
		}
	}
	if err := config.WriteListenersFile(dir, config.ListenersFile, config.EncodeListeners(bound)); err != nil {
		a.log.Error("could not write the bound listeners", slog.String("file", config.ListenersFile), slog.Any("err", err))
	}
	if l.fromNext {
		if err := os.Remove(filepath.Join(dir, config.ListenersNextFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			a.log.Error("could not remove the applied listeners", slog.String("file", config.ListenersNextFile), slog.Any("err", err))
		}
	}
	if len(l.savedFailed) == 0 {
		_ = os.Remove(failedPath)
		return
	}
	l.savedAt = time.Now().UTC()
	if err := config.WriteListenersFile(dir, config.ListenersFailedFile, config.EncodeListeners(l.candidate)); err != nil {
		a.log.Error("could not write the failed listeners", slog.String("file", config.ListenersFailedFile), slog.Any("err", err))
	}
}

// --- api.ListenerManager ---

// SavedListeners returns listeners.next.json, else listeners.json.
func (a *App) SavedListeners() (map[string][]string, bool) {
	if a.cfg.RunAs != "" {
		return nil, false
	}
	next, err := config.ReadListenersFile(a.cfg.DataDir, config.ListenersNextFile)
	if err == nil && next != nil {
		return next, true
	}
	cur, _ := config.ReadListenersFile(a.cfg.DataDir, config.ListenersFile)
	return cur, false
}

// FailedListeners returns the saved roles this start could not bind.
func (a *App) FailedListeners() *api.ListenersFailed {
	if len(a.ln.savedFailed) == 0 {
		return nil
	}
	return &api.ListenersFailed{Time: a.ln.savedAt, Roles: maps.Clone(a.ln.savedFailed), Saved: maps.Clone(a.ln.candidate)}
}

// SaveListeners writes listeners.next.json (applied at the next start).
func (a *App) SaveListeners(roles map[string][]string) error {
	if err := config.ValidateListeners(roles); err != nil {
		return err
	}
	return config.WriteListenersFile(a.cfg.DataDir, config.ListenersNextFile, config.EncodeListeners(roles))
}

func bindErr(role, addr string, err error) error {
	hint := ""
	switch {
	case isAddrInUse(err) && (role == "DNS (udp)" || role == "DNS (tcp)") && portOf(addr) == "53":
		hint = " (port 53 is in use; is systemd-resolved's stub listener or another DNS server running? See docs/DEPLOYMENT.md)"
	case isAddrInUse(err):
		hint = " (the port is used by another program; change the PICACHE_*_LISTEN setting)"
	case isPermission(err):
		hint = " (binding ports below 1024 needs CAP_NET_BIND_SERVICE; use the systemd unit or Docker setup from deploy/)"
	}
	return fmt.Errorf("bind %s on %s: %w%s", role, addr, err, hint)
}

// portOf returns the port of host:port ("" if it does not parse).
func portOf(addr string) string {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return p
}

// hasPort reports whether a listener is bound to port p.
func hasPort(ls []net.Listener, p string) bool {
	for _, ln := range ls {
		if portOf(ln.Addr().String()) == p {
			return true
		}
	}
	return false
}

// tlsBound reports whether any TLS listener (web-tls, dot, doh) is bound.
func (l *listeners) tlsBound() bool { return len(l.webTLS)+len(l.dot)+len(l.doh) > 0 }

func (l *listeners) info() api.ListenerInfo {
	addrs := func(ls []net.Listener) []string {
		out := make([]string, 0, len(ls))
		for _, x := range ls {
			out = append(out, x.Addr().String())
		}
		return out
	}
	packets := func(pcs []net.PacketConn) []string {
		out := make([]string, 0, len(pcs))
		for _, pc := range pcs {
			out = append(out, pc.LocalAddr().String())
		}
		return out
	}
	failed := make(map[string]string, len(l.failed))
	maps.Copy(failed, l.failed)
	return api.ListenerInfo{
		Bound: map[string][]string{
			"dns-udp": packets(l.dnsUDP), "dns-tcp": addrs(l.dnsTCP), "cache": addrs(l.cache),
			"sni": addrs(l.sni), "web": addrs(l.web), "web-tls": addrs(l.webTLS),
			"dot": addrs(l.dot), "doh": addrs(l.doh), "ntp": packets(l.ntp),
		},
		Failed: failed,
	}
}

// savedFailures returns the messages of the saved roles that fell back
// (the health check "listeners" fails with them).
func (l *listeners) savedFailures() []string {
	var out []string
	for role := range l.savedFailed {
		out = append(out, l.failed[roleKey(role)])
	}
	sort.Strings(out)
	return slices.Compact(out)
}

func (l *listeners) closeAll() {
	l.closeOnce.Do(func() {
		l.dhcp.Close()
		for _, pc := range append(slices.Clone(l.dnsUDP), l.ntp...) {
			_ = pc.Close()
		}
		for _, group := range [][]net.Listener{l.dnsTCP, l.cache, l.sni, l.web, l.webTLS, l.dot, l.doh} {
			for _, ln := range group {
				_ = ln.Close()
			}
		}
	})
}
