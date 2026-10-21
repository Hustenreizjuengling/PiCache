package epic

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"slices"
	"strconv"
	"strings"
)

// An Epic build manifest (binary, magic 0x44BEC00C; older builds JSON):
// a header, then (zlib-compressed) sections, each starting with its size:
// the meta (with the feature level), the chunk data list (column by column:
// GUIDs, hashes, SHA-1s, group numbers, window sizes, file sizes), the file
// list and custom fields. A prefill needs the chunk list only; it is never
// encrypted, also in the manifests whose file names are.

const (
	manifestMagic   = 0x44BEC00C
	maxManifest     = 64 << 20
	maxDecompressed = 512 << 20
	maxChunks       = 4 << 20
)

// Chunk is one chunk of a build.
type Chunk struct {
	GUID       [4]uint32
	Hash       uint64
	Group      uint8
	FileSize   int64 // the download size
	SecretGUID [4]uint32
}

// Manifest is what a prefill needs of a build manifest.
type Manifest struct {
	FeatureLevel uint32
	Chunks       []Chunk // unique by GUID
	// Bytes is the sum of the chunks' download sizes.
	Bytes int64
}

var errManifest = errors.New("epic: malformed manifest")

// ParseManifest reads a downloaded manifest; want, if not empty, is the
// SHA-1 (hex) the manifest API gave for the file.
func ParseManifest(b []byte, want string) (*Manifest, error) {
	if len(b) > maxManifest {
		return nil, fmt.Errorf("%w: %d bytes", errManifest, len(b))
	}
	if want != "" {
		sum := sha1.Sum(b)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), want) {
			return nil, errors.New("epic: the manifest does not match its hash")
		}
	}
	if t := bytes.TrimLeft(b, " \t\r\n\xef\xbb\xbf"); len(t) > 0 && t[0] == '{' {
		return parseJSONManifest(t)
	}
	return parseBinaryManifest(b)
}

func parseBinaryManifest(b []byte) (*Manifest, error) {
	if len(b) < 41 || binary.LittleEndian.Uint32(b) != manifestMagic {
		return nil, fmt.Errorf("%w: no manifest header", errManifest)
	}
	headerSize := binary.LittleEndian.Uint32(b[4:])
	storedAs := b[36]
	if headerSize < 41 || uint64(headerSize) > uint64(len(b)) {
		return nil, fmt.Errorf("%w: header", errManifest)
	}
	// Only the meta and the chunk data list are read, straight from the
	// (decompressed) stream (also of manifests whose file names are
	// encrypted, storedAs bit 2: their chunk list is not): the file list after them, the bulk of a large
	// game's manifest, is never held in memory. The whole file was checked
	// against the manifest API's SHA-1 before.
	var src io.Reader = bytes.NewReader(b[headerSize:])
	if storedAs&1 != 0 {
		zr, err := zlib.NewReader(src)
		if err != nil {
			return nil, fmt.Errorf("epic: manifest: %w", err)
		}
		defer zr.Close()
		src = io.LimitReader(zr, maxDecompressed)
	}
	section := func(what string, limit uint32) ([]byte, error) {
		var sz [4]byte
		if _, err := io.ReadFull(src, sz[:]); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", errManifest, what, err)
		}
		n := binary.LittleEndian.Uint32(sz[:])
		if n < 9 || n > limit {
			return nil, fmt.Errorf("%w: %s of %d bytes", errManifest, what, n)
		}
		buf := make([]byte, n)
		copy(buf, sz[:])
		if _, err := io.ReadFull(src, buf[4:]); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", errManifest, what, err)
		}
		return buf, nil
	}
	// Meta: size, data version, feature level; the rest is skipped.
	meta, err := section("meta", 16<<20)
	if err != nil {
		return nil, err
	}
	mr := &reader{b: meta, off: 4}
	mr.u8()
	m := &Manifest{FeatureLevel: mr.u32()}
	if mr.err != nil {
		return nil, fmt.Errorf("%w: meta", errManifest)
	}
	// Chunk data list.
	cdl, err := section("chunk list", maxDecompressed)
	if err != nil {
		return nil, err
	}
	r := &reader{b: cdl, off: 4}
	r.u8()
	count := r.u32()
	cdlSize := uint32(len(cdl))
	if r.err != nil {
		return nil, fmt.Errorf("%w: chunk list", errManifest)
	}
	per := uint64(16 + 8 + 20 + 1 + 4 + 8)
	if m.FeatureLevel >= 22 {
		per += 16 + 4 + 16
	}
	if count > maxChunks || uint64(count)*per > uint64(cdlSize) {
		return nil, fmt.Errorf("%w: %d chunks", errManifest, count)
	}
	n := int(count)
	chunks := make([]Chunk, n)
	for i := range chunks {
		for j := range 4 {
			chunks[i].GUID[j] = r.u32()
		}
	}
	for i := range chunks {
		chunks[i].Hash = r.u64()
	}
	r.skip(20 * n) // SHA-1s
	for i := range chunks {
		chunks[i].Group = r.u8()
	}
	r.skip(4 * n) // window sizes
	for i := range chunks {
		chunks[i].FileSize = int64(r.u64())
	}
	if m.FeatureLevel >= 22 {
		for i := range chunks {
			for j := range 4 {
				chunks[i].SecretGUID[j] = r.u32()
			}
		}
	}
	if r.err != nil {
		return nil, fmt.Errorf("%w: chunk list", errManifest)
	}
	return m.add(chunks)
}

