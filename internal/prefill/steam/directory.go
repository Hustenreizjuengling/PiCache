package steam

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
)

// DefaultDirectoryURL lists the CMs that accept WebSocket connections.
const DefaultDirectoryURL = "https://api.steampowered.com/ISteamDirectory/GetCMListForConnect/v1/?cellid=0&cmtype=websockets&realm=steamglobal&format=json"

const maxDirectory = 1 << 20

// cmServers returns the WebSocket endpoints (host:port) of the directory,
// least loaded first.
func cmServers(ctx context.Context, opt *Options) ([]string, error) {
	raw := opt.DirectoryURL
	if raw == "" {
		raw = DefaultDirectoryURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	opt.used(u.Hostname(), u.Scheme, "steam-directory")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := opt.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("steam server list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("steam server list: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDirectory+1))
	if err != nil {
		return nil, fmt.Errorf("steam server list: %w", err)
	}
	if len(b) > maxDirectory {
		return nil, errors.New("steam server list: answer too large")
	}
	var doc struct {
		Response struct {
			Success    bool `json:"success"`
			ServerList []struct {
				Endpoint string `json:"endpoint"`
				Type     string `json:"type"`
				Realm    string `json:"realm"`
				WtdLoad  any    `json:"wtd_load"` // a number (a string in VDF)
			} `json:"serverlist"`
		} `json:"response"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("steam server list: %w", err)
	}
	type cand struct {
		ep   string
		load float64
	}
	var cs []cand
	for _, s := range doc.Response.ServerList {
		if s.Type != "websockets" || (s.Realm != "" && s.Realm != "steamglobal") {
			continue
		}
		if _, _, err := net.SplitHostPort(s.Endpoint); err != nil {
			continue
		}
		cs = append(cs, cand{s.Endpoint, number(s.WtdLoad)})
	}
	if len(cs) == 0 {
		return nil, errors.New("steam server list: no WebSocket server")
	}
	slices.SortStableFunc(cs, func(a, b cand) int { return cmp.Compare(a.load, b.load) })
	// One of the five least loaded first, like Steam's clients, so that
	// not every client picks the same server.
	if n := min(len(cs), 5); n > 1 {
		i := rand.IntN(n)
		cs[0], cs[i] = cs[i], cs[0]
	}
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.ep)
	}
	return out, nil
}

func number(v any) float64 {
	switch v := v.(type) {
	case float64:
		return v
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	}
	return 0
}
