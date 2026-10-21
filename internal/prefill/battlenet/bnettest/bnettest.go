// Package bnettest is a fake Battle.net for tests: the version services
// (TACT channels, Ribbit v2) and a CDN, acting as the cache, that serves a
// product's configs, manifests, archives and indexes in their real
// formats, with ranges.
package bnettest

import (
	"bytes"
	"compress/zlib"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// File is a file of a build.
type File struct {
	Name string
	Size int // its BLTE blob's size (≥ 9)
	Tags []string
	// Loose files are outside the archives; Unindexed ones are not in the
	// loose-file index either.
	Loose, Unindexed bool
	// Install files are named by the install manifest (by content key),
	// not the download manifest; Root is the build's root.
	Install, Root bool
}

// TagTypes are the fake's tag types.
var TagTypes = map[string]uint16{
	"Windows": 1, "OSX": 1, "x86_64": 2, "x86_32": 2, "enUS": 3, "deDE": 3, "EU": 4, "US": 4, "speech": 5, "text": 5, "Alternate": 0x4000,
}

// Product is a fake product.
type Product struct {
	Code, Version string
	Files         []File
	// EncryptedBuild makes the build config unreadable.
	EncryptedBuild bool
}

// Request is a CDN request the fake saw.
type Request struct {
	Host, Path, Range string
}

// Server is the fake.
type Server struct {
	HTTP *httptest.Server

	mu       sync.Mutex
	products map[string]*built
	objects  map[string][]byte // CDN path → content
	channels map[string][]byte // channel file hash → content
	summary  []byte
	requests []Request
	hits     map[string]bool
	noChan   bool
	ribbit   int
}

type built struct {
	p                       Product
	build, cdn, productConf string
	path                    string
	ekeys                   map[string]string // file name → EKey
	offsets                 map[string][2]int // file name → archive number, offset
	archives                []string
}

// New starts the fake.
func New(products ...Product) *Server {
	s := &Server{products: map[string]*built{}, objects: map[string][]byte{}, channels: map[string][]byte{}, hits: map[string]bool{}}
	for _, p := range products {
		s.add(p)
	}
	s.publish()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /summary", s.summaryH)
	mux.HandleFunc("GET /blue/", s.channelH)
	mux.HandleFunc("GET /v2/{region}/products/{p}/{what}", s.ribbitH)
	mux.HandleFunc("GET /lancache-heartbeat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-LanCache-Processed-By", "fake-cache")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /tpr/", s.cdnH)
	s.HTTP = httptest.NewServer(mux)
	return s
}

func (s *Server) Close()      { s.HTTP.Close() }
func (s *Server) URL() string { return s.HTTP.URL }

// Addr is the fake's host:port (the cache address).
func (s *Server) Addr() string { return strings.TrimPrefix(s.HTTP.URL, "http://") }

// RibbitURL is the fake's Ribbit v2 base of a region.
func (s *Server) RibbitURL(region string) string { return s.HTTP.URL + "/v2/" + region }

// DisableChannels makes the TACT channels fail (the Ribbit fallback).
func (s *Server) DisableChannels(off bool) {
	s.mu.Lock()
	s.noChan = off
	s.mu.Unlock()
}

// RibbitCalls counts the Ribbit requests.
func (s *Server) RibbitCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ribbit
}

// Requests lists the CDN requests.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// ResetRequests forgets the CDN requests.
func (s *Server) ResetRequests() {
	s.mu.Lock()
	s.requests = nil
	s.mu.Unlock()
}

// SetProduct replaces (or adds) a product: a new build.
func (s *Server) SetProduct(p Product) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.add(p)
	s.publish()
}

// EKey is a file's encoding key; Archive its archive and offset (-1 for a
// loose file).
func (s *Server) EKey(code, file string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.products[code].ekeys[file]
}

// Location returns a file's archive key and offset ("" for a loose file).
func (s *Server) Location(code, file string) (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.products[code]
	o, ok := b.offsets[file]
	if !ok {
		return "", -1
	}
	return b.archives[o[0]], o[1]
}

// Path is a product's CDN path.
func Path(code string) string { return "tpr/" + code }

func md5hex(b []byte) string {
	h := md5.Sum(b)
	return hex.EncodeToString(h[:])
}

func hashPath(h string) string { return h[0:2] + "/" + h[2:4] + "/" + h }

// blteN is a single raw block without a block table.
func blteN(data []byte) []byte {
	return append([]byte("BLTE\x00\x00\x00\x00N"), data...)
}

