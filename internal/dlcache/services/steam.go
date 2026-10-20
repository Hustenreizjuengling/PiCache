package services

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image/jpeg"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
)

// Steam names (downloadCache.steamNames, off by default: it sends data out;
// docs/ARCHITECTURE.md 8.4): the Library names a Steam depot after the app
// it belongs to. PiCache only sees depot IDs (/depot/<id>/…); the app is
// found through the public mirror of Steam's app info, api.steamcmd.net:
// the apps <depot>, <depot>-1, … are asked in turn (at most steamWalk) until
// one lists the depot as its own, as its workshop depot or as a shared
// depot, which names its owner (depotfromapp; that app is then asked for
// its name). Every answer stores all depots the app lists, so the other
// depots of a game cost no request. The depots of the Steamworks
// redistributables are known (steamRedists: no walk; their built-in label
// names the component, only the app's icon is looked up). The icon (Steam's 32×32 JPEG) is
// fetched from Steam's CDN, must decode as a JPEG of at most 256×256 pixels
// and is kept in picache.db; the web UI loads it from PiCache (GET
// /cache/icons/steam/{app}), never from Steam (CSP img-src 'self').
//
// Bounds: one worker off the request path (Label only enqueues); at most one
// request per second and 2000 per day; a queue of 256 depots (beyond it
// dropped, enqueued again when next seen); 15 s and 1 MiB per answer, 64 KiB
// per icon; names are cleaned like untrusted text and cut to 128 bytes.
// Results are kept 90 days, depots without an app 7 days; at most 20 000
// depots and 10 000 apps (the oldest dropped). After a failed request the
// worker pauses 10 minutes. Requests go out directly (never through the
// outbound proxy) with the app's lookup client: public destinations only,
// TLS 1.2+ verified, no redirects. While the setting is off nothing is
// looked up and stored names are not shown.
//
// Tables (picache.db, component "services"): services_steam_apps,
// services_steam_depots (a cache: never restored).

const (
	steamInfoURL  = "https://api.steamcmd.net/v1/info/%d"
	steamIconURL  = "https://cdn.cloudflare.steamstatic.com/steamcommunity/public/images/apps/%d/%s.jpg"
	steamWalk     = 24 // apps asked per depot: <depot> down to <depot>-23
	steamQueue    = 256
	steamMaxBody  = 1 << 20
	steamMaxIcon  = 64 << 10
	steamMaxPixel = 256
	steamMaxName  = 128
	steamFound    = 90 * 24 * time.Hour
	steamNotFound = 7 * 24 * time.Hour
	steamMaxDepot = 20_000
	steamMaxApps  = 10_000
	steamMaxList  = 1024 // depots taken from one app
)

// Depot kinds.
const (
	depotContent  = ""         // the app's own content
	depotWorkshop = "workshop" // the app's workshop depot
	depotDLC      = "dlc"      // content of a DLC of the app
	depotShared   = "shared"   // a shared depot of another app (depotfromapp)
)

var iconHashRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

type steamDepot struct {
	app  uint32 // 0: no app lists it
	kind string
	at   time.Time
}

type steamApp struct {
	name     string // "": no such app
	typ      string
	iconHash string
	icon     bool // an icon is stored
	at       time.Time
}

// steamNames resolves Steam depots to app names (see above).
type steamNames struct {
	fetcher // the requests to api.steamcmd.net and Steam's CDN
	db      *db.DB
	log     *slog.Logger
	enabled func() bool

	// Replaced in tests.
	infoURL func(app uint32) string
	iconURL func(app uint32, hash string) string

	mu     sync.RWMutex
	depots map[uint32]steamDepot
	apps   map[uint32]steamApp
	queued map[uint32]bool
	queue  chan uint32
}

func newSteamNames(d *db.DB, log *slog.Logger, enabled func() bool) *steamNames {
	return &steamNames{fetcher: newFetcher("Steam name", log), db: d, log: log, enabled: enabled,
		infoURL: func(app uint32) string { return fmt.Sprintf(steamInfoURL, app) },
		iconURL: func(app uint32, hash string) string { return fmt.Sprintf(steamIconURL, app, hash) },
		depots:  map[uint32]steamDepot{}, apps: map[uint32]steamApp{}, queued: map[uint32]bool{},
		queue: make(chan uint32, steamQueue)}
}

