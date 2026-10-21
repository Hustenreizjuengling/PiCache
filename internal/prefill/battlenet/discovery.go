package battlenet

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Which build is current comes from Blizzard's version services, asked
// directly (they are not cached): the TACT channels the Battle.net app
// uses since 2026 (distribution.version.battle.net, JSON), then Ribbit v2
// (<region>.version.battle.net/v2, PSV), then its HTTP proxy on port 1119.

const (
	defaultChannels = "https://distribution.version.battle.net"
	maxDiscovery    = 8 << 20
	callTimeout     = 30 * time.Second
	// UserAgent is sent to the version services.
	UserAgent = "PiCache-Prefill"
)

// Options configure a Client.
type Options struct {
	// ChannelsURL is the TACT channels service ("" = Blizzard's; "-" =
	// not used).
	ChannelsURL string
	// RibbitURL and PatchURL return a region's Ribbit v2 base
	// (https://eu.version.battle.net/v2) and HTTP proxy
	// (http://eu.patch.battle.net:1119); nil = Blizzard's.
	RibbitURL, PatchURL func(region string) string
	// DialContext, if set, opens every connection (tests, pinned
	// addresses).
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// Used is told about every host contacted and why.
	Used func(host, protocol, purpose string)
	Log  *slog.Logger
	// SliceSize is the cache's slice size (tests; 0 = 1 MiB).
	SliceSize int64
}

// Client asks the version services.
type Client struct {
	o  Options
	hc *http.Client

	mu      sync.Mutex
	catalog *channelCatalog
	files   map[string][]byte // channel files (content-addressed)
}

// New returns a Client.
func New(o Options) *Client {
	if o.ChannelsURL == "" {
		o.ChannelsURL = defaultChannels
	}
	if o.RibbitURL == nil {
		o.RibbitURL = func(r string) string { return "https://" + r + ".version.battle.net/v2" }
	}
	if o.PatchURL == nil {
		o.PatchURL = func(r string) string { return "http://" + r + ".patch.battle.net:1119" }
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	tr := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: callTimeout,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
	}
	if o.DialContext != nil {
		tr.DialContext = o.DialContext
	}
	return &Client{o: o, hc: &http.Client{Transport: tr, Timeout: callTimeout}, files: map[string][]byte{}}
}

// Version is a product's current build in a region.
type Version struct {
	Product, Region string
	BuildConfig     Key
	CDNConfig       Key
	KeyRing         Key // zero if none
	ProductConfig   Key
	// Name is the version name ("12.1.5.70448").
	Name string
	// Path is the product's CDN path ("tpr/wow"); ConfigPath the product
	// configs' ("tpr/configs/data").
	Path, ConfigPath string
	// Hosts are the CDN hosts, the cache's first.
	Hosts []string
	// Source names the service that answered.
	Source string
}

// cacheHosts are the Blizzard CDN hosts the cache serves (cache-domains
// "blizzard"); requests go to them, the first answering.
func cacheHost(h string) bool {
	return h == "level3.blizzard.com" || h == "cdn.blizzard.com" || strings.HasSuffix(h, ".cdn.blizzard.com") ||
		h == "blizzard.vo.llnwd.net" || strings.HasPrefix(h, "blzddist") && strings.HasSuffix(h, "-a.akamaihd.net")
}

