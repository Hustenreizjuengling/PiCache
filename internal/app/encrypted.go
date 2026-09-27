package app

import (
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Encrypted DNS for clients (docs/ARCHITECTURE.md 19): the DoT and DoH
// listeners, the snapshot the DNS server reads (dnsserver.EncryptedState),
// the status (api.EncryptedDNS) and the health check "encrypted-dns".

// Bounds of the encrypted DNS listeners.
const (
	encPerClient = 32   // connections per client key (DoT, dedicated DoH)
	encTotal     = 1024 // connections in total per listener
)

// encryptedSnapshot builds the state of encrypted DNS: a protocol is
// serving while it is switched on, a listener of it is bound and the
// certificate is usable (no fallback, not expired).
func (a *App) encryptedSnapshot(now time.Time) *dnsserver.EncryptedState {
	e := a.set.Get().DNS.Encrypted
	usable := a.webTLS != nil && a.webTLS.listener && a.webTLS.usable(now)
	st := &dnsserver.EncryptedState{
		DoT:      e.DoT && len(a.ln.dot) > 0 && usable,
		DoH:      e.DoH && len(a.ln.doh)+len(a.ln.webTLS) > 0 && usable,
		DoTPorts: distinctPorts(a.ln.dot),
		DoHPorts: distinctPorts(a.ln.doh, a.ln.webTLS),
	}
	if a.webTLS != nil {
		st.LeafIPs = a.webTLS.leafIPs()
	}
	return st
}

// distinctPorts returns the distinct ports of the listeners in order.
func distinctPorts(groups ...[]net.Listener) []uint16 {
	var out []uint16
	for _, g := range groups {
		for _, ln := range g {
			if ap, err := netip.ParseAddrPort(ln.Addr().String()); err == nil && !slices.Contains(out, ap.Port()) {
				out = append(out, ap.Port())
			}
		}
	}
	return out
}

// refreshEncrypted rebuilds the snapshot (settings, listener and
// certificate changes, every tick) and logs an ERROR once each time plain
// DNS falls back to serving everyone because it is switched off but
// nothing encrypted serves. Refreshes run one at a time (encMu): one that
// read older settings or an older certificate can never overwrite the
// snapshot of a later one.
func (a *App) refreshEncrypted() {
	a.encMu.Lock()
	defer a.encMu.Unlock()
	st := a.encryptedSnapshot(time.Now())
	a.encState.Store(st)
	failOpen := !a.set.Get().DNS.PlainDNS && !st.Serving()
	if prev := a.encFailOpen.Swap(failOpen); failOpen && !prev {
		a.log.Error("plain DNS is still served: no encrypted DNS listener is running",
			slog.String("hint", "check PICACHE_DOT_LISTEN, PICACHE_DOH_LISTEN and the certificate"))
	}
}

// encrypted returns the snapshot for the DNS server.
func (a *App) encrypted() *dnsserver.EncryptedState { return a.encState.Load() }

// serveEncrypted starts the DoT and dedicated DoH listeners: the DNS ACL
// and the connection caps at accept (before TLS), the switch of the
// protocol (closed right after accept while it is off; open connections
// closed when it is switched off), then TLS with the certificate of the
// TLS listeners. It returns the DoT listeners for the DNS server and the
// DoH servers (shut down with the web servers).
func (a *App) serveEncrypted(goRun func(string, func() error)) (dot []net.Listener, servers []*http.Server) {
	aclFn := a.acl.Get
	var dotSw, dohSw []*netutil.SwitchListener
	for _, ln := range a.ln.dot {
		sw := netutil.NewSwitchListener(netutil.LimitListener(ln, aclFn, encPerClient, encTotal),
			func() bool { return a.set.Get().DNS.Encrypted.DoT }, encTotal)
		dotSw = append(dotSw, sw)
		dot = append(dot, tls.NewListener(sw, a.webTLS.tlsConfigFor("dot")))
	}
	if len(a.ln.doh) > 0 {
		srv := &http.Server{
			Handler:           a.dohHandler(),
			TLSConfig:         a.webTLS.tlsConfigFor("h2", "http/1.1"),
			ReadHeaderTimeout: 5 * time.Second, // also bounds the TLS handshake
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      15 * time.Second,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    16 << 10,
			HTTP2:             &http.HTTP2Config{MaxConcurrentStreams: 32},
			ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelDebug),
		}
		servers = append(servers, srv)
		for _, ln := range a.ln.doh {
			sw := netutil.NewSwitchListener(netutil.LimitListener(ln, aclFn, encPerClient, encTotal),
				func() bool { return a.set.Get().DNS.Encrypted.DoH }, encTotal)
			dohSw = append(dohSw, sw)
			goRun("doh", func() error { return srv.ServeTLS(sw, "", "") })
		}
	}
	a.set.Subscribe(func(o, n *settings.All) {
		if o.DNS.Encrypted.DoT && !n.DNS.Encrypted.DoT {
			for _, sw := range dotSw {
				sw.CloseAll()
			}
		}
		if o.DNS.Encrypted.DoH && !n.DNS.Encrypted.DoH {
			for _, sw := range dohSw {
				sw.CloseAll()
			}
		}
	})
	return dot, servers
}

