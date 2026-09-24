// Command picache is the PiCache daemon and admin CLI.
//
//	picache [serve] [flags]          run the server (default)
//	picache version                  print version information
//	picache healthcheck [url]        exit 0 if the local web UI answers /healthz
//	picache reset-password [user]    set a new admin password (reads it from stdin)
//	picache setup-token              print the first-run setup token
//	picache storage apply <id>       (root) write a systemd mount unit for a NAS target
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hustenreizjuengling/picache/internal/app"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/storage"
	"github.com/hustenreizjuengling/picache/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve(args)
	case "version", "--version":
		v := version.Get()
		fmt.Printf("picache %s (commit %s, built %s, %s, %s/%s)\n", v.Version, v.Commit, v.Date, v.GoVersion, v.OS, v.Arch)
		return 0
	case "healthcheck":
		return healthcheck(args)
	case "reset-password":
		return resetPassword(args)
	case "setup-token":
		return setupToken(args)
	case "storage":
		return storageCmd(args)
	case "help", "-h", "--help":
		fmt.Println(strings.TrimSpace(usage))
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s\n", cmd, strings.TrimSpace(usage))
		return 2
	}
}

const usage = `
usage: picache [command] [flags]

commands:
  serve                  run PiCache (default)
  version                print version information
  healthcheck [url]      check the local web endpoint (default http://127.0.0.1:8080/healthz)
  reset-password [user]  set a new password for user (default admin); reads it from stdin
  setup-token            print the first-run setup token
  storage apply <id>     (root) mount a NAS storage target via a systemd mount unit

Configuration is read from PICACHE_* environment variables; see docs/DEPLOYMENT.md.
`

func newLogger(cfg *config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var h slog.Handler
	if cfg.LogFormat == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h)
}

func serve(args []string) int {
	cfg, err := config.Load(args, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "picache: configuration error:", err)
		return 2
	}
	log := newLogger(cfg)
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx, cfg, log); err != nil {
		log.Error("picache exited with error", slog.Any("err", err))
		return 1
	}
	return 0
}

func healthcheck(args []string) int {
	url := "http://127.0.0.1:8080/healthz"
	if len(args) > 0 {
		url = args[0]
	} else if v := os.Getenv("PICACHE_WEB_LISTEN"); v != "" {
		addr := strings.Split(v, ",")[0]
		if _, port, err := net.SplitHostPort(strings.TrimSpace(addr)); err == nil {
			url = "http://127.0.0.1:" + port + "/healthz"
		}
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "ok" {
		fmt.Fprintf(os.Stderr, "unhealthy: HTTP %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

func resetPassword(args []string) int {
	user := "admin"
	if len(args) > 0 {
		user = args[0]
	}
	cfg, err := config.Load(nil, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	fmt.Fprintf(os.Stderr, "New password for %q (min. 10 characters): ", user)
	pw, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, "read password:", err)
		return 1
	}
	pw = strings.TrimRight(pw, "\r\n")
	d, err := db.Open(cfg.Paths().ConfigDB, 1)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer d.Close()
	if err := auth.ResetPassword(context.Background(), d, user, pw); err != nil {
		fmt.Fprintln(os.Stderr, "reset password:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "Password updated; all sessions were signed out.")
	return 0
}

func setupToken(args []string) int {
	cfg, err := config.Load(args, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	b, err := os.ReadFile(cfg.Paths().SetupTokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "no setup token (setup already completed, or PiCache has not been started yet)")
		return 1
	}
	fmt.Println(strings.TrimSpace(string(b)))
	return 0
}

func storageCmd(args []string) int {
	if len(args) != 2 || args[0] != "apply" {
		fmt.Fprintln(os.Stderr, "usage: picache storage apply <target-id>")
		return 2
	}
	cfg, err := config.Load(nil, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	log := newLogger(cfg)
	if err := storage.ApplyHost(context.Background(), cfg, args[1], log); err != nil {
		fmt.Fprintln(os.Stderr, "storage apply:", err)
		return 1
	}
	return 0
}
