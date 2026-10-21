package battlenet

import (
	"encoding/binary"
	"fmt"
)

// An archive index (<archive>.index) lists the files in an archive: blocks
// of 4 KiB with records (key, size, offset; big-endian), then a table of
// contents and a 28-byte footer. The loose-file index has no offsets.

const indexFooter = 28

// ParseIndex calls fn for every record of an index; it returns the
// footer's offset size (4 for archives, 0 for the loose-file index).
func ParseIndex(b []byte, fn func(k *Key, size, offset uint64)) (int, error) {
	if len(b) < indexFooter {
		return 0, fmt.Errorf("battlenet: index of %d bytes", len(b))
	}
	f := b[len(b)-indexFooter:]
	version, blockKB, offBytes, sizeBytes, keyBytes, hashBytes := f[8], f[11], int(f[12]), int(f[13]), int(f[14]), int(f[15])
	if version != 1 || blockKB != 4 || sizeBytes != 4 || keyBytes != 16 || hashBytes != 8 ||
		(offBytes != 0 && offBytes != 4 && offBytes != 5 && offBytes != 6) {
		return 0, fmt.Errorf("battlenet: index footer (version %d, %d-byte offsets)", version, offBytes)
	}
	count := binary.LittleEndian.Uint32(f[16:])
	if count>>24 != 0 {
		count = binary.BigEndian.Uint32(f[16:])
	}
	const block = 4096
	per := block + keyBytes + hashBytes
	body := len(b) - indexFooter
	if body%per != 0 {
		return 0, fmt.Errorf("battlenet: index of %d bytes is not whole blocks", len(b))
	}
	blocks := body / per
	rec := keyBytes + sizeBytes + offBytes
	var seen uint32
	for bi := 0; bi < blocks && seen < count; bi++ {
		blk := b[bi*block : (bi+1)*block]
		for p := 0; p+rec <= block && seen < count; p += rec {
			k := (*Key)(blk[p : p+16])
			if k.IsZero() {
				break
			}
			size := uint64(binary.BigEndian.Uint32(blk[p+16:]))
			var off uint64
			for _, c := range blk[p+20 : p+rec] {
				off = off<<8 | uint64(c)
			}
			fn(k, size, off)
			seen++
		}
	}
	return offBytes, nil
}