// dohHandler serves the dedicated DoH listeners: only the two DoH paths
// (the TCP peer is the source; requests through a proxy belong on the web
// listener), a bare 404 for everything else (no UI, API, health or
// metrics, no cookies).
func (a *App) dohHandler() http.Handler {
	doh := func(w http.ResponseWriter, r *http.Request) {
		if !a.set.Get().DNS.Encrypted.DoH {
			dnsserver.DoHNotFound(w)
			return
		}
		if len(r.Header.Values("X-Forwarded-For"))+len(r.Header.Values("Forwarded"))+len(r.Header.Values("X-Real-IP")) > 0 {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "DoH through a proxy is served on the web listener only", http.StatusForbidden)
			return
		}
		a.dns.ServeDoH(w, r, netutil.PeerFromRemote(r.RemoteAddr))
	}
	mux := http.NewServeMux()
	mux.HandleFunc(dnsserver.DoHPath, doh)
	mux.HandleFunc(dnsserver.DoHPrefix, doh)
	mux.HandleFunc("/", http.NotFound)
	return mux
}

// --- api.EncryptedDNS ---

// EncryptedStatus returns the state of encrypted DNS (GET /dns/encrypted).
func (a *App) EncryptedStatus() api.EncryptedDNSStatus {
	set := a.set.Get()
	e := set.DNS.Encrypted
	st := a.encState.Load()
	if st == nil {
		st = &dnsserver.EncryptedState{}
	}
	li := a.Listeners()
	out := api.EncryptedDNSStatus{
		ServerName: e.ServerName,
		PlainDNS:   api.EncryptedPlainDNS{Enabled: set.DNS.PlainDNS, Served: set.DNS.PlainDNS || !st.Serving()},
		DoT:        api.EncryptedDoT{Enabled: e.DoT, Serving: st.DoT, Listeners: li.Bound["dot"], Host: e.ServerName},
		DoH:        api.EncryptedDoH{Enabled: e.DoH, Serving: st.DoH, Listeners: []api.DoHListener{}, URLs: []string{}},
	}
	if out.DoT.Listeners == nil {
		out.DoT.Listeners = []string{}
	}
	if len(li.Bound["dot"]) > 0 {
		if ap, err := netip.ParseAddrPort(li.Bound["dot"][0]); err == nil {
			out.DoT.Port = int(ap.Port())
		}
	}
	for _, role := range []string{"doh", "web-tls"} {
		for _, addr := range li.Bound[role] {
			out.DoH.Listeners = append(out.DoH.Listeners, api.DoHListener{Address: addr, Role: role})
		}
	}
	if e.ServerName != "" {
		for _, p := range st.DoHPorts {
			u := "https://" + e.ServerName
			if p != 443 {
				u += ":" + strconv.Itoa(int(p))
			}
			out.DoH.URLs = append(out.DoH.URLs, u+"/dns-query")
		}
	}

	tst := api.TLSStatus{Source: sourceNone}
	if a.webTLS != nil {
		tst = a.webTLS.Status()
	}
	cert := api.EncryptedCertificate{Source: tst.Source, Error: tst.Error}
	if a.webTLS != nil {
		cert.Usable = a.webTLS.listener && a.webTLS.usable(time.Now())
		if leaf := a.webTLS.served(); leaf != nil && e.ServerName != "" {
			cert.Covered = leaf.VerifyHostname(e.ServerName) == nil
			cert.WildcardCovered = leaf.VerifyHostname(wildcardProbe+"."+e.ServerName) == nil
		}
	}
	cert.LocalCARenewalNeeded = tst.LocalCA != nil && tst.LocalCA.RenewalNeeded
	out.Certificate = cert

	certErr := func() string {
		switch {
		case cert.Usable:
			return ""
		case tst.Error != "":
			return "the certificate cannot be used: " + tst.Error
		case tst.Certificate != nil && !tst.Certificate.NotAfter.After(time.Now()):
			return "the certificate cannot be used: it expired on " + tst.Certificate.NotAfter.UTC().Format(time.DateOnly)
		}
		return "the certificate cannot be used: no certificate is served"
	}
	if e.DoT && !st.DoT {
		switch {
		case len(a.cfg.DoTListen) == 0:
			out.DoT.Error = "PICACHE_DOT_LISTEN is off"
		case len(li.Bound["dot"]) == 0 && li.Failed["dot"] != "":
			out.DoT.Error = li.Failed["dot"]
		default:
			out.DoT.Error = certErr()
		}
	}
	if e.DoH && !st.DoH {
		var binds []string
		for _, role := range []string{"web-tls", "doh"} {
			if f := li.Failed[role]; f != "" {
				binds = append(binds, f)
			}
		}
		switch {
		case len(a.cfg.WebTLSListen) == 0 && len(a.cfg.DoHListen) == 0:
			out.DoH.Error = "no DoH listener: PICACHE_WEB_TLS_LISTEN and PICACHE_DOH_LISTEN are off"
		case len(li.Bound["web-tls"])+len(li.Bound["doh"]) == 0 && len(binds) > 0:
			out.DoH.Error = strings.Join(binds, "; ")
		default:
			out.DoH.Error = certErr()
		}
	}

	out.DDR.Reason = dnsserver.DDRReason(e, st, a.lanAddrs())
	out.DDR.Active = out.DDR.Reason == ""
	if a.dns != nil {
		out.Queries.DoT, out.Queries.DoH = a.dns.EncryptedQueries()
	}
	return out
}

