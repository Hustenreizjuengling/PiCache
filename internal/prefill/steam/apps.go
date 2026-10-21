package steam

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"
)

// Which apps an account owns and which depots a game client would install
// (the rules of SteamPrefill and DepotDownloader): licenses → packages (app
// and depot IDs) → app info, whose "depots" section lists every depot with
// its configuration (operating systems, architecture, language) and the
// public branch's manifest. Depots of owned DLC count to their game.

// Filter chooses depots like the settings of a Steam client.
type Filter struct {
	OS        []string // windows, macos, linux
	Languages []string // Steam language names
}

// Game is an app with the depots a client would download.
type Game struct {
	ID     uint32
	Name   string
	Type   string // game, application, demo, beta, …
	Depots []Depot
}

// Bytes is the download size of the depots (as far as Steam says).
func (g Game) Bytes() int64 {
	var n int64
	for _, d := range g.Depots {
		n += d.Bytes
	}
	return n
}

// Depot is one depot of a game in its public version.
type Depot struct {
	ID       uint32
	Manifest uint64
	Bytes    int64 // the compressed download size, 0 when unknown
	// App is the app the manifest request code is asked for: the DLC or
	// the app that shares the depot, else the game.
	App uint32
}

// listedTypes are the app types offered for a prefill.
var listedTypes = []string{"game", "application", "demo", "beta"}

// excludedApps and excludedDepots are skipped like SteamPrefill does:
// Spacewar (the SDK's test app), workshop tools, depots without request
// codes, CS2's system depots and the Visual C++ redistributables.
var (
	excludedApps   = []uint32{480}
	excludedDepots = []uint32{381450, 2347779, 1770480, 731, 732, 734,
		228981, 228982, 228983, 228984, 228985, 228986, 228987, 228988, 228989, 228990}
)

const (
	maxOwnedApps     = 50000
	licenseExpired   = 0x08
	maxDLCPerGame    = 2000
	maxExtraAppInfos = 20000
)

// owned is what the account's licenses grant.
type owned struct {
	apps   map[uint32]bool
	depots map[uint32]bool
}

// OwnedGames lists the account's games with their depots under f.
func (s *Session) OwnedGames(ctx context.Context, f Filter) ([]Game, error) {
	own, err := s.owned(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]uint32, 0, len(own.apps))
	for id := range own.apps {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return s.games(ctx, ids, own, f, true)
}

// Games reads the current depots of the given apps (a job's selection),
// whatever their type; apps the account does not own are left out.
func (s *Session) Games(ctx context.Context, ids []uint32, f Filter) ([]Game, error) {
	own, err := s.owned(ctx)
	if err != nil {
		return nil, err
	}
	var mine []uint32
	for _, id := range ids {
		if own.apps[id] {
			mine = append(mine, id)
		}
	}
	return s.games(ctx, mine, own, f, false)
}

// owned reads the app and depot IDs of the account's licenses (expired
// ones left out).
func (s *Session) owned(ctx context.Context) (owned, error) {
	lics, err := s.Licenses(ctx)
	if err != nil {
		return owned{}, err
	}
	refs := make([]ProductRef, 0, len(lics))
	seen := map[uint32]bool{}
	for _, l := range lics {
		if l.Flags&licenseExpired != 0 || seen[l.PackageID] {
			continue
		}
		seen[l.PackageID] = true
		refs = append(refs, ProductRef{ID: l.PackageID, AccessToken: l.AccessToken})
	}
	_, pkgs, err := s.ProductInfo(ctx, nil, refs)
	if err != nil {
		return owned{}, err
	}
	own := owned{apps: map[uint32]bool{}, depots: map[uint32]bool{}}
	for _, p := range pkgs {
		for _, list := range []struct {
			key string
			dst map[uint32]bool
		}{{"appids", own.apps}, {"depotids", own.depots}} {
			for _, a := range p.Info.child(list.key).children {
				if id, err := strconv.ParseUint(a.value, 10, 32); err == nil && id > 0 && len(list.dst) < maxOwnedApps {
					list.dst[uint32(id)] = true
				}
			}
		}
	}
	return own, nil
}

// appInfos reads the app info of ids with their access tokens.
func (s *Session) appInfos(ctx context.Context, ids []uint32) (map[uint32]Product, error) {
	if len(ids) == 0 {
		return map[uint32]Product{}, nil
	}
	toks, _, err := s.AccessTokens(ctx, ids, nil)
	if err != nil {
		return nil, err
	}
	refs := make([]ProductRef, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, ProductRef{ID: id, AccessToken: toks[id]})
	}
	apps, _, err := s.ProductInfo(ctx, refs, nil)
	return apps, err
}

