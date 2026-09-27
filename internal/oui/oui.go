// Package oui names the vendor of a MAC address from the IEEE registries
// MA-L (24-bit prefixes), MA-M (28 bits) and MA-S (36 bits), embedded as
// oui.bin (docs/ARCHITECTURE.md 4). It imports no PiCache package.
//
// The table is searched directly in the embedded bytes (binary search per
// registry, the longest prefix first); nothing is decoded into maps or
// slices at start. oui.bin is written by the generator gen/main.go
// (`make oui`, scripts/oui-update.sh), which shares the layout constants
// below and cleans the vendor names like other untrusted names (invalid
// UTF-8 and the Unicode categories Cc and Cf removed, white space
// collapsed, at most 100 characters).
package oui

import (
	_ "embed"
	"encoding/binary"
	"sort"
	"sync"
)

//go:embed oui.bin
var table []byte

// Layout of oui.bin (private to this package and its generator):
//
//	header    HeaderSize bytes: Magic, then big-endian uint32 counts of the
//	          MA-L, MA-M and MA-S records and the length of the name table
//	MA-L      CountL records of KeyL+OffsetSize bytes
//	MA-M      CountM records of KeyM+OffsetSize bytes
//	MA-S      CountS records of KeyS+OffsetSize bytes
//	names     the de-duplicated vendor names, each a big-endian uint16
//	          length and that many bytes of UTF-8
//
// A record is the prefix (the top 24, 28 or 36 bits of the address as a
// big-endian number of KeyL, KeyM or KeyS bytes) and the offset of its
// name in the name table (big-endian, OffsetSize bytes). The records of
// each registry are sorted by prefix, without duplicates.
const (
	Magic      = "PCOUI\x00\x01\x00" // format version 1
	HeaderSize = len(Magic) + 16
	OffsetSize = 3
	KeyL       = 3 // 24 bits
	KeyM       = 4 // 28 bits
	KeyS       = 5 // 36 bits
	BitsL      = 24
	BitsM      = 28
	BitsS      = 36
	// MaxSize bounds oui.bin (a test fails beyond it).
	MaxSize = 2 << 20
	// MaxNameLen is the longest vendor name in characters.
	MaxNameLen = 100
)

// registry is one sorted record array of the table.
type registry struct {
	recs    []byte
	keySize int
	bits    uint
}

// parsed is the view of the table (slices of the embedded bytes).
type parsed struct {
	regs  [3]registry // MA-S, MA-M, MA-L: the longest prefix first
	names []byte
}

var (
	once sync.Once
	view *parsed // nil: the table is empty or malformed
)

// load checks the header and slices the embedded table (no copy).
func load(b []byte) *parsed {
	if len(b) < HeaderSize || string(b[:len(Magic)]) != Magic {
		return nil
	}
	h := b[len(Magic):]
	nL, nM, nS := int(binary.BigEndian.Uint32(h)), int(binary.BigEndian.Uint32(h[4:])), int(binary.BigEndian.Uint32(h[8:]))
	nNames := int(binary.BigEndian.Uint32(h[12:]))
	sizes := []int{nL * (KeyL + OffsetSize), nM * (KeyM + OffsetSize), nS * (KeyS + OffsetSize)}
	total := HeaderSize + sizes[0] + sizes[1] + sizes[2] + nNames
	if nL < 0 || nM < 0 || nS < 0 || nNames < 0 || total != len(b) {
		return nil
	}
	p := &parsed{}
	off := HeaderSize
	cut := func(n int) []byte { s := b[off : off+n : off+n]; off += n; return s }
	l, m, s := cut(sizes[0]), cut(sizes[1]), cut(sizes[2])
	p.regs = [3]registry{{s, KeyS, BitsS}, {m, KeyM, BitsM}, {l, KeyL, BitsL}}
	p.names = cut(nNames)
	return p
}

// Lookup returns the vendor of a MAC address (six octets as hex pairs
// separated by ':' or '-', or twelve hex digits) and whether it is a
// locally administered ("randomised") address: bit 0x02 of the first
// octet. A locally administered or group (0x01) address never gets a
// vendor; an address that does not parse gives ("", false).
func Lookup(mac string) (vendor string, randomized bool) {
	addr, ok := parseMAC(mac)
	if !ok {
		return "", false
	}
	first := byte(addr >> 40)
	randomized = first&0x02 != 0
	if first&0x03 != 0 {
		return "", randomized
	}
	once.Do(func() { view = load(table) })
	if view == nil {
		return "", false
	}
	return view.lookup(addr), false
}

// lookup finds the longest registered prefix of addr (48 bits).
func (p *parsed) lookup(addr uint64) string {
	for _, r := range p.regs {
		size := r.keySize + OffsetSize
		n := len(r.recs) / size
		key := addr >> (48 - r.bits)
		i := sort.Search(n, func(i int) bool { return r.key(i) >= key })
		if i < n && r.key(i) == key {
			rec := r.recs[i*size+r.keySize : (i+1)*size]
			return p.name(int(rec[0])<<16 | int(rec[1])<<8 | int(rec[2]))
		}
	}
	return ""
}

// key returns the prefix of record i.
func (r registry) key(i int) uint64 {
	rec := r.recs[i*(r.keySize+OffsetSize):]
	var k uint64
	for j := range r.keySize {
		k = k<<8 | uint64(rec[j])
	}
	return k
}

// name returns the vendor name at off in the name table ("" when the
// offset is out of range).
func (p *parsed) name(off int) string {
	if off+2 > len(p.names) {
		return ""
	}
	n := int(binary.BigEndian.Uint16(p.names[off:]))
	if off+2+n > len(p.names) {
		return ""
	}
	return string(p.names[off+2 : off+2+n])
}

// parseMAC parses a 48-bit MAC address: "aa:bb:cc:dd:ee:ff",
// "aa-bb-cc-dd-ee-ff" or "aabbccddeeff" (either case).
func parseMAC(s string) (uint64, bool) {
	var v uint64
	digits := 0
	var sep byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		case (c == ':' || c == '-') && digits%2 == 0 && digits > 0 && digits < 12:
			// A separator only between two octets, the same everywhere.
			if sep == 0 {
				sep = c
			}
			if c != sep || s[i-1] == sep {
				return 0, false
			}
			continue
		default:
			return 0, false
		}
		if digits == 12 {
			return 0, false
		}
		v = v<<4 | uint64(d)
		digits++
	}
	if digits != 12 || (sep != 0 && len(s) != 17) {
		return 0, false
	}
	return v, true
}
