package epic

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Asset is an installable build of the account (the launcher's library
// for Windows, label Live).
type Asset struct {
	AppName       string `json:"appName"`
	Namespace     string `json:"namespace"`
	CatalogItemID string `json:"catalogItemId"`
	BuildVersion  string `json:"buildVersion"`
	LabelName     string `json:"labelName"`
}

// Assets lists the account's Windows builds.
func (c *Client) Assets(ctx context.Context, s *Session) ([]Asset, error) {
	var out []Asset
	err := c.do(ctx, http.MethodGet, c.o.LauncherURL+"/launcher/api/public/assets/Windows?label=Live", "bearer "+s.AccessToken,
		"epic-launcher", nil, nil, &out)
	return out, err
}

// CatalogItem is what the catalog says about an item.
type CatalogItem struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Namespace  string `json:"namespace"`
	Categories []struct {
		Path string `json:"path"`
	} `json:"categories"`
	MainGameItem *struct {
		ID string `json:"id"`
	} `json:"mainGameItem"`
	EntitlementType  string               `json:"entitlementType"`
	CustomAttributes map[string]attribute `json:"customAttributes"`
}

type attribute struct {
	Value any `json:"value"`
}

const catalogBatch = 50

// Catalog reads the catalog items of one namespace (batched).
func (c *Client) Catalog(ctx context.Context, s *Session, namespace string, ids []string) (map[string]CatalogItem, error) {
	out := map[string]CatalogItem{}
	for len(ids) > 0 {
		n := min(len(ids), catalogBatch)
		q := url.Values{"id": {strings.Join(ids[:n], ",")}, "includeDLCDetails": {"true"}, "includeMainGameDetails": {"true"},
			"country": {"US"}, "locale": {"en"}}
		ids = ids[n:]
		var page map[string]CatalogItem
		u := c.o.CatalogURL + "/catalog/api/shared/namespace/" + url.PathEscape(namespace) + "/bulk/items?" + q.Encode()
		if err := c.do(ctx, http.MethodGet, u, "bearer "+s.AccessToken, "epic-launcher", nil, nil, &page); err != nil {
			return out, err
		}
		for k, v := range page {
			out[k] = v
		}
	}
	return out, nil
}

// Game is a game of the account offered for a prefill.
type Game struct {
	AppName       string
	Name          string
	Namespace     string
	CatalogItemID string
	BuildVersion  string
}

// Unreal Engine and Fab content and the categories that are no games.
var (
	skipNamespaces = []string{"ue", "89efe5924d3d467c839449ab6ab52e7f"}
	skipCategories = []string{"assets", "asset-format", "plugins", "projects", "mods"}
)

const maxGames = 5000

// Games lists the account's games: its Windows builds without Unreal
// Engine content, plugins, mods and DLC, named from the catalog.
func (c *Client) Games(ctx context.Context, s *Session) ([]Game, error) {
	assets, err := c.Assets(ctx, s)
	if err != nil {
		return nil, err
	}
	byNS := map[string][]Asset{}
	seen := map[string]bool{}
	for _, a := range assets {
		if a.AppName == "" || a.Namespace == "" || slices.Contains(skipNamespaces, a.Namespace) || seen[a.AppName] {
			continue
		}
		seen[a.AppName] = true
		byNS[a.Namespace] = append(byNS[a.Namespace], a)
		if len(seen) >= maxGames {
			break
		}
	}
	var out []Game
	for ns, list := range byNS {
		ids := make([]string, 0, len(list))
		for _, a := range list {
			ids = append(ids, a.CatalogItemID)
		}
		items, err := c.Catalog(ctx, s, ns, ids)
		if err != nil {
			// Unnamed rather than missing.
			c.log().Debug("epic catalog not read", "namespace", ns, "err", err)
		}
		for _, a := range list {
			it, ok := items[a.CatalogItemID]
			if ok && !isGame(it) {
				continue
			}
			name := strings.TrimSpace(it.Title)
			if name == "" {
				name = a.AppName
			}
			out = append(out, Game{AppName: a.AppName, Name: name, Namespace: a.Namespace, CatalogItemID: a.CatalogItemID, BuildVersion: a.BuildVersion})
		}
	}
	slices.SortFunc(out, func(a, b Game) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.AppName, b.AppName))
	})
	return out, nil
}

func (c *Client) log() interface{ Debug(string, ...any) } {
	if c.o.Log != nil {
		return c.o.Log
	}
	return noLog{}
}

type noLog struct{}

func (noLog) Debug(string, ...any) {}

