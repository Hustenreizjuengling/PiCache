package clients

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// The name source whois (clients.nameSources.whois, off by default: it
// sends data out) annotates public source addresses with the owner of
// their network, looked up by RDAP (docs/ARCHITECTURE.md 6.1 "WHOIS
// privacy"). It is never a name. Only public unicast sources are looked up
// that are not inside a network this machine is connected to, not in the
// neighbour table, not inside an entry of dns.allowedNetworks of /24 or
// longer (IPv4) or /48 or longer (IPv6), not a trusted EDNS forwarder, not
// inside the /48 of one of this machine's global IPv6 addresses, and never
// while client addresses are anonymised. The query is for the masked
// network (IPv4 /24, IPv6 /48), never the host address. The IANA bootstrap
// files are fetched when needed and kept 24 h; results are kept in an LRU
// of 4096 networks (found 7 days, not found 24 h; memory only). At most
// one lookup per 10 s and 100 per day, a queue of 64 networks (beyond it
// dropped), one worker off the DNS hot path (the query path only
// enqueues). The HTTP client (app: https only, TLS 1.2+ verified against
// the system roots, public destinations only, no proxy, resolution via the
// upstream bypass resolver) is limited here to 10 s per request, 256 KiB
// per response and 3 https redirects.

// WhoisInfo is the owner of the network of a public source address.
type WhoisInfo struct {
	Org     string `json:"org"`
	Country string `json:"country,omitempty"`
}

// Bounds and intervals of the WHOIS lookups.
const (
	whoisCacheSize    = 4096
	whoisPositive     = 7 * 24 * time.Hour
	whoisNegative     = 24 * time.Hour
	whoisGap          = 10 * time.Second
	whoisPerDay       = 100
	whoisQueue        = 64
	whoisTimeout      = 10 * time.Second
	whoisMaxBody      = 256 << 10
	whoisMaxRedirects = 3
	whoisBootstrapTTL = 24 * time.Hour
	maxWhoisText      = 100
)

// IANA's RDAP bootstrap files.
const (
	rdapBootstrapV4 = "https://data.iana.org/rdap/ipv4.json"
	rdapBootstrapV6 = "https://data.iana.org/rdap/ipv6.json"
)

type whoisEntry struct {
	info *WhoisInfo // nil: not found
	at   time.Time
}

type whoisItem struct {
	ip      netip.Addr
	network netip.Prefix
}

// bootService maps networks to the RDAP servers of a registry.
type bootService struct {
	prefixes []netip.Prefix
	url      string // the first https URL
}

type bootstrap struct {
	services []bootService
	at       time.Time
}

type whoisLookup struct {
	r         *Registry
	client    atomic.Pointer[http.Client]
	userAgent atomic.Pointer[string]

	// Replaced in tests.
	bootstrapURL func(v6 bool) string
	now          func() time.Time
	sleep        func(ctx context.Context, d time.Duration) bool
	connected    func() []netip.Prefix
	hostAddrs    func() []netutil.HostAddr

	mu       sync.Mutex
	cache    *lru[netip.Prefix, whoisEntry]
	queued   map[netip.Prefix]bool
	queue    chan whoisItem
	boot     [2]*bootstrap // IPv4, IPv6
	last     time.Time     // the last lookup
	dayStart time.Time
	dayCount int
	lookups  atomic.Int64 // RDAP requests made (tests, diagnostics)
}

func newWhoisLookup(r *Registry) *whoisLookup {
	return &whoisLookup{r: r, cache: newLRU[netip.Prefix, whoisEntry](whoisCacheSize), queued: map[netip.Prefix]bool{},
		queue: make(chan whoisItem, whoisQueue), now: time.Now, sleep: sleepContext,
		connected: netutil.ConnectedSubnets, hostAddrs: netutil.HostAddrs,
		bootstrapURL: func(v6 bool) string {
			if v6 {
				return rdapBootstrapV6
			}
			return rdapBootstrapV4
		}}
}

