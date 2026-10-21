//go:build liveepic

package epic

// Manual live test against Epic: the device-code sign-in starts (nobody
// confirms it) and its client session is ended.
//
//	go test -tags liveepic -run TestLive -v ./internal/prefill/epic/
//
// Every host is resolved through public DNS over HTTPS and dialled by
// address, never through the local resolver.

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

func dohResolve(ctx context.Context, host string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://cloudflare-dns.com/dns-query?name="+host+"&type=A", nil)
	req.Header.Set("Accept", "application/dns-json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var doc struct {
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		} `json:"Answer"`
	}
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", err
	}
	for _, a := range doc.Answer {
		if a.Type == 1 {
			return a.Data, nil
		}
	}
	return "", fmt.Errorf("no A record for %s", host)
}

func pinnedDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ip, err := dohResolve(ctx, host)
	if err != nil {
		return nil, err
	}
	return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip, port))
}

func TestLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := New(Options{DialContext: pinnedDial, Log: log, Used: func(h, p, why string) { t.Logf("used %s://%s (%s)", p, h, why) }})
	l, err := c.StartDeviceLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("code %s at %s, interval %s, expires %s", l.UserCode, l.VerificationURL, l.Interval(), l.ExpiresAt.Format(time.TimeOnly))
	s, err := l.Poll(ctx)
	if err != nil || s != nil {
		t.Fatalf("an unconfirmed code: %v %v", s, err)
	}
	t.Log("the code waits for its confirmation, as expected")
}
