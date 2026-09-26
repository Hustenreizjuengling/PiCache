package logs

import (
	"encoding/base64"
	"math"
	"math/bits"
)

// Unique domains (docs/ARCHITECTURE.md 11): one HyperLogLog sketch per hour
// (and per day in the daily top table), precision 11: 2048 one-byte
// registers, about 2.3 % standard error, linear counting for small
// cardinalities. Sketches are stored in the top tables as kind "unique",
// key "", label = base64 (standard) of the registers, count = the estimate;
// a range merges its sketches register by register (maximum).
const (
	hllPrecision = 11
	hllRegisters = 1 << hllPrecision
	hllMaxRank   = 64 - hllPrecision + 1 // largest register value
)

// hll is a HyperLogLog sketch.
type hll [hllRegisters]uint8

// hashName hashes a normalised query name: 64-bit FNV-1a followed by the
// splitmix64 finaliser (FNV alone mixes the high bits, which pick the
// register, too little).
func hashName(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	h ^= h >> 30
	h *= 0xbf58476d1ce4e5b9
	h ^= h >> 27
	h *= 0x94d049bb133111eb
	h ^= h >> 31
	return h
}

// add counts a hash.
func (h *hll) add(x uint64) {
	idx := x >> (64 - hllPrecision)
	w := x<<hllPrecision | 1<<(hllPrecision-1) // the guard bit bounds the rank
	if r := uint8(bits.LeadingZeros64(w) + 1); r > h[idx] {
		h[idx] = r
	}
}

// merge adds the elements of o (register-wise maximum).
func (h *hll) merge(o *hll) {
	for i, v := range o {
		if v > h[i] {
			h[i] = v
		}
	}
}

// empty reports whether nothing was counted.
func (h *hll) empty() bool { return *h == hll{} }

// estimate returns the estimated number of distinct elements.
func (h *hll) estimate() int64 {
	const m = float64(hllRegisters)
	sum, zeros := 0.0, 0
	for _, v := range h {
		sum += math.Ldexp(1, -int(v))
		if v == 0 {
			zeros++
		}
	}
	if zeros == hllRegisters {
		return 0
	}
	e := 0.7213 / (1 + 1.079/m) * m * m / sum
	if e <= 2.5*m && zeros > 0 {
		e = m * math.Log(m/float64(zeros)) // linear counting
	}
	return int64(math.Round(e))
}

// encode returns the stored form of the sketch.
func (h *hll) encode() string { return base64.StdEncoding.EncodeToString(h[:]) }

// decodeHLL parses a stored sketch; false for anything that is not exactly
// the registers of a sketch (a row of another version or a damaged one is
// ignored).
func decodeHLL(s string) (*hll, bool) {
	if len(s) != base64.StdEncoding.EncodedLen(hllRegisters) {
		return nil, false
	}
	buf := make([]byte, base64.StdEncoding.DecodedLen(len(s)))
	n, err := base64.StdEncoding.Decode(buf, []byte(s))
	if err != nil || n != hllRegisters {
		return nil, false
	}
	var h hll
	copy(h[:], buf)
	for _, v := range h {
		if v > hllMaxRank {
			return nil, false
		}
	}
	return &h, true
}