// SetWhoisClient sets the HTTP client of the RDAP lookups (nil: none are
// made) and the User-Agent they send.
func (r *Registry) SetWhoisClient(c *http.Client, userAgent string) {
	r.whois.client.Store(c)
	r.whois.userAgent.Store(&userAgent)
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// whoisNetwork returns the network looked up for ip (IPv4 /24, IPv6 /48).
func whoisNetwork(ip netip.Addr) netip.Prefix {
	bits := 48
	if ip.Is4() {
		bits = 24
	}
	p, _ := ip.Prefix(bits)
	return p
}

// candidate reports the cheap part of the eligibility (the query path):
// the source is on, client addresses are not anonymised, the address is
// public unicast (no embedded IPv4) and a client is set.
func (w *whoisLookup) candidate(ip netip.Addr) bool {
	cfg := w.r.config()
	if !cfg.Sources.WHOIS || cfg.Anonymize || w.client.Load() == nil {
		return false
	}
	if _, embedded := netutil.EmbeddedIPv4(ip); embedded {
		return false
	}
	return netutil.IsPublicUnicast(ip)
}

// eligible reports the whole eligibility of a source address (the worker).
func (w *whoisLookup) eligible(ip netip.Addr) bool {
	if !w.candidate(ip) {
		return false
	}
	cfg := w.r.config()
	for _, p := range w.connected() {
		if p.Contains(ip) {
			return false
		}
	}
	if (*w.r.arp.Load())[ip] != "" {
		return false
	}
	for _, p := range cfg.AllowedNetworks {
		if p.Contains(ip) && ((ip.Is4() && p.Bits() >= 24) || (ip.Is6() && p.Bits() >= 48)) {
			return false
		}
	}
	for _, f := range cfg.TrustedForwarders {
		if f == ip {
			return false
		}
	}
	if ip.Is6() {
		for _, a := range w.hostAddrs() {
			h := a.Prefix.Addr()
			if h.Is6() && h.IsGlobalUnicast() && !netutil.IsULA(h) && !h.IsLinkLocalUnicast() {
				if p, _ := h.Prefix(48); p.Contains(ip) {
					return false
				}
			}
		}
	}
	return true
}

// enqueue schedules a lookup of the network of ip (non-blocking; the query
// path calls it for new addresses).
func (w *whoisLookup) enqueue(ip netip.Addr) {
	ip = netutil.Canon(ip)
	if !w.candidate(ip) {
		return
	}
	n := whoisNetwork(ip)
	w.mu.Lock()
	defer w.mu.Unlock()
	if e, ok := w.cache.peek(n); (ok && w.fresh(e)) || w.queued[n] {
		return
	}
	select {
	case w.queue <- whoisItem{ip: ip, network: n}:
		w.queued[n] = true
	default: // full: dropped
	}
}

// fresh reports whether a cache entry is still valid.
func (w *whoisLookup) fresh(e whoisEntry) bool {
	ttl := whoisPositive
	if e.info == nil {
		ttl = whoisNegative
	}
	return w.now().Sub(e.at) < ttl
}

// info returns the cached annotation of ip (nil if none, the source is off
// or client addresses are anonymised) and schedules a lookup when an
// eligible address has none.
func (w *whoisLookup) info(ip netip.Addr) *WhoisInfo {
	cfg := w.r.config()
	if !cfg.Sources.WHOIS || cfg.Anonymize {
		return nil
	}
	ip = netutil.Canon(ip)
	if !w.candidate(ip) {
		return nil
	}
	w.mu.Lock()
	e, ok := w.cache.peek(whoisNetwork(ip))
	w.mu.Unlock()
	if !ok || !w.fresh(e) {
		w.enqueue(ip)
	}
	if ok && e.info != nil && w.now().Sub(e.at) < whoisPositive {
		c := *e.info
		return &c
	}
	return nil
}

// reset drops the queue and the cache (the source was switched off, client
// addresses are anonymised, the seen data was flushed).
func (w *whoisLookup) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cache.clear()
	clear(w.queued)
	for {
		select {
		case <-w.queue:
		default:
			return
		}
	}
}

