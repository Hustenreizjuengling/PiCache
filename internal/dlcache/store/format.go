package cachestore

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/http/httpguts"
)

// On-disk format of a slice file (ARCHITECTURE 9.1):
//
//	"PCS1" | uint32 LE header length | JSON header | data
const (
	sliceMagic   = "PCS1"
	prefixLen    = 8        // magic + header length
	maxHeaderLen = 64 << 10 // largest accepted JSON header
	firstRead    = 4 << 10  // bytes read at once when opening a slice

	maxStoredHeader = 4 << 10 // filtered response headers per object
	maxHeaderNames  = 64
	maxPathLen      = 4096
	maxHostLen      = 253
	maxGroupKeyLen  = 512
	maxRebuildValue = 256 // Content-Type / Last-Modified taken from a header on rebuild
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func crc32c(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

// sliceHeader is the self-describing JSON header of a slice file.
type sliceHeader struct {
	O string      `json:"o"` // object id
	I int64       `json:"i"` // slice index
	S string      `json:"s"` // service
	H string      `json:"h"` // host
	P string      `json:"p"` // canonical path (no query)
	T int64       `json:"t"` // object total size
	Z int64       `json:"z"` // slice size of the store
	C int64       `json:"c"` // created, unix ms
	K uint32      `json:"k"` // crc32c (Castagnoli) of the data
	M http.Header `json:"m,omitempty"`
}

// errCorrupt marks a slice file that fails validation (as opposed to an I/O
// error while reading it).
type errCorrupt struct{ reason string }

func (e *errCorrupt) Error() string { return "cachestore: corrupt slice file: " + e.reason }

func corrupt(format string, args ...any) error {
	return &errCorrupt{reason: fmt.Sprintf(format, args...)}
}

func isCorrupt(err error) bool {
	var c *errCorrupt
	return errors.As(err, &c)
}

// ObjectID derives the object id: first 32 hex chars of SHA-256(service + "\x00" + path).
func ObjectID(service, path string) string {
	h := sha256.New()
	_, _ = io.WriteString(h, service)
	_, _ = h.Write([]byte{0})
	_, _ = io.WriteString(h, path)
	var sum [sha256.Size]byte
	return hex.EncodeToString(h.Sum(sum[:0])[:16])
}

// ValidObjectID reports whether id is a well-formed object id.
func ValidObjectID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !isLowerHex(id[i]) {
			return false
		}
	}
	return true
}

func isLowerHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' }

// slicesFor returns the number of slices of an object of total bytes.
func slicesFor(total, sliceSize int64) int64 {
	if total <= 0 {
		return 0
	}
	return (total-1)/sliceSize + 1
}

// sliceLen returns the data length of slice idx: min(S, total − idx·S).
func sliceLen(total, sliceSize, idx int64) int64 {
	return min(sliceSize, total-idx*sliceSize)
}

// sliceDir is the lazily created shard directory of an object.
func sliceDir(id string) string { return "slices/" + id[0:2] + "/" + id[2:4] }

// sliceName is the root-relative file name of one slice.
func sliceName(id string, idx int64) string {
	return sliceDir(id) + "/" + id + "." + strconv.FormatInt(idx, 10)
}

// parseSliceFile parses a slice file name "<objectId>.<index>" (canonical
// decimal index, no sign or leading zeros).
func parseSliceFile(name string) (id string, idx int64, ok bool) {
	if len(name) < 34 || name[32] != '.' || !ValidObjectID(name[:32]) {
		return "", 0, false
	}
	num := name[33:]
	if len(num) > 1 && num[0] == '0' || len(num) > 12 {
		return "", 0, false
	}
	for i := 0; i < len(num); i++ {
		if num[i] < '0' || num[i] > '9' {
			return "", 0, false
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return name[:32], n, true
}

// isShard reports whether s is a two-character lowercase hex shard directory name.
func isShard(s string) bool { return len(s) == 2 && isLowerHex(s[0]) && isLowerHex(s[1]) }

// encodePrefix builds magic + length + JSON header.
func encodePrefix(h *sliceHeader) ([]byte, error) {
	j, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("cachestore: encode slice header: %w", err)
	}
	if len(j) > maxHeaderLen {
		return nil, errors.New("cachestore: slice header too large")
	}
	b := make([]byte, prefixLen, prefixLen+len(j))
	copy(b, sliceMagic)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(j)))
	return append(b, j...), nil
}