// isGame drops DLC, Unreal content and mods, and the third-party-store
// titles that have no Epic build.
func isGame(it CatalogItem) bool {
	if it.MainGameItem != nil && it.MainGameItem.ID != "" {
		return false
	}
	for _, c := range it.Categories {
		for _, skip := range skipCategories {
			if c.Path == skip || strings.HasPrefix(c.Path, skip+"/") {
				return false
			}
		}
	}
	for _, k := range []string{"ThirdPartyManagedApp", "ThirdPartyManagedProvider"} {
		if v, ok := it.CustomAttributes[k]; ok {
			if s, _ := v.Value.(string); strings.Contains(strings.ToLower(s), "origin") || strings.Contains(strings.ToLower(s), "ea app") {
				return false
			}
		}
	}
	return true
}

// ManifestInfo is the manifest of an app's current Windows build.
type ManifestInfo struct {
	BuildVersion string
	// Hash is the SHA-1 (hex) of the manifest file: what identifies the
	// version.
	Hash         string
	UseSignedURL bool
	// URLs are the manifest's download addresses (with their short-lived
	// tokens), in Epic's order.
	URLs []*url.URL
}

// Manifest asks for the current manifest of an app.
func (c *Client) Manifest(ctx context.Context, s *Session, g Game) (*ManifestInfo, error) {
	u := fmt.Sprintf("%s/launcher/api/public/assets/v2/platform/Windows/namespace/%s/catalogItem/%s/app/%s/label/Live",
		c.o.LauncherURL, url.PathEscape(g.Namespace), url.PathEscape(g.CatalogItemID), url.PathEscape(g.AppName))
	var a struct {
		Elements []struct {
			BuildVersion string `json:"buildVersion"`
			Hash         string `json:"hash"`
			UseSignedURL bool   `json:"useSignedUrl"`
			Manifests    []struct {
				URI         string `json:"uri"`
				QueryParams []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"queryParams"`
			} `json:"manifests"`
		} `json:"elements"`
	}
	if err := c.do(ctx, http.MethodGet, u, "bearer "+s.AccessToken, "epic-launcher", nil, nil, &a); err != nil {
		return nil, err
	}
	if len(a.Elements) == 0 {
		return nil, errors.New("epic: no build for Windows")
	}
	e := a.Elements[0]
	mi := &ManifestInfo{BuildVersion: e.BuildVersion, Hash: strings.ToLower(e.Hash), UseSignedURL: e.UseSignedURL}
	for _, m := range e.Manifests {
		mu, err := url.Parse(m.URI)
		if err != nil || mu.Host == "" || (mu.Scheme != "https" && mu.Scheme != "http") {
			continue
		}
		q := url.Values{}
		for _, p := range m.QueryParams {
			q.Add(p.Name, p.Value)
		}
		mu.RawQuery = q.Encode()
		mi.URLs = append(mi.URLs, mu)
	}
	if len(mi.URLs) == 0 {
		return nil, errors.New("epic: the build names no manifest")
	}
	return mi, nil
}

// SignedChunkURLs asks for signed addresses of chunks (builds with
// useSignedUrl): a download ticket, then the addresses in batches of 50.
// The answer maps each chunk path to its URL.
func (c *Client) SignedChunkURLs(ctx context.Context, s *Session, g Game, buildVersion string, paths []string) (map[string]*url.URL, error) {
	var t struct {
		SignedTicket string `json:"signedTicket"`
	}
	body := map[string]string{"catalogItemId": g.CatalogItemID, "buildVersion": buildVersion, "appName": g.AppName,
		"namespace": g.Namespace, "label": "Live", "platform": "Windows"}
	if err := c.do(ctx, http.MethodPost, c.o.TicketURL+"/launcher/api/public/assets/v2/ticket", "bearer "+s.AccessToken,
		"epic-launcher", nil, body, &t); err != nil {
		return nil, err
	}
	if t.SignedTicket == "" {
		return nil, errors.New("epic: no download ticket")
	}
	out := make(map[string]*url.URL, len(paths))
	for i := 0; i < len(paths); i += 50 {
		batch := paths[i:min(i+50, len(paths))]
		var page map[string]string
		req := map[string]any{"signedTicket": t.SignedTicket, "chunkIds": batch, "deltaId": "", "clientMetrics": map[string]any{}}
		u := c.o.DeliveryURL + "/artifact-delivery/api/public/v1/delivery/account/" + url.PathEscape(s.AccountID) + "/downloadurls"
		if err := c.do(ctx, http.MethodPost, u, "bearer "+s.AccessToken, "epic-launcher", nil, req, &page); err != nil {
			return nil, err
		}
		for k, v := range page {
			if pu, err := url.Parse(v); err == nil && pu.Host != "" {
				out[k] = pu
			}
		}
	}
	return out, nil
}