// add keeps the chunks unique by GUID and sums their sizes.
func (m *Manifest) add(chunks []Chunk) (*Manifest, error) {
	seen := make(map[[4]uint32]bool, len(chunks))
	for _, c := range chunks {
		if c.FileSize < 0 {
			return nil, fmt.Errorf("%w: a negative chunk size", errManifest)
		}
		if seen[c.GUID] {
			continue
		}
		seen[c.GUID] = true
		m.Chunks = append(m.Chunks, c)
		m.Bytes += c.FileSize
	}
	return m, nil
}

// reader reads little-endian values; a read past the end sets err.
type reader struct {
	b   []byte
	off int
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil || n < 0 || r.off+n > len(r.b) {
		r.err = errManifest
		return nil
	}
	v := r.b[r.off : r.off+n]
	r.off += n
	return v
}

func (r *reader) skip(n int) { r.take(n) }

func (r *reader) u8() uint8 {
	if v := r.take(1); v != nil {
		return v[0]
	}
	return 0
}

func (r *reader) u32() uint32 {
	if v := r.take(4); v != nil {
		return binary.LittleEndian.Uint32(v)
	}
	return 0
}

func (r *reader) u64() uint64 {
	if v := r.take(8); v != nil {
		return binary.LittleEndian.Uint64(v)
	}
	return 0
}

// parseJSONManifest reads the JSON manifests of older builds: the chunk
// lists are maps from the GUID (32 hex digits) to "blobs" (3-digit decimal
// bytes, little endian).
func parseJSONManifest(b []byte) (*Manifest, error) {
	var j struct {
		ManifestFileVersion string            `json:"ManifestFileVersion"`
		ChunkHashList       map[string]string `json:"ChunkHashList"`
		ChunkFilesizeList   map[string]string `json:"ChunkFilesizeList"`
		DataGroupList       map[string]string `json:"DataGroupList"`
	}
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("epic: JSON manifest: %w", err)
	}
	fl := uint64(13)
	if j.ManifestFileVersion != "" {
		v, err := blobToNum(j.ManifestFileVersion)
		if err != nil {
			return nil, err
		}
		fl = v
	}
	if len(j.ChunkHashList) > maxChunks {
		return nil, errManifest
	}
	m := &Manifest{FeatureLevel: uint32(min(fl, 1<<20))}
	var chunks []Chunk
	for key, hb := range j.ChunkHashList {
		g, err := guidFromHex(key)
		if err != nil {
			return nil, err
		}
		h, err := blobToNum(hb)
		if err != nil {
			return nil, err
		}
		var size uint64
		if s, ok := j.ChunkFilesizeList[key]; ok {
			if size, err = blobToNum(s); err != nil {
				return nil, err
			}
		}
		c := Chunk{GUID: g, Hash: h, FileSize: int64(min(size, 1<<62))}
		if s, ok := j.DataGroupList[key]; ok {
			v, err := blobToNum(s)
			if err != nil {
				return nil, err
			}
			c.Group = uint8(v % 100)
		} else {
			c.Group = defaultGroup(g)
		}
		chunks = append(chunks, c)
	}
	// Map order is random: a stable order (by GUID) for the requests.
	sortChunks(chunks)
	return m.add(chunks)
}

