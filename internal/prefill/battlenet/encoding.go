package battlenet

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// The encoding file maps content keys to encoding keys (the CDN names).
// The prefill needs it for the install manifest's files and the root;
// the download manifest names encoding keys itself.

const (
	maxEncodingPages = 1 << 22
	maxESpecBlock    = 64 << 20
)

// ScanEncoding reads a (decoded) encoding file and returns the first
// encoding key of every content key in need. It stops after the
// content-key pages; the caller drains the rest.
func ScanEncoding(r io.Reader, need map[Key]bool) (map[Key]Key, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var h [22]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		return nil, fmt.Errorf("battlenet: encoding header: %w", err)
	}
	if string(h[:2]) != "EN" || h[2] != 1 || h[3] != 16 || h[4] != 16 {
		return nil, fmt.Errorf("battlenet: not an encoding file (version %d)", h[2])
	}
	pageSize := int(binary.BigEndian.Uint16(h[5:])) * 1024
	pages := binary.BigEndian.Uint32(h[9:])
	espec := binary.BigEndian.Uint32(h[18:])
	if pageSize == 0 || pageSize > 1<<20 || pages > maxEncodingPages || espec > maxESpecBlock {
		return nil, fmt.Errorf("battlenet: encoding header: page size %d, %d pages", pageSize, pages)
	}
	if _, err := io.CopyN(io.Discard, br, int64(espec)+int64(pages)*32); err != nil {
		return nil, fmt.Errorf("battlenet: encoding: %w", err)
	}
	out := make(map[Key]Key, len(need))
	page := make([]byte, pageSize)
	for range pages {
		if len(out) == len(need) {
			break
		}
		if _, err := io.ReadFull(br, page); err != nil {
			return nil, fmt.Errorf("battlenet: encoding page: %w", err)
		}
		for p := 0; p+22 <= len(page); {
			n := int(page[p])
			if n == 0 {
				break
			}
			end := p + 22 + 16*n
			if end > len(page) {
				return nil, fmt.Errorf("battlenet: encoding page entry crosses the page")
			}
			var ck Key
			copy(ck[:], page[p+6:p+22])
			if need[ck] {
				if _, done := out[ck]; !done {
					var ek Key
					copy(ek[:], page[p+22:p+38])
					out[ck] = ek
				}
			}
			p = end
		}
	}
	return out, nil
}