// blteZ splits data into two zlib blocks with a block table.
func blteZ(data []byte) []byte {
	parts := [][]byte{data[:len(data)/2], data[len(data)/2:]}
	var blocks [][]byte
	for _, p := range parts {
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		zw.Write(p)
		zw.Close()
		blocks = append(blocks, append([]byte{'Z'}, z.Bytes()...))
	}
	hdr := []byte("BLTE")
	hdr = binary.BigEndian.AppendUint32(hdr, uint32(12+24*len(blocks)))
	hdr = append(hdr, 0x0F, 0, 0, byte(len(blocks)))
	for i, b := range blocks {
		hdr = binary.BigEndian.AppendUint32(hdr, uint32(len(b)))
		hdr = binary.BigEndian.AppendUint32(hdr, uint32(len(parts[i])))
		sum := md5.Sum(b)
		hdr = append(hdr, sum[:]...)
	}
	out := hdr
	for _, b := range blocks {
		out = append(out, b...)
	}
	return out
}

// blob is a file's content: its BLTE blob of size bytes, unique per build.
func blob(p Product, f File) []byte {
	data := []byte(fmt.Sprintf("%s/%s/%s;", p.Code, p.Version, f.Name))
	for len(data) < f.Size-9 {
		data = append(data, byte('a'+len(data)%26))
	}
	return blteN(data[:max(f.Size-9, 0)])
}

func keyBytes(h string) []byte {
	b, _ := hex.DecodeString(h)
	return b
}

func tagSection(names []string, flags [][]string) []byte {
	var out []byte
	n := len(flags)
	for _, t := range names {
		out = append(out, t...)
		out = append(out, 0)
		out = binary.BigEndian.AppendUint16(out, TagTypes[t])
		mask := make([]byte, (n+7)/8)
		for i, f := range flags {
			if slices.Contains(f, t) {
				mask[i>>3] |= 0x80 >> (i & 7)
			}
		}
		out = append(out, mask...)
	}
	return out
}

func allTags() []string {
	names := make([]string, 0, len(TagTypes))
	for t := range TagTypes {
		names = append(names, t)
	}
	sort.Strings(names)
	return names
}

// index builds an .index: 24-byte records (or 20 for loose), 4 KiB blocks,
// a table of contents and the footer.
func index(recs [][3]uint64, keys []string, offBytes int) []byte {
	type rec struct {
		key       []byte
		size, off uint64
	}
	rs := make([]rec, len(keys))
	for i := range keys {
		rs[i] = rec{keyBytes(keys[i]), recs[i][0], recs[i][1]}
	}
	slices.SortFunc(rs, func(a, b rec) int { return bytes.Compare(a.key, b.key) })
	size := 16 + 4 + offBytes
	per := 4096 / size
	var blocks, last [][]byte
	for i := 0; i < len(rs); i += per {
		chunk := rs[i:min(i+per, len(rs))]
		blk := make([]byte, 0, 4096)
		for _, r := range chunk {
			blk = append(blk, r.key...)
			blk = binary.BigEndian.AppendUint32(blk, uint32(r.size))
			if offBytes == 4 {
				blk = binary.BigEndian.AppendUint32(blk, uint32(r.off))
			}
		}
		last = append(last, chunk[len(chunk)-1].key)
		blocks = append(blocks, append(blk, make([]byte, 4096-len(blk))...))
	}
	var out []byte
	for _, b := range blocks {
		out = append(out, b...)
	}
	for _, k := range last {
		out = append(out, k...)
	}
	for range blocks {
		out = append(out, make([]byte, 8)...)
	}
	f := make([]byte, 28)
	f[8], f[11], f[12], f[13], f[14], f[15] = 1, 4, byte(offBytes), 4, 16, 8
	binary.LittleEndian.PutUint32(f[16:], uint32(len(rs)))
	return append(out, f...)
}

