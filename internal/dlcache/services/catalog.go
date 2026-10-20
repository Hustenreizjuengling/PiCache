package services

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // icon formats
	_ "image/png"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
)

// Store names (downloadCache.xboxNames and downloadCache.psnNames, off by
// default: they send data out; docs/ARCHITECTURE.md 8.4): the Library names
// a Microsoft Store / Xbox package or a PlayStation title after its store
// entry and shows its icon.
//
//   - xbox:<package>: the package family name (<package>_<publisher id>,
//     taken from the file name of a cached object of the group: the group
//     key has no publisher) is looked up in Microsoft's public product
//     catalog (displaycatalog.mp.microsoft.com, products/lookup by
//     PackageFamilyName, market US); the icon is the first square Logo,
//     BoxArt or Tile image, scaled to 64×64 by store-images.s-microsoft.com.
//   - psn:<title>: PS4 titles (CUSA…) are looked up in the PlayStation
//     Store's title container (store.playstation.com, title_name; 404: no
//     such title), the icon from its image route at 64×64. PS5 titles
//     (PPSA…) are not listed there and keep their title ID.
//
// Bounds as for the Steam names, per source: one request per second and
// 2000 per day, a 10 minute pause after a failed request; one worker off
// the request path (Label only enqueues; a queue of 256 groups); 15 s and
// 1 MiB per answer, 64 KiB per icon, which must come from the source's own
// image host and decode as a JPEG or PNG of at most 256×256 pixels; names
// cleaned like untrusted text and cut to 128 bytes. Results are kept 90
// days, groups without an entry 7 days, at most 10 000 groups (the oldest
// dropped). While a source is off nothing is looked up for it and its
// stored names and icons are not used.
//
// Table (picache.db, component "services"): services_catalog (a cache:
// never restored).

const (
	xboxCatalogURL = "https://displaycatalog.mp.microsoft.com/v7.0/products/lookup?alternateId=PackageFamilyName&market=US&languages=en-US&value="
	xboxImageHost  = "store-images.s-microsoft.com"
	psnTitleURL    = "https://store.playstation.com/store/api/chihiro/00_09_000/titlecontainer/US/en/999/%s_00"
	psnImageHost   = "store.playstation.com"

	catalogQueue    = 256
	catalogMaxBody  = 1 << 20
	catalogMaxIcon  = 64 << 10
	catalogMaxPixel = 256
	catalogMaxName  = 128
	catalogMax      = 10_000
	catalogIconSize = "w=64&h=64"
)

var (
	psnTitleRE = regexp.MustCompile(`^CUSA\d{5}$`)
	xboxPkgRE  = regexp.MustCompile(`^[A-Za-z0-9.\-]{1,128}$`)

	// errNoPath: no cached object of the group names its package family
	// (asked again when the group is shown next).
	errNoPath = errors.New("no cached file of the group")
	// errBadIcon: the icon is not a small JPEG or PNG of the image host.
	errBadIcon = errors.New("not an icon")
)

// catalogSource is one store whose names are looked up.
type catalogSource struct {
	fetcher         // its request budget
	ns       string // group key namespace: "xbox", "psn"
	enabled  func() bool
	accept   *regexp.Regexp // IDs that can be looked up
	iconHost string
	// lookup returns the name ("" when the store has no entry) and the icon
	// URL ("" when none) of an ID.
	lookup func(ctx context.Context, c *catalogNames, src *catalogSource, id string) (name, icon string, err error)
}

type catalogEntry struct {
	name string // "": no entry
	icon bool   // an icon is stored
	at   time.Time
}

// PathLookup returns the path of a cached object of a group ("" when there
// is none).
type PathLookup func(ctx context.Context, service, groupKey string) (string, error)

// catalogNames resolves store package and title IDs to names (see above).
type catalogNames struct {
	db      *db.DB
	log     *slog.Logger
	sources map[string]*catalogSource
	pathOf  atomic.Pointer[PathLookup]

	mu      sync.RWMutex
	entries map[string]catalogEntry
	queued  map[string]bool
	queue   chan string
}

func newCatalogNames(d *db.DB, log *slog.Logger, xbox, psn func() bool) *catalogNames {
	c := &catalogNames{db: d, log: log, entries: map[string]catalogEntry{}, queued: map[string]bool{},
		queue: make(chan string, catalogQueue)}
	c.sources = map[string]*catalogSource{
		"xbox": {fetcher: newFetcher("Microsoft Store name", log), ns: "xbox", enabled: xbox, accept: xboxPkgRE,
			iconHost: xboxImageHost, lookup: xboxLookup},
		"psn": {fetcher: newFetcher("PlayStation name", log), ns: "psn", enabled: psn, accept: psnTitleRE,
			iconHost: psnImageHost, lookup: psnLookup},
	}
	return c
}