func (s *Session) games(ctx context.Context, ids []uint32, own owned, f Filter, listing bool) ([]Game, error) {
	infos, err := s.appInfos(ctx, ids)
	if err != nil {
		return nil, err
	}
	// Owned DLC and the apps that share depots are read as well.
	var extra []uint32
	add := func(id uint32) {
		if _, ok := infos[id]; !ok && id != 0 && !slices.Contains(extra, id) && len(extra) < maxExtraAppInfos {
			extra = append(extra, id)
		}
	}
	for _, id := range ids {
		info := infos[id].Info
		if !wanted(id, info, listing) {
			continue
		}
		for _, dlc := range dlcOf(info, own) {
			add(dlc)
		}
		for _, d := range info.child("depots").children {
			add(uint32(d.uint("depotfromapp")))
		}
	}
	if len(extra) > 0 {
		more, err := s.appInfos(ctx, extra)
		if err != nil {
			return nil, err
		}
		for id, p := range more {
			infos[id] = p
		}
		// The shared depots of DLC.
		var second []uint32
		for _, id := range extra {
			for _, d := range infos[id].Info.child("depots").children {
				if from := uint32(d.uint("depotfromapp")); from != 0 {
					if _, ok := infos[from]; !ok && !slices.Contains(second, from) {
						second = append(second, from)
					}
				}
			}
		}
		if len(second) > 0 {
			more, err := s.appInfos(ctx, second)
			if err != nil {
				return nil, err
			}
			for id, p := range more {
				infos[id] = p
			}
		}
	}
	var out []Game
	for _, id := range ids {
		info := infos[id].Info
		if !wanted(id, info, listing) {
			continue
		}
		g := Game{ID: id, Name: strings.TrimSpace(info.str("common", "name")), Type: strings.ToLower(info.str("common", "type"))}
		if g.Name == "" {
			g.Name = "App " + strconv.FormatUint(uint64(id), 10)
		}
		g.Depots = depotsOf(id, info, infos, own, f)
		for _, dlc := range dlcOf(info, own) {
			for _, d := range depotsOf(dlc, infos[dlc].Info, infos, own, f) {
				if !slices.ContainsFunc(g.Depots, func(x Depot) bool { return x.ID == d.ID }) {
					g.Depots = append(g.Depots, d)
				}
			}
		}
		out = append(out, g)
	}
	slices.SortFunc(out, func(a, b Game) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// wanted says whether an app is offered: listed types only when listing,
// never the excluded apps or unavailable ones.
func wanted(id uint32, info *kv, listing bool) bool {
	if info == nil || slices.Contains(excludedApps, id) {
		return false
	}
	if rs := strings.ToLower(info.str("common", "releasestate")); rs == "unavailable" || rs == "disabled" {
		return false
	}
	return !listing || slices.Contains(listedTypes, strings.ToLower(info.str("common", "type")))
}

// dlcOf lists the owned DLC of an app (extended/listofdlc).
func dlcOf(info *kv, own owned) []uint32 {
	var out []uint32
	for v := range strings.SplitSeq(info.str("extended", "listofdlc"), ",") {
		id, err := strconv.ParseUint(strings.TrimSpace(v), 10, 32)
		if err == nil && own.apps[uint32(id)] && len(out) < maxDLCPerGame {
			out = append(out, uint32(id))
		}
	}
	return out
}

// depotsOf chooses the depots of app like a client with settings f.
func depotsOf(app uint32, info *kv, infos map[uint32]Product, own owned, f Filter) []Depot {
	var out []Depot
	free := info.str("common", "FreeToDownload") == "1"
	for _, d := range info.child("depots").children {
		id64, err := strconv.ParseUint(d.key, 10, 32)
		if err != nil || id64 == 0 || len(d.children) == 0 {
			continue // branches, baselanguages, …
		}
		id := uint32(id64)
		if slices.Contains(excludedDepots, id) || !depotWanted(d, f) {
			continue
		}
		if !own.depots[id] && !own.apps[id] && !free {
			continue
		}
		holder := cmp.Or(uint32(d.uint("dlcappid")), uint32(d.uint("depotfromapp")), app)
		if holder != app && !own.apps[holder] && !free {
			continue
		}
		gid, bytes := publicManifest(d)
		if from := uint32(d.uint("depotfromapp")); gid == 0 && from != 0 && from != app {
			gid, bytes = publicManifest(infos[from].Info.get("depots", d.key))
		}
		if gid == 0 || slices.ContainsFunc(out, func(x Depot) bool { return x.ID == id }) {
			continue
		}
		out = append(out, Depot{ID: id, Manifest: gid, Bytes: bytes, App: holder})
	}
	return out
}

// depotWanted applies the depot's config: operating systems, 32-bit
// builds, languages, low-violence builds, other realms.
func depotWanted(d *kv, f Filter) bool {
	cfg := d.child("config")
	if list := cfg.str("oslist"); list != "" && !anyIn(list, f.OS) {
		return false
	}
	if cfg.str("osarch") == "32" {
		return false
	}
	if list := cfg.str("language"); list != "" && !anyIn(list, f.Languages) {
		return false
	}
	if cfg.str("lowviolence") == "1" {
		return false
	}
	if realm := cfg.str("realm"); realm != "" && realm != "steamglobal" {
		return false
	}
	return true
}

// anyIn reports whether the comma list names one of want.
func anyIn(list string, want []string) bool {
	for v := range strings.SplitSeq(list, ",") {
		if slices.Contains(want, strings.TrimSpace(v)) {
			return true
		}
	}
	return false
}

// publicManifest is the public branch's manifest of a depot: "manifests"
// { "public" { "gid" … "download" … } } or the older "public" "<gid>".
// Encrypted manifests (password branches) are not used.
func publicManifest(d *kv) (gid uint64, bytes int64) {
	pub := d.get("manifests", "public")
	if pub == nil {
		return 0, 0
	}
	if len(pub.children) == 0 {
		v, _ := strconv.ParseUint(strings.TrimSpace(pub.value), 10, 64)
		return v, 0
	}
	return pub.uint("gid"), int64(pub.uint("download"))
}