// load reads the stored depots and apps (the newest first, within the
// bounds).
func (s *steamNames) load(ctx context.Context) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT depot_id, app_id, kind, fetched_at FROM services_steam_depots
		ORDER BY fetched_at DESC LIMIT ?`, steamMaxDepot)
	if err != nil {
		return fmt.Errorf("services: load steam depots: %w", err)
	}
	defer rows.Close()
	depots := map[uint32]steamDepot{}
	for rows.Next() {
		var id, app uint32
		var kind string
		var at int64
		if err := rows.Scan(&id, &app, &kind, &at); err != nil {
			return err
		}
		depots[id] = steamDepot{app: app, kind: kind, at: time.UnixMilli(at)}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows2, err := s.db.R.QueryContext(ctx, `SELECT app_id, name, type, icon_hash, icon IS NOT NULL, fetched_at
		FROM services_steam_apps ORDER BY fetched_at DESC LIMIT ?`, steamMaxApps)
	if err != nil {
		return fmt.Errorf("services: load steam apps: %w", err)
	}
	defer rows2.Close()
	apps := map[uint32]steamApp{}
	for rows2.Next() {
		var id uint32
		var a steamApp
		var at int64
		if err := rows2.Scan(&id, &a.name, &a.typ, &a.iconHash, &a.icon, &at); err != nil {
			return err
		}
		a.at = time.UnixMilli(at)
		apps[id] = a
	}
	if err := rows2.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.depots, s.apps = depots, apps
	s.mu.Unlock()
	return nil
}

// steamDepotID returns the depot of a "steam:depot:<id>" group key.
func steamDepotID(groupKey string) (uint32, bool) {
	rest, ok := strings.CutPrefix(groupKey, "steam:depot:")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(rest, 10, 32)
	if err != nil || n == 0 {
		return 0, false
	}
	return uint32(n), true
}

// label returns the name of a depot's app, enqueuing the depot when it is
// unknown or stale. ok=false: no name (yet).
func (s *steamNames) label(depot uint32) (label string, iconApp uint32, ok bool) {
	if !s.enabled() {
		return "", 0, false
	}
	now := s.now()
	s.mu.RLock()
	d, known := s.depots[depot]
	var a steamApp
	if known && d.app != 0 {
		a = s.apps[d.app]
	}
	s.mu.RUnlock()
	// Unknown or stale; a shared depot whose owner was not asked yet; an
	// icon not fetched yet (a failure pauses the worker, a broken icon
	// clears its hash).
	if needsFind(depot, d, known, now) || (d.app != 0 && a.at.IsZero()) ||
		(a.name != "" && a.iconHash != "" && !a.icon) {
		s.enqueue(depot)
	}
	if !known || d.app == 0 || a.name == "" {
		return "", 0, false
	}
	switch d.kind {
	case depotWorkshop:
		label = a.name + " · Workshop"
	case depotDLC:
		label = a.name + " · DLC"
	default:
		label = a.name
	}
	if a.icon {
		iconApp = d.app
	}
	return label, iconApp, true
}

func stale(at time.Time, found bool, now time.Time) bool {
	if found {
		return now.Sub(at) > steamFound
	}
	return now.Sub(at) > steamNotFound
}

// needsFind reports whether the app of a depot must be found (again): it is
// unknown or stale, or a Steamworks redistributable stored before it was
// known (e.g. as not found).
func needsFind(depot uint32, d steamDepot, known bool, now time.Time) bool {
	if _, redist := steamRedists[depot]; redist && known && d.app != steamRedistApp {
		return true
	}
	return !known || stale(d.at, d.app != 0, now)
}

// enqueue queues a depot for the worker (dropped when the queue is full).
func (s *steamNames) enqueue(depot uint32) {
	if s.client.Load() == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queued[depot] {
		return
	}
	select {
	case s.queue <- depot:
		s.queued[depot] = true
	default:
	}
}

// search returns the depots whose name contains q (lower case).
func (s *steamNames) search(q string) []uint32 {
	if !s.enabled() {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []uint32
	for id, d := range s.depots {
		if a := s.apps[d.app]; d.app != 0 && a.name != "" && strings.Contains(strings.ToLower(a.name), q) {
			out = append(out, id)
		}
	}
	return out
}

// icon returns the stored icon of an app.
func (s *steamNames) icon(ctx context.Context, app uint32) ([]byte, bool, error) {
	if !s.enabled() {
		return nil, false, nil
	}
	var b []byte
	err := s.db.R.QueryRowContext(ctx, `SELECT icon FROM services_steam_apps WHERE app_id = ? AND icon IS NOT NULL`, app).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return b, err == nil, err
}

// run resolves queued depots until ctx is done.
func (s *steamNames) run(ctx context.Context) {
	for {
		var depot uint32
		select {
		case <-ctx.Done():
			return
		case depot = <-s.queue:
		}
		if s.enabled() && s.client.Load() != nil {
			s.resolve(ctx, depot)
		}
		s.mu.Lock()
		delete(s.queued, depot)
		s.mu.Unlock()
	}
}

// resolve finds the app of a depot, its name and icon.
func (s *steamNames) resolve(ctx context.Context, depot uint32) {
	now := s.now()
	s.mu.RLock()
	d, known := s.depots[depot]
	s.mu.RUnlock()
	if needsFind(depot, d, known, now) && !s.find(ctx, depot, now) {
		return
	}
	s.mu.RLock()
	d = s.depots[depot]
	a, asked := s.apps[d.app]
	s.mu.RUnlock()
	if d.app == 0 {
		return
	}
	// The owner of a shared depot is asked for its name.
	if !asked || stale(a.at, a.name != "", s.now()) {
		if err := s.fetchApp(ctx, d.app); err != nil {
			s.fail(err)
			return
		}
		s.mu.RLock()
		a = s.apps[d.app]
		s.mu.RUnlock()
	}
	if a.name != "" && a.iconHash != "" && !a.icon {
		if err := s.fetchIcon(ctx, d.app, a.iconHash); err != nil {
			s.fail(err)
		}
	}
}

// find stores the app of a depot: a Steamworks redistributable's is known,
// otherwise the apps <depot>, <depot>-1, … are asked. false: not found or
// failed.
func (s *steamNames) find(ctx context.Context, depot uint32, now time.Time) bool {
	if _, redist := steamRedists[depot]; redist {
		if err := s.store(ctx, nil, map[uint32]steamDepot{depot: {app: steamRedistApp, at: now}}); err != nil {
			s.fail(err)
			return false
		}
		return true
	}
	for i := uint32(0); i < steamWalk && i < depot; i++ {
		app := depot - i
		s.mu.RLock()
		a, asked := s.apps[app]
		s.mu.RUnlock()
		// An app asked within the not-found period already gave its
		// depots: ask it again only when older (a depot added since).
		if !asked || now.Sub(a.at) > steamNotFound {
			if err := s.fetchApp(ctx, app); err != nil {
				s.fail(err)
				return false
			}
		}
		s.mu.RLock()
		d, found := s.depots[depot]
		s.mu.RUnlock()
		if found && d.app != 0 && !stale(d.at, true, s.now()) {
			return true
		}
	}
	if err := s.store(ctx, nil, map[uint32]steamDepot{depot: {at: s.now()}}); err != nil {
		s.fail(err)
	}
	return false
}

// picsApp is the part of an app's info that is used.
type picsApp struct {
	Common struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Icon string `json:"icon"`
	} `json:"common"`
	Depots map[string]jsontext.Value `json:"depots"`
}

type picsDepot struct {
	DepotFromApp jsontext.Value `json:"depotfromapp"`
	DLCAppID     jsontext.Value `json:"dlcappid"`
}

// picsID reads an ID given as a JSON string or number (0: none or invalid).
func picsID(v jsontext.Value) uint32 {
	t := strings.Trim(string(bytes.TrimSpace(v)), `"`)
	n, err := strconv.ParseUint(t, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(n)
}

