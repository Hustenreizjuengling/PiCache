package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Encrypted DNS for clients (docs/ARCHITECTURE.md 19, docs/API.md "DNS
// over HTTPS"): DoH on the web listeners, the status, the configuration
// profiles of Apple devices and the links to them.

// EncryptedDNSStatus is the state of encrypted DNS (GET /dns/encrypted).
// Lists are never null.
type EncryptedDNSStatus struct {
	ServerName  string               `json:"serverName"`
	PlainDNS    EncryptedPlainDNS    `json:"plainDns"`
	DoT         EncryptedDoT         `json:"dot"`
	DoH         EncryptedDoH         `json:"doh"`
	Certificate EncryptedCertificate `json:"certificate"`
	DDR         EncryptedDDR         `json:"ddr"`
	Queries     EncryptedQueries     `json:"queries"`
}

// EncryptedPlainDNS: Enabled is dns.plainDns; Served reports whether
// plain DNS answers other devices now (it stays open while nothing
// serves encrypted DNS).
type EncryptedPlainDNS struct {
	Enabled bool `json:"enabled"`
	Served  bool `json:"served"`
}

// EncryptedDoT is the state of DoT: Host is the server name (Android's
// "Private DNS provider hostname"), Port the port of the first bound DoT
// listener; Error says why an enabled DoT is not serving.
type EncryptedDoT struct {
	Enabled   bool     `json:"enabled"`
	Serving   bool     `json:"serving"`
	Listeners []string `json:"listeners"`
	Host      string   `json:"host,omitempty"`
	Port      int      `json:"port,omitzero"`
	Error     string   `json:"error,omitempty"`
}

// EncryptedDoH is the state of DoH: the bound listeners (role web-tls or
// doh) and the DoH URL of every distinct port.
type EncryptedDoH struct {
	Enabled   bool          `json:"enabled"`
	Serving   bool          `json:"serving"`
	Listeners []DoHListener `json:"listeners"`
	URLs      []string      `json:"urls"`
	Error     string        `json:"error,omitempty"`
}

// DoHListener is a bound listener that serves DoH.
type DoHListener struct {
	Address string `json:"address"`
	Role    string `json:"role"` // web-tls | doh
}

// EncryptedCertificate describes the certificate of the TLS listeners for
// the server name: Covered and WildcardCovered report whether the served
// leaf covers serverName and <label>.<serverName>.
type EncryptedCertificate struct {
	Source               string `json:"source"` // files | uploaded | local-ca | self-signed | none
	Usable               bool   `json:"usable"`
	Covered              bool   `json:"covered"`
	WildcardCovered      bool   `json:"wildcardCovered"`
	LocalCARenewalNeeded bool   `json:"localCaRenewalNeeded"`
	Error                string `json:"error,omitempty"`
}

// EncryptedDDR reports whether designated resolvers are announced to a
// client on this machine's LAN (Reason when not: off, no-server-name,
// not-serving, no-ip-address).
type EncryptedDDR struct {
	Active bool   `json:"active"`
	Reason string `json:"reason,omitempty"`
}

// EncryptedQueries are the DoT and DoH queries answered since the start.
type EncryptedQueries struct {
	DoT int64 `json:"dot"`
	DoH int64 `json:"doh"`
}

// EncryptedDNS is implemented by internal/app.
type EncryptedDNS interface {
	// EncryptedStatus returns the state of encrypted DNS now.
	EncryptedStatus() EncryptedDNSStatus
	// ProfileAddresses returns the ServerAddresses of a configuration
	// profile: per family dns.serverNameAddresses when set, else this
	// machine's own addresses without loopback (at most 8 per family).
	ProfileAddresses() []netip.Addr
}

// registerEncryptedRoutes registers DoH on the web listeners and the
// encrypted-DNS endpoints (docs/API.md).
func (s *Server) registerEncryptedRoutes() {
	// RFC 8484, outside /api/v1: public, no lock class, not in the route
	// registry. "/dns-query/" is more specific than the UI catch-all.
	s.mux.HandleFunc(dnsserver.DoHPath, s.webDoH)
	s.mux.HandleFunc(dnsserver.DoHPrefix, s.webDoH)

	s.route("GET /api/v1/dns/encrypted", permRead, s.dnsEncrypted)
	s.route("GET /api/v1/dns/profile.mobileconfig", permAdmin, s.dnsProfileDownload)
	// Creating a link changes no configuration: exempt from the lock.
	s.route("POST /api/v1/dns/profile-links", permAdmin, s.dnsProfileLinkCreate, routeExempt)
	s.route("GET /api/v1/dns/profile-links/{token}", permPublic, s.dnsProfileLinkGet)
	s.route("GET /api/v1/clients/dns-client-ids", permRead, s.clientsDNSClientIDs)
}

// webDoH serves DoH on the web listeners (after the web access, the host
// allowlist and the cross-origin protection): only when the effective
// scheme is https (404 on plain HTTP without a trusted proxy, never a
// redirect), never through a proxy that is not trusted (403), for the
// effective client.
func (s *Server) webDoH(w http.ResponseWriter, r *http.Request) {
	if s.d.DNS == nil || !s.d.Settings.Get().DNS.Encrypted.DoH {
		dnsserver.DoHNotFound(w)
		return
	}
	ci := requestClient(r)
	if !ci.https {
		dnsserver.DoHNotFound(w)
		return
	}
	if !s.web.Get().TrustedProxy(ci.peer) && hasProxyHeaders(r) {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "DoH through a proxy that is not in web.trustedProxies is refused", http.StatusForbidden)
		return
	}
	s.d.DNS.ServeDoH(w, r, ci.client)
}