func (s *Server) add(p Product) {
	b := &built{p: p, path: Path(p.Code), ekeys: map[string]string{}, offsets: map[string][2]int{}}
	put := func(folder, key string, data []byte, suffix string) {
		s.objects["/"+b.path+"/"+folder+"/"+hashPath(key)+suffix] = data
	}
	blobs := map[string][]byte{}
	ckeys := map[string]string{}
	for _, f := range p.Files {
		bl := blob(p, f)
		blobs[f.Name] = bl
		b.ekeys[f.Name] = md5hex(bl)
		ckeys[f.Name] = md5hex(bl[9:])
	}
	// Two archives; loose files on their own.
	var arch [2][]byte
	var archRecs [2][][3]uint64
	var archKeys [2][]string
	var looseRecs [][3]uint64
	var looseKeys []string
	n := 0
	for _, f := range p.Files {
		bl := blobs[f.Name]
		if f.Loose {
			put("data", b.ekeys[f.Name], bl, "")
			if !f.Unindexed {
				looseRecs = append(looseRecs, [3]uint64{uint64(len(bl)), 0})
				looseKeys = append(looseKeys, b.ekeys[f.Name])
			}
			continue
		}
		a := n % 2
		n++
		b.offsets[f.Name] = [2]int{a, len(arch[a])}
		archRecs[a] = append(archRecs[a], [3]uint64{uint64(len(bl)), uint64(len(arch[a]))})
		archKeys[a] = append(archKeys[a], b.ekeys[f.Name])
		arch[a] = append(arch[a], bl...)
	}
	for a := range arch {
		key := md5hex(append([]byte(p.Code+p.Version), arch[a]...))
		b.archives = append(b.archives, key)
		put("data", key, arch[a], "")
		put("data", key, index(archRecs[a], archKeys[a], 4), ".index")
	}
	fileIndex := md5hex([]byte(p.Code + p.Version + "fileindex"))
	put("data", fileIndex, index(looseRecs, looseKeys, 0), ".index")
	patchArchive := md5hex([]byte(p.Code + p.Version + "patch"))
	put("patch", patchArchive, index(nil, nil, 4), ".index")
	patchFileIndex := md5hex([]byte(p.Code + p.Version + "patchfiles"))
	put("patch", patchFileIndex, index(nil, nil, 0), ".index")

	// Download manifest v3 (the archived and loose files, not the install
	// files or the root), zlib blocks.
	var dl []byte
	var flags [][]string
	var entries []File
	for _, f := range p.Files {
		if !f.Install && !f.Root {
			entries = append(entries, f)
		}
	}
	dl = append(dl, 'D', 'L', 3, 16, 0)
	dl = binary.BigEndian.AppendUint32(dl, uint32(len(entries)))
	tags := allTags()
	dl = binary.BigEndian.AppendUint16(dl, uint16(len(tags)))
	dl = append(dl, 0, 0, 0, 0, 0)
	for _, f := range entries {
		dl = append(dl, keyBytes(b.ekeys[f.Name])...)
		size := uint64(len(blobs[f.Name]))
		dl = append(dl, byte(size>>32))
		dl = binary.BigEndian.AppendUint32(dl, uint32(size))
		dl = append(dl, 0)
		flags = append(flags, f.Tags)
	}
	dl = append(dl, tagSection(tags, flags)...)
	dlBlob := blteZ(dl)
	dlKey := md5hex(dlBlob[:12+24*2])
	put("data", dlKey, dlBlob, "")

	// Install manifest v1: the install files.
	var inst []byte
	var iflags [][]string
	var ifiles []File
	for _, f := range p.Files {
		if f.Install {
			ifiles = append(ifiles, f)
			iflags = append(iflags, f.Tags)
		}
	}
	inst = append(inst, 'I', 'N', 1, 16)
	inst = binary.BigEndian.AppendUint16(inst, uint16(len(tags)))
	inst = binary.BigEndian.AppendUint32(inst, uint32(len(ifiles)))
	inst = append(inst, tagSection(tags, iflags)...)
	for _, f := range ifiles {
		inst = append(inst, f.Name...)
		inst = append(inst, 0)
		inst = append(inst, keyBytes(ckeys[f.Name])...)
		inst = binary.BigEndian.AppendUint32(inst, uint32(len(blobs[f.Name])-9))
	}
	instBlob := blteN(inst)
	instKey := md5hex(instBlob)
	put("data", instKey, instBlob, "")

	// Encoding: one 4 KiB page with the install files and the root.
	page := make([]byte, 0, 4096)
	var rootCKey string
	var ce []File
	for _, f := range p.Files {
		if f.Install || f.Root {
			ce = append(ce, f)
		}
		if f.Root {
			rootCKey = ckeys[f.Name]
		}
	}
	slices.SortFunc(ce, func(a, b File) int { return strings.Compare(ckeys[a.Name], ckeys[b.Name]) })
	for _, f := range ce {
		page = append(page, 1, 0)
		page = binary.BigEndian.AppendUint32(page, uint32(len(blobs[f.Name])-9))
		page = append(page, keyBytes(ckeys[f.Name])...)
		page = append(page, keyBytes(b.ekeys[f.Name])...)
	}
	page = append(page, make([]byte, 4096-len(page))...)
	espec := []byte("n\x00z\x00")
	enc := []byte{'E', 'N', 1, 16, 16, 0, 4, 0, 4}
	enc = binary.BigEndian.AppendUint32(enc, 1)
	enc = binary.BigEndian.AppendUint32(enc, 0)
	enc = append(enc, 0)
	enc = binary.BigEndian.AppendUint32(enc, uint32(len(espec)))
	enc = append(enc, espec...)
	enc = append(enc, make([]byte, 32)...) // the page index
	enc = append(enc, page...)
	encBlob := blteN(enc)
	encKey := md5hex(encBlob)
	put("data", encKey, encBlob, "")

	sizeBlob := blteN([]byte("DS" + p.Version))
	sizeKey := md5hex(sizeBlob)
	put("data", sizeKey, sizeBlob, "")
	vfsBlob := blteN([]byte("TVFS" + p.Version))
	vfsKey := md5hex(vfsBlob)
	put("data", vfsKey, vfsBlob, "")

	if rootCKey == "" {
		rootCKey = strings.Repeat("0", 32)
	}
	bc := fmt.Sprintf("# Build Configuration\n\nroot = %s\ninstall = %s %s\ninstall-size = %d %d\ndownload = %s %s\ndownload-size = %d %d\n"+
		"size = %s %s\nsize-size = %d %d\nencoding = %s %s\nencoding-size = %d %d\nvfs-root = %s %s\nvfs-root-size = %d %d\nbuild-name = %s\n",
		rootCKey, md5hex(inst), instKey, len(inst), len(instBlob), md5hex(dl), dlKey, len(dl), len(dlBlob),
		md5hex(sizeBlob[9:]), sizeKey, len(sizeBlob)-9, len(sizeBlob), md5hex(enc), encKey, len(enc), len(encBlob),
		md5hex(vfsBlob[9:]), vfsKey, len(vfsBlob)-9, len(vfsBlob), p.Version)
	bcData := []byte(bc)
	if p.EncryptedBuild {
		bcData = []byte("\x8a\x01binary")
	}
	b.build = md5hex(bcData)
	put("config", b.build, bcData, "")
	cc := fmt.Sprintf("# CDN Configuration\narchives = %s\narchives-index-size = 1 1\nfile-index = %s\nfile-index-size = 1\npatch-archives = %s\npatch-file-index = %s\n",
		strings.Join(b.archives, " "), fileIndex, patchArchive, patchFileIndex)
	b.cdn = md5hex([]byte(cc))
	put("config", b.cdn, []byte(cc), "")
	pcJSON := `{"all":{"config":{"product":"` + p.Code + `","supported_locales":["enUS","deDE"],"replacement_locales":{"enGB":"enUS"}}},` +
		`"platform":{"win":{"config":{"tags":["Windows"],"tags_64bit":["x86_64"],"tags_32bit":["x86_32"]}},"mac":{"config":{"tags":["OSX"]}}}}`
	b.productConf = md5hex([]byte(pcJSON + p.Code))
	s.objects["/tpr/configs/data/"+hashPath(b.productConf)] = []byte(pcJSON)
	s.products[p.Code] = b
}

