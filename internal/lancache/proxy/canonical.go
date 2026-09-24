package proxy

import (
	"strings"
	"unicode/utf8"
)

// maxPathLen is the longest decoded path that is cached (the store's limit).
const maxPathLen = 4096

// canonicalPath reports whether a request path is canonical (ARCHITECTURE
// 8.2 step 8). escaped is r.URL.EscapedPath(), decoded is r.URL.Path.
//
// Only canonical paths are cached: their decoded form (part of the cache
// key) identifies exactly the resource that is fetched upstream, which is
// the client's escaped form byte for byte. A path is not canonical if it
// does not start with "/", contains "//", a "." or ".." segment, a raw
// "\", an encoded "/", "\" or NUL, a percent-encoded unreserved character
// (RFC 3986 2.3), or – after decoding – any other control character. Paths
// the store cannot record (invalid UTF-8, longer than 4 KiB) are treated
// the same way.
func canonicalPath(escaped, decoded string) bool {
	if escaped == "" || escaped[0] != '/' || len(decoded) > maxPathLen || !utf8.ValidString(decoded) {
		return false
	}
	if strings.Contains(escaped, "//") || strings.ContainsRune(escaped, '\\') {
		return false
	}
	for seg := range strings.SplitSeq(escaped[1:], "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	for i := 0; i < len(escaped); i++ {
		if escaped[i] != '%' {
			continue
		}
		if i+2 >= len(escaped) {
			return false
		}
		c, ok := unhex(escaped[i+1], escaped[i+2])
		if !ok || c == '/' || c == '\\' || c == 0 || isUnreserved(c) {
			return false
		}
		i += 2
	}
	for i := 0; i < len(decoded); i++ {
		if c := decoded[i]; c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// isUnreserved reports whether c is an RFC 3986 unreserved character.
func isUnreserved(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
		c == '-' || c == '.' || c == '_' || c == '~'
}

func unhex(hi, lo byte) (byte, bool) {
	h, ok1 := hexVal(hi)
	l, ok2 := hexVal(lo)
	return h<<4 | l, ok1 && ok2
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