// readHeader reads and decodes the header of a slice file of size bytes.
// It returns the header and the offset of the data.
func readHeader(r io.ReaderAt, size int64) (sliceHeader, int64, error) {
	var h sliceHeader
	if size < prefixLen+2 {
		return h, 0, corrupt("file too short")
	}
	buf := make([]byte, min(size, prefixLen+firstRead))
	if _, err := r.ReadAt(buf, 0); err != nil {
		return h, 0, fmt.Errorf("cachestore: read slice header: %w", err)
	}
	if string(buf[:4]) != sliceMagic {
		return h, 0, corrupt("bad magic")
	}
	hl := int64(binary.LittleEndian.Uint32(buf[4:8]))
	if hl < 2 || hl > maxHeaderLen || hl > size-prefixLen {
		return h, 0, corrupt("bad header length %d", hl)
	}
	var js []byte
	if prefixLen+hl <= int64(len(buf)) {
		js = buf[prefixLen : prefixLen+hl]
	} else {
		js = make([]byte, hl)
		if _, err := r.ReadAt(js, prefixLen); err != nil {
			return h, 0, fmt.Errorf("cachestore: read slice header: %w", err)
		}
	}
	if err := json.Unmarshal(js, &h); err != nil {
		return h, 0, corrupt("invalid header JSON")
	}
	return h, prefixLen + hl, nil
}

// validate checks a decoded header against the file it came from
// (ARCHITECTURE 9.1). dataOff is the data offset returned by readHeader.
func (h *sliceHeader) validate(id string, idx, sliceSize, fileSize, dataOff int64) error {
	switch {
	case h.O != id || h.I != idx:
		return corrupt("header does not match file name")
	case !ValidObjectID(h.O) || ObjectID(h.S, h.P) != h.O:
		return corrupt("object id does not match service and path")
	case h.Z != sliceSize:
		return corrupt("slice size %d differs from the store (%d)", h.Z, sliceSize)
	case h.T <= 0 || h.T > MaxTotal:
		return corrupt("invalid total %d", h.T)
	case h.I < 0 || h.I >= slicesFor(h.T, h.Z):
		return corrupt("slice index out of range")
	case fileSize != dataOff+sliceLen(h.T, h.Z, h.I):
		return corrupt("file size %d, want %d", fileSize, dataOff+sliceLen(h.T, h.Z, h.I))
	}
	return nil
}

var serviceRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func validService(s string) bool { return serviceRE.MatchString(s) }

func validHost(h string) bool {
	if h == "" || len(h) > maxHostLen {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// validText reports whether s is valid UTF-8 without control characters.
func validText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validPath(p string) bool {
	return len(p) <= maxPathLen && strings.HasPrefix(p, "/") && validText(p)
}

func validGroupKey(k string) bool { return k != "" && len(k) <= maxGroupKeyLen && validText(k) }

// cleanHeader validates stored response headers and returns a private copy
// and its approximate size.
func cleanHeader(h http.Header) (http.Header, int, error) {
	if len(h) > maxHeaderNames {
		return nil, 0, errors.New("cachestore: too many stored headers")
	}
	out := make(http.Header, len(h))
	size := 0
	for k, vs := range h {
		if !httpguts.ValidHeaderFieldName(k) {
			return nil, 0, fmt.Errorf("cachestore: invalid header name %q", k)
		}
		for _, v := range vs {
			if !httpguts.ValidHeaderFieldValue(v) || !utf8.ValidString(v) {
				return nil, 0, fmt.Errorf("cachestore: invalid value for header %s", k)
			}
			size += len(k) + len(v) + 4
		}
		if len(vs) > 0 {
			out[k] = append([]string(nil), vs...)
		}
	}
	if size > maxStoredHeader {
		return nil, 0, fmt.Errorf("cachestore: stored headers exceed %d bytes", maxStoredHeader)
	}
	return out, size, nil
}

// rebuildHeader keeps only Content-Type and Last-Modified of a slice header's
// stored headers (hostile content: values are validated).
func rebuildHeader(m http.Header) http.Header {
	out := http.Header{}
	for _, k := range []string{"Content-Type", "Last-Modified"} {
		v := m.Get(k)
		if v != "" && len(v) <= maxRebuildValue && httpguts.ValidHeaderFieldValue(v) && utf8.ValidString(v) {
			out.Set(k, v)
		}
	}
	return out
}

// validateMeta checks the object metadata passed to SetMeta.
func validateMeta(id string, m *Meta) error {
	switch {
	case !validService(m.Service):
		return errors.New("cachestore: invalid service id")
	case !validHost(m.Host):
		return errors.New("cachestore: invalid host")
	case !validPath(m.Path):
		return errors.New("cachestore: invalid path")
	case !validGroupKey(m.GroupKey):
		return errors.New("cachestore: invalid group key")
	case m.Total <= 0 || m.Total > MaxTotal:
		return fmt.Errorf("cachestore: invalid total %d", m.Total)
	case ObjectID(m.Service, m.Path) != id:
		return errors.New("cachestore: object id does not match service and path")
	}
	return nil
}

func headerEqual(a, b http.Header) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if av[i] != bv[i] {
				return false
			}
		}
	}
	return true
}
