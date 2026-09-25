package update

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/version"
)

const (
	DefaultAPIBase      = "https://api.github.com"
	DefaultDownloadBase = "https://github.com"

	maxAPIResponse = 2 << 20   // release list or one release
	maxNotes       = 64 << 10  // bytes of release notes kept
	maxBinarySize  = 256 << 20 // release binary
	listPerPage    = 30
	// CheckTimeout bounds one check (the release list).
	CheckTimeout = 15 * time.Second
	// maxRedirects bounds the redirects of a download (GitHub redirects
	// release assets to its object storage).
	maxRedirects = 5
)

// Client reads release information and files of Repository.
type Client struct {
	HTTP         *http.Client // nil: NewHTTPClient()
	APIBase      string       // "": DefaultAPIBase (tests: an httptest server)
	DownloadBase string       // "": DefaultDownloadBase
	Arch         string       // "": runtime.GOARCH
	UserAgent    string       // "": PiCache/<version>
}

// NewHTTPClient is the download client of the CLI and the root helper: the
// host's resolver, no environment proxy, verified TLS, at most 5 redirects
// and never from https to http. (The service uses its own client, which
// resolves through PiCache's upstreams.)
func NewHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: checkRedirect,
	}
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("too many redirects")
	}
	if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return errors.New("refusing a redirect from https to http")
	}
	return nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return NewHTTPClient()
}

func (c *Client) apiBase() string {
	return strings.TrimSuffix(cmp.Or(c.APIBase, DefaultAPIBase), "/")
}

func (c *Client) downloadBase() string {
	return strings.TrimSuffix(cmp.Or(c.DownloadBase, DefaultDownloadBase), "/")
}

func (c *Client) arch() string { return cmp.Or(c.Arch, runtime.GOARCH) }

// AssetName returns the release binary for a GOARCH (arm → armv7).
func AssetName(goarch string) (string, bool) {
	switch goarch {
	case "amd64", "arm64":
		return "picache-linux-" + goarch, true
	case "arm":
		return "picache-linux-armv7", true
	}
	return "", false
}