// publish writes the TACT channel files.
func (s *Server) publish() {
	type region struct {
		Branches []string `json:"branches"`
		CDNs     string   `json:"cdns"`
	}
	type build struct {
		Branches   []string `json:"branches"`
		Definition string   `json:"definition"`
	}
	type product struct {
		Product string   `json:"product"`
		Variant string   `json:"variant"`
		Builds  []build  `json:"builds"`
		Regions []region `json:"regions"`
	}
	cdns, _ := json.Marshal(map[string]any{"hosts": []any{
		map[string]string{"host": "http://level3.blizzard.com?maxhosts=4"},
		map[string]string{"host": "http://eu.cdn.blizzard.com?maxhosts=4"},
		map[string]string{"host": "https://level3.ssl.blizzard.com?maxhosts=4&fallback=1"},
	}})
	cdnHash := md5hex(cdns)
	s.channels[cdnHash] = cdns
	var ps []product
	for _, code := range slices.Sorted(maps.Keys(s.products)) {
		b := s.products[code]
		def, _ := json.Marshal(map[string]any{"armadilloKey": "", "buildKey": b.build, "cdnKey": b.cdn, "keyRing": "", "path": b.path,
			"productConfig": map[string]string{"cdnPath": "", "hash": b.productConf}, "versionString": b.p.Version})
		dh := md5hex(def)
		s.channels[dh] = def
		ps = append(ps, product{Product: code, Variant: code, Builds: []build{{Branches: []string{"eu", "kr", "us"}, Definition: dh}},
			Regions: []region{{Branches: []string{"eu", "us"}, CDNs: cdnHash}}})
	}
	cat, _ := json.Marshal(map[string]any{"json_version": 2, "name": "public", "products": ps})
	ch := md5hex(cat)
	s.channels[ch] = cat
	s.summary, _ = json.Marshal(map[string]any{"path": "blue/", "public": map[string]string{"channel": ch}})
}