// fetchApp asks for an app's info and stores its name and depots.
func (s *steamNames) fetchApp(ctx context.Context, app uint32) error {
	b, err := s.get(ctx, s.infoURL(app), steamMaxBody)
	if err != nil {
		return err
	}
	var resp struct {
		Status string                    `json:"status"`
		Data   map[string]jsontext.Value `json:"data"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return fmt.Errorf("app %d: %w", app, err)
	}
	if resp.Status != "success" {
		return fmt.Errorf("app %d: status %q", app, cleanText(resp.Status, 64))
	}
	now := s.now()
	a := steamApp{at: now}
	depots := map[uint32]steamDepot{}
	if raw, ok := resp.Data[strconv.FormatUint(uint64(app), 10)]; ok {
		var info picsApp
		if err := json.Unmarshal(raw, &info); err != nil {
			return fmt.Errorf("app %d: %w", app, err)
		}
		a.name = cleanText(info.Common.Name, steamMaxName)
		a.typ = strings.ToLower(cleanText(info.Common.Type, 32))
		if iconHashRE.MatchString(info.Common.Icon) {
			a.iconHash = info.Common.Icon
		}
		for k, v := range info.Depots {
			if len(depots) >= steamMaxList {
				break
			}
			if k == "workshopdepot" {
				if w := picsID(v); w != 0 {
					depots[w] = steamDepot{app: app, kind: depotWorkshop, at: now}
				}
				continue
			}
			id, err := strconv.ParseUint(k, 10, 32)
			if err != nil || id == 0 {
				continue
			}
			var pd picsDepot
			if json.Unmarshal(v, &pd) != nil {
				continue
			}
			switch owner := picsID(pd.DepotFromApp); {
			case owner != 0 && owner != app:
				depots[uint32(id)] = steamDepot{app: owner, kind: depotShared, at: now}
			case picsID(pd.DLCAppID) != 0:
				depots[uint32(id)] = steamDepot{app: app, kind: depotDLC, at: now}
			default:
				if _, set := depots[uint32(id)]; !set {
					depots[uint32(id)] = steamDepot{app: app, kind: depotContent, at: now}
				}
			}
		}
	}
	return s.store(ctx, map[uint32]steamApp{app: a}, depots)
}

// fetchIcon downloads, checks and stores an app's icon.
func (s *steamNames) fetchIcon(ctx context.Context, app uint32, hash string) error {
	b, err := s.get(ctx, s.iconURL(app, hash), steamMaxIcon)
	if err != nil {
		return err
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
	if err == nil && (cfg.Width > steamMaxPixel || cfg.Height > steamMaxPixel || cfg.Width < 1 || cfg.Height < 1) {
		err = fmt.Errorf("%d×%d pixels", cfg.Width, cfg.Height)
	}
	if err == nil {
		_, err = jpeg.Decode(bytes.NewReader(b))
	}
	if err != nil {
		// Not an icon: not asked again until the app is asked again.
		s.log.Debug("ignoring a Steam icon", slog.Uint64("app", uint64(app)), slog.Any("err", err))
		_, err = s.db.W.ExecContext(ctx, `UPDATE services_steam_apps SET icon_hash = '' WHERE app_id = ?`, app)
		s.mu.Lock()
		if a, ok := s.apps[app]; ok {
			a.iconHash = ""
			s.apps[app] = a
		}
		s.mu.Unlock()
		return err
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE services_steam_apps SET icon = ? WHERE app_id = ? AND icon_hash = ?`,
		b, app, hash); err != nil {
		return err
	}
	s.mu.Lock()
	if a, ok := s.apps[app]; ok && a.iconHash == hash {
		a.icon = true
		s.apps[app] = a
	}
	s.mu.Unlock()
	return nil
}

