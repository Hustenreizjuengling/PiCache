//go:build livesteam

package steam

// Manual live test against Steam (anonymous logon, app 1007 depot 1004):
//
//	go test -tags livesteam -run TestLive -v ./internal/prefill/steam/
//
// Every host is resolved through public DNS over HTTPS and dialled by
// address, never through the local resolver.

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

func dohResolve(ctx context.Context, host string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://cloudflare-dns.com/dns-query?name="+host+"&type=A", nil)
	req.Header.Set("Accept", "application/dns-json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var doc struct {
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		} `json:"Answer"`
	}
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", err
	}
	for _, a := range doc.Answer {
		if a.Type == 1 {
			return a.Data, nil
		}
	}
	return "", fmt.Errorf("no A record for %s", host)
}

func pinnedDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ip, err := dohResolve(ctx, host)
	if err != nil {
		return nil, err
	}
	return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip, port))
}

func TestLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	opt := Options{DialContext: pinnedDial, Log: log, Used: func(h, p, why string) { t.Logf("used %s://%s (%s)", p, h, why) }}
	c, err := Connect(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	t.Logf("connected to %s", c.Host())

	// The QR sign-in starts (nobody scans it).
	q, err := c.BeginQR(ctx, "PiCache Prefill (live test)")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("QR challenge %s interval %s", q.URL(), q.Interval())
	res, err := q.Poll(ctx)
	t.Logf("poll: %v %v scanned=%v", res, err, q.Scanned())
	if err != nil || res != nil {
		t.Fatal("the first poll should wait")
	}

	sess, err := c.SignIn(ctx, LogOn{LoginID: 0x5049434b, MachineID: "live-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("anonymous session %x cell %d", sess.SteamID, sess.CellID)
	games, err := sess.Games(ctx, []uint32{1007}, Filter{OS: []string{"windows"}, Languages: []string{"english"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(games) != 1 || len(games[0].Depots) == 0 {
		t.Fatalf("games = %+v", games)
	}
	g := games[0]
	t.Logf("game %d %q type %s depots %+v bytes %d", g.ID, g.Name, g.Type, g.Depots, g.Bytes())
	servers, err := sess.ContentServers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range servers[:min(len(servers), 6)] {
		t.Logf("server %+v", s)
	}
	var host string
	for _, s := range servers {
		if (s.Type == "CDN" || s.Type == "SteamCache") && !s.Proxy && len(s.AppIDs) == 0 {
			host = s.Host
			break
		}
	}
	if host == "" {
		t.Fatal("no HTTP content server")
	}
	d := g.Depots[0]
	code, err := sess.ManifestRequestCode(ctx, d.App, d.ID, d.Manifest, "public")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("depot %d manifest %d code %d via %s", d.ID, d.Manifest, code, host)
	hc := &http.Client{Transport: &http.Transport{DialContext: pinnedDial, Proxy: nil}, Timeout: time.Minute}
	get := func(path string) []byte {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+path, nil)
		req.Header.Set("User-Agent", UserAgent)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: HTTP %d %v %q", path, resp.StatusCode, resp.Header, b[:min(len(b), 200)])
		}
		t.Logf("%s: HTTP %d, %d bytes", path, resp.StatusCode, len(b))
		return b
	}
	m, err := ParseManifest(get(ManifestPath(d.ID, d.Manifest, code)))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("manifest depot %d gid %d: %d chunks, %d bytes (app info said %d)", m.Depot, m.GID, len(m.Chunks), m.Bytes, d.Bytes)
	for _, ch := range m.Chunks[:min(2, len(m.Chunks))] {
		b := get(ch.Path(d.ID))
		if uint32(len(b)) != ch.Compressed {
			t.Errorf("chunk %x: %d bytes, manifest says %d", ch.SHA, len(b), ch.Compressed)
		}
	}
	sess.LogOff()
}
