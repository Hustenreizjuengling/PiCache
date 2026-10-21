package battlenet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"
)

// Fetcher downloads from the CDN through the cache.
type Fetcher interface {
	// Fetch gets path (rng "" or "bytes=a-b") and passes the body to read
	// (nil: dropped). read may be called again on a retry.
	Fetch(ctx context.Context, path, rng string, read func(io.Reader) error) error
	// FetchAll gets reqs, dropping the bodies.
	FetchAll(ctx context.Context, reqs []Request) error
	// Planned reports the bytes the prefill will download.
	Planned(bytes int64)
}

// Request is one download of the plan.
type Request struct {
	Path  string
	Range string // "" or "bytes=a-b"
	Bytes int64  // expected
	// Optional: a missing file (404) is not an error (a loose file the
	// indexes do not name may not be on every CDN).
	Optional bool
}

// Result describes a prefill.
type Result struct {
	Query    string
	Superset bool // the tags were chosen by the safe fallback
	// Tags are the download manifest's tags ("name/type") and Entries
	// its files; SupersetFiles the files the fallback would choose.
	Tags          []string
	Entries       int
	SupersetFiles int
	Files         int // files chosen (with the install files and the root)
	Loose         int // of them, outside the archives
	// FileBytes is the chosen files' size; Bytes what the plan downloads
	// (whole slices of the cache, the manifests and indexes).
	FileBytes int64
	Bytes     int64
}

const (
	maxConfig    = 8 << 20
	maxIndex     = 64 << 20
	maxRun       = 64 << 20 // one archive request
	defaultSlice = 1 << 20
)

func (v *Version) configPath(k Key) string { return "/" + v.Path + "/config/" + hashPath(k.String()) }
func (v *Version) dataPath(k Key) string   { return "/" + v.Path + "/data/" + hashPath(k.String()) }
func (v *Version) indexPath(folder string, k Key) string {
	return "/" + v.Path + "/" + folder + "/" + hashPath(k.String()) + ".index"
}

func whole(size int64) string {
	if size <= 0 {
		return ""
	}
	return "bytes=0-" + strconv.FormatInt(size-1, 10)
}

func fetchKeep(ctx context.Context, f Fetcher, path, rng string, limit int64) ([]byte, error) {
	var out []byte
	err := f.Fetch(ctx, path, rng, func(r io.Reader) error {
		b, err := io.ReadAll(io.LimitReader(r, limit+1))
		if err != nil {
			return err
		}
		if int64(len(b)) > limit {
			return fmt.Errorf("%s is larger than %d bytes", path, limit)
		}
		out = b
		return nil
	})
	return out, err
}

// selection is the chosen files, sorted by key.
type selection struct {
	keys  []Key
	sizes []uint64
	found Bitset
}

func (s *selection) Len() int           { return len(s.keys) }
func (s *selection) Less(i, j int) bool { return cmpKey(&s.keys[i], &s.keys[j]) < 0 }
func (s *selection) Swap(i, j int) {
	s.keys[i], s.keys[j] = s.keys[j], s.keys[i]
	s.sizes[i], s.sizes[j] = s.sizes[j], s.sizes[i]
}

func (s *selection) find(k *Key) int {
	i, ok := slices.BinarySearchFunc(s.keys, k, func(a Key, b *Key) int { return cmpKey(&a, b) })
	if !ok {
		return -1
	}
	return i
}

