package battlenet_test

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/prefill/battlenet"
	"github.com/hustenreizjuengling/picache/internal/prefill/battlenet/bnettest"
)

func TestBLTE(t *testing.T) {
	data := bytes.Repeat([]byte("battle.net "), 300)
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(data[1000:])
	zw.Close()
	blocks := [][]byte{append([]byte{'N'}, data[:1000]...), append([]byte{'Z'}, z.Bytes()...)}
	hdr := []byte("BLTE")
	hdr = binary.BigEndian.AppendUint32(hdr, uint32(12+24*len(blocks)))
	hdr = append(hdr, 0x0F, 0, 0, 2)
	for i, b := range blocks {
		hdr = binary.BigEndian.AppendUint32(hdr, uint32(len(b)))
		hdr = binary.BigEndian.AppendUint32(hdr, uint32([]int{1000, len(data) - 1000}[i]))
		sum := md5.Sum(b)
		hdr = append(hdr, sum[:]...)
	}
	file := append(slices.Clone(hdr), append(blocks[0], blocks[1]...)...)
	ekey := battlenet.Key(md5.Sum(hdr))
	read := func(b []byte, k battlenet.Key) ([]byte, error) {
		r, err := battlenet.NewBLTEReader(bytes.NewReader(b), k)
		if err != nil {
			return nil, err
		}
		return io.ReadAll(r)
	}
	got, err := read(file, ekey)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("chunked: %v", err)
	}
	if _, err := read(file, battlenet.Key{1}); err == nil {
		t.Error("a wrong key was accepted")
	}
	bad := slices.Clone(file)
	bad[len(hdr)+10] ^= 1
	if _, err := read(bad, ekey); err == nil {
		t.Error("a damaged block was accepted")
	}
	single := append([]byte("BLTE\x00\x00\x00\x00N"), data...)
	if got, err := read(single, battlenet.Key(md5.Sum(single))); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("single: %v", err)
	}
	enc := []byte("BLTE\x00\x00\x00\x00E\x08abcdefgh\x04iviv")
	if _, err := read(enc, battlenet.Key{}); !errors.Is(err, battlenet.ErrEncrypted) {
		t.Errorf("encrypted: %v", err)
	}
	for i := range len(file) {
		_, _ = read(file[:i], battlenet.Key{})
	}
}

func TestSelect(t *testing.T) {
	// Entries: 0 common, 1 Windows deDE, 2 OSX, 3 Windows enUS, 4 censored.
	mask := func(es ...int) []byte {
		m := make([]byte, 1)
		for _, e := range es {
			m[0] |= 0x80 >> e
		}
		return m
	}
	tags := []battlenet.Tag{
		{Name: "Windows", Type: 1, Mask: mask(0, 1, 3, 4)},
		{Name: "OSX", Type: 1, Mask: mask(0, 2)},
		{Name: "deDE", Type: 3, Mask: mask(0, 1, 2, 4)},
		{Name: "enUS", Type: 3, Mask: mask(0, 2, 3, 4)},
		{Name: "speech", Type: 5, Mask: mask(0, 1, 2, 3, 4)},
		{Name: "text", Type: 5, Mask: mask(0, 1, 2, 3, 4)},
		{Name: "Alternate", Type: 0x4000, Mask: mask(4)},
	}
	q := battlenet.Query{{"Windows", "deDE", "speech"}, {"Windows", "deDE", "text"}}
	got := battlenet.Select(tags, 5, q)
	var in []int
	for i := range 5 {
		if got.Has(i) {
			in = append(in, i)
		}
	}
	if !slices.Equal(in, []int{0, 1}) {
		t.Errorf("selected %v", in)
	}
	// Without a locale the type narrows to what all locales share.
	got = battlenet.Select(tags, 5, battlenet.Query{{"Windows"}})
	if !got.Has(0) || got.Has(1) || got.Has(3) || got.Count() != 1 {
		t.Errorf("no locale: %d", got.Count())
	}
	if c, super := battlenet.Choose(tags, 5, battlenet.Query{{"nothing-matches"}, {"Windows", "OSX"}}); c.Count() == 0 || super && c.Count() != 5 {
		t.Errorf("choose %d %v", c.Count(), super)
	}
}