// hasProxyHeaders reports a request that carries a client address a proxy
// added (X-Forwarded-For, Forwarded, X-Real-IP).
func hasProxyHeaders(r *http.Request) bool {
	return len(r.Header.Values("X-Forwarded-For")) > 0 || len(r.Header.Values("Forwarded")) > 0 || len(r.Header.Values("X-Real-IP")) > 0
}

func (s *Server) dnsEncrypted(w http.ResponseWriter, r *http.Request) error {
	if s.d.Encrypted == nil {
		return apperr.Unavailable("encrypted DNS is not available")
	}
	return ok(w, s.d.Encrypted.EncryptedStatus())
}

func (s *Server) clientsDNSClientIDs(w http.ResponseWriter, r *http.Request) error {
	if s.d.Clients == nil {
		return apperr.Unavailable("clients are not available")
	}
	return ok(w, s.d.Clients.DNSClientIDs())
}

// --- configuration profiles ---

// profileTargetFor checks the options against the current settings and
// listeners (the 409s of the profile checks, in order) and returns what
// the profile points at.
func (s *Server) profileTargetFor(o ProfileOptions) (profileTarget, error) {
	set := s.d.Settings.Get()
	e := set.DNS.Encrypted
	if e.ServerName == "" {
		return profileTarget{}, apperr.Conflict("set dns.encrypted.serverName first")
	}
	var st EncryptedDNSStatus
	if s.d.Encrypted != nil {
		st = s.d.Encrypted.EncryptedStatus()
	}
	li := ListenerInfo{}
	if s.d.Runtime != nil {
		li = s.d.Runtime.Listeners()
	}
	t := profileTarget{serverName: e.ServerName}
	if s.d.Runtime != nil {
		t.instance = s.d.Runtime.InstanceID()
	}
	switch o.Protocol {
	case profileProtoDoH:
		if !e.DoH {
			return t, apperr.Conflict("DoH is not enabled")
		}
		if !st.DoH.Serving {
			return t, apperr.Conflict("DoH is not serving")
		}
		t.dohPort = firstPort(li.Bound["doh"])
		if t.dohPort == 0 {
			t.dohPort = firstPort(li.Bound["web-tls"])
		}
	default:
		if !e.DoT {
			return t, apperr.Conflict("DoT is not enabled")
		}
		if !st.DoT.Serving {
			return t, apperr.Conflict("DoT is not serving")
		}
		if !hasPort(li.Bound["dot"], 853) {
			return t, apperr.Conflict("Apple devices use DoT on port 853 only; PICACHE_DOT_LISTEN has no listener on port 853")
		}
	}
	if o.Addresses && s.d.Encrypted != nil {
		t.addrs = s.d.Encrypted.ProfileAddresses()
	}
	return t, nil
}

// firstPort returns the port of the first address (0 if none).
func firstPort(addrs []string) int {
	for _, a := range addrs {
		if _, p, err := net.SplitHostPort(a); err == nil {
			if n, err := strconv.Atoi(p); err == nil && n > 0 && n <= 65535 {
				return n
			}
		}
	}
	return 0
}

func hasPort(addrs []string, port int) bool {
	for _, a := range addrs {
		if firstPort([]string{a}) == port {
			return true
		}
	}
	return false
}

