// Command picache is the PiCache daemon and admin CLI.
//
//	picache [serve] [flags]               run the server (default)
//	picache version                       print version information
//	picache healthcheck [url]             exit 0 if the local web UI and DNS answer
//	picache reset-password [user]         set a new password for an existing account (reads it from stdin)
//	picache setup-token                   print the first-run setup token
//	picache storage apply <id>            (root) write a systemd mount unit for a NAS target
//	picache storage apply-pending         (root) process mount requests queued by the web UI
//	picache storage remove <id>           (root) remove the mount unit and credentials of a NAS target
//	picache update --check                check for a newer release
//	picache update [flags]                (root) install a release (signed, rolled back if unhealthy)
//	picache update apply-pending          (root) install an update queued by the web UI
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	neturl "net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/app"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/storage"
	"github.com/hustenreizjuengling/picache/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cmd := "serve"
	if len(args) > 0 {
		if c, ok := commandFlags[args[0]]; ok {
			cmd, args = c, args[1:]
		} else if !strings.HasPrefix(args[0], "-") {
			cmd, args = args[0], args[1:]
		}
	}
	if cmd != "version" && cmd != "help" {
		if err := loadEnvFile(); err != nil {
			fmt.Fprintln(os.Stderr, "picache:", err)
			return 2
		}
	}
	switch cmd {
	case "serve":
		return serve(args)
	case "version":
		v := version.Get()
		fmt.Printf("picache %s (commit %s, built %s, %s, %s/%s)\n", v.Version, v.Commit, v.Date, v.GoVersion, v.OS, v.Arch)
		return 0
	case "healthcheck":
		return healthcheck(args)
	case "reset-password":
		return resetPassword(args)
	case "setup-token":
		return setupToken()
	case "storage":
		return storageCmd(args)
	case "update":
		return updateCmd(args)
	case "help":
		fmt.Println(strings.TrimSpace(usage))
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s\n", cmd, strings.TrimSpace(usage))
		return 2
	}
}

// commandFlags are flags that stand for a command; any other first argument
// starting with "-" is a flag of serve.
var commandFlags = map[string]string{
	"--version": "version", "-version": "version",
	"--help": "help", "-help": "help", "-h": "help",
}

const usage = `
usage: picache [command] [flags]

commands:
  serve                         run PiCache (default)
  version, --version            print version information
  help, --help, -h              print this help
  healthcheck [url]             check the local web endpoint and DNS
  reset-password [user]         set a new password for user (default admin), read from stdin;
                                disables TOTP, signs out all sessions and revokes all API tokens.
                                Unknown names are refused (a user is created only if none exists)
  setup-token                   print the first-run setup token
  storage apply <id>            (root) mount a NAS storage target via a systemd mount unit
                                [--password-stdin] reads the NAS password from stdin
  storage apply-pending         (root) process mount requests queued by the web UI
  storage remove <id>           (root) unmount a NAS storage target and remove its mount unit and credentials
  update --check [--prerelease] check GitHub for a newer release
                                (exit code 0: up to date, 10: update available, 1: error)
  update [flags]                (root) install the newest release: verify its signature,
                                replace the binary, restart PiCache, roll back if it is not healthy.
                                [--version vX.Y.Z] [--prerelease] [--allow-downgrade] [--yes];
                                --from DIR installs from downloaded release files
  update apply-pending          (root) install an update queued in the web UI (picache-update.service)

Configuration is read from PICACHE_* environment variables and, for all
commands, from /etc/picache/picache.env (or $PICACHE_ENV_FILE) if present.
See docs/DEPLOYMENT.md.
`

// loadEnvFile applies KEY=VALUE lines from $PICACHE_ENV_FILE or
// /etc/picache/picache.env. Variables already set in the environment win.
func loadEnvFile() error {
	path := os.Getenv("PICACHE_ENV_FILE")
	explicit := path != ""
	if !explicit {
		path = "/etc/picache/picache.env"
	}
	f, err := os.Open(path)
	if err != nil {
		if !explicit && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission)) {
			return nil
		}
		return fmt.Errorf("env file: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(strings.TrimPrefix(k, "export "))
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if !strings.HasPrefix(k, "PICACHE_") {
			continue
		}
		if _, set := os.LookupEnv(k); !set {
			_ = os.Setenv(k, v)
		}
	}
	return sc.Err()
}

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
	if errors.Is(err, flag.ErrHelp) { // picache serve -h
		fmt.Println(strings.TrimSpace(usage))
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "picache: configuration error:", err)
		return 2
	}
	log := newLogger(cfg)
	slog.SetDefault(log)
	setMemoryLimit(log)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = app.Run(ctx, cfg, log)
	switch {
	case errors.Is(err, app.ErrRestart):
		return app.ExitRestart
	case err != nil:
		log.Error("picache exited with error", slog.Any("err", err))
		return 1
	}
	return 0
}

