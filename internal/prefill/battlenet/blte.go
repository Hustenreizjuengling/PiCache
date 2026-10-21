package battlenet

import (
	"bytes"
	"compress/zlib"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
)

// BLTE is the container of every file on the CDN: a header with a table
// of blocks, each block a mode byte and its data (N raw, Z zlib, F nested
// BLTE, E encrypted, 4 LZ4). The prefill decodes manifests only; archive
// data is never decoded.

var (
	// ErrEncrypted: the data is encrypted (the product cannot be
	// prefilled without its keys).
	ErrEncrypted = errors.New("battlenet: the data is encrypted")
	errBLTE      = errors.New("battlenet: malformed BLTE")
)

const maxBLTEBlocks = 1 << 20

type blteBlock struct {
	enc, dec uint32
	sum      [16]byte
}

type blteReader struct {
	src    io.Reader
	ekey   Key
	blocks []blteBlock
	single bool // no block table: one block to the end
	whole  hash.Hash
	next   int
	raw    *io.LimitedReader
	h      hash.Hash
	cur    io.Reader
	out    int64
	err    error
}

// NewBLTEReader decodes a BLTE stream. ekey, if not zero, is checked
// against the header (or the whole file without a block table); every
// block is checked against its MD5.
func NewBLTEReader(r io.Reader, ekey Key) (io.Reader, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("%w: %v", errBLTE, err)
	}
	if string(hdr[:4]) != "BLTE" {
		return nil, fmt.Errorf("%w: no BLTE magic", errBLTE)
	}
	b := &blteReader{src: r, ekey: ekey}
	size := binary.BigEndian.Uint32(hdr[4:])
	if size == 0 {
		b.single = true
		b.whole = md5.New()
		b.whole.Write(hdr[:])
		b.src = io.TeeReader(r, b.whole)
		return b, nil
	}
	if size < 12 || size > 12+40*maxBLTEBlocks {
		return nil, fmt.Errorf("%w: header size %d", errBLTE, size)
	}
	rest := make([]byte, size-8)
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, fmt.Errorf("%w: %v", errBLTE, err)
	}
	if !ekey.IsZero() {
		h := md5.New()
		h.Write(hdr[:])
		h.Write(rest)
		if !bytes.Equal(h.Sum(nil), ekey[:]) {
			return nil, fmt.Errorf("battlenet: BLTE header does not match the key %s", ekey)
		}
	}
	format := rest[0]
	n := int(rest[1])<<16 | int(rest[2])<<8 | int(rest[3])
	entry := 24
	switch format {
	case 0x0F:
	case 0x10:
		entry = 40
	default:
		return nil, fmt.Errorf("%w: table format %#x", errBLTE, format)
	}
	if n == 0 || int(size) != 12+n*entry {
		return nil, fmt.Errorf("%w: %d blocks in a %d-byte header", errBLTE, n, size)
	}
	b.blocks = make([]blteBlock, n)
	for i := range b.blocks {
		e := rest[4+i*entry:]
		b.blocks[i].enc = binary.BigEndian.Uint32(e)
		b.blocks[i].dec = binary.BigEndian.Uint32(e[4:])
		copy(b.blocks[i].sum[:], e[8:24])
		if b.blocks[i].enc == 0 {
			return nil, fmt.Errorf("%w: an empty block", errBLTE)
		}
	}
	return b, nil
}

func (b *blteReader) Read(p []byte) (int, error) {
	for {
		if b.err != nil {
			return 0, b.err
		}
		if b.cur == nil {
			if err := b.open(); err != nil {
				b.err = err
				return 0, err
			}
		}
		n, err := b.cur.Read(p)
		b.out += int64(n)
		if err == io.EOF {
			if ferr := b.finish(); ferr != nil {
				b.err = ferr
				return n, ferr
			}
			b.cur = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		if err != nil {
			b.err = err
		}
		return n, err
	}
}

// open starts the next block.
func (b *blteReader) open() error {
	limit := int64(math.MaxInt64)
	if b.single {
		if b.next > 0 {
			return io.EOF
		}
	} else {
		if b.next == len(b.blocks) {
			return io.EOF
		}
		limit = int64(b.blocks[b.next].enc)
	}
	b.h = md5.New()
	b.raw = &io.LimitedReader{R: io.TeeReader(b.src, b.h), N: limit}
	b.out = 0
	b.next++
	var mode [1]byte
	if _, err := io.ReadFull(b.raw, mode[:]); err != nil {
		return fmt.Errorf("%w: %v", errBLTE, err)
	}
	switch mode[0] {
	case 'N':
		b.cur = b.raw
	case 'Z':
		zr, err := zlib.NewReader(b.raw)
		if err != nil {
			return fmt.Errorf("battlenet: BLTE block: %w", err)
		}
		b.cur = zr
	case 'F':
		nr, err := NewBLTEReader(b.raw, Key{})
		if err != nil {
			return err
		}
		b.cur = nr
	case 'E':
		return ErrEncrypted
	default:
		return fmt.Errorf("%w: block mode %q is not supported", errBLTE, mode[0])
	}
	return nil
}

// finish checks a block that was read to its end.
func (b *blteReader) finish() error {
	if _, err := io.Copy(io.Discard, b.raw); err != nil {
		return err
	}
	if b.single {
		if !b.ekey.IsZero() && !bytes.Equal(b.whole.Sum(nil), b.ekey[:]) {
			return fmt.Errorf("battlenet: BLTE file does not match the key %s", b.ekey)
		}
		return nil
	}
	blk := b.blocks[b.next-1]
	if b.raw.N != 0 {
		return fmt.Errorf("%w: %v", errBLTE, io.ErrUnexpectedEOF)
	}
	if !bytes.Equal(b.h.Sum(nil), blk.sum[:]) {
		return fmt.Errorf("%w: block %d does not match its checksum", errBLTE, b.next-1)
	}
	if b.out != int64(blk.dec) {
		return fmt.Errorf("%w: block %d decoded to %d bytes, not %d", errBLTE, b.next-1, b.out, blk.dec)
	}
	return nil
}