// ghRelease is the part of a GitHub release object PiCache reads.
type ghRelease struct {
	TagName     string `json:"tag_name"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	PublishedAt string `json:"published_at"`
	Body        string `json:"body"`
	Assets      []struct {
		Name string `json:"name"`
	} `json:"assets"`
}

// Latest returns the newest eligible release: not a draft, a pre-release
// only with includePre, a valid version, and the binary for this
// architecture, SHA256SUMS and SHA256SUMS.sig attached. nil if there is none.
func (c *Client) Latest(ctx context.Context, includePre bool) (*Release, error) {
	asset, ok := AssetName(c.arch())
	if !ok {
		return nil, fmt.Errorf("no release binaries are built for linux/%s", c.arch())
	}
	var list []ghRelease
	u := fmt.Sprintf("%s/repos/%s/releases?per_page=%d", c.apiBase(), Repository, listPerPage)
	if err := c.getJSON(ctx, u, &list); err != nil {
		return nil, err
	}
	var best *Release
	var bestV Version
	for _, gr := range list {
		rel, v, err := c.eligible(gr, asset)
		if err != nil || (rel.Prerelease && !includePre) {
			continue
		}
		if best == nil || Compare(v, bestV) > 0 {
			best, bestV = rel, v
		}
	}
	return best, nil
}

// Release returns the release with exactly this version (the root helper
// and `picache update --version`). Pre-releases are accepted: the version
// was chosen explicitly.
func (c *Client) Release(ctx context.Context, ver string) (*Release, error) {
	if _, err := ParseVersion(ver); err != nil {
		return nil, err
	}
	asset, ok := AssetName(c.arch())
	if !ok {
		return nil, fmt.Errorf("no release binaries are built for linux/%s", c.arch())
	}
	var gr ghRelease
	u := fmt.Sprintf("%s/repos/%s/releases/tags/%s", c.apiBase(), Repository, url.PathEscape(ver))
	if err := c.getJSON(ctx, u, &gr); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, fmt.Errorf("there is no release %s", ver)
		}
		return nil, err
	}
	if gr.TagName != ver {
		return nil, fmt.Errorf("GitHub answered with release %q instead of %s", clip(gr.TagName, 40), ver)
	}
	rel, _, err := c.eligible(gr, asset)
	return rel, err
}

// eligible converts a GitHub release that PiCache can install.
func (c *Client) eligible(gr ghRelease, asset string) (*Release, Version, error) {
	v, err := ParseVersion(gr.TagName)
	switch {
	case err != nil:
		return nil, v, err
	case gr.Draft:
		return nil, v, fmt.Errorf("release %s is a draft", gr.TagName)
	}
	have := map[string]bool{}
	for _, a := range gr.Assets {
		have[a.Name] = true
	}
	for _, name := range []string{asset, SumsFile, SigFile} {
		if !have[name] {
			return nil, v, fmt.Errorf("release %s has no %s", gr.TagName, name)
		}
	}
	rel := &Release{
		Version:    gr.TagName,
		URL:        c.downloadBase() + "/" + Repository + "/releases/tag/" + gr.TagName,
		Notes:      notes(gr.Body),
		Prerelease: gr.Prerelease || v.Prerelease(),
	}
	if t, err := time.Parse(time.RFC3339, gr.PublishedAt); err == nil {
		rel.PublishedAt = t.UTC()
	}
	return rel, v, nil
}

// notes keeps at most maxNotes bytes of valid UTF-8.
func notes(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= maxNotes {
		return s
	}
	cut := maxNotes
	for cut > 0 && s[cut]&0xC0 == 0x80 { // do not split a rune
		cut--
	}
	return s[:cut] + "\n…"
}

var errNotFound = errors.New("not found")

// getJSON fetches an API document (≤ 2 MiB).
func (c *Client) getJSON(ctx context.Context, u string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", c.userAgent())
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("release information is not reachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponse+1))
	if err != nil {
		return fmt.Errorf("release information is not reachable: %w", err)
	}
	if len(b) > maxAPIResponse {
		return errors.New("the release information from GitHub is larger than 2 MiB")
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("the release information from GitHub is invalid: %w", err)
	}
	return nil
}

func (c *Client) userAgent() string { return cmp.Or(c.UserAgent, "PiCache/"+version.Version) }

// statusError explains an HTTP error of the GitHub API.
func statusError(resp *http.Response) error {
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("release information is not reachable (HTTP 404: the repository %s is private, "+
			"has no releases or does not exist): %w", Repository, errNotFound)
	case (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) &&
		resp.Header.Get("X-RateLimit-Remaining") == "0":
		return fmt.Errorf("release information is not reachable (HTTP %d: GitHub rate limit, try again later)", resp.StatusCode)
	}
	return fmt.Errorf("release information is not reachable (HTTP %d)", resp.StatusCode)
}

// Files opens the files of one release by name.
type Files interface {
	// Open returns the file and its size (-1 if unknown).
	Open(ctx context.Context, name string) (io.ReadCloser, int64, error)
	// Describe names the source for messages.
	Describe() string
}

// Files returns the release files of ver on GitHub.
func (c *Client) Files(ver string) Files { return &githubFiles{c: c, version: ver} }

type githubFiles struct {
	c       *Client
	version string
}

func (g *githubFiles) Describe() string { return "GitHub release " + g.version }

func (g *githubFiles) Open(ctx context.Context, name string) (io.ReadCloser, int64, error) {
	if _, err := ParseVersion(g.version); err != nil {
		return nil, 0, err
	}
	u := fmt.Sprintf("%s/%s/releases/download/%s/%s", g.c.downloadBase(), Repository, url.PathEscape(g.version), url.PathEscape(name))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", g.c.userAgent())
	resp, err := g.c.httpClient().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("download %s: %w", name, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("download %s: HTTP %d", name, resp.StatusCode)
	}
	return resp.Body, resp.ContentLength, nil
}

// DirFiles returns release files from a local directory (`picache update
// --from`). Only regular files are opened, never symbolic links.
func DirFiles(dir string) Files { return dirFiles(dir) }

type dirFiles string

func (d dirFiles) Describe() string { return string(d) }

func (d dirFiles) Open(_ context.Context, name string) (io.ReadCloser, int64, error) {
	r, err := os.OpenRoot(string(d))
	if err != nil {
		return nil, 0, err
	}
	defer r.Close()
	fi, err := r.Lstat(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, fmt.Errorf("%s is missing in %s", name, d)
		}
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", filepath.Join(string(d), name))
	}
	f, err := r.OpenFile(name, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

// readLimited reads a whole release file of at most limit bytes.
func readLimited(ctx context.Context, files Files, name string, limit int64) ([]byte, error) {
	rc, size, err := files.Open(ctx, name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if size > limit {
		return nil, fmt.Errorf("%s is larger than %d KiB", name, limit>>10)
	}
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d KiB", name, limit>>10)
	}
	return b, nil
}