// setMemoryLimit sets a soft memory limit of 60 % of the cgroup limit (or of
// the system RAM) unless GOMEMLIMIT is set, so the GC works harder before
// the kernel OOM killer takes DNS down on small machines.
func setMemoryLimit(log *slog.Logger) {
	if os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	limit := memoryLimit()
	if limit <= 0 {
		return
	}
	soft := limit * 6 / 10
	debug.SetMemoryLimit(soft)
	log.Info("memory limit", slog.Int64("soft_limit_mib", soft>>20), slog.Int64("available_mib", limit>>20))
}

func healthcheck(args []string) int {
	var url string
	var own bool
	if len(args) > 0 {
		url = args[0]
	} else {
		url, own = localURL(), true
	}
	if err := webCheck(context.Background(), url, own); err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy (web):", err)
		return 1
	}
	if len(args) == 0 {
		if err := dnsCheck(); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy (dns):", err)
			return 1
		}
	}
	return 0
}

// localHealth runs the checks of `picache healthcheck` without arguments:
// the local web listener and the DNS probe (the health wait of an update).
func localHealth(ctx context.Context) error {
	if err := webCheck(ctx, localURL(), true); err != nil {
		return fmt.Errorf("web: %w", err)
	}
	if err := dnsCheck(); err != nil {
		return fmt.Errorf("dns: %w", err)
	}
	return nil
}

// webCheck expects "ok" from url (/healthz). own: url is PiCache's own
// listener derived from its configuration.
func webCheck(ctx context.Context, url string, own bool) error {
	c := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		// PiCache's own listener (derived from its configuration) usually
		// has a self-signed certificate that cannot be verified; the check
		// only tests liveness and sends nothing secret. An explicit URL is
		// verified unless it points to the loopback interface.
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: own || isLoopbackURL(url)}, //nolint:gosec
		DisableKeepAlives: true,
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "ok" {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// isLoopbackURL reports whether url targets a loopback address or localhost.
func isLoopbackURL(raw string) bool {
	u, err := neturl.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

func loopbackFor(addr string) (string, string, bool) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return "", "", false
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return host, port, true
}

func localURL() string {
	listen := os.Getenv("PICACHE_WEB_LISTEN")
	if listen == "" {
		listen = ":8080"
	}
	if strings.EqualFold(listen, "off") || strings.EqualFold(listen, "none") {
		tlsListen := os.Getenv("PICACHE_WEB_TLS_LISTEN")
		if tlsListen == "" {
			tlsListen = ":8443"
		}
		if h, p, ok := loopbackFor(strings.Split(tlsListen, ",")[0]); ok {
			return "https://" + net.JoinHostPort(h, p) + "/healthz"
		}
	}
	if h, p, ok := loopbackFor(strings.Split(listen, ",")[0]); ok {
		return "http://" + net.JoinHostPort(h, p) + "/healthz"
	}
	return "http://127.0.0.1:8080/healthz"
}

// dnsCheck asks the local DNS listener for the health probe name, which
// PiCache answers with 127.0.0.1 without upstreams and without counting or
// logging the query (a different server on the port answers NXDOMAIN).
func dnsCheck() error {
	listen := os.Getenv("PICACHE_DNS_LISTEN")
	if listen == "" {
		listen = ":53"
	}
	h, p, ok := loopbackFor(strings.Split(listen, ",")[0])
	if !ok {
		return fmt.Errorf("cannot parse PICACHE_DNS_LISTEN %q", listen)
	}
	m := new(dns.Msg).SetQuestion(dnsserver.HealthProbeName, dns.TypeA)
	c := &dns.Client{Timeout: 2 * time.Second}
	r, _, err := c.Exchange(m, net.JoinHostPort(h, p))
	if err != nil {
		return err
	}
	for _, rr := range r.Answer {
		if a, ok := rr.(*dns.A); ok && a.A.IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("unexpected answer (rcode %s)", dns.RcodeToString[r.Rcode])
}

// configDB loads the configuration and makes sure the database exists (the
// CLI never creates an empty database by accident). When run as root it
// switches to the owner of the data directory first.
func configDB() (*config.Config, string, error) {
	cfg, err := config.Load(nil, os.Getenv)
	if err != nil {
		return nil, "", fmt.Errorf("configuration error: %w", err)
	}
	path := cfg.Paths().ConfigDB
	if _, err := os.Stat(path); err != nil {
		return nil, "", fmt.Errorf("no PiCache database at %s (check PICACHE_DATA_DIR or /etc/picache/picache.env): %w", path, err)
	}
	return cfg, path, nil
}

func resetPassword(args []string) int {
	user := "admin"
	if len(args) > 0 {
		user = args[0]
	}
	cfg, path, err := configDB()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := becomeOwnerOf(cfg.DataDir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	d, err := db.Open(path, 1)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer d.Close()
	ctx := context.Background()
	// Refuse an unknown name before asking for the password: resetting must
	// never add a second admin while the real account stays unchanged.
	names, err := auth.Usernames(ctx, d)
	if err != nil && !strings.Contains(err.Error(), "no such table") {
		fmt.Fprintln(os.Stderr, "reset password:", err)
		return 1
	}
	if len(names) > 0 && !slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, user) }) {
		fmt.Fprintf(os.Stderr, "reset password: there is no account named %q. Existing accounts: %s\n", user, strings.Join(names, ", "))
		fmt.Fprintln(os.Stderr, "Run `picache reset-password <name>` with one of them.")
		return 2
	}
	fmt.Fprintf(os.Stderr, "New password for %q (min. 10 characters): ", user)
	pw, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, "read password:", err)
		return 1
	}
	pw = strings.TrimRight(pw, "\r\n")
	// This is the recovery path after a compromise: remove what could keep
	// revoked credentials alive first (PiCache creates no triggers or views;
	// the service also removes them at start).
	dropped, err := db.DropTriggersAndViews(ctx, d.W)
	for _, o := range dropped {
		fmt.Fprintf(os.Stderr, "Removed the %s from the database (PiCache creates no triggers or views; it was planted).\n", o)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "reset password:", err)
		return 1
	}
	res, err := auth.ResetPassword(ctx, d, user, pw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reset password:", err)
		return 1
	}
	fmt.Fprint(os.Stderr, resetSummary(res))
	return 0
}

