package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/hustenreizjuengling/picache/internal/config"
)

// The listeners of the command line tools (docs/ARCHITECTURE.md 2
// "Listeners"): healthcheck, the update helper's health wait and the API
// client's base URL use config.EffectiveListeners (PICACHE_*_LISTEN >
// listeners.json, what the running process bound from the saved set >
// the default), so a listener moved in the web UI is found.

// cliListeners returns the effective listeners; problems with
// listeners.json are printed as warnings (never its content).
func cliListeners() config.Listeners {
	dataDir := ""
	if cfg, err := config.LoadWithoutSecrets(os.Getenv); err == nil {
		dataDir = cfg.DataDir
	}
	l, warnings := config.EffectiveListeners(os.Getenv, dataDir)
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "picache: warning:", w)
	}
	return l
}

// localURL is the /healthz URL of the local web listener: the first web
// listener on all addresses or on loopback (else the first such HTTPS
// listener), reached on loopback. The API client sends a token over plain
// http to loopback only, and a saved set of listeners always keeps such an
// address (PUT /system/listeners). Without one: the first web listener
// (else the first HTTPS listener) as it is.
func localURL() string {
	l := cliListeners()
	web, webTLS := l.Roles[config.RoleWeb], l.Roles[config.RoleWebTLS]
	for _, c := range []struct {
		scheme string
		addrs  []string
	}{{"http", web}, {"https", webTLS}} {
		for _, a := range c.addrs {
			if h, p, ok := loopbackFor(a); ok {
				if ip, err := netip.ParseAddr(h); err == nil && ip.IsLoopback() {
					return c.scheme + "://" + net.JoinHostPort(h, p) + "/healthz"
				}
			}
		}
	}
	if len(web) > 0 {
		if h, p, ok := loopbackFor(web[0]); ok {
			return "http://" + net.JoinHostPort(h, p) + "/healthz"
		}
	}
	if len(webTLS) > 0 {
		if h, p, ok := loopbackFor(webTLS[0]); ok {
			return "https://" + net.JoinHostPort(h, p) + "/healthz"
		}
	}
	return "http://127.0.0.1:8080/healthz"
}

// dnsAddr is the address of the first DNS listener on loopback.
func dnsAddr() (string, error) {
	dns := cliListeners().Roles[config.RoleDNS]
	if len(dns) == 0 {
		return "", errors.New("no DNS listener is configured")
	}
	h, p, ok := loopbackFor(dns[0])
	if !ok {
		return "", fmt.Errorf("cannot parse the DNS listener %q", dns[0])
	}
	return net.JoinHostPort(h, p), nil
}

const listenersUsage = "usage: picache listeners --reset"

// listenersCmd is `picache listeners --reset`: the saved listeners
// (listeners.json, listeners.next.json) are removed, so the next start
// uses the environment and the defaults. As root it switches to the owner
// of the data directory first.
func listenersCmd(args []string) int {
	if len(args) != 1 || (args[0] != "--reset" && args[0] != "-reset") {
		fmt.Fprintln(os.Stderr, listenersUsage)
		return 2
	}
	cfg, err := config.LoadWithoutSecrets(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	if err := becomeOwnerOf(cfg.DataDir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, name := range []string{config.ListenersNextFile, config.ListenersFile} {
		err := os.Remove(filepath.Join(cfg.DataDir, name))
		switch {
		case err == nil, errors.Is(err, fs.ErrNotExist):
		case errors.Is(err, fs.ErrPermission):
			fmt.Fprintln(os.Stderr, "permission denied: run `sudo picache listeners --reset`")
			return 1
		default:
			fmt.Fprintln(os.Stderr, "listeners:", err)
			return 1
		}
	}
	fmt.Println("the listeners use the environment and the defaults from the next start: systemctl restart picache")
	return 0
}
