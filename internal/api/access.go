package api

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// clientInfo is what the access middleware learned about a request.
type clientInfo struct {
	peer   netip.Addr // the TCP peer (canonical)
	client netip.Addr // the effective client: the peer, or the address a trusted proxy forwarded
	https  bool       // the effective scheme is https
}

// requestClient returns the client information of a request (derived from
// the request itself for handlers called without the middleware).
func requestClient(r *http.Request) clientInfo {
	if ci, ok := r.Context().Value(clientKey).(clientInfo); ok {
		return ci
	}
	ip := netutil.AddrFromRemote(r.RemoteAddr)
	return clientInfo{peer: ip, client: ip, https: r.TLS != nil}
}

// isHTTPS reports the effective scheme of a request: https when it arrived
// over TLS, or when a trusted proxy's right-most X-Forwarded-Proto value is
// https. Cookie names and flags, the HTTPS redirect, HSTS and the upload of
// a private key follow it.
func isHTTPS(r *http.Request) bool { return requestClient(r).https }

// clientAccess is the first middleware after recover (docs/ARCHITECTURE.md
// 12): it resolves the effective client of every request (X-Forwarded-For
// of a trusted proxy, netutil.ForwardedClient) and the effective scheme,
// refuses addresses outside the web ACL (stage 2; stage 1 closes such
// connections at accept), and rewrites r.RemoteAddr to the effective
// client so every consumer (throttles, sessions, audit, lookups) uses it.
// Only canonical addresses are stored, never header text.
func (s *Server) clientAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acl := s.web.Get()
		peer := netutil.AddrFromRemote(r.RemoteAddr)
		trusted := acl.TrustedProxy(peer)
		https := r.TLS != nil || (trusted && netutil.ForwardedHTTPS(r.Header.Values("X-Forwarded-Proto")))
		client, err := netutil.ForwardedClient(peer, r.Header.Values("X-Forwarded-For"), acl.TrustedProxy)
		if err != nil {
			s.setSecurityHeaders(w, https)
			writeError(w, r, s.log, apperr.Invalid("", "malformed X-Forwarded-For header"))
			return
		}
		if !acl.Allowed(peer) || !acl.Allowed(client) {
			s.web.Refuse(client, peer)
			refused := client
			if !acl.Allowed(peer) {
				refused = peer
			}
			s.setSecurityHeaders(w, https)
			s.refuseAccess(w, r, refused)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), clientKey, clientInfo{peer: peer, client: client, https: https}))
		if client.IsValid() {
			r.RemoteAddr = netip.AddrPortFrom(client, 0).String()
		}
		next.ServeHTTP(w, r)
	})
}

// refuseAccess answers a request from an address outside the web ACL: 403
// forbidden, or a plain-text page for browsers that ask for HTML.
func (s *Server) refuseAccess(w http.ResponseWriter, r *http.Request, addr netip.Addr) {
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "PiCache does not allow the web UI from "+addr.String()+". Ask the administrator to allow it "+
			"(System > Users & security > Web access), or run `picache web-access --reset` on the PiCache host.\n")
		return
	}
	writeError(w, r, s.log, apperr.Forbidden("this address may not use the web UI"))
}

// lockoutKind says why a web ACL would refuse a request (lockout).
type lockoutKind int

const (
	lockoutNone    lockoutKind = iota
	lockoutAddress             // the effective client is refused
	lockoutPeer                // the TCP peer is refused (a proxy that is no longer trusted)
	lockoutHeader              // the X-Forwarded-For header cannot be read with the candidate proxies
)