// resetSummary describes exactly what reset-password changed.
func resetSummary(r auth.ResetResult) string {
	var b strings.Builder
	if r.Created {
		fmt.Fprintf(&b, "No account existed: created the account %q with the new password.\n", r.Username)
	} else {
		fmt.Fprintf(&b, "Password set for %q.\n", r.Username)
	}
	if r.TOTPDisabled {
		b.WriteString("Two-factor authentication was disabled; set it up again after signing in.\n")
	} else if !r.Created {
		b.WriteString("Two-factor authentication was not enabled.\n")
	}
	fmt.Fprintf(&b, "%d session(s) signed out and %d API token(s) revoked.\n", r.SessionsRevoked, r.TokensRevoked)
	return b.String()
}

func setupToken() int {
	cfg, err := config.Load(nil, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	b, err := os.ReadFile(cfg.Paths().SetupTokenFile)
	switch {
	case errors.Is(err, fs.ErrPermission):
		fmt.Fprintln(os.Stderr, "permission denied: run `sudo picache setup-token` or `docker exec -u 65532:65532 <container> /picache setup-token`")
		return 1
	case err != nil:
		fmt.Fprintln(os.Stderr, "no setup token (setup already completed, or PiCache has not been started yet)")
		return 1
	}
	fmt.Println(strings.TrimSpace(string(b)))
	return 0
}

const storageUsage = "usage: picache storage apply <target-id> [--password-stdin] | picache storage apply-pending | picache storage remove <target-id>"

func storageCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, storageUsage)
		return 2
	}
	ctx := context.Background()
	if args[0] == "remove" {
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, storageUsage)
			return 2
		}
		// No database needed: this also cleans up after a deleted target.
		cfg, err := config.Load(nil, os.Getenv)
		if err != nil {
			fmt.Fprintln(os.Stderr, "configuration error:", err)
			return 2
		}
		if err := storage.RemoveHost(ctx, cfg, args[1], newLogger(cfg)); err != nil {
			fmt.Fprintln(os.Stderr, "storage remove:", err)
			return 1
		}
		return 0
	}
	cfg, _, err := configDB()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	log := newLogger(cfg)
	switch {
	case args[0] == "apply-pending" && len(args) == 1:
		if err := storage.ApplyPending(ctx, cfg, log); err != nil {
			fmt.Fprintln(os.Stderr, "storage apply-pending:", err)
			return 1
		}
		return 0
	case args[0] == "apply" && (len(args) == 2 || (len(args) == 3 && args[2] == "--password-stdin")):
		var pw []byte
		if len(args) == 3 {
			fmt.Fprint(os.Stderr, "NAS password: ")
			line, err := bufio.NewReader(os.Stdin).ReadString('\n')
			if err != nil && err != io.EOF {
				fmt.Fprintln(os.Stderr, "read password:", err)
				return 1
			}
			pw = []byte(strings.TrimRight(line, "\r\n"))
		}
		if err := storage.ApplyHost(ctx, cfg, args[1], pw, log); err != nil {
			fmt.Fprintln(os.Stderr, "storage apply:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(os.Stderr, storageUsage)
	return 2
}