// load reads the stored entries (the newest first, within the bound).
func (c *catalogNames) load(ctx context.Context) error {
	rows, err := c.db.R.QueryContext(ctx, `SELECT group_key, name, icon IS NOT NULL, fetched_at FROM services_catalog
		ORDER BY fetched_at DESC LIMIT ?`, catalogMax)
	if err != nil {
		return fmt.Errorf("services: load store names: %w", err)
	}
	defer rows.Close()
	entries := map[string]catalogEntry{}
	for rows.Next() {
		var k string
		var e catalogEntry
		var at int64
		if err := rows.Scan(&k, &e.name, &e.icon, &at); err != nil {
			return err
		}
		e.at = time.UnixMilli(at)
		entries[k] = e
	}
	if err := rows.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.entries = entries
	c.mu.Unlock()
	return nil
}

// source returns the enabled source of a group key and its ID (nil: none).
func (c *catalogNames) source(key string) (*catalogSource, string) {
	ns, id, ok := strings.Cut(key, ":")
	src := c.sources[ns]
	if !ok || src == nil || !src.enabled() || !src.accept.MatchString(id) {
		return nil, ""
	}
	return src, id
}

// label returns the store name of a group, enqueuing the group when it is
// unknown or stale. ok=false: no name (yet).
func (c *catalogNames) label(key string) (name string, icon bool, ok bool) {
	src, _ := c.source(key)
	if src == nil {
		return "", false, false
	}
	c.mu.RLock()
	e, known := c.entries[key]
	c.mu.RUnlock()
	if !known || stale(e.at, e.name != "", src.now()) {
		c.enqueue(src, key)
	}
	if !known || e.name == "" {
		return "", false, false
	}
	return e.name, e.icon, true
}

// enqueue queues a group for the worker (dropped when the queue is full).
func (c *catalogNames) enqueue(src *catalogSource, key string) {
	if src.client.Load() == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.queued[key] {
		return
	}
	select {
	case c.queue <- key:
		c.queued[key] = true
	default:
	}
}

// search returns the group keys whose name contains q (lower case).
func (c *catalogNames) search(q string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []string
	for k, e := range c.entries {
		if e.name != "" && strings.Contains(strings.ToLower(e.name), q) {
			if src, _ := c.source(k); src != nil {
				out = append(out, k)
			}
		}
	}
	return out
}

// icon returns the stored icon of a group.
func (c *catalogNames) icon(ctx context.Context, key string) ([]byte, bool, error) {
	if src, _ := c.source(key); src == nil {
		return nil, false, nil
	}
	var b []byte
	err := c.db.R.QueryRowContext(ctx, `SELECT icon FROM services_catalog WHERE group_key = ? AND icon IS NOT NULL`, key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return b, err == nil, err
}

// run resolves queued groups until ctx is done.
func (c *catalogNames) run(ctx context.Context) {
	for {
		var key string
		select {
		case <-ctx.Done():
			return
		case key = <-c.queue:
		}
		if src, id := c.source(key); src != nil && src.client.Load() != nil {
			c.resolve(ctx, src, key, id)
		}
		c.mu.Lock()
		delete(c.queued, key)
		c.mu.Unlock()
	}
}

// resolve looks up a group's name and icon and stores them.
func (c *catalogNames) resolve(ctx context.Context, src *catalogSource, key, id string) {
	name, iconURL, err := src.lookup(ctx, c, src, id)
	switch {
	case errors.Is(err, errNotFound):
		name, iconURL = "", ""
	case errors.Is(err, errNoPath):
		c.log.Debug("no cached file names the package", slog.String("group", key))
		return
	case err != nil:
		src.fail(err)
		return
	}
	name = cleanText(name, catalogMaxName)
	var icon []byte
	if name != "" && iconURL != "" {
		icon, err = c.fetchIcon(ctx, src, iconURL)
		switch {
		case errors.Is(err, errNotFound) || errors.Is(err, errBadIcon):
			c.log.Debug("ignoring a store icon", slog.String("group", key), slog.Any("err", err))
			icon = nil
		case err != nil:
			src.fail(err)
			return
		}
	}
	if err := c.store(ctx, key, catalogEntry{name: name, icon: icon != nil, at: src.now()}, icon); err != nil {
		src.fail(err)
	}
}

// fetchIcon downloads and checks an icon: from the source's image host, a
// JPEG or PNG of at most catalogMaxPixel pixels a side.
func (c *catalogNames) fetchIcon(ctx context.Context, src *catalogSource, raw string) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != src.iconHost || u.User != nil {
		return nil, fmt.Errorf("%w: %q is not on %s", errBadIcon, cleanText(raw, 128), src.iconHost)
	}
	b, err := src.get(ctx, u.String(), catalogMaxIcon)
	if err != nil {
		return nil, err
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: %v", errBadIcon, err)
	case format != "jpeg" && format != "png":
		return nil, fmt.Errorf("%w: %s", errBadIcon, format)
	case cfg.Width < 1 || cfg.Height < 1 || cfg.Width > catalogMaxPixel || cfg.Height > catalogMaxPixel:
		return nil, fmt.Errorf("%w: %d×%d pixels", errBadIcon, cfg.Width, cfg.Height)
	}
	if _, _, err := image.Decode(bytes.NewReader(b)); err != nil {
		return nil, fmt.Errorf("%w: %v", errBadIcon, err)
	}
	return b, nil
}