// lockout judges whether acl (built from candidate settings) would refuse
// the request: its original peer, and the effective client resolved again
// from the peer's X-Forwarded-For with the candidate trusted proxies. It
// returns the refused address (the peer for lockoutHeader and lockoutPeer).
// A refused peer is lockoutPeer when the request came through a proxy (its
// current effective client is another address): the candidate no longer
// trusts that proxy, so it would resolve to the refused proxy itself.
func lockout(r *http.Request, acl *netutil.WebACL) (lockoutKind, netip.Addr) {
	cur := requestClient(r)
	peer := cur.peer
	client, err := netutil.ForwardedClient(peer, r.Header.Values("X-Forwarded-For"), acl.TrustedProxy)
	switch {
	case err != nil:
		return lockoutHeader, peer
	case !acl.Allowed(peer) && cur.client != peer:
		return lockoutPeer, peer
	case !acl.Allowed(peer) || !acl.Allowed(client):
		return lockoutAddress, client
	}
	return lockoutNone, netip.Addr{}
}

// checkWebLockout refuses a settings change that would lock the requester
// out of the web UI (docs/ARCHITECTURE.md 6.1): whenever
// web.restrictToNetworks, web.allowedNetworks, web.trustedProxies or
// dns.allowedNetworks change, for every principal, admin tokens included.
// Requests from this machine always pass (the ACL always allows it). A
// document that does not validate is left to the settings store's error.
func (s *Server) checkWebLockout(r *http.Request, old, next *settings.All) error {
	cand := next.Clone()
	cand.Normalize()
	if cand.Validate() != nil {
		return nil
	}
	ow, nw := old.Web, cand.Web
	proxies := !slices.Equal(ow.TrustedProxies, nw.TrustedProxies)
	networks := !slices.Equal(ow.AllowedNetworks, nw.AllowedNetworks)
	dnsNetworks := !slices.Equal(old.DNS.AllowedNetworks, cand.DNS.AllowedNetworks)
	if ow.RestrictToNetworks == nw.RestrictToNetworks && !proxies && !networks && !dnsNetworks {
		return nil
	}
	field := "web.allowedNetworks"
	switch {
	case !ow.RestrictToNetworks && nw.RestrictToNetworks:
		field = "web.restrictToNetworks"
	case proxies:
		field = "web.trustedProxies"
	case !networks && dnsNetworks:
		field = "dns.allowedNetworks"
	}
	switch kind, addr := lockout(r, netutil.NewWebACL(cand)); kind {
	case lockoutAddress:
		return apperr.Invalid(field, "this change would lock out your address %s; allow it first", addr)
	case lockoutPeer:
		return apperr.Invalid(field, "this change would lock out your connection from %s; allow it first", addr)
	case lockoutHeader:
		return apperr.Invalid(field, "this change would lock you out: the X-Forwarded-For header of %s cannot be read", addr)
	}
	return nil
}

// checkTLSMinVersion refuses web.tlsMinVersion 1.3 from a request that
// itself arrived over TLS older than 1.3: that browser could not connect
// over HTTPS after the change.
func checkTLSMinVersion(r *http.Request, old, next *settings.All) error {
	v := strings.TrimSpace(next.Web.TLSMinVersion)
	if v == old.Web.TLSMinVersion || v != settings.TLSVersion13 || r.TLS == nil || r.TLS.Version >= tls.VersionTLS13 {
		return nil
	}
	return apperr.Invalid("web.tlsMinVersion", "your browser is connected with TLS 1.2 and could not connect over HTTPS after this change")
}

// restoreWarning judges the settings of a staged restore like a settings
// change (checkWebLockout, the TLS version) and returns the warning for
// the requester, or "". Nothing is refused: the restore may be meant for
// another network.
func restoreWarning(r *http.Request, staged *settings.All) string {
	if staged == nil {
		return ""
	}
	if kind, addr := lockout(r, netutil.NewWebACL(staged)); kind != lockoutNone {
		return "After the restart the restored settings will not allow your address " + addr.String() +
			" to use the web UI. Open PiCache from an allowed network or run `picache web-access --reset` on the host."
	}
	if staged.Web.TLSMinVersion == settings.TLSVersion13 && r.TLS != nil && r.TLS.Version < tls.VersionTLS13 {
		return "After the restart the restored settings require TLS 1.3, which your browser does not use for this connection. " +
			"Open PiCache over HTTP or run `picache web-access --reset` on the host."
	}
	return ""
}
