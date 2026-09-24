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
// Only canonical paths are cached: their key path (cacheKeyPath, part of
// the cache key) identifies exactly the resource that is fetched upstream,
// which is the escaped form byte for byte. A path is not canonical if it
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

// cacheKeyPath returns the path of the cache key of a canonical escaped
// path: percent-encodings are decoded except those of characters that may
// also appear unencoded in a path with another meaning (RFC 3986 2.2: the
// sub-delims, ":", "@", "[", "]") and of "%" itself, which stay encoded
// with upper-case hex digits (RFC 3986 6.2.2). "/a%2Bb" and "/a+b" are
// different resources and get different keys; any two escaped forms with
// the same key path are equivalent URIs. Without such encodings the key
// path is the decoded path.
func cacheKeyPath(escaped string) string {
	if !strings.Contains(escaped, "%") {
		return escaped
	}
	var b strings.Builder
	b.Grow(len(escaped))
	for i := 0; i < len(escaped); i++ {
		c := escaped[i]
		if c == '%' && i+2 < len(escaped) {
			if d, ok := unhex(escaped[i+1], escaped[i+2]); ok {
				if keepEncoded(d) {
					const hexDigits = "0123456789ABCDEF"
					b.WriteByte('%')
					b.WriteByte(hexDigits[d>>4])
					b.WriteByte(hexDigits[d&15])
				} else {
					b.WriteByte(d)
				}
				i += 2
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// keepEncoded reports whether a percent-encoded c stays encoded in a key
// path: the characters that may appear unencoded in an escaped path
// besides the unreserved ones and "/" (net/url leaves "[" and "]" alone),
// and "%".
func keepEncoded(c byte) bool {
	return strings.IndexByte("!$&'()*+,;=:@[]%", c) >= 0
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
