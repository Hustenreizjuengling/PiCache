package prefillapi

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"
)

// Bounds of the client.
const (
	maxRequest  = 64 << 10
	maxResponse = 8 << 20
	callTimeout = 30 * time.Second
)

// ErrNotRunning says that nothing listens on the socket: the prefill service
// is not installed or not started.
var ErrNotRunning = errors.New("the prefill service is not running")

// Client talks to the prefill service over its Unix socket.
type Client struct {
	socket string
	hc     *http.Client
}

// NewClient returns a client for the socket at path.
func NewClient(path string) *Client {
	d := &net.Dialer{Timeout: 3 * time.Second}
	return &Client{
		socket: path,
		hc: &http.Client{
			Timeout: callTimeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return d.DialContext(ctx, "unix", path)
				},
				MaxIdleConns:      2,
				IdleConnTimeout:   30 * time.Second,
				DisableKeepAlives: false,
				Proxy:             nil,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Socket is the socket's path.
func (c *Client) Socket() string { return c.socket }

// Installed reports whether the socket exists (the service is installed
// and was started at least once since boot).
func (c *Client) Installed() bool {
	fi, err := os.Lstat(c.socket)
	return err == nil && fi.Mode().Type() == fs.ModeSocket
}

// Response is a raw answer: its status and body (JSON).
type Response struct {
	Status int
	Body   []byte
}

// Do sends one request; body is marshalled to JSON unless it is nil or
// already a []byte. A missing or refused socket is ErrNotRunning.
func (c *Client) Do(ctx context.Context, method, path string, body any) (Response, error) {
	var rd io.Reader
	if body != nil {
		b, ok := body.([]byte)
		if !ok {
			var err error
			if b, err = json.Marshal(body); err != nil {
				return Response{}, err
			}
		}
		if len(b) > maxRequest {
			return Response{}, fmt.Errorf("prefill request too large (%d bytes)", len(b))
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://prefill"+path, rd)
	if err != nil {
		return Response{}, err
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
			return Response{}, ErrNotRunning
		}
		return Response{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return Response{}, err
	}
	if len(b) > maxResponse {
		return Response{}, errors.New("prefill answer too large")
	}
	return Response{Status: resp.StatusCode, Body: b}, nil
}

// Status fetches GET /v1/status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	r, err := c.Do(ctx, http.MethodGet, "/v1/status", nil)
	if err != nil {
		return st, err
	}
	if r.Status != http.StatusOK {
		return st, fmt.Errorf("prefill status: HTTP %d", r.Status)
	}
	return st, json.Unmarshal(r.Body, &st)
}