func TestProductConfig(t *testing.T) {
	pc := `{"eu+kr+us":{"all":{"config":{"noigr_tags":["noigr"],"tact_has_account_country_tags":true,"tact_has_geoip_country_tags":"true",
		"supported_locales":["enUS","deDE"],"replacement_locales":{"enGB":"enUS"},
		"opaque_complex_data":{"tags_config_uri":{"install_packages":[{"package_id":"base","required":true},
		{"package_id":"mw","install_packages":[{"package_id":"mw-cp","tags":["dlc05_01"]}]},{"package_id":"hd","tags":["hires"]}]}}}},
		"platform":{"win":{"config":{"tags":["Windows"],"tags_64bit":["x86_64"],"tags_32bit":["x86_32"]}}},
		"cn":{"config":{"extra_tags":["Alternate"]}}}}`
	c, err := battlenet.ParseProductConfig([]byte(pc), "eu")
	if err != nil {
		t.Fatal(err)
	}
	q := c.Query("eu", "deDE").String()
	if q != "Windows x86_64 noigr dlc05_01 hires EU geoip-DE acct-DEU deDE speech:Windows x86_64 noigr dlc05_01 hires EU geoip-DE acct-DEU deDE text" {
		t.Errorf("query %q", q)
	}
	if q := c.Query("us", "enGB").String(); !strings.Contains(q, " enUS speech") {
		t.Errorf("replacement locale %q", q)
	}
	if c, _ := battlenet.ParseProductConfig([]byte(`{"all":{"config":{"decryption_key_name":"x"}}}`), "eu"); c == nil || !c.Encrypted {
		t.Error("encryption not seen")
	}
}

// fetcher acts as PiCache's prefill: every request to the fake with the
// CDN host in Host.
type fetcher struct {
	base    string
	planned int64
	mu      sync.Mutex
	missing int
}

func (f *fetcher) get(ctx context.Context, path, rng string) (*http.Response, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.base+path, nil)
	req.Host = "level3.blizzard.com"
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	return resp, nil
}

