package battlenet

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// The download manifest lists every file of a build (encoding key and
// size) and the tags (platform, architecture, locale, region, …) that
// choose them; the install manifest lists the files outside the game's
// data store by content key, with the same tags.

const (
	maxManifestEntries = 16 << 20
	maxTags            = 4096
	maxTagName         = 256
)

var errManifest = errors.New("battlenet: malformed manifest")

// Tag is a manifest tag: the entries it marks (bit i, most significant
// bit first).
type Tag struct {
	Name string
	Type uint16
	Mask []byte
}

// Has reports whether entry i has the tag.
func (t *Tag) Has(i int) bool { return t.Mask[i>>3]&(0x80>>(i&7)) != 0 }

// Download is a download manifest.
type Download struct {
	N     int
	Keys  []Key
	Sizes []uint64 // encoded sizes: the bytes on the CDN
	Tags  []Tag
}

func readU40(b []byte) uint64 {
	return uint64(b[0])<<32 | uint64(binary.BigEndian.Uint32(b[1:]))
}

// ParseDownload reads a (decoded) download manifest.
func ParseDownload(r io.Reader) (*Download, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var h [16]byte
	if _, err := io.ReadFull(br, h[:11]); err != nil {
		return nil, fmt.Errorf("%w: %v", errManifest, err)
	}
	if string(h[:2]) != "DL" || h[2] < 1 || h[2] > 3 || h[3] != 16 {
		return nil, fmt.Errorf("%w: not a download manifest (version %d)", errManifest, h[2])
	}
	version, checksum := h[2], h[4] != 0
	n := binary.BigEndian.Uint32(h[5:])
	tags := int(binary.BigEndian.Uint16(h[9:]))
	flagBytes := 0
	switch version {
	case 2:
		if _, err := io.ReadFull(br, h[11:12]); err != nil {
			return nil, fmt.Errorf("%w: %v", errManifest, err)
		}
		flagBytes = int(h[11])
	case 3:
		if _, err := io.ReadFull(br, h[11:16]); err != nil {
			return nil, fmt.Errorf("%w: %v", errManifest, err)
		}
		flagBytes = int(h[11])
	}
	if n > maxManifestEntries || tags > maxTags || flagBytes > 8 {
		return nil, fmt.Errorf("%w: %d entries, %d tags", errManifest, n, tags)
	}
	d := &Download{N: int(n), Keys: make([]Key, n), Sizes: make([]uint64, n)}
	size := 16 + 6 + flagBytes
	if checksum {
		size += 4
	}
	e := make([]byte, size)
	for i := range d.N {
		if _, err := io.ReadFull(br, e); err != nil {
			return nil, fmt.Errorf("%w: entry %d: %v", errManifest, i, err)
		}
		copy(d.Keys[i][:], e)
		d.Sizes[i] = readU40(e[16:])
	}
	var err error
	if d.Tags, err = readTags(br, tags, d.N); err != nil {
		return nil, err
	}
	return d, nil
}

func readTags(br *bufio.Reader, count, entries int) ([]Tag, error) {
	tags := make([]Tag, count)
	for i := range tags {
		name, err := br.ReadSlice(0)
		if err != nil || len(name) > maxTagName {
			return nil, fmt.Errorf("%w: tag %d", errManifest, i)
		}
		tags[i].Name = string(name[:len(name)-1])
		var t [2]byte
		if _, err := io.ReadFull(br, t[:]); err != nil {
			return nil, fmt.Errorf("%w: tag %d: %v", errManifest, i, err)
		}
		tags[i].Type = binary.BigEndian.Uint16(t[:])
		tags[i].Mask = make([]byte, (entries+7)/8)
		if _, err := io.ReadFull(br, tags[i].Mask); err != nil {
			return nil, fmt.Errorf("%w: tag %d: %v", errManifest, i, err)
		}
	}
	return tags, nil
}

// Install is an install manifest.
type Install struct {
	N     int
	CKeys []Key
	Tags  []Tag
}

// ParseInstall reads a (decoded) install manifest.
func ParseInstall(r io.Reader) (*Install, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var h [10]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		return nil, fmt.Errorf("%w: %v", errManifest, err)
	}
	if string(h[:2]) != "IN" || h[2] != 1 || h[3] != 16 {
		return nil, fmt.Errorf("%w: not a version 1 install manifest (version %d)", errManifest, h[2])
	}
	tags := int(binary.BigEndian.Uint16(h[4:]))
	n := binary.BigEndian.Uint32(h[6:])
	if n > maxManifestEntries || tags > maxTags {
		return nil, fmt.Errorf("%w: %d entries, %d tags", errManifest, n, tags)
	}
	in := &Install{N: int(n), CKeys: make([]Key, n)}
	var err error
	if in.Tags, err = readTags(br, tags, in.N); err != nil {
		return nil, err
	}
	for i := range in.N {
		name, err := br.ReadSlice(0)
		if err != nil {
			return nil, fmt.Errorf("%w: install entry %d", errManifest, i)
		}
		_ = name
		var e [20]byte
		if _, err := io.ReadFull(br, e[:]); err != nil {
			return nil, fmt.Errorf("%w: install entry %d: %v", errManifest, i, err)
		}
		copy(in.CKeys[i][:], e[:16])
	}
	return in, nil
}