func sortChunks(cs []Chunk) {
	less := func(a, b Chunk) int {
		for i := range 4 {
			if a.GUID[i] != b.GUID[i] {
				if a.GUID[i] < b.GUID[i] {
					return -1
				}
				return 1
			}
		}
		return 0
	}
	slices.SortFunc(cs, less)
}

// blobToNum decodes a JSON manifest number: 3-digit decimal bytes, the
// least significant first.
func blobToNum(s string) (uint64, error) {
	if len(s)%3 != 0 || len(s) > 24 {
		return 0, fmt.Errorf("%w: blob %q", errManifest, s)
	}
	var n uint64
	for i := 0; i < len(s); i += 3 {
		v, err := strconv.ParseUint(s[i:i+3], 10, 16)
		if err != nil || v > 255 {
			return 0, fmt.Errorf("%w: blob %q", errManifest, s)
		}
		n |= v << (8 * (i / 3))
	}
	return n, nil
}

func guidFromHex(s string) ([4]uint32, error) {
	var g [4]uint32
	if len(s) != 32 {
		return g, fmt.Errorf("%w: GUID %q", errManifest, s)
	}
	for i := range 4 {
		v, err := strconv.ParseUint(s[i*8:i*8+8], 16, 32)
		if err != nil {
			return g, fmt.Errorf("%w: GUID %q", errManifest, s)
		}
		g[i] = uint32(v)
	}
	return g, nil
}

func guidBytes(g [4]uint32) []byte {
	b := make([]byte, 16)
	for i, v := range g {
		binary.LittleEndian.PutUint32(b[i*4:], v)
	}
	return b
}

// defaultGroup is the group of a chunk that names none: CRC-32 of the
// GUID's bytes modulo 100.
func defaultGroup(g [4]uint32) uint8 {
	return uint8(crc32.ChecksumIEEE(guidBytes(g)) % 100)
}

// Path is the chunk's path below the build's base URL.
func (c Chunk) Path(featureLevel uint32) string {
	if featureLevel >= 22 {
		secret := "plain"
		if c.SecretGUID != ([4]uint32{}) {
			secret = base64.RawURLEncoding.EncodeToString(guidBytes(c.SecretGUID))
		}
		var h [8]byte
		binary.LittleEndian.PutUint64(h[:], c.Hash)
		return fmt.Sprintf("ChunksV5/%s/%02d/%s_%s.chunk", secret, c.Group,
			base64.RawURLEncoding.EncodeToString(h[:]), base64.RawURLEncoding.EncodeToString(guidBytes(c.GUID)))
	}
	dir := "Chunks"
	switch {
	case featureLevel >= 15:
		dir = "ChunksV4"
	case featureLevel >= 6:
		dir = "ChunksV3"
	case featureLevel >= 3:
		dir = "ChunksV2"
	}
	return fmt.Sprintf("%s/%02d/%016X_%08X%08X%08X%08X.chunk", dir, c.Group, c.Hash, c.GUID[0], c.GUID[1], c.GUID[2], c.GUID[3])
}
