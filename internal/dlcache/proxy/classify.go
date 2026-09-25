package proxy

import (
	"net/http"

	cachestore "github.com/hustenreizjuengling/picache/internal/dlcache/store"
)

// respKind classifies an upstream answer to a slice request
// (ARCHITECTURE 8.2 steps 10 and 13).
type respKind uint8

const (
	kindFailed     respKind = iota // no response (transport error)
	kindSlice                      // valid slice: aligned 206, or a complete object of ≤ S bytes
	kindRangeFail                  // 200 larger than S or a misaligned 206: usable body, no slicing
	kindNoLength                   // 200 without Content-Length: served, never stored
	kindPassClient                 // not cacheable (content coding, > 1 TiB, 416 on slice 0, malformed): pass the client's request through
	kind416                        // 416 for a slice > 0
	kindUpstream                   // any other status: passed through unmodified, never stored
)

// respInfo is the classification of one upstream response.
type respInfo struct {
	kind         respKind
	status       int
	total        int64       // object size (-1 unknown)
	start        int64       // absolute offset of the first body byte
	length       int64       // body length (-1 unknown)
	dataIdx      int64       // kindSlice: slice index the body is
	header       http.Header // filtered headers for storing and replaying
	rangeFailure bool        // counts as a range failure of the host
}

// classifyResponse classifies the answer to a request for slice idx
// (rangeSent: the request carried Range: bytes=idx·S-(idx·S+S-1)).
func classifyResponse(resp *http.Response, idx, sliceSize int64, rangeSent bool) respInfo {
	info := respInfo{status: resp.StatusCode, total: -1, length: -1}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		if !isIdentity(resp.Header) {
			info.kind = kindPassClient
			return info
		}
		a, b, t, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || t < 0 || t > cachestore.MaxTotal || (resp.ContentLength >= 0 && resp.ContentLength != b-a+1) {
			info.kind = kindPassClient
			info.rangeFailure = rangeSent && (!ok || t < 0)
			return info
		}
		info.total, info.start, info.length = t, a, b-a+1
		info.header = storedHeaders(resp.Header)
		if rangeSent && a == idx*sliceSize && b+1 == min(a+sliceSize, t) {
			info.kind, info.dataIdx = kindSlice, idx
			return info
		}
		info.kind, info.rangeFailure = kindRangeFail, rangeSent
	case http.StatusOK:
		cl := resp.ContentLength
		switch {
		case !isIdentity(resp.Header) || cl > cachestore.MaxTotal:
			info.kind = kindPassClient
			return info
		case cl == 0:
			info.kind = kindUpstream // empty object: relayed, nothing to store
			return info
		}
		info.header = storedHeaders(resp.Header)
		if cl < 0 {
			info.kind = kindNoLength
			return info
		}
		info.total, info.start, info.length = cl, 0, cl
		if cl <= sliceSize {
			info.kind, info.dataIdx = kindSlice, 0
			return info
		}
		info.kind, info.rangeFailure = kindRangeFail, rangeSent
	case http.StatusRequestedRangeNotSatisfiable:
		if idx == 0 {
			info.kind = kindPassClient
		} else {
			info.kind = kind416
		}
	default:
		info.kind = kindUpstream
	}
	return info
}

// sliceLen is the length of slice idx of an object of size total.
func sliceLen(idx, sliceSize, total int64) int64 {
	return max(0, min(sliceSize, total-idx*sliceSize))
}