// store writes an entry and drops the oldest beyond the bound.
func (c *catalogNames) store(ctx context.Context, key string, e catalogEntry, icon []byte) error {
	if _, err := c.db.W.ExecContext(ctx, `INSERT INTO services_catalog (group_key, name, icon, fetched_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(group_key) DO UPDATE SET name = excluded.name, icon = excluded.icon, fetched_at = excluded.fetched_at`,
		key, e.name, icon, e.at.UnixMilli()); err != nil {
		return err
	}
	c.mu.Lock()
	c.entries[key] = e
	prune := len(c.entries) > catalogMax
	c.mu.Unlock()
	if !prune {
		return nil
	}
	// Down to 90 % of the bound, the oldest first.
	if _, err := c.db.W.ExecContext(ctx, `DELETE FROM services_catalog WHERE group_key IN (SELECT group_key FROM services_catalog
		ORDER BY fetched_at LIMIT max(0, (SELECT count(*) FROM services_catalog) - ?))`, catalogMax*9/10); err != nil {
		return err
	}
	return c.load(ctx)
}

// familyName returns the package family name of an xbox group from the file
// name of one of its cached objects (Xbox Live or Windows Update).
func (c *catalogNames) familyName(ctx context.Context, pkg string) (string, error) {
	f := c.pathOf.Load()
	if f == nil {
		return "", errNoPath
	}
	for _, svc := range []string{"xboxlive", "wsus"} {
		p, err := (*f)(ctx, svc, "xbox:"+pkg)
		if err != nil {
			return "", fmt.Errorf("%w: %v", errNoPath, err)
		}
		if m := msPackageRE.FindStringSubmatch(lastSegment(p)); m != nil && m[1] == pkg {
			return m[1] + "_" + m[4], nil
		}
	}
	return "", errNoPath
}

// xboxLookup asks Microsoft's product catalog for a package.
func xboxLookup(ctx context.Context, c *catalogNames, src *catalogSource, pkg string) (string, string, error) {
	pfn, err := c.familyName(ctx, pkg)
	if err != nil {
		return "", "", err
	}
	b, err := src.get(ctx, xboxCatalogURL+url.QueryEscape(pfn), catalogMaxBody)
	if err != nil {
		return "", "", err
	}
	var resp struct {
		Products []struct {
			LocalizedProperties []struct {
				ProductTitle string
				Images       []struct {
					ImagePurpose  string
					Uri           string
					Width, Height int
				}
			}
		}
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return "", "", fmt.Errorf("package %s: %w", pfn, err)
	}
	if len(resp.Products) == 0 || len(resp.Products[0].LocalizedProperties) == 0 {
		return "", "", errNotFound
	}
	lp := resp.Products[0].LocalizedProperties[0]
	for _, purpose := range []string{"Logo", "BoxArt", "Tile"} {
		for _, im := range lp.Images {
			if im.ImagePurpose == purpose && im.Width == im.Height && im.Width > 0 && strings.HasPrefix(im.Uri, "//") {
				return lp.ProductTitle, "https:" + im.Uri + "?" + catalogIconSize, nil
			}
		}
	}
	return lp.ProductTitle, "", nil
}

// psnLookup asks the PlayStation Store for a PS4 title.
func psnLookup(ctx context.Context, _ *catalogNames, src *catalogSource, title string) (string, string, error) {
	u := fmt.Sprintf(psnTitleURL, title)
	b, err := src.get(ctx, u, catalogMaxBody)
	if err != nil {
		return "", "", err
	}
	var resp struct {
		TitleName string `json:"title_name"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return "", "", fmt.Errorf("title %s: %w", title, err)
	}
	name := resp.TitleName
	if name == "" {
		name = resp.Name
	}
	if name == "" {
		return "", "", errNotFound
	}
	return name, u + "/image?" + catalogIconSize, nil
}

// SetLookupClient sets the HTTP client of the name lookups (Steam and store
// names; nil: none are made) and the User-Agent they send.
func (r *Registry) SetLookupClient(c *http.Client, userAgent string) {
	r.steam.setClient(c, userAgent)
	for _, src := range r.catalog.sources {
		src.setClient(c, userAgent)
	}
}

// SetPathLookup sets how the store names find a cached file of a group
// (the publisher of an Xbox package is only in its file names).
func (r *Registry) SetPathLookup(f PathLookup) {
	r.catalog.pathOf.Store(&f)
}