// run is the worker: one lookup at a time, at most one per 10 s and 100 per
// day.
func (w *whoisLookup) run(ctx context.Context) {
	for {
		var it whoisItem
		select {
		case <-ctx.Done():
			return
		case it = <-w.queue:
		}
		w.process(ctx, it)
	}
}

// process looks up one queued network.
func (w *whoisLookup) process(ctx context.Context, it whoisItem) {
	defer func() {
		w.mu.Lock()
		delete(w.queued, it.network)
		w.mu.Unlock()
	}()
	if !w.eligible(it.ip) {
		return
	}
	w.mu.Lock()
	if e, ok := w.cache.peek(it.network); ok && w.fresh(e) {
		w.mu.Unlock()
		return
	}
	now := w.now()
	if now.Sub(w.dayStart) >= 24*time.Hour {
		w.dayStart, w.dayCount = now, 0
	}
	if w.dayCount >= whoisPerDay {
		w.mu.Unlock()
		return
	}
	wait := whoisGap - now.Sub(w.last)
	w.mu.Unlock()
	if !w.last.IsZero() && wait > 0 && !w.sleep(ctx, wait) {
		return
	}
	w.mu.Lock()
	w.last = w.now()
	w.dayCount++
	w.mu.Unlock()
	info, err := w.lookup(ctx, it.network)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		w.r.log.Debug("WHOIS lookup failed", slog.String("network", it.network.String()), slog.Any("err", err))
		info = nil
	}
	cfg := w.r.config()
	if !cfg.Sources.WHOIS || cfg.Anonymize {
		return // switched off meanwhile
	}
	w.mu.Lock()
	w.cache.put(it.network, whoisEntry{info: info, at: w.now()})
	w.mu.Unlock()
}

// lookup asks the RDAP server of the network's registry.
func (w *whoisLookup) lookup(ctx context.Context, network netip.Prefix) (*WhoisInfo, error) {
	base, err := w.rdapBase(ctx, network)
	if err != nil {
		return nil, err
	}
	b, err := w.get(ctx, strings.TrimSuffix(base, "/")+"/ip/"+network.Addr().String())
	if err != nil {
		return nil, err
	}
	return parseRDAP(b)
}

// rdapBase returns the RDAP base URL for a network from IANA's bootstrap
// file of its family (fetched when needed, kept 24 h).
func (w *whoisLookup) rdapBase(ctx context.Context, network netip.Prefix) (string, error) {
	fam := 0
	if network.Addr().Is6() {
		fam = 1
	}
	w.mu.Lock()
	bs := w.boot[fam]
	w.mu.Unlock()
	if bs == nil || w.now().Sub(bs.at) >= whoisBootstrapTTL {
		b, err := w.get(ctx, w.bootstrapURL(fam == 1))
		if err != nil {
			return "", fmt.Errorf("bootstrap: %w", err)
		}
		services, err := parseBootstrap(b)
		if err != nil {
			return "", fmt.Errorf("bootstrap: %w", err)
		}
		bs = &bootstrap{services: services, at: w.now()}
		w.mu.Lock()
		w.boot[fam] = bs
		w.mu.Unlock()
	}
	best, url := -1, ""
	for _, s := range bs.services {
		for _, p := range s.prefixes {
			if p.Contains(network.Addr()) && p.Bits() > best {
				best, url = p.Bits(), s.url
			}
		}
	}
	if url == "" {
		return "", errors.New("no RDAP server for the network")
	}
	return url, nil
}