func orderHosts(hosts []string) []string {
	var out []string
	for _, h := range hosts {
		if cacheHost(h) && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	for _, h := range []string{"level3.blizzard.com", "us.cdn.blizzard.com"} {
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

func (c *Client) get(ctx context.Context, raw string) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if c.o.Used != nil {
		c.o.Used(u.Host, u.Scheme, "battlenet-versions")
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscovery+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", u.Host, resp.StatusCode)
	}
	if len(b) > maxDiscovery {
		return nil, fmt.Errorf("%s: answer too large", u.Host)
	}
	return b, nil
}

// Version asks the services in turn for product's build in region.
func (c *Client) Version(ctx context.Context, product, region string) (*Version, error) {
	var errs []error
	if c.o.ChannelsURL != "-" {
		v, err := c.channelVersion(ctx, product, region)
		if err == nil {
			return v, nil
		}
		errs = append(errs, fmt.Errorf("TACT channels: %w", err))
		if errors.Is(err, ErrEncrypted) || ctx.Err() != nil {
			return nil, errors.Join(errs...)
		}
	}
	pe := url.PathEscape(product)
	for _, s := range []struct{ name, base string }{
		{"ribbit", c.o.RibbitURL(region) + "/products/" + pe},
		{"patch", c.o.PatchURL(region) + "/" + pe},
	} {
		v, err := c.psvVersion(ctx, s.base, product, region)
		if err == nil {
			v.Source = s.name
			return v, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", s.name, err))
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.Join(errs...)
}

// TACT channels.

type channelCatalog struct {
	path     string
	products map[string]channelProduct // by variant (the product code)
	fetched  time.Time
}

type channelProduct struct {
	Variant string `json:"variant"`
	Builds  []struct {
		Branches   []string `json:"branches"`
		Definition string   `json:"definition"`
	} `json:"builds"`
	Regions []struct {
		Branches []string `json:"branches"`
		CDNs     string   `json:"cdns"`
	} `json:"regions"`
	Infrastructure bool `json:"infrastructure"`
}

func validHash(h string) bool {
	_, err := ParseKey(h)
	return err == nil
}

func (c *Client) channelFile(ctx context.Context, path, h string) ([]byte, error) {
	if !validHash(h) {
		return nil, fmt.Errorf("bad channel file name %q", h)
	}
	c.mu.Lock()
	b, ok := c.files[h]
	c.mu.Unlock()
	if ok {
		return b, nil
	}
	b, err := c.get(ctx, strings.TrimRight(c.o.ChannelsURL, "/")+"/"+strings.Trim(path, "/")+"/"+hashPath(h))
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.files[h] = b
	c.mu.Unlock()
	return b, nil
}

func (c *Client) channelCatalog(ctx context.Context) (*channelCatalog, error) {
	c.mu.Lock()
	cat := c.catalog
	c.mu.Unlock()
	if cat != nil && time.Since(cat.fetched) < 10*time.Minute {
		return cat, nil
	}
	b, err := c.get(ctx, strings.TrimRight(c.o.ChannelsURL, "/")+"/summary")
	if err != nil {
		return nil, err
	}
	var sum struct {
		Path   string `json:"path"`
		Public struct {
			Channel string `json:"channel"`
		} `json:"public"`
	}
	if err := json.Unmarshal(b, &sum); err != nil {
		return nil, fmt.Errorf("summary: %w", err)
	}
	if strings.Contains(sum.Path, "..") || strings.ContainsAny(sum.Path, "?#\\") {
		return nil, fmt.Errorf("summary: bad path %q", sum.Path)
	}
	b, err = c.channelFile(ctx, sum.Path, sum.Public.Channel)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Products []channelProduct `json:"products"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	cat = &channelCatalog{path: sum.Path, products: map[string]channelProduct{}, fetched: time.Now()}
	for _, p := range raw.Products {
		if !p.Infrastructure && p.Variant != "" {
			cat.products[p.Variant] = p
		}
	}
	c.mu.Lock()
	c.catalog = cat
	c.mu.Unlock()
	return cat, nil
}

// ErrUnknown: the service does not know the product (in the region).
var ErrUnknown = errors.New("battlenet: unknown product")

func (c *Client) channelVersion(ctx context.Context, product, region string) (*Version, error) {
	cat, err := c.channelCatalog(ctx)
	if err != nil {
		return nil, err
	}
	p, ok := cat.products[product]
	if !ok {
		return nil, ErrUnknown
	}
	def := ""
	for _, b := range p.Builds {
		if slices.Contains(b.Branches, region) {
			def = b.Definition
			break
		}
	}
	if def == "" {
		for _, b := range p.Builds {
			if slices.Contains(b.Branches, "us") {
				def = b.Definition
				break
			}
		}
	}
	if def == "" {
		return nil, fmt.Errorf("%w in %s", ErrUnknown, region)
	}
	b, err := c.channelFile(ctx, cat.path, def)
	if err != nil {
		return nil, err
	}
	var d struct {
		ArmadilloKey string `json:"armadilloKey"`
		BuildKey     string `json:"buildKey"`
		CDNKey       string `json:"cdnKey"`
		KeyRing      string `json:"keyRing"`
		Path         string `json:"path"`
		Product      struct {
			Hash string `json:"hash"`
		} `json:"productConfig"`
		VersionString string `json:"versionString"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("build definition: %w", err)
	}
	if d.ArmadilloKey != "" {
		return nil, ErrEncrypted
	}
	v := &Version{Product: product, Region: region, Name: d.VersionString, Path: d.Path, ConfigPath: "tpr/configs/data", Source: "channels"}
	if err := v.setKeys(d.BuildKey, d.CDNKey, d.KeyRing, d.Product.Hash); err != nil {
		return nil, err
	}
	cdns := ""
	for _, r := range p.Regions {
		if slices.Contains(r.Branches, region) {
			cdns = r.CDNs
			break
		}
	}
	if cdns == "" && len(p.Regions) > 0 {
		cdns = p.Regions[0].CDNs
	}
	var hosts []string
	if cdns != "" {
		b, err := c.channelFile(ctx, cat.path, cdns)
		if err != nil {
			return nil, err
		}
		var cd struct {
			Hosts []struct {
				Host string `json:"host"`
			} `json:"hosts"`
		}
		if err := json.Unmarshal(b, &cd); err != nil {
			return nil, fmt.Errorf("cdn definition: %w", err)
		}
		for _, h := range cd.Hosts {
			u, err := url.Parse(h.Host)
			if err != nil || u.Scheme != "http" || u.Query().Get("fallback") == "1" {
				continue
			}
			hosts = append(hosts, u.Hostname())
		}
	}
	v.Hosts = orderHosts(hosts)
	return v, v.check()
}

func (v *Version) setKeys(build, cdn, keyRing, product string) error {
	var err error
	if v.BuildConfig, err = ParseKey(build); err != nil {
		return err
	}
	if v.CDNConfig, err = ParseKey(cdn); err != nil {
		return err
	}
	if keyRing != "" {
		if v.KeyRing, err = ParseKey(keyRing); err != nil {
			return err
		}
	}
	if product != "" {
		if v.ProductConfig, err = ParseKey(product); err != nil {
			return err
		}
	}
	return nil
}

// check refuses paths that would leave the CDN layout.
func (v *Version) check() error {
	for _, p := range []string{v.Path, v.ConfigPath} {
		if p == "" || strings.Contains(p, "..") || strings.ContainsAny(p, "?#\\ ") || strings.HasPrefix(p, "/") {
			return fmt.Errorf("battlenet: bad CDN path %q", p)
		}
	}
	return nil
}

// PSV (Ribbit v2, the HTTP proxy).

// parsePSV reads a pipe-separated table: rows by column name.
func parsePSV(b []byte) ([]map[string]string, error) {
	var cols []string
	var rows []map[string]string
	for line := range strings.SplitSeq(strings.ReplaceAll(string(b), "\x00", ""), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "|")
		if cols == nil {
			for _, c := range f {
				name, _, _ := strings.Cut(c, "!")
				cols = append(cols, strings.ToLower(name))
			}
			continue
		}
		row := map[string]string{}
		for i, c := range cols {
			if i < len(f) {
				row[c] = f[i]
			}
		}
		rows = append(rows, row)
	}
	if cols == nil {
		return nil, errors.New("empty table")
	}
	return rows, nil
}

func pickRow(rows []map[string]string, col, region string) map[string]string {
	for _, want := range []string{region, "us"} {
		for _, r := range rows {
			if r[col] == want {
				return r
			}
		}
	}
	return nil
}

func (c *Client) psvVersion(ctx context.Context, base, product, region string) (*Version, error) {
	b, err := c.get(ctx, base+"/versions")
	if err != nil {
		return nil, err
	}
	rows, err := parsePSV(b)
	if err != nil {
		return nil, fmt.Errorf("versions: %w", err)
	}
	r := pickRow(rows, "region", region)
	if r == nil {
		return nil, fmt.Errorf("%w in %s", ErrUnknown, region)
	}
	name := r["versionsname"]
	if name == "" {
		name = r["versionname"]
	}
	kr := r["keyring"]
	v := &Version{Product: product, Region: region, Name: name}
	if err := v.setKeys(r["buildconfig"], r["cdnconfig"], kr, r["productconfig"]); err != nil {
		return nil, err
	}
	b, err = c.get(ctx, base+"/cdns")
	if err != nil {
		return nil, err
	}
	rows, err = parsePSV(b)
	if err != nil {
		return nil, fmt.Errorf("cdns: %w", err)
	}
	cr := pickRow(rows, "name", region)
	if cr == nil {
		return nil, fmt.Errorf("no CDN for %s", region)
	}
	v.Path, v.ConfigPath = cr["path"], cr["configpath"]
	if v.ConfigPath == "" {
		v.ConfigPath = "tpr/configs/data"
	}
	v.Hosts = orderHosts(strings.Fields(cr["hosts"]))
	return v, v.check()
}