// writeProfile answers with the profile (HEAD: the headers only).
func writeProfile(w http.ResponseWriter, r *http.Request, o ProfileOptions, t profileTarget) error {
	b := buildProfile(o, t)
	h := w.Header()
	h.Set("Content-Type", profileMediaType)
	h.Set("Content-Disposition", `attachment; filename="`+profileFilename(o)+`"`)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
	return nil
}

// dnsProfileDownload serves a profile directly: only over HTTPS or to this
// machine (on plain HTTP a profile could be replaced on the way). Not
// audited.
func (s *Server) dnsProfileDownload(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	o := ProfileOptions{Protocol: q.Get("protocol"), DNSClientID: q.Get("dnsClientId"), SSIDs: q["ssid"]}
	switch v := strings.TrimSpace(q.Get("addresses")); v {
	case "", "false":
	case "true":
		o.Addresses = true
	default:
		return apperr.Invalid("addresses", "must be true or false")
	}
	if err := o.normalize(); err != nil {
		return err
	}
	t, err := s.profileTargetFor(o)
	if err != nil {
		return err
	}
	if ci := requestClient(r); !ci.https && !ci.client.IsLoopback() {
		return apperr.Conflict("download profiles over HTTPS: on plain HTTP a profile could be replaced on the way")
	}
	return writeProfile(w, r, o, t)
}

// Profile links (docs/ARCHITECTURE.md 19): 32 random bytes (base64url),
// kept in memory keyed by their SHA-256, at most 32 (the oldest dropped),
// valid 15 minutes and reusable until then, lost at restart. The token
// never appears in logs, the audit or error messages.
const (
	maxProfileLinks   = 32
	profileLinkTTL    = 15 * time.Minute
	profileLinkPath   = "/api/v1/dns/profile-links/"
	profileLinkRate   = 20 // requests per minute and client key
	profileLinkKeys   = 256
	profileLinkWindow = time.Minute
)

// profileLinks is the in-memory store of profile links and the throttle
// of their downloads.
type profileLinks struct {
	mu    sync.Mutex
	links map[[32]byte]*profileLink
	now   func() time.Time

	hits map[netip.Prefix]*linkHits // per client key, within the window
}

type profileLink struct {
	opts    ProfileOptions
	created time.Time
	expires time.Time
}

type linkHits struct {
	start time.Time
	n     int
}

func newProfileLinks() *profileLinks {
	return &profileLinks{links: map[[32]byte]*profileLink{}, hits: map[netip.Prefix]*linkHits{}, now: time.Now}
}

// add stores a link and returns its token and expiry.
func (p *profileLinks) add(o ProfileOptions) (string, time.Time, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, l := range p.links {
		if !now.Before(l.expires) {
			delete(p.links, k)
		}
	}
	for len(p.links) >= maxProfileLinks {
		var oldest [32]byte
		var first *profileLink
		for k, l := range p.links {
			if first == nil || l.created.Before(first.created) {
				oldest, first = k, l
			}
		}
		delete(p.links, oldest)
	}
	l := &profileLink{opts: o, created: now, expires: now.Add(profileLinkTTL)}
	p.links[sha256.Sum256([]byte(token))] = l
	return token, l.expires, nil
}

// get returns the options of a valid link.
func (p *profileLinks) get(token string) (ProfileOptions, bool) {
	key := sha256.Sum256([]byte(token))
	p.mu.Lock()
	defer p.mu.Unlock()
	l, ok := p.links[key]
	if !ok {
		return ProfileOptions{}, false
	}
	if !p.now().Before(l.expires) {
		delete(p.links, key)
		return ProfileOptions{}, false
	}
	return l.opts, true
}

// allow counts a download of key: at most 20 per minute; at most 256 keys
// are tracked (beyond: refused).
func (p *profileLinks) allow(key netip.Prefix) bool {
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.hits[key]
	if ok && now.Sub(h.start) >= profileLinkWindow {
		h.start, h.n = now, 0
	}
	if !ok {
		if len(p.hits) >= profileLinkKeys {
			for k, v := range p.hits {
				if now.Sub(v.start) >= profileLinkWindow {
					delete(p.hits, k)
				}
			}
			if len(p.hits) >= profileLinkKeys {
				return false
			}
		}
		h = &linkHits{start: now}
		p.hits[key] = h
	}
	h.n++
	return h.n <= profileLinkRate
}

