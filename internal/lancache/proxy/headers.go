package proxy

import (
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/http/httpguts"
)

const (
	processedByHeader = "X-LanCache-Processed-By"
	cacheStatusHeader = "X-Upstream-Cache-Status"
	// maxStoredHeaderBytes and maxStoredHeaderNames bound the headers
	// recorded with an object (the store's limits).
	maxStoredHeaderBytes = 4 << 10
	maxStoredHeaderNames = 64
)

// hopByHop are connection-specific headers never forwarded in either
// direction (RFC 9110 7.6.1 plus the de-facto ones).
var hopByHop = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// neverForwarded are request headers never sent upstream: framing is
// rebuilt by the transport, client addresses stay private (no
// X-Forwarded-For) and X-LanCache-Processed-By is replaced by ours.
var neverForwarded = map[string]bool{
	"Host":                    true,
	"Content-Length":          true,
	"Expect":                  true,
	"X-Forwarded-For":         true,
	"X-Forwarded-Host":        true,
	"X-Forwarded-Proto":       true,
	"Forwarded":               true,
	"X-Real-Ip":               true,
	"X-Lancache-Processed-By": true,
}

// storedDenied are upstream response headers never recorded with an object
// (ARCHITECTURE 8.2 step 10): framing, validators, cookies and per-response
// or per-edge metadata.
var storedDenied = map[string]bool{
	"Content-Length":            true,
	"Content-Range":             true,
	"Content-Encoding":          true,
	"Transfer-Encoding":         true,
	"Accept-Ranges":             true,
	"Etag":                      true,
	"Set-Cookie":                true,
	"Age":                       true,
	"Date":                      true,
	"Expires":                   true,
	"Cache-Control":             true,
	"Pragma":                    true,
	"Vary":                      true,
	"Alt-Svc":                   true,
	"Strict-Transport-Security": true,
	"Connection":                true,
	"Keep-Alive":                true,
	"Trailer":                   true,
	"Upgrade":                   true,
	"Via":                       true,
	"Server-Timing":             true,
}

// storedDeniedPrefixes are lower-case name prefixes never recorded.
var storedDeniedPrefixes = []string{"x-cache", "cf-", "x-amz-cf-", "x-lancache-", "x-upstream-"}

// connectionTokens returns the header names listed in Connection (they are
// hop-by-hop for this message), canonicalised.
func connectionTokens(h http.Header) map[string]bool {
	var out map[string]bool
	for _, v := range h.Values("Connection") {
		for tok := range strings.SplitSeq(v, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				if out == nil {
					out = make(map[string]bool)
				}
				out[http.CanonicalHeaderKey(tok)] = true
			}
		}
	}
	return out
}

// forwardHeaders builds the upstream request headers from the client's:
// end-to-end headers only, plus X-LanCache-Processed-By. For fills and
// direct slice fetches (shared or cached results) it also drops Range,
// If-*, Cookie and Accept-Encoding and asks for identity encoding; the
// caller adds its own Range.
func forwardHeaders(in http.Header, instanceID string, fill bool) http.Header {
	conn := connectionTokens(in)
	out := make(http.Header, len(in)+2)
	for k, vv := range in {
		ck := http.CanonicalHeaderKey(k)
		if hopByHop[ck] || conn[ck] || neverForwarded[ck] {
			continue
		}
		if fill && (ck == "Range" || strings.HasPrefix(ck, "If-") || ck == "Cookie" || ck == "Accept-Encoding") {
			continue
		}
		out[ck] = slices.Clone(vv)
	}
	if _, ok := out["User-Agent"]; !ok {
		out["User-Agent"] = []string{""} // no Go default User-Agent
	}
	if instanceID != "" {
		out.Set(processedByHeader, instanceID)
	}
	if fill {
		out.Set("Accept-Encoding", "identity")
	}
	return out
}

// withoutCredentials returns h without headers that must not follow a
// redirect to another host.
func withoutCredentials(h http.Header) http.Header {
	out := h.Clone()
	for _, k := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
		delete(out, k)
	}
	return out
}

// copyResponseHeaders copies end-to-end upstream response headers to dst
// (Content-Length is set by the caller).
func copyResponseHeaders(dst, src http.Header) {
	conn := connectionTokens(src)
	for k, vv := range src {
		ck := http.CanonicalHeaderKey(k)
		if hopByHop[ck] || conn[ck] || ck == "Content-Length" {
			continue
		}
		dst[ck] = slices.Clone(vv)
	}
}

// storedHeaders filters upstream response headers for recording with an
// object and replaying to clients: denied names and prefixes are dropped,
// invalid names and values (CR, LF, NUL or other control characters,
// invalid UTF-8) are dropped, and the result is capped at 4 KiB and 64
// names (Content-Type and Last-Modified first, then by name).
func storedHeaders(src http.Header) http.Header {
	conn := connectionTokens(src)
	names := make([]string, 0, len(src))
	for k := range src {
		ck := http.CanonicalHeaderKey(k)
		if storedDenied[ck] || hopByHop[ck] || conn[ck] || deniedPrefix(ck) || !httpguts.ValidHeaderFieldName(k) {
			continue
		}
		names = append(names, k)
	}
	rank := func(k string) int {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Type":
			return 0
		case "Last-Modified":
			return 1
		}
		return 2
	}
	slices.SortFunc(names, func(a, b string) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		return strings.Compare(a, b)
	})
	out := make(http.Header, min(len(names), maxStoredHeaderNames))
	size := 0
	for _, k := range names {
		ck := http.CanonicalHeaderKey(k)
		if _, ok := out[ck]; !ok && len(out) == maxStoredHeaderNames {
			break
		}
		for _, v := range src[k] {
			if !httpguts.ValidHeaderFieldValue(v) || !utf8.ValidString(v) {
				continue
			}
			n := len(ck) + len(v) + 4 // "Name: value\r\n"
			if size+n > maxStoredHeaderBytes {
				continue
			}
			size += n
			out[ck] = append(out[ck], v)
		}
	}
	return out
}

func deniedPrefix(name string) bool {
	l := strings.ToLower(name)
	for _, p := range storedDeniedPrefixes {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// isIdentity reports whether a response has no content coding.
func isIdentity(h http.Header) bool {
	vv := h.Values("Content-Encoding")
	switch len(vv) {
	case 0:
		return true
	case 1:
		v := strings.TrimSpace(vv[0])
		return v == "" || strings.EqualFold(v, "identity")
	}
	return false
}
