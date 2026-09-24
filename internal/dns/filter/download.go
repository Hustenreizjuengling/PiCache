package filter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/version"
)

const (
	maxRedirects    = 5
	downloadTimeout = 5 * time.Minute
	maxValidatorLen = 256 // ETag / Last-Modified values we store and send back
	maxErrorLen     = 300
)

// fetched is the result of a download attempt.
type fetched struct {
	notModified        bool
	tmp                string // downloaded body (removed by the caller)
	etag, lastModified string
	hash               string // hex SHA-256 of the body
	size               int64
}

// fetchList downloads a list into e.tmpPath(id). With conditional set, the
// stored validators are sent and a 304 yields notModified.
func (e *Engine) fetchList(ctx context.Context, id int64, rawURL, etag, lastModified string, conditional bool) (fetched, error) {
	u, err := e.checkListURL(rawURL) // again: the row may come from a restored backup
	if err != nil {
		return fetched{}, err
	}
	if u.Scheme == "file" {
		return e.copyLocal(ctx, id, u)
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	origHost := u.Hostname()
	private := isPrivateLiteral(origHost)
	client := *e.fetch
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for hop := 0; ; hop++ {
		reqCtx := ctx
		if private && u.Hostname() == origHost {
			reqCtx = netutil.WithAllowPrivate(ctx)
		}
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
		if err != nil {
			return fetched{}, errors.New("invalid URL")
		}
		req.Header.Set("User-Agent", "PiCache/"+version.Version)
		if conditional {
			if etag != "" {
				req.Header.Set("If-None-Match", etag)
			}
			if lastModified != "" {
				req.Header.Set("If-Modified-Since", lastModified)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			return fetched{}, err
		}
		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
			http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			loc := resp.Header.Get("Location")
			drain(resp)
			if hop >= maxRedirects {
				return fetched{}, fmt.Errorf("more than %d redirects", maxRedirects)
			}
			next, err := u.Parse(loc)
			if err != nil || loc == "" {
				return fetched{}, errors.New("invalid redirect location")
			}
			if err := checkRedirect(next, origHost, private); err != nil {
				return fetched{}, err
			}
			u = next
		case http.StatusNotModified:
			drain(resp)
			if !conditional {
				return fetched{}, errors.New("unexpected HTTP 304 Not Modified")
			}
			return fetched{notModified: true}, nil
		case http.StatusOK:
			defer resp.Body.Close()
			f, err := e.saveBody(ctx, id, resp.Body)
			if err != nil {
				return fetched{}, err
			}
			f.etag = validator(resp.Header.Get("ETag"))
			f.lastModified = validator(resp.Header.Get("Last-Modified"))
			return f, nil
		default:
			drain(resp)
			return fetched{}, fmt.Errorf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		}
	}
}

// checkRedirect validates a redirect target: https only (http only back to
// the same private host), no credentials, and never a private or local IP
// literal unless it is the configured private host itself. Host names are
// checked at dial time: the SafeDialer refuses private addresses because
// the request context is not marked with WithAllowPrivate for other hosts.
func checkRedirect(next *url.URL, origHost string, private bool) error {
	sameHost := private && next.Hostname() == origHost
	if next.User != nil {
		return errors.New("redirect to a URL with credentials refused")
	}
	if next.Hostname() == "" {
		return errors.New("invalid redirect location")
	}
	if ip, err := netip.ParseAddr(next.Hostname()); err == nil && !sameHost && !netutil.IsPublicUnicast(ip) {
		return fmt.Errorf("redirect to a private address refused (%s)", ip)
	}
	if next.Scheme != "https" && !(next.Scheme == "http" && sameHost) {
		return fmt.Errorf("redirect to a %s URL refused (https required)", next.Scheme)
	}
	return nil
}

// saveBody streams r to the temp file, hashing it and enforcing the size cap.
func (e *Engine) saveBody(ctx context.Context, id int64, r io.Reader) (fetched, error) {
	tmp := e.tmpPath(id)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return fetched{}, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(ctxReader{ctx, r}, e.maxBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > e.maxBytes {
		err = fmt.Errorf("the list is larger than %s", sizeText(e.maxBytes))
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fetched{}, err
	}
	return fetched{tmp: tmp, hash: hex.EncodeToString(h.Sum(nil)), size: n}, nil
}

// copyLocal copies a file:// list from <lists>/local/ (through os.Root, so
// symlinks cannot escape the directory).
func (e *Engine) copyLocal(ctx context.Context, id int64, u *url.URL) (fetched, error) {
	rel, err := e.localPath(u)
	if err != nil {
		return fetched{}, err
	}
	root, err := os.OpenRoot(e.localDir)
	if err != nil {
		return fetched{}, err
	}
	defer root.Close()
	f, err := root.Open(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return fetched{}, errors.New("the local list file does not exist")
	}
	if err != nil {
		return fetched{}, errors.New("the local list file cannot be opened")
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return fetched{}, errors.New("the local list is not a regular file")
	}
	return e.saveBody(ctx, id, f)
}

// ctxReader aborts reads once ctx is done (local files have no deadline).
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// drain discards a small rest of the body so the connection can be reused.
func drain(resp *http.Response) {
	_, _ = io.CopyN(io.Discard, resp.Body, 4<<10)
	_ = resp.Body.Close()
}

// validator returns v if it is safe to store and send back.
func validator(v string) string {
	if len(v) > maxValidatorLen {
		return ""
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] > 0x7e {
			return ""
		}
	}
	return v
}

// errorText returns a message for the list status that never contains the
// request URL (it may carry a token in its query string).
func errorText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var msg string
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		msg = "download timed out"
	case errors.Is(err, netutil.ErrForbiddenDestination):
		msg = "destination address not allowed (private or local addresses are refused)"
	default:
		msg = err.Error()
	}
	if len(msg) > maxErrorLen {
		msg = msg[:maxErrorLen]
	}
	return strings.ToValidUTF8(msg, "?")
}

// sizeText formats a size limit for messages.
func sizeText(n int64) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}