func (s *Server) summaryH(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	off, b := s.noChan, s.summary
	s.mu.Unlock()
	if off {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Write(b)
}

func (s *Server) channelH(w http.ResponseWriter, r *http.Request) {
	h := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
	s.mu.Lock()
	b, ok := s.channels[h]
	off := s.noChan
	s.mu.Unlock()
	if !ok || off || r.URL.Path != "/blue/"+hashPath(h) {
		http.NotFound(w, r)
		return
	}
	w.Write(b)
}

func (s *Server) ribbitH(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.ribbit++
	b, ok := s.products[r.PathValue("p")]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch r.PathValue("what") {
	case "versions":
		fmt.Fprintf(w, "Region!STRING:0|BuildConfig!HEX:16|CDNConfig!HEX:16|KeyRing!HEX:16|BuildId!DEC:4|VersionsName!String:0|ProductConfig!HEX:16\n## seqn = 1\n")
		for _, reg := range []string{"us", "eu"} {
			fmt.Fprintf(w, "%s|%s|%s||1|%s|%s\n", reg, b.build, b.cdn, b.p.Version, b.productConf)
		}
	case "cdns":
		fmt.Fprintf(w, "Name!STRING:0|Path!STRING:0|Hosts!STRING:0|Servers!STRING:0|ConfigPath!STRING:0\n## seqn = 1\n")
		fmt.Fprintf(w, "eu|%s|level3.blizzard.com eu.cdn.blizzard.com|http://level3.blizzard.com/?maxhosts=4|tpr/configs/data\n", b.path)
		fmt.Fprintf(w, "us|%s|level3.blizzard.com us.cdn.blizzard.com|http://level3.blizzard.com/?maxhosts=4|tpr/configs/data\n", b.path)
	default:
		http.NotFound(w, r)
	}
}

// cdnH serves the CDN through the "cache": MISS then HIT, ranges as 206.
func (s *Server) cdnH(w http.ResponseWriter, r *http.Request) {
	rng := r.Header.Get("Range")
	s.mu.Lock()
	s.requests = append(s.requests, Request{Host: r.Host, Path: r.URL.Path, Range: rng})
	b, ok := s.objects[r.URL.Path]
	hit := s.hits[r.URL.Path+rng]
	s.hits[r.URL.Path+rng] = true
	s.mu.Unlock()
	w.Header().Set("X-LanCache-Processed-By", "fake-cache")
	if hit {
		w.Header().Set("X-Upstream-Cache-Status", "HIT")
	} else {
		w.Header().Set("X-Upstream-Cache-Status", "MISS")
	}
	if !ok || !strings.HasSuffix(r.Host, "blizzard.com") {
		http.NotFound(w, r)
		return
	}
	if rng == "" {
		w.Write(b)
		return
	}
	spec, ok := strings.CutPrefix(rng, "bytes=")
	los, his, ok2 := strings.Cut(spec, "-")
	lo, err1 := strconv.Atoi(los)
	hi, err2 := strconv.Atoi(his)
	if !ok || !ok2 || err1 != nil || err2 != nil || strings.Contains(spec, ",") || lo > hi || lo >= len(b) {
		http.Error(w, "range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	hi = min(hi, len(b)-1)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", lo, hi, len(b)))
	w.Header().Set("Content-Length", strconv.Itoa(hi-lo+1))
	w.WriteHeader(http.StatusPartialContent)
	w.Write(b[lo : hi+1])
}
