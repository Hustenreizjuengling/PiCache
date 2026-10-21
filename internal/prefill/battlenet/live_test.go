//go:build livebnet

package battlenet

// Manual live test against Blizzard: the version services, then for each
// product (PREFILL_LIVE_PRODUCTS, default s1) the product, build and CDN
// configs, the manifests and every archive index straight from the CDN,
// and the plan of a deDE Windows install. No archive data is downloaded.
//
//	go test -tags livebnet -run TestLive -v ./internal/prefill/battlenet/
//
// Every host is resolved through public DNS over HTTPS and dialled by
// address, never through the local resolver (which may be a cache).

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
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

var (
	pinMu sync.Mutex
	pins  = map[string]string{}
)

func pinnedDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	pinMu.Lock()
	ip, ok := pins[host]
	pinMu.Unlock()
	if !ok {
		if ip, err = dohResolve(ctx, host); err != nil {
			return nil, err
		}
		pinMu.Lock()
		pins[host] = ip
		pinMu.Unlock()
	}
	return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip, port))
}

// direct fetches from the CDN itself (no cache); FetchAll only plans.
type direct struct {
	t     *testing.T
	hc    *http.Client
	host  string
	bytes int64
	reqs  int
	plan  []Request
}

func (d *direct) Fetch(ctx context.Context, path, rng string, read func(io.Reader) error) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+d.host+path, nil)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		return fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	cr := &counter{r: resp.Body}
	if read != nil {
		if err := read(cr); err != nil {
			return err
		}
	}
	_, err = io.Copy(io.Discard, cr)
	d.bytes += cr.n
	d.reqs++
	return err
}

type counter struct {
	r io.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (d *direct) FetchAll(_ context.Context, reqs []Request) error {
	d.plan = reqs
	return nil
}

func (d *direct) Planned(int64) {}

func TestLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	used := func(h, p, why string) { t.Logf("used %s://%s (%s)", p, h, why) }
	c := New(Options{DialContext: pinnedDial, Log: log, Used: used})
	products := strings.Fields(os.Getenv("PREFILL_LIVE_PRODUCTS"))
	if len(products) == 0 {
		products = []string{"s1"}
	}
	// The version services: every product of the list via the channels,
	// one via Ribbit and the HTTP proxy.
	for _, p := range Products {
		v, err := c.Version(ctx, p.Code, "eu")
		if err != nil {
			t.Logf("%-16s %v", p.Code, err)
			continue
		}
		t.Logf("%-16s %-24s %-24s %s via %s, hosts %v", p.Code, v.Name, v.Path, v.BuildConfig, v.Source, v.Hosts)
	}
	for _, src := range []Options{{ChannelsURL: "-"}, {ChannelsURL: "-", RibbitURL: func(string) string { return "http://127.0.0.1:1" }}} {
		src.DialContext, src.Used = pinnedDial, used
		v, err := New(src).Version(ctx, "s1", "eu")
		if err != nil {
			t.Errorf("fallback: %v", err)
			continue
		}
		t.Logf("s1 via %s: %s %s", v.Source, v.Name, v.BuildConfig)
	}
	for _, code := range products {
		v, err := c.Version(ctx, code, "eu")
		if err != nil {
			t.Fatal(err)
		}
		d := &direct{t: t, host: v.Hosts[0], hc: &http.Client{Transport: &http.Transport{DialContext: pinnedDial, DisableCompression: true}}}
		used(v.Hosts[0], "http", "cdn (direct, live test)")
		start := time.Now()
		res, err := c.Prefill(ctx, v, "deDE", 8, d)
		if err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		t.Logf("%s tags %v; %d entries, %d by the superset", code, res.Tags, res.Entries, res.SupersetFiles)
		t.Logf("%s %s: query %q (superset %v): %d files (%d loose) of %.2f GB; plan %d requests, %.2f GB in whole cache slices; read %d requests, %.1f MB in %s",
			code, v.Name, res.Query, res.Superset, res.Files, res.Loose, float64(res.FileBytes)/1e9, len(d.plan), float64(res.Bytes)/1e9,
			d.reqs, float64(d.bytes)/1e6, time.Since(start).Round(time.Second))
	}
}
