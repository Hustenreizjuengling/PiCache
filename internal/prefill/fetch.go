package prefill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

// Every download of a job goes to the download cache, never to the
// Internet directly: the request names the CDN host in its Host header
// (like a game client whose DNS points the CDN at the cache) and is sent
// to the cache's address. The cache fetches what it does not have; the
// body is read and dropped.

const (
	heartbeatPath     = "/lancache-heartbeat"
	processedByHeader = "X-LanCache-Processed-By"
	cacheStatusHeader = "X-Upstream-Cache-Status"

	fetchTimeout  = 10 * time.Minute
	headerTimeout = 2 * time.Minute
	readChunk     = 64 << 10
	maxErrorBody  = 512
)

// errCacheRefused is a 403 of the cache (the download cache is off, or the
// host is not a download service).
var errCacheRefused = errors.New("the download cache refused the request")

// cacheClient sends requests to the cache at base.
type cacheClient struct {
	base *url.URL
	hc   *http.Client
}

// parseCacheURL checks PICACHE_PREFILL_CACHE: http://host[:port] and
// nothing else.
func parseCacheURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("the cache URL must be http://host[:port], not %q", raw)
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), "80")
	}
	u.Path = ""
	return u, nil
}

func newCacheClient(base *url.URL, conns int) *cacheClient {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	addr := base.Host
	return &cacheClient{
		base: base,
		hc: &http.Client{
			Transport: &http.Transport{
				// Whatever the request's host, the connection goes to the cache.
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					return d.DialContext(ctx, network, addr)
				},
				Proxy:                  nil,
				MaxConnsPerHost:        conns,
				MaxIdleConnsPerHost:    conns,
				IdleConnTimeout:        90 * time.Second,
				ResponseHeaderTimeout:  headerTimeout,
				DisableCompression:     true,
				MaxResponseHeaderBytes: 64 << 10,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (c *cacheClient) close() { c.hc.CloseIdleConnections() }

// heartbeat checks that base is a lancache-compatible cache and returns the
// instance ID it reports.
func (c *cacheClient) heartbeat(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+c.base.Host+heartbeatPath, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("the download cache at %s does not answer: %w", c.base.Host, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	id := resp.Header.Get(processedByHeader)
	if resp.StatusCode != http.StatusNoContent || id == "" {
		return "", fmt.Errorf("%s is not a download cache (heartbeat: HTTP %d)", c.base.Host, resp.StatusCode)
	}
	return id, nil
}

// result is what one download read.
type result struct {
	bytes int64
	hit   bool
}

// get downloads http://<host><path> through the cache. With keep, the
// body (at most limit bytes) is returned; otherwise it is dropped. lim, if
// not nil, limits the rate in bytes per second.
func (c *cacheClient) get(ctx context.Context, host, path, userAgent string, lim *rate.Limiter, keep bool, limit int64) (result, []byte, error) {
	var body []byte
	var read func(io.Reader) error
	if keep {
		read = func(r io.Reader) error {
			b, err := io.ReadAll(io.LimitReader(r, limit+1))
			if err != nil {
				return err
			}
			if int64(len(b)) > limit {
				return fmt.Errorf("answer larger than %d bytes", limit)
			}
			body = b
			return nil
		}
	}
	res, err := c.stream(ctx, fetchReq{host: host, path: path, userAgent: userAgent, lim: lim}, read)
	if err != nil {
		return res, nil, err
	}
	return res, body, nil
}

// fetchReq is one download through the cache.
type fetchReq struct {
	host, path, userAgent string
	// rng, if set, is the one range requested: "bytes=a-b".
	rng string
	lim *rate.Limiter
}

// stream downloads q through the cache. read, if not nil, reads the body
// (it may stop early); the rest is read and dropped, so the cache always
// gets the whole answer.
func (c *cacheClient) stream(ctx context.Context, q fetchReq, read func(io.Reader) error) (result, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+q.host+q.path, nil)
	if err != nil {
		return result{}, err
	}
	// An empty User-Agent sends none (the Battle.net agent sends none).
	req.Header.Set("User-Agent", q.userAgent)
	req.Header.Set("Accept", "*/*")
	if q.rng != "" {
		req.Header.Set("Range", q.rng)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && (q.rng == "" || resp.StatusCode != http.StatusPartialContent) {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		msg := strings.TrimSpace(string(bytes.ToValidUTF8(b, nil)))
		if resp.StatusCode == http.StatusForbidden && resp.Header.Get(processedByHeader) == "" {
			return result{}, fmt.Errorf("%w (%s)", errCacheRefused, msg)
		}
		return result{}, &statusError{code: resp.StatusCode, msg: msg}
	}
	cr := &countReader{ctx: ctx, r: resp.Body, lim: q.lim}
	res := result{hit: strings.EqualFold(resp.Header.Get(cacheStatusHeader), "HIT")}
	if read != nil {
		err = read(cr)
	}
	if err == nil {
		_, err = io.CopyBuffer(io.Discard, cr, make([]byte, readChunk))
	}
	res.bytes = cr.n
	if err != nil {
		return res, err
	}
	if resp.ContentLength >= 0 && cr.n != resp.ContentLength {
		return res, io.ErrUnexpectedEOF
	}
	return res, nil
}

// countReader counts what is read and keeps to the rate limit.
type countReader struct {
	ctx context.Context
	r   io.Reader
	lim *rate.Limiter
	n   int64
}

func (c *countReader) Read(p []byte) (int, error) {
	if len(p) > readChunk {
		p = p[:readChunk]
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	if n > 0 && c.lim != nil {
		if werr := c.lim.WaitN(c.ctx, n); werr != nil {
			return n, werr
		}
	}
	return n, err
}

// statusError is a non-200 answer that came through the cache.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("HTTP %d", e.code)
	}
	return fmt.Sprintf("HTTP %d: %s", e.code, e.msg)
}

// newLimiter returns the rate limiter of maxMbps (nil without a limit).
func newLimiter(maxMbps int) *rate.Limiter {
	if maxMbps <= 0 {
		return nil
	}
	bps := float64(maxMbps) * 1e6 / 8
	burst := max(int(bps/10), readChunk)
	return rate.NewLimiter(rate.Limit(bps), burst)
}