// profileLinkResult is the response of POST /dns/profile-links.
type profileLinkResult struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// dnsProfileLinkCreate creates a link to a profile on the HTTPS web
// listener under the server name (a device that can open it can resolve
// and verify the server the profile names).
func (s *Server) dnsProfileLinkCreate(w http.ResponseWriter, r *http.Request) error {
	var o ProfileOptions
	if err := decode(w, r, &o); err != nil {
		return err
	}
	if err := o.normalize(); err != nil {
		return err
	}
	if _, err := s.profileTargetFor(o); err != nil {
		return err
	}
	port := s.httpsPort()
	if port == 0 {
		return apperr.Conflict("profile links need the HTTPS web listener (PICACHE_WEB_TLS_LISTEN)")
	}
	token, expires, err := s.links.add(o)
	if err != nil {
		return err
	}
	u := "https://" + s.d.Settings.Get().DNS.Encrypted.ServerName
	if port != 443 {
		u += ":" + strconv.Itoa(port)
	}
	s.audit(r, "dns.profile_link.create", o.Protocol, map[string]string{"protocol": o.Protocol, "dnsClientId": o.DNSClientID})
	return created(w, profileLinkResult{URL: u + profileLinkPath + token, ExpiresAt: expires.UTC()})
}

// dnsProfileLinkGet serves the profile of a link (built from the current
// settings; HEAD answers the headers), throttled per client key.
func (s *Server) dnsProfileLinkGet(w http.ResponseWriter, r *http.Request) error {
	if !s.links.allow(netutil.ClientKey(requestClient(r).client)) {
		return apperr.TooMany("too many profile downloads; try again in a minute")
	}
	o, found := s.links.get(r.PathValue("token"))
	if !found {
		return &apperr.Error{Kind: apperr.KindNotFound, Message: "the profile link does not exist or has expired"}
	}
	t, err := s.profileTargetFor(o)
	if err != nil {
		return err
	}
	return writeProfile(w, r, o, t)
}

// logPath returns the path of a request for the log: a profile link's
// token is never logged.
func logPath(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, profileLinkPath) {
		return profileLinkPath + "…"
	}
	return r.URL.Path
}

// --- settings rules (updateSettings) and the restore warning ---

// checkEncryptedDNS applies the rules of encrypted DNS that need the
// running system (after settings.Validate): the server name must not be
// a resolv.conf search domain or a parent of one; plain DNS can only be
// off while an enabled protocol has a bound listener (checked when plain
// DNS is switched off or dns.encrypted changes). It runs inside the
// store's update callback, before the store validates: a candidate that
// settings.Validate refuses is left to it, so its rules (a malformed
// server name, plain DNS off with both protocols off) win.
func (s *Server) checkEncryptedDNS(old, next *settings.All) error {
	cand := next.Clone()
	cand.Normalize()
	if cand.Validate() != nil {
		return nil // the store's Validate reports it
	}
	e := cand.DNS.Encrypted
	if e.ServerName != "" && e.ServerName != old.DNS.Encrypted.ServerName {
		search := netutil.ResolvConfSearch
		if s.searchDomains != nil {
			search = s.searchDomains
		}
		for _, d := range search() {
			d = strings.Trim(strings.ToLower(d), ".")
			if d != "" && (d == e.ServerName || strings.HasSuffix(d, "."+e.ServerName)) {
				return apperr.Invalid("dns.encrypted.serverName", "must not be a search domain or a parent of one")
			}
		}
	}
	if cand.DNS.PlainDNS || (!old.DNS.PlainDNS && old.DNS.Encrypted == e) {
		return nil
	}
	if !s.encryptedListenerBound(e) {
		return apperr.Invalid("dns.plainDns", "plain DNS can only be switched off while DoT or DoH is enabled and its listener is running")
	}
	return nil
}

// encryptedListenerBound reports whether an enabled protocol has a bound
// listener on this host (DoT: a dot listener; DoH: a web-tls or doh
// listener).
func (s *Server) encryptedListenerBound(e settings.EncryptedDNS) bool {
	if s.d.Runtime == nil {
		return false
	}
	b := s.d.Runtime.Listeners().Bound
	return (e.DoT && len(b["dot"]) > 0) || (e.DoH && (len(b["web-tls"]) > 0 || len(b["doh"]) > 0))
}

// restoreDNSWarning warns when the staged settings turn plain DNS off
// but none of their enabled protocols has a listener on this host.
func (s *Server) restoreDNSWarning(staged *settings.All) string {
	if staged == nil || staged.DNS.PlainDNS || s.encryptedListenerBound(staged.DNS.Encrypted) {
		return ""
	}
	return "After the restart the restored settings turn plain DNS off, but no encrypted DNS listener of this host serves the enabled protocols: plain DNS stays on until one does."
}