// lanAddrs returns this machine's own addresses (A) without loopback.
func (a *App) lanAddrs() []netip.Addr {
	if a.webTLS == nil {
		return nil
	}
	var out []netip.Addr
	for _, ip := range ownAddrs(a.webTLS.hostAddrs(), a.webTLS.bridge()) {
		if !ip.IsLoopback() {
			out = append(out, ip)
		}
	}
	return out
}

// ProfileAddresses returns the ServerAddresses of a configuration profile:
// per family dns.serverNameAddresses when set, else this machine's own
// addresses without loopback, at most 8 per family.
func (a *App) ProfileAddresses() []netip.Addr {
	sa := a.set.Get().DNS.ServerNameAddresses
	own := a.lanAddrs()
	family := func(configured []string, v6 bool) []netip.Addr {
		var out []netip.Addr
		for _, s := range configured {
			if ip, err := netip.ParseAddr(s); err == nil && ip.Is6() == v6 {
				out = append(out, ip)
			}
		}
		if len(out) == 0 {
			for _, ip := range own {
				if ip.Is6() == v6 {
					out = append(out, ip)
				}
			}
		}
		return out[:min(len(out), settings.MaxServerNameAddresses)]
	}
	return append(family(sa.IPv4, false), family(sa.IPv6, true)...)
}

// --- health check "encrypted-dns" ---

// encryptedHealth evaluates the check "encrypted-dns" (present while DoT
// or DoH is on or plain DNS is off; the first matching row wins).
func (a *App) encryptedHealth() (status, msg, hint string, show bool) {
	set := a.set.Get()
	e := set.DNS.Encrypted
	if !e.Enabled() && set.DNS.PlainDNS {
		return "", "", "", false
	}
	st := a.EncryptedStatus()
	switch {
	case !set.DNS.PlainDNS && !st.DoT.Serving && !st.DoH.Serving:
		return "fail", "plain DNS is still served: no encrypted DNS listener is running",
			"check PICACHE_DOT_LISTEN, PICACHE_DOH_LISTEN and the certificate (System > HTTPS certificate); " +
				"PiCache closes plain DNS again as soon as DoT or DoH is serving", true
	case (e.DoT && !st.DoT.Serving) || (e.DoH && !st.DoH.Serving):
		var parts []string
		if e.DoT && !st.DoT.Serving {
			parts = append(parts, "DoT is enabled but not serving: "+st.DoT.Error)
		}
		if e.DoH && !st.DoH.Serving {
			parts = append(parts, "DoH is enabled but not serving: "+st.DoH.Error)
		}
		return "warn", strings.Join(parts, "; "), "see DNS > Settings > Encrypted DNS", true
	case e.ServerName != "" && !st.Certificate.Covered:
		return "warn", "the certificate does not cover " + e.ServerName + ": devices refuse encrypted DNS",
			"upload a certificate for it under System > HTTPS certificate, or use the local CA on devices that trust it", true
	}
	return "ok", "", "", true
}