func (f *fetcher) Fetch(ctx context.Context, path, rng string, read func(io.Reader) error) error {
	resp, err := f.get(ctx, path, rng)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if read != nil {
		if err := read(resp.Body); err != nil {
			return err
		}
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func (f *fetcher) FetchAll(ctx context.Context, reqs []battlenet.Request) error {
	for _, r := range reqs {
		if err := f.Fetch(ctx, r.Path, r.Range, nil); err != nil {
			if r.Optional && strings.Contains(err.Error(), "404") {
				f.mu.Lock()
				f.missing++
				f.mu.Unlock()
				continue
			}
			return err
		}
	}
	return nil
}

func (f *fetcher) Planned(n int64) { f.planned = n }

// Files of 192 bytes (3 slices of 64): which are chosen for Windows
// 64-bit, deDE, EU.
func testFiles() []bnettest.File {
	all := []string{"Windows", "OSX", "x86_64", "x86_32", "enUS", "deDE", "EU", "US", "speech", "text"}
	return []bnettest.File{
		{Name: "common", Size: 192, Tags: all}, // chosen (archive 0)
		{Name: "mac", Size: 192, Tags: []string{"OSX", "x86_64", "x86_32", "enUS", "deDE", "EU", "US", "speech", "text"}},            // not (archive 1)
		{Name: "win-de", Size: 192, Tags: []string{"Windows", "x86_64", "deDE", "EU", "US", "text"}},                                 // chosen (archive 0)
		{Name: "win-en", Size: 192, Tags: []string{"Windows", "x86_64", "enUS", "EU", "US", "speech", "text"}},                       // not (archive 1)
		{Name: "censored", Size: 192, Tags: append(slices.Clone(all), "Alternate")},                                                  // not (archive 0)
		{Name: "us-only", Size: 192, Tags: []string{"Windows", "x86_64", "deDE", "US", "speech", "text"}},                            // not (archive 1)
		{Name: "loose-de", Size: 100, Tags: []string{"Windows", "x86_64", "deDE", "EU", "speech"}, Loose: true},                      // chosen, file index
		{Name: "loose-unindexed", Size: 90, Tags: []string{"Windows", "x86_64", "deDE", "EU", "text"}, Loose: true, Unindexed: true}, // chosen, as the manifest says
		{Name: "Launcher.exe", Size: 80, Tags: all, Install: true, Loose: true},
		{Name: "root", Size: 70, Root: true},
	}
}

func client(fs *bnettest.Server) *battlenet.Client {
	return battlenet.New(battlenet.Options{ChannelsURL: fs.URL(), RibbitURL: fs.RibbitURL,
		PatchURL: func(string) string { return fs.URL() + "/none" }, SliceSize: 64})
}

func TestPrefill(t *testing.T) {
	fs := bnettest.New(bnettest.Product{Code: "game", Version: "1.0.0.1", Files: testFiles()})
	defer fs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := client(fs)
	v, err := c.Version(ctx, "game", "eu")
	if err != nil {
		t.Fatal(err)
	}
	if v.Source != "channels" || v.Name != "1.0.0.1" || v.Path != "tpr/game" || !slices.Equal(v.Hosts, []string{"level3.blizzard.com", "eu.cdn.blizzard.com", "us.cdn.blizzard.com"}) {
		t.Fatalf("version %+v", v)
	}
	f := &fetcher{base: fs.URL()}
	res, err := c.Prefill(ctx, v, "deDE", 4, f)
	if err != nil {
		t.Fatal(err)
	}
	if res.Superset || res.Files != 6 || res.Loose != 3 || !strings.Contains(res.Query, "EU deDE speech") || f.planned != res.Bytes {
		t.Fatalf("result %+v planned %d", res, f.planned)
	}
	// What was downloaded of the archives: the chosen files' slices only.
	got := map[string][][2]int{}
	loose := map[string]bool{}
	for _, r := range fs.Requests() {
		if r.Host != "level3.blizzard.com" {
			t.Errorf("host %s", r.Host)
		}
		key := r.Path[strings.LastIndexByte(r.Path, '/')+1:]
		if r.Range == "" || strings.HasSuffix(key, ".index") || strings.Contains(r.Path, "/config/") {
			continue
		}
		lo, hi, _ := strings.Cut(strings.TrimPrefix(r.Range, "bytes="), "-")
		a, _ := strconv.Atoi(lo)
		b, _ := strconv.Atoi(hi)
		got[key] = append(got[key], [2]int{a, b})
		loose[key] = true
	}
	sizes := map[string]int{}
	for _, f := range testFiles() {
		sizes[f.Name] = f.Size
	}
	covered := func(file string) bool {
		arch, off := fs.Location("game", file)
		if arch == "" {
			return loose[fs.EKey("game", file)]
		}
		for _, r := range got[arch] {
			if r[0] <= off && off+sizes[file]-1 <= r[1] {
				return true
			}
		}
		return false
	}
	touched := func(file string) bool {
		arch, off := fs.Location("game", file)
		for _, r := range got[arch] {
			if r[0] <= off+sizes[file]-1 && off <= r[1] {
				return true
			}
		}
		return false
	}
	for _, f := range []string{"common", "win-de", "loose-de", "loose-unindexed", "Launcher.exe", "root"} {
		if !covered(f) {
			t.Errorf("%s was not downloaded: %v", f, got)
		}
	}
	for _, f := range []string{"mac", "win-en", "censored", "us-only"} {
		if touched(f) {
			t.Errorf("%s was downloaded: %v", f, got)
		}
	}
	if f.missing != 0 {
		t.Errorf("%d missing", f.missing)
	}

	// Without the channels, Ribbit answers.
	fs.DisableChannels(true)
	v2, err := client(fs).Version(ctx, "game", "eu")
	if err != nil || v2.Source != "ribbit" || v2.BuildConfig != v.BuildConfig || v2.ProductConfig != v.ProductConfig || v2.Path != v.Path {
		t.Fatalf("ribbit %+v %v", v2, err)
	}
	if _, err := client(fs).Version(ctx, "nope", "eu"); err == nil {
		t.Error("an unknown product has a version")
	}
}

func TestEncryptedBuild(t *testing.T) {
	fs := bnettest.New(bnettest.Product{Code: "sea", Version: "2", Files: testFiles(), EncryptedBuild: true})
	defer fs.Close()
	ctx := context.Background()
	c := client(fs)
	v, err := c.Version(ctx, "sea", "eu")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Prefill(ctx, v, "enUS", 2, &fetcher{base: fs.URL()}); !errors.Is(err, battlenet.ErrEncrypted) {
		t.Errorf("encrypted build: %v", err)
	}
}
