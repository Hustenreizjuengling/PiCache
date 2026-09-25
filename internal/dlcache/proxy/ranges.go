package proxy

import (
	"crypto/rand"
	"net/http"
	"strconv"
	"strings"
)

const (
	// maxRanges is the largest multi-range request answered as
	// multipart/byteranges (more ranges get the full object).
	maxRanges = 16
	// maxRangeHeader bounds the Range header we parse.
	maxRangeHeader = 4096
)

// byteRange is an absolute, inclusive byte range.
type byteRange struct{ start, end int64 }

func (r byteRange) length() int64 { return r.end - r.start + 1 }

// rangeSpec is one element of a Range header: "a-b", "a-" (end < 0) or the
// suffix "-n" (start < 0, end = n).
type rangeSpec struct{ start, end int64 }

// resolve maps the spec onto an object of size total. ok=false means the
// spec is not satisfiable.
func (sp rangeSpec) resolve(total int64) (byteRange, bool) {
	if sp.start < 0 {
		if sp.end == 0 || total == 0 {
			return byteRange{}, false
		}
		return byteRange{start: max(0, total-sp.end), end: total - 1}, true
	}
	if sp.start >= total {
		return byteRange{}, false
	}
	end := sp.end
	if end < 0 || end >= total {
		end = total - 1
	}
	return byteRange{start: sp.start, end: end}, true
}

// rangeHeader is a syntactically valid Range header.
type rangeHeader struct {
	specs   []rangeSpec
	tooMany bool // more than maxRanges specs: answered with the full object
}

// parseRange parses the request's Range header. It returns nil if there is
// none or it is invalid: an invalid Range header is ignored (RFC 9110 14.2).
func parseRange(h http.Header) *rangeHeader {
	vv := h.Values("Range")
	if len(vv) != 1 || len(vv[0]) > maxRangeHeader {
		return nil
	}
	unit, set, ok := strings.Cut(vv[0], "=")
	if !ok || !strings.EqualFold(strings.TrimSpace(unit), "bytes") {
		return nil
	}
	rh := &rangeHeader{}
	for part := range strings.SplitSeq(set, ",") {
		part = strings.Trim(part, " \t")
		if part == "" {
			continue
		}
		if len(rh.specs) == maxRanges {
			rh.tooMany = true
			break
		}
		first, last, ok := strings.Cut(part, "-")
		if !ok {
			return nil
		}
		first, last = strings.Trim(first, " \t"), strings.Trim(last, " \t")
		if first == "" {
			n, ok := parseDigits(last)
			if !ok {
				return nil
			}
			rh.specs = append(rh.specs, rangeSpec{start: -1, end: n})
			continue
		}
		a, ok := parseDigits(first)
		if !ok {
			return nil
		}
		b := int64(-1)
		if last != "" {
			if b, ok = parseDigits(last); !ok || b < a {
				return nil
			}
		}
		rh.specs = append(rh.specs, rangeSpec{start: a, end: b})
	}
	if len(rh.specs) == 0 {
		return nil
	}
	return rh
}

// parseDigits parses 1 to 18 ASCII digits (no sign, no overflow).
func parseDigits(s string) (int64, bool) {
	if s == "" || len(s) > 18 {
		return 0, false
	}
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}

// firstSliceIndex is the slice fetched first when the total is unknown: the
// slice of the start of a single, non-suffix range without If-Range, else
// slice 0 (the nginx slice module's rule).
func firstSliceIndex(rh *rangeHeader, h http.Header, sliceSize int64) int64 {
	if rh == nil || rh.tooMany || len(rh.specs) != 1 || rh.specs[0].start < 0 || h.Get("If-Range") != "" {
		return 0
	}
	return rh.specs[0].start / sliceSize
}

// plan is the response to a GET/HEAD on the cache path.
type plan struct {
	status        int         // 200, 206, 304 or 416
	total         int64       // object size
	ranges        []byteRange // body ranges (ascending)
	contentLength int64
	multipart     bool
	contentType   string   // multipart: the object's content type (per part)
	boundary      string   // multipart boundary
	parts         []string // multipart: part headers, parallel to ranges
	trailer       string   // multipart: closing delimiter
}

