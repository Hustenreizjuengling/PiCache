package steam

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// A depot manifest (GET /depot/<depot>/manifest/<gid>/5/<request code>) is
// a ZIP archive with one file: sections of (uint32 magic, uint32 length,
// protobuf), ending with the end magic. Only the payload's chunk list is
// needed: each chunk is fetched as /depot/<depot>/chunk/<sha1 hex>. File
// names may be encrypted; they do not matter here.

const (
	magicPayload  = 0x71F617D0
	magicMetadata = 0x1F4812BE
	magicSign     = 0x1B81B817
	magicEnd      = 0x32C415AB

	// MaxManifest bounds a downloaded manifest.
	MaxManifest   = 64 << 20
	maxUnzipManif = 512 << 20
	maxChunks     = 4 << 20
)

// Chunk is one chunk of a depot: its SHA-1 and the size the CDN sends.
type Chunk struct {
	SHA        [20]byte
	Compressed uint32
}

// Path is the chunk's CDN path within depot.
func (c Chunk) Path(depot uint32) string {
	return fmt.Sprintf("/depot/%d/chunk/%s", depot, hex.EncodeToString(c.SHA[:]))
}

// Manifest is what a prefill needs of a depot manifest.
type Manifest struct {
	Depot  uint32
	GID    uint64
	Chunks []Chunk // unique, in manifest order
	// Bytes is the sum of the chunks' compressed sizes.
	Bytes int64
}

// ManifestPath is the CDN path of a manifest.
func ManifestPath(depot uint32, gid, requestCode uint64) string {
	if requestCode == 0 {
		return fmt.Sprintf("/depot/%d/manifest/%d/5", depot, gid)
	}
	return fmt.Sprintf("/depot/%d/manifest/%d/5/%d", depot, gid, requestCode)
}

var errManifest = errors.New("steam: malformed depot manifest")

// ParseManifest reads a downloaded manifest (the ZIP archive).
func ParseManifest(b []byte) (*Manifest, error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, fmt.Errorf("steam: depot manifest: %w", err)
	}
	if len(zr.File) != 1 {
		return nil, fmt.Errorf("%w: %d files in the archive", errManifest, len(zr.File))
	}
	f := zr.File[0]
	if f.UncompressedSize64 > maxUnzipManif {
		return nil, fmt.Errorf("%w: %d bytes uncompressed", errManifest, f.UncompressedSize64)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("steam: depot manifest: %w", err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, maxUnzipManif+1))
	if err != nil {
		return nil, fmt.Errorf("steam: depot manifest: %w", err)
	}
	if len(raw) > maxUnzipManif {
		return nil, errManifest
	}
	return parseManifestSections(raw)
}

func parseManifestSections(raw []byte) (*Manifest, error) {
	m := &Manifest{}
	var payload []byte
	for {
		if len(raw) < 4 {
			return nil, fmt.Errorf("%w: no end marker", errManifest)
		}
		magic := binary.LittleEndian.Uint32(raw)
		raw = raw[4:]
		if magic == magicEnd {
			break
		}
		if len(raw) < 4 {
			return nil, errManifest
		}
		n := binary.LittleEndian.Uint32(raw)
		raw = raw[4:]
		if uint64(n) > uint64(len(raw)) {
			return nil, errManifest
		}
		sec := raw[:n]
		raw = raw[n:]
		switch magic {
		case magicPayload:
			payload = sec
		case magicMetadata:
			for f, err := range fields(sec) {
				if err != nil {
					return nil, err
				}
				switch f.num {
				case 1:
					m.Depot = uint32(f.n)
				case 2:
					m.GID = f.n
				}
			}
		case magicSign:
		default:
			return nil, fmt.Errorf("%w: section %#x", errManifest, magic)
		}
	}
	if payload == nil {
		return nil, fmt.Errorf("%w: no payload", errManifest)
	}
	seen := map[[20]byte]bool{}
	for f, err := range fields(payload) {
		if err != nil {
			return nil, err
		}
		if f.num != 1 || f.wire != wireBytes {
			continue
		}
		for g, err := range fields(f.b) {
			if err != nil {
				return nil, err
			}
			if g.num != 6 || g.wire != wireBytes {
				continue
			}
			var c Chunk
			var shaOK bool
			for h, err := range fields(g.b) {
				if err != nil {
					return nil, err
				}
				switch h.num {
				case 1:
					if len(h.b) == 20 {
						copy(c.SHA[:], h.b)
						shaOK = true
					}
				case 5:
					c.Compressed = uint32(h.n)
				}
			}
			if !shaOK || seen[c.SHA] {
				continue
			}
			if len(m.Chunks) >= maxChunks {
				return nil, fmt.Errorf("%w: more than %d chunks", errManifest, maxChunks)
			}
			seen[c.SHA] = true
			m.Chunks = append(m.Chunks, c)
			m.Bytes += int64(c.Compressed)
		}
	}
	return m, nil
}