// store writes apps and depots (an app whose icon changed loses its icon)
// and drops the oldest rows beyond the bounds.
func (s *steamNames) store(ctx context.Context, apps map[uint32]steamApp, depots map[uint32]steamDepot) error {
	tx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, a := range apps {
		if _, err := tx.ExecContext(ctx, `INSERT INTO services_steam_apps (app_id, name, type, icon_hash, icon, fetched_at)
			VALUES (?, ?, ?, ?, NULL, ?)
			ON CONFLICT(app_id) DO UPDATE SET name = excluded.name, type = excluded.type, fetched_at = excluded.fetched_at,
				icon = CASE WHEN icon_hash = excluded.icon_hash THEN icon END, icon_hash = excluded.icon_hash`,
			id, a.name, a.typ, a.iconHash, a.at.UnixMilli()); err != nil {
			return err
		}
	}
	for id, d := range depots {
		if _, err := tx.ExecContext(ctx, `INSERT INTO services_steam_depots (depot_id, app_id, kind, fetched_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(depot_id) DO UPDATE SET app_id = excluded.app_id, kind = excluded.kind, fetched_at = excluded.fetched_at`,
			id, d.app, d.kind, d.at.UnixMilli()); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	for id, a := range apps {
		old, ok := s.apps[id]
		a.icon = ok && old.icon && old.iconHash == a.iconHash
		s.apps[id] = a
	}
	for id, d := range depots {
		s.depots[id] = d
	}
	prune := len(s.depots) > steamMaxDepot || len(s.apps) > steamMaxApps
	s.mu.Unlock()
	if !prune {
		return nil
	}
	// Down to 90 % of the bounds, the oldest first.
	for _, q := range []struct {
		table, id string
		keep      int
	}{{"services_steam_depots", "depot_id", steamMaxDepot * 9 / 10}, {"services_steam_apps", "app_id", steamMaxApps * 9 / 10}} {
		if _, err := s.db.W.ExecContext(ctx, `DELETE FROM `+q.table+` WHERE `+q.id+` IN (SELECT `+q.id+` FROM `+q.table+
			` ORDER BY fetched_at LIMIT max(0, (SELECT count(*) FROM `+q.table+`) - ?))`, q.keep); err != nil {
			return err
		}
	}
	return s.load(ctx)
}