// makePlan decides the response from the request headers, the object size
// and its stored headers: If-Modified-Since → 304; If-Range (dates only)
// can disable the Range; one range → 206; unsatisfiable → 416; up to
// maxRanges ascending, non-overlapping ranges → 206 multipart/byteranges;
// anything else → 200 with the full object.
func makePlan(rh *rangeHeader, req http.Header, total int64, stored http.Header) plan {
	lm := stored.Get("Last-Modified")
	if notModified(req, lm) {
		return plan{status: http.StatusNotModified, total: total}
	}
	if rh != nil {
		if ir := req.Get("If-Range"); ir != "" && !ifRangeMatches(ir, lm) {
			rh = nil
		}
	}
	full := plan{status: http.StatusOK, total: total, contentLength: total}
	if total > 0 {
		full.ranges = []byteRange{{start: 0, end: total - 1}}
	}
	if rh == nil || rh.tooMany {
		return full
	}
	var rs []byteRange
	for _, sp := range rh.specs {
		if br, ok := sp.resolve(total); ok {
			rs = append(rs, br)
		}
	}
	switch {
	case len(rs) == 0:
		return plan{status: http.StatusRequestedRangeNotSatisfiable, total: total}
	case len(rs) == 1:
		return plan{status: http.StatusPartialContent, total: total, ranges: rs, contentLength: rs[0].length()}
	}
	for i := 1; i < len(rs); i++ {
		if rs[i].start <= rs[i-1].end {
			return full
		}
	}
	p := plan{
		status:      http.StatusPartialContent,
		total:       total,
		ranges:      rs,
		multipart:   true,
		contentType: stored.Get("Content-Type"),
		boundary:    rand.Text(),
	}
	for i, r := range rs {
		var b strings.Builder
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString("--" + p.boundary + "\r\n")
		if p.contentType != "" {
			b.WriteString("Content-Type: " + p.contentType + "\r\n")
		}
		b.WriteString("Content-Range: " + contentRange(r, total) + "\r\n\r\n")
		p.parts = append(p.parts, b.String())
		p.contentLength += int64(b.Len()) + r.length()
	}
	p.trailer = "\r\n--" + p.boundary + "--\r\n"
	p.contentLength += int64(len(p.trailer))
	return p
}

// contentRange formats a Content-Range value.
func contentRange(r byteRange, total int64) string {
	return "bytes " + strconv.FormatInt(r.start, 10) + "-" + strconv.FormatInt(r.end, 10) + "/" + strconv.FormatInt(total, 10)
}

// notModified evaluates If-Modified-Since against the stored Last-Modified
// (ignored when If-None-Match is present: we never send ETags, so it
// cannot match and takes precedence).
func notModified(req http.Header, lastModified string) bool {
	ims := req.Get("If-Modified-Since")
	if ims == "" || lastModified == "" || req.Get("If-None-Match") != "" {
		return false
	}
	t, err := http.ParseTime(ims)
	if err != nil {
		return false
	}
	m, err := http.ParseTime(lastModified)
	if err != nil {
		return false
	}
	return !m.After(t)
}

// ifRangeMatches reports whether an If-Range value allows the Range: only
// an HTTP date equal to the stored Last-Modified does. Entity tags never
// match because ETags are never sent to clients.
func ifRangeMatches(v, lastModified string) bool {
	v = strings.TrimSpace(v)
	if v == "" || lastModified == "" || strings.HasPrefix(v, `"`) || strings.HasPrefix(v, "W/") {
		return false
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return false
	}
	m, err := http.ParseTime(lastModified)
	return err == nil && t.Equal(m)
}

// parseContentRange parses "bytes a-b/total" (total -1 for "*").
func parseContentRange(v string) (start, end, total int64, ok bool) {
	unit, rest, found := strings.Cut(strings.TrimSpace(v), " ")
	if !found || !strings.EqualFold(unit, "bytes") {
		return 0, 0, 0, false
	}
	rng, tot, found := strings.Cut(strings.TrimSpace(rest), "/")
	if !found {
		return 0, 0, 0, false
	}
	a, b, found := strings.Cut(rng, "-")
	if !found {
		return 0, 0, 0, false
	}
	if start, ok = parseDigits(a); !ok {
		return 0, 0, 0, false
	}
	if end, ok = parseDigits(b); !ok || end < start {
		return 0, 0, 0, false
	}
	if tot == "*" {
		return start, end, -1, true
	}
	if total, ok = parseDigits(tot); !ok || end >= total {
		return 0, 0, 0, false
	}
	return start, end, total, true
}
