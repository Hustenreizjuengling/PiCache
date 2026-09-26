package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/config"
)

// The CLI's API client (docs/DEPLOYMENT.md "Command line"; also used by
// later commands): the token comes from PICACHE_TOKEN in the process
// environment or --token-file (never from picache.env), and is sent only to
// a loopback http URL or to an https URL whose certificate is verified
// against the system roots plus PiCache's local CA (<data>/tls/ca.crt). A
// loopback https URL without a readable CA is not verified (like
// healthcheck). No proxy, no redirects.

const maxTokenFile = 4 << 10

// errRefused is the refusal to send the token to an URL.
type errRefused struct{ url string }

func (e errRefused) Error() string { return "refusing to send the API token to " + e.url }

// apiClient calls the PiCache API with a bearer token.
type apiClient struct {
	base  *neturl.URL // scheme://host[:port][/prefix]
	token string
	http  *http.Client
}

// readTokenFile reads an API token from a regular file (never through a
// symbolic link, at most 4 KiB, trimmed); it warns on stderr when group or
// others can read the file.
func readTokenFile(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("token file: %s is not a regular file", path)
	}
	if fi.Size() > maxTokenFile {
		return "", fmt.Errorf("token file: %s is larger than 4 KiB", path)
	}
	if fi.Mode().Perm()&0o044 != 0 && tokenFileWarnings {
		fmt.Fprintf(os.Stderr, "picache: warning: %s can be read by other users (chmod 600 it)\n", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxTokenFile+1))
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("token file: %s is empty", path)
	}
	return tok, nil
}

// tokenFileWarnings is false on systems without Unix permissions.
var tokenFileWarnings = filepath.Separator == '/'

// baseURL returns the API base URL: --url, else PICACHE_URL, else the
// local web listener (loopback, like healthcheck).
func baseURL(flagURL string) string {
	if flagURL != "" {
		return flagURL
	}
	if u := os.Getenv("PICACHE_URL"); u != "" {
		return u
	}
	return strings.TrimSuffix(localURL(), "/healthz")
}

// newAPIClient checks where the token may go and builds the client. The
// error is a usage error (exit 2) when usage is true.
func newAPIClient(flagURL, tokenFile string) (c *apiClient, usage bool, err error) {
	token := ""
	if tokenFile != "" {
		if token, err = readTokenFile(tokenFile); err != nil {
			return nil, false, err
		}
	} else if token = strings.TrimSpace(os.Getenv("PICACHE_TOKEN")); token == "" {
		return nil, true, errors.New("no API token: set PICACHE_TOKEN or use --token-file (a read token is enough)")
	}
	raw := baseURL(flagURL)
	u, err := neturl.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, true, fmt.Errorf("invalid URL %q: use http(s)://host[:port]", raw)
	}
	u.RawQuery, u.Fragment = "", ""
	u.Path = strings.TrimSuffix(u.Path, "/")
	loopback := isLoopbackURL(u.String())
	tr := &http.Transport{Proxy: nil, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second}
	switch {
	case u.Scheme == "http" && !loopback:
		return nil, false, errRefused{u.Redacted()}
	case u.Scheme == "https":
		roots, _ := x509.SystemCertPool()
		if roots == nil {
			roots = x509.NewCertPool()
		}
		ca := false
		if cfg, err := config.LoadWithoutSecrets(os.Getenv); err == nil {
			if pem, err := os.ReadFile(filepath.Join(cfg.DataDir, "tls", "ca.crt")); err == nil {
				ca = roots.AppendCertsFromPEM(pem)
			}
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
		if loopback && !ca {
			// This machine's own listener, certificate unknown: like
			// healthcheck, the connection cannot leave the host.
			tr.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec
		}
	}
	return &apiClient{base: u, token: token, http: &http.Client{Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, false, nil
}

// get sends GET <base><path>?<query> with the token.
func (c *apiClient) get(ctx context.Context, path string, q neturl.Values, accept string) (*http.Response, error) {
	u := *c.base
	u.Path += path
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	return c.http.Do(req)
}

// apiError returns the message of an API error answer. A redirect (never
// followed: the token must not travel on) names where PiCache points, e.g.
// its HTTPS listener while "Redirect HTTP to HTTPS" is on.
func apiError(resp *http.Response) error {
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		to := "another address"
		if u, err := resp.Location(); err == nil && u.Host != "" {
			to = u.Scheme + "://" + u.Host
		}
		return fmt.Errorf("HTTP %d: PiCache redirects to %s; pass that address with --url or PICACHE_URL",
			resp.StatusCode, escapeControls(to))
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var body struct {
		Error struct {
			Message string `json:"message"`
			Field   string `json:"field"`
		} `json:"error"`
	}
	msg := ""
	if json.Unmarshal(bytes.TrimSpace(b), &body) == nil && body.Error.Message != "" {
		msg = body.Error.Message
		if body.Error.Field != "" {
			msg = body.Error.Field + ": " + msg
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(string(b))
		if len(msg) > 200 {
			msg = msg[:200]
		}
	}
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, escapeControls(msg))
}

// createExclusive creates path for writing: never an existing path, never
// through a symbolic link, mode 0600.
func createExclusive(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("%s exists; choose a new file name", path)
	}
	return f, err
}
