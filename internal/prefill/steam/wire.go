package steam

import (
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"math"
)

// The Steam protocol carries protocol buffers. Only the few messages a
// prefill needs are encoded and decoded, by hand, so that every field that
// leaves this machine is visible in this package (wire format:
// https://protobuf.dev/programming-guides/encoding/).

// Wire types.
const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5
)

var errWire = errors.New("steam: malformed protobuf")

// pb builds one protobuf message. Zero values are written too: the caller
// leaves out what it does not want to send.
type pb struct{ b []byte }

func (e *pb) key(field, wire int) {
	e.b = binary.AppendUvarint(e.b, uint64(field)<<3|uint64(wire))
}

func (e *pb) uint(field int, v uint64) {
	e.key(field, wireVarint)
	e.b = binary.AppendUvarint(e.b, v)
}

// int32 encodes like protobuf's int32: a negative value takes ten bytes.
func (e *pb) int32(field int, v int32) { e.uint(field, uint64(int64(v))) }

func (e *pb) bool(field int, v bool) {
	var n uint64
	if v {
		n = 1
	}
	e.uint(field, n)
}

func (e *pb) fixed64(field int, v uint64) {
	e.key(field, wireFixed64)
	e.b = binary.LittleEndian.AppendUint64(e.b, v)
}

func (e *pb) fixed32(field int, v uint32) {
	e.key(field, wireFixed32)
	e.b = binary.LittleEndian.AppendUint32(e.b, v)
}

func (e *pb) bytes(field int, v []byte) {
	e.key(field, wireBytes)
	e.b = binary.AppendUvarint(e.b, uint64(len(v)))
	e.b = append(e.b, v...)
}

func (e *pb) string(field int, v string) { e.bytes(field, []byte(v)) }

// field is one decoded field: num and wire, the number for varints and
// fixed values, the payload for length-delimited ones (a sub-slice of the
// input).
type field struct {
	num  int
	wire int
	n    uint64
	b    []byte
}

func (f field) int32() int32     { return int32(f.n) }
func (f field) bool() bool       { return f.n != 0 }
func (f field) str() string      { return string(f.b) }
func (f field) float32() float32 { return math.Float32frombits(uint32(f.n)) }

// fields iterates over the fields of a message. Groups (wire types 3 and
// 4) are not used by Steam and end the iteration with an error.
func fields(b []byte) iter.Seq2[field, error] {
	return func(yield func(field, error) bool) {
		for len(b) > 0 {
			k, n := binary.Uvarint(b)
			if n <= 0 || k>>3 == 0 || k>>3 > math.MaxInt32 {
				yield(field{}, errWire)
				return
			}
			b = b[n:]
			f := field{num: int(k >> 3), wire: int(k & 7)}
			switch f.wire {
			case wireVarint:
				v, n := binary.Uvarint(b)
				if n <= 0 {
					yield(field{}, errWire)
					return
				}
				f.n, b = v, b[n:]
			case wireFixed64:
				if len(b) < 8 {
					yield(field{}, errWire)
					return
				}
				f.n, b = binary.LittleEndian.Uint64(b), b[8:]
			case wireFixed32:
				if len(b) < 4 {
					yield(field{}, errWire)
					return
				}
				f.n, b = uint64(binary.LittleEndian.Uint32(b)), b[4:]
			case wireBytes:
				l, n := binary.Uvarint(b)
				if n <= 0 || l > uint64(len(b)-n) {
					yield(field{}, errWire)
					return
				}
				f.b, b = b[n:n+int(l)], b[n+int(l):]
			default:
				yield(field{}, fmt.Errorf("%w: wire type %d", errWire, f.wire))
				return
			}
			if !yield(f, nil) {
				return
			}
		}
	}
}

// packedUints appends the values of a repeated varint field, packed or
// not, to dst.
func packedUints(dst []uint64, f field) ([]uint64, error) {
	if f.wire == wireVarint {
		return append(dst, f.n), nil
	}
	if f.wire != wireBytes {
		return dst, errWire
	}
	b := f.b
	for len(b) > 0 {
		v, n := binary.Uvarint(b)
		if n <= 0 {
			return dst, errWire
		}
		dst, b = append(dst, v), b[n:]
	}
	return dst, nil
}