// Prefill downloads v's Windows install in locale through f, with conns
// parallel index downloads.
func (c *Client) Prefill(ctx context.Context, v *Version, locale string, conns int, f Fetcher) (*Result, error) {
	pc := &ProductConfig{Tags: []string{"Windows", "x86_64"}}
	if !v.ProductConfig.IsZero() {
		b, err := fetchKeep(ctx, f, "/"+v.ConfigPath+"/"+hashPath(v.ProductConfig.String()), "", maxConfig)
		if err != nil {
			return nil, fmt.Errorf("product config: %w", err)
		}
		if pc, err = ParseProductConfig(b, v.Region); err != nil {
			return nil, err
		}
		if pc.Encrypted {
			return nil, ErrEncrypted
		}
	}
	q := pc.Query(v.Region, locale)
	b, err := fetchKeep(ctx, f, v.configPath(v.BuildConfig), "", maxConfig)
	if err != nil {
		return nil, fmt.Errorf("build config: %w", err)
	}
	bc, err := ParseConfig(b, "Build Configuration")
	if err != nil {
		return nil, err
	}
	if b, err = fetchKeep(ctx, f, v.configPath(v.CDNConfig), "", maxConfig); err != nil {
		return nil, fmt.Errorf("CDN config: %w", err)
	}
	cc, err := ParseConfig(b, "CDN Configuration")
	if err != nil {
		return nil, err
	}
	if !v.KeyRing.IsZero() {
		if err := f.Fetch(ctx, v.configPath(v.KeyRing), "", nil); err != nil {
			return nil, fmt.Errorf("key ring: %w", err)
		}
	}
	manifest := func(name string, parse func(io.Reader) error) error {
		ek, ok := bc.Key(name, 1)
		if !ok {
			return fmt.Errorf("the build config names no %s manifest", name)
		}
		size, _ := bc.Int(name+"-size", 1)
		return f.Fetch(ctx, v.dataPath(ek), whole(size), func(r io.Reader) error {
			br, err := NewBLTEReader(r, ek)
			if err != nil {
				return err
			}
			return parse(br)
		})
	}

	// The download manifest: the chosen files, compacted in place.
	var d *Download
	if err := manifest("download", func(r io.Reader) (err error) { d, err = ParseDownload(r); return }); err != nil {
		return nil, fmt.Errorf("download manifest: %w", err)
	}
	chosen, super := Choose(d.Tags, d.N, q)
	res := &Result{Query: q.String(), Superset: super, Entries: d.N, SupersetFiles: Superset(d.Tags, d.N, q).Count()}
	for _, t := range d.Tags {
		res.Tags = append(res.Tags, t.Name+"/"+strconv.Itoa(int(t.Type)))
	}
	sel := &selection{keys: d.Keys, sizes: d.Sizes}
	j := 0
	for i := range d.N {
		if chosen.Has(i) && d.Sizes[i] > 0 {
			sel.keys[j], sel.sizes[j] = d.Keys[i], d.Sizes[i]
			j++
		}
	}
	sel.keys, sel.sizes = sel.keys[:j], sel.sizes[:j]
	d = nil

	// The install manifest's files and the root, by content key.
	need := map[Key]bool{}
	var in *Install
	if _, ok := bc.Key("install", 1); ok {
		if err := manifest("install", func(r io.Reader) (err error) { in, err = ParseInstall(r); return }); err != nil {
			return nil, fmt.Errorf("install manifest: %w", err)
		}
		ichosen, _ := Choose(in.Tags, in.N, q)
		for i := range in.N {
			if ichosen.Has(i) {
				need[in.CKeys[i]] = true
			}
		}
	}
	if root, ok := bc.Key("root", 0); ok && !root.IsZero() {
		need[root] = true
	}
	var ekeys map[Key]Key
	if err := manifest("encoding", func(r io.Reader) (err error) { ekeys, err = ScanEncoding(r, need); return }); err != nil {
		return nil, fmt.Errorf("encoding: %w", err)
	}
	for _, ek := range ekeys {
		sel.keys = append(sel.keys, ek)
		sel.sizes = append(sel.sizes, 0)
	}
	sort.Sort(sel)
	// Unique keys (the largest size).
	w := 0
	for i := range sel.keys {
		if w > 0 && sel.keys[w-1] == sel.keys[i] {
			sel.sizes[w-1] = max(sel.sizes[w-1], sel.sizes[i])
			continue
		}
		sel.keys[w], sel.sizes[w] = sel.keys[i], sel.sizes[i]
		w++
	}
	sel.keys, sel.sizes = sel.keys[:w], sel.sizes[:w]
	sel.found = newBitset(w, false)
	res.Files = w

	// The other manifests the agent fetches: each whole, by its own key.
	var meta []Request
	seen := map[Key]bool{}
	for _, name := range slices.Sorted(maps.Keys(bc)) {
		if strings.HasSuffix(name, "-size") || !(name == "size" || name == "patch-index" || strings.HasPrefix(name, "vfs-")) {
			continue
		}
		ek, ok := bc.Key(name, 1)
		if !ok || seen[ek] {
			continue
		}
		seen[ek] = true
		size, _ := bc.Int(name+"-size", 1)
		meta = append(meta, Request{Path: v.dataPath(ek), Range: whole(size), Bytes: size})
	}
	for _, k := range cc.Keys("patch-archives") {
		meta = append(meta, Request{Path: v.indexPath("patch", k)})
	}
	if k, ok := cc.Key("patch-file-index", 0); ok {
		meta = append(meta, Request{Path: v.indexPath("patch", k)})
	}

	// The archive indexes: where each chosen file lies.
	slice := c.o.SliceSize
	if slice <= 0 {
		slice = defaultSlice
	}
	pl := newPlanner(slice)
	archives := cc.Keys("archives")
	if len(archives) == 0 {
		return nil, errors.New("the CDN config names no archives")
	}
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(conns, 1))
	for ai, a := range archives {
		g.Go(func() error {
			b, err := fetchKeep(gctx, f, v.indexPath("data", a), "", maxIndex)
			if err != nil {
				return fmt.Errorf("archive index %s: %w", a, err)
			}
			type hit struct {
				i         int
				off, size uint64
			}
			var hits []hit
			var end uint64
			if _, err := ParseIndex(b, func(k *Key, size, off uint64) {
				end = max(end, off+size)
				if i := sel.find(k); i >= 0 {
					hits = append(hits, hit{i, off, size})
				}
			}); err != nil {
				return fmt.Errorf("archive index %s: %w", a, err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, h := range hits {
				if !sel.found.Has(h.i) {
					sel.found.set(h.i)
					pl.addRange(ai, a, h.off, h.size, end)
					res.FileBytes += int64(h.size)
				}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Loose files: by the loose-file index, else as the manifest says.
	sizeOf := map[int]uint64{}
	if k, ok := cc.Key("file-index", 0); ok {
		b, err := fetchKeep(ctx, f, v.indexPath("data", k), "", maxIndex)
		if err != nil {
			return nil, fmt.Errorf("file index: %w", err)
		}
		if _, err := ParseIndex(b, func(k *Key, size, _ uint64) {
			if i := sel.find(k); i >= 0 && !sel.found.Has(i) {
				sizeOf[i] = size
			}
		}); err != nil {
			return nil, fmt.Errorf("file index: %w", err)
		}
	}
	var loose []Request
	for i := range sel.keys {
		if sel.found.Has(i) {
			continue
		}
		size, indexed := sizeOf[i]
		if !indexed {
			size = sel.sizes[i]
		}
		if size == 0 {
			continue
		}
		loose = append(loose, Request{Path: v.dataPath(sel.keys[i]), Range: whole(int64(size)), Bytes: int64(size), Optional: !indexed})
		res.FileBytes += int64(size)
	}
	res.Loose = len(loose)
	reqs := append(meta, pl.requests(v, maxRun)...)
	reqs = append(reqs, loose...)
	for _, r := range reqs {
		res.Bytes += r.Bytes
	}
	f.Planned(res.Bytes)
	if err := f.FetchAll(ctx, reqs); err != nil {
		return res, err
	}
	return res, nil
}

// planner collects the archive ranges as runs of the cache's slices: the
// cache fills whole slices, so a slice-aligned request costs nothing more
// upstream and covers every range a client asks for inside it.
type planner struct {
	s    int64
	arch map[int]*archPlan
}

type archPlan struct {
	key    Key
	slices Bitset
	end    int64 // the archive's size (the end of its last file)
}

func newPlanner(s int64) *planner { return &planner{s: s, arch: map[int]*archPlan{}} }

func (p *planner) addRange(ai int, key Key, off, size, end uint64) {
	a := p.arch[ai]
	if a == nil {
		a = &archPlan{key: key}
		p.arch[ai] = a
	}
	a.end = max(a.end, int64(end))
	if size == 0 {
		return
	}
	first, last := int(int64(off)/p.s), int(int64(off+size-1)/p.s)
	if need := last/64 + 1; need > len(a.slices) {
		a.slices = append(a.slices, make(Bitset, need-len(a.slices))...)
	}
	for i := first; i <= last; i++ {
		a.slices.set(i)
	}
}

func (p *planner) requests(v *Version, maxRun int64) []Request {
	var out []Request
	per := max(int(maxRun/p.s), 1)
	for _, ai := range slices.Sorted(maps.Keys(p.arch)) {
		a := p.arch[ai]
		n := len(a.slices) * 64
		for i := 0; i < n; {
			if !a.slices.Has(i) {
				i++
				continue
			}
			j := i
			for j+1 < n && a.slices.Has(j+1) && j+1-i < per {
				j++
			}
			lo, hi := int64(i)*p.s, min(int64(j+1)*p.s, a.end)-1
			if hi >= lo {
				out = append(out, Request{Path: v.dataPath(a.key), Range: fmt.Sprintf("bytes=%d-%d", lo, hi), Bytes: hi - lo + 1})
			}
			i = j + 1
		}
	}
	return out
}