// get fetches a URL: https only, 10 s, at most 256 KiB, at most 3 https
// redirects, 200 only.
func (w *whoisLookup) get(ctx context.Context, u string) ([]byte, error) {
	base := w.client.Load()
	if base == nil {
		return nil, errors.New("no client")
	}
	if !strings.HasPrefix(u, "https://") {
		return nil, errors.New("not an https URL")
	}
	c := *base
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > whoisMaxRedirects {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" {
			return errors.New("a redirect to a URL that is not https")
		}
		return nil
	}
	c.Timeout = whoisTimeout
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/rdap+json, application/json")
	if ua := w.userAgent.Load(); ua != nil && *ua != "" {
		req.Header.Set("User-Agent", *ua)
	}
	w.lookups.Add(1)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, whoisMaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > whoisMaxBody {
		return nil, errors.New("the answer is larger than 256 KiB")
	}
	return b, nil
}

// parseBootstrap parses an IANA RDAP bootstrap file (RFC 9224):
// {"services":[[["<prefix>", …], ["<url>", …]], …]}; services without an
// https URL are skipped.
func parseBootstrap(b []byte) ([]bootService, error) {
	var doc struct {
		Services [][][]string `json:"services"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	var out []bootService
	for _, s := range doc.Services {
		if len(s) != 2 {
			continue
		}
		var svc bootService
		for _, u := range s[1] {
			if strings.HasPrefix(u, "https://") {
				svc.url = u
				break
			}
		}
		if svc.url == "" {
			continue
		}
		for _, p := range s[0] {
			if pr, err := netip.ParsePrefix(p); err == nil {
				svc.prefixes = append(svc.prefixes, pr.Masked())
			}
		}
		if len(svc.prefixes) > 0 {
			out = append(out, svc)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no services")
	}
	return out, nil
}

// parseRDAP extracts the owner of an RDAP IP network object: org = the
// "fn" of the first entity with the role registrant, else the network's
// name; country = the network's country (2 letters). Both are cleaned like
// untrusted names (at most 100 characters).
func parseRDAP(b []byte) (*WhoisInfo, error) {
	var doc struct {
		Name     jsontext.Value `json:"name"`
		Country  jsontext.Value `json:"country"`
		Entities []struct {
			Roles      []jsontext.Value `json:"roles"`
			VCardArray jsontext.Value   `json:"vcardArray"`
		} `json:"entities"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	org := ""
	for _, e := range doc.Entities {
		registrant := false
		for _, r := range e.Roles {
			if jsonString(r) == "registrant" {
				registrant = true
			}
		}
		if registrant {
			org = vcardFN(e.VCardArray)
			break
		}
	}
	if org = cleanUntrusted(org, maxWhoisText); org == "" {
		org = cleanUntrusted(jsonString(doc.Name), maxWhoisText)
	}
	if org == "" {
		return nil, errors.New("no owner")
	}
	info := &WhoisInfo{Org: org}
	if c := strings.ToUpper(strings.TrimSpace(jsonString(doc.Country))); len(c) == 2 && c[0] >= 'A' && c[0] <= 'Z' && c[1] >= 'A' && c[1] <= 'Z' {
		info.Country = c
	}
	return info, nil
}

// jsonString returns a JSON string's value ("" for any other value).
func jsonString(v jsontext.Value) string {
	var s string
	if v.Kind() != '"' || json.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}

// vcardFN returns the "fn" of a jCard (RFC 7095): ["vcard", [[name,
// params, type, value], …]].
func vcardFN(v jsontext.Value) string {
	var card []jsontext.Value
	if json.Unmarshal(v, &card) != nil || len(card) < 2 {
		return ""
	}
	var props [][]jsontext.Value
	if json.Unmarshal(card[1], &props) != nil {
		return ""
	}
	for _, p := range props {
		if len(p) >= 4 && jsonString(p[0]) == "fn" {
			return jsonString(p[3])
		}
	}
	return ""
}

// cleanUntrusted makes untrusted text a name: invalid UTF-8 and every
// character of the Unicode categories Cc and Cf removed, white space
// collapsed, cut to n characters.
func cleanUntrusted(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Cf) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > n {
		s = strings.TrimSpace(string([]rune(s)[:n]))
	}
	return s
}
