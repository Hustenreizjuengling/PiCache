// Package config holds the bootstrap configuration: paths, listen addresses
// and process-level options that are read once at start from environment
// variables (PICACHE_*) and command-line flags. Everything else is a runtime
// setting stored in the database (package settings) and edited in the UI.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Config is the bootstrap configuration. It is immutable after Load.
type Config struct {
	DataDir  string // PICACHE_DATA_DIR: databases, keys, lists (local disk!)
	CacheDir string // PICACHE_CACHE_DIR: default local cache store

	DNSListen    []string // PICACHE_DNS_LISTEN, default ":53"
	CacheListen  []string // PICACHE_CACHE_LISTEN, default ":80"
	SNIListen    []string // PICACHE_SNI_LISTEN, default ":443" (empty disables)
	WebListen    []string // PICACHE_WEB_LISTEN, default ":8080"
	WebTLSListen []string // PICACHE_WEB_TLS_LISTEN, default ":8443" (empty disables)
	// DoTListen (PICACHE_DOT_LISTEN, default ":853") serves DNS over TLS
	// while dns.encrypted.dot is on (bound either way, so the switch needs
	// no restart); DoHListen (PICACHE_DOH_LISTEN, default off) serves only
	// DNS over HTTPS (/dns-query) while dns.encrypted.doh is on.
	DoTListen []string
	DoHListen []string
	// NTPListen (PICACHE_NTP_LISTEN, default off, e.g. ":123"): the NTP
	// server's UDP addresses, bound at start; ntp.enabled switches
	// answering without a restart.
	NTPListen []string
	// ListenerLock names, per role of the listeners files (RoleDNS, …),
	// what set it on the host: "flag" or the variable (PICACHE_DNS_LISTEN,
	// …). A locked role ignores the listeners files and cannot be changed
	// in the UI; the Listen members above hold the flag, environment or
	// default values (the fallback of a saved role that cannot be bound).
	ListenerLock   map[string]string
	WebTLSCertFile string   // PICACHE_WEB_TLS_CERT (optional, else self-signed)
	WebTLSKeyFile  string   // PICACHE_WEB_TLS_KEY
	WebHosts       []string // PICACHE_WEB_HOSTS: extra allowed Host names for the UI
	// WebSecureCookies (PICACHE_WEB_SECURE_COOKIES) sets the Secure flag on
	// the session and device cookies for plain-HTTP requests too: for a
	// TLS-terminating reverse proxy in front of the HTTP listener. Requests
	// over TLS always get it.
	WebSecureCookies bool

	// ConfigLocked (PICACHE_CONFIG_LOCKED, default off) refuses
	// configuration changes from interactive sessions (the API answers
	// config_locked); admin API tokens still write. For infrastructure as
	// code: it is not an access control against admins.
	ConfigLocked bool
	// DestructiveAPI (PICACHE_DESTRUCTIVE_API, default on): off refuses the
	// destructive API routes (restore, DHCP reset, purges, …) for every
	// principal.
	DestructiveAPI bool

	RunAs string // PICACHE_RUN_AS "uid:gid": drop privileges after binding when started as root

	LogLevel  slog.Level // PICACHE_LOG_LEVEL: debug|info|warn|error
	LogFormat string     // PICACHE_LOG_FORMAT: text|json
	// LogFile (PICACHE_LOG_FILE, default off): the application log is also
	// written to this file (an absolute path of a <name>.log file below
	// /var/log/picache, directly in the data directory or below its logs/;
	// LogFileAllowed), rotated at 10 MiB.
	LogFile string
	// LogSyslogNetwork and LogSyslogAddr (PICACHE_LOG_SYSLOG
	// udp://host:port or tcp://host:port, default off): the application log
	// is also sent to this syslog server (RFC 5424).
	LogSyslogNetwork string
	LogSyslogAddr    string
	// PProf (PICACHE_PPROF on|off, default off) serves the Go profiles
	// under /debug/pprof/ to admins from this machine.
	PProf bool
	// InitialConfig (PICACHE_INITIAL_CONFIG) is a settings document applied
	// at the first start only.
	InitialConfig string

	AdminUser string // PICACHE_ADMIN_USER (default "admin"), used with AdminPassword
	// AdminPassword (PICACHE_ADMIN_PASSWORD_FILE preferred, or PICACHE_ADMIN_PASSWORD)
	// provisions the first admin. The app clears it after provisioning.
	AdminPassword        string `json:"-"`
	AdminPasswordFromEnv bool   `json:"-"` // true if the plain env var was used (warn)

	MasterKeyFile string // PICACHE_MASTER_KEY_FILE: optional external master key

	MountRoot string // PICACHE_MOUNT_ROOT: the only place NAS stores may live, default /srv/picache

	// MemoryLimit is the memory PiCache may use in bytes (the cgroup v2
	// limit, else MemTotal rounded up to the machine's nominal size,
	// filter.NominalMemory; 0 = unknown). Not an environment variable:
	// `picache serve` reads it at start; the entry budget of the
	// blocklists follows it (filter.BudgetFor).
	MemoryLimit uint64

	// DHCP (PICACHE_DHCP) decides which DHCP sockets are opened at start,
	// before the privilege drop: unset, the markers the DHCP service keeps
	// in the data directory; off, none ever (the DHCP server cannot be
	// switched on); on (earlier versions' opt-in), all of them.
	DHCP DHCPMode

	Dev bool // PICACHE_DEV: development mode (relaxed platform checks, verbose errors in log)
}

// DHCPMode is the value of PICACHE_DHCP.
type DHCPMode int

const (
	// DHCPAuto (unset): the DHCP server is available and switched on in
	// the web UI; the DHCP service's markers decide what opens at start.
	DHCPAuto DHCPMode = iota
	// DHCPOff (off, no, 0, false, …): opt-out, no DHCP socket is ever opened.
	DHCPOff
	// DHCPOn (on, yes, 1, true, …): the opt-in of versions before 0.8.0,
	// kept for upgrades: every DHCP socket is opened at start and the
	// service closes what the settings do not need.
	DHCPOn
)

// Paths are files and directories derived from DataDir.
type Paths struct {
	ConfigDB        string // picache.db
	LogsDB          string // logs.db
	CacheIndexDir   string // cache-index/
	ListsDir        string // lists/
	CacheDomainsDir string // cache-domains/
	KeysDir         string // keys/
	MasterKeyFile   string // keys/master.key (unless MasterKeyFile is set)
	TLSDir          string // tls/
	SetupTokenFile  string // setup-token
}

// Paths returns the derived paths.
func (c *Config) Paths() Paths {
	d := c.DataDir
	p := Paths{
		ConfigDB:        filepath.Join(d, "picache.db"),
		LogsDB:          filepath.Join(d, "logs.db"),
		CacheIndexDir:   filepath.Join(d, "cache-index"),
		ListsDir:        filepath.Join(d, "lists"),
		CacheDomainsDir: filepath.Join(d, "cache-domains"),
		KeysDir:         filepath.Join(d, "keys"),
		MasterKeyFile:   filepath.Join(d, "keys", "master.key"),
		TLSDir:          filepath.Join(d, "tls"),
		SetupTokenFile:  filepath.Join(d, "setup-token"),
	}
	if c.MasterKeyFile != "" {
		p.MasterKeyFile = c.MasterKeyFile
	}
	return p
}

// defaults returns platform-appropriate defaults. On Linux the FHS locations
// are used; elsewhere (development) directories relative to the working dir.
func defaults() Config {
	c := Config{
		DNSListen:    []string{":53"},
		CacheListen:  []string{":80"},
		SNIListen:    []string{":443"},
		WebListen:    []string{":8080"},
		WebTLSListen: []string{":8443"},
		DoTListen:    []string{":853"},
		ListenerLock: map[string]string{},
		LogLevel:     slog.LevelInfo,
		LogFormat:    "text",
		AdminUser:    "admin",
		MountRoot:    "/srv/picache",

		DestructiveAPI: true,
	}
	if runtime.GOOS == "linux" {
		c.DataDir = "/var/lib/picache"
		c.CacheDir = "/var/cache/picache"
	} else {
		c.DataDir = "data"
		c.CacheDir = "cache"
		c.MountRoot = "mounts"
	}
	return c
}

// Load builds the configuration from defaults, then environment variables,
// then flags (flags win). getenv is usually os.Getenv. args excludes the
// program name and subcommand.
func Load(args []string, getenv func(string) string) (*Config, error) {
	return load(args, getenv, true)
}

// LoadWithoutSecrets is Load for the maintenance commands (setup-token,
// users, reset-password, web-access, update, …): it never reads
// PICACHE_ADMIN_PASSWORD_FILE, which only serve needs, so a root-only
// secret file left in place (Docker) cannot break the recovery commands
// that run as the service user.
func LoadWithoutSecrets(getenv func(string) string) (*Config, error) {
	return load(nil, getenv, false)
}

func load(args []string, getenv func(string) string, secrets bool) (*Config, error) {
	c := defaults()
	if err := c.applyEnv(getenv, secrets); err != nil {
		return nil, err
	}

	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dataDir := fs.String("data-dir", c.DataDir, "data directory (PICACHE_DATA_DIR)")
	cacheDir := fs.String("cache-dir", c.CacheDir, "default cache store directory (PICACHE_CACHE_DIR)")
	dnsListen := fs.String("dns-listen", strings.Join(c.DNSListen, ","), "DNS listen addresses (PICACHE_DNS_LISTEN)")
	cacheListen := fs.String("cache-listen", strings.Join(c.CacheListen, ","), "HTTP cache listen addresses (PICACHE_CACHE_LISTEN)")
	sniListen := fs.String("sni-listen", strings.Join(c.SNIListen, ","), "SNI pass-through listen addresses (PICACHE_SNI_LISTEN)")
	webListen := fs.String("web-listen", strings.Join(c.WebListen, ","), "web UI listen addresses (PICACHE_WEB_LISTEN)")
	webTLSListen := fs.String("web-tls-listen", strings.Join(c.WebTLSListen, ","), "web UI HTTPS listen addresses (PICACHE_WEB_TLS_LISTEN)")
	dotListen := fs.String("dot-listen", strings.Join(c.DoTListen, ","), "DNS over TLS listen addresses (PICACHE_DOT_LISTEN)")
	dohListen := fs.String("doh-listen", strings.Join(c.DoHListen, ","), "DNS over HTTPS listen addresses (PICACHE_DOH_LISTEN)")
	ntpListen := fs.String("ntp-listen", strings.Join(c.NTPListen, ","), "NTP server listen addresses (PICACHE_NTP_LISTEN)")
	logLevel := fs.String("log-level", c.LogLevel.String(), "log level (PICACHE_LOG_LEVEL)")
	dev := fs.Bool("dev", c.Dev, "development mode (PICACHE_DEV)")
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("flags: %w", err)
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	c.DataDir, c.CacheDir, c.Dev = *dataDir, *cacheDir, *dev
	c.DNSListen = splitList(*dnsListen)
	c.CacheListen = splitList(*cacheListen)
	c.SNIListen = splitList(*sniListen)
	c.WebListen = splitList(*webListen)
	c.WebTLSListen = splitList(*webTLSListen)
	c.DoTListen = splitList(*dotListen)
	c.DoHListen = splitList(*dohListen)
	c.NTPListen = splitList(*ntpListen)
	fs.Visit(func(f *flag.Flag) {
		for role, name := range listenerFlag {
			if f.Name == name {
				c.ListenerLock[role] = "flag"
			}
		}
	})
	lvl, err := parseLevel(*logLevel)
	if err != nil {
		return nil, err
	}
	c.LogLevel = lvl

	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyEnv(getenv func(string) string, secrets bool) error {
	str := func(key string, dst *string) {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			*dst = v
		}
	}
	list := func(key string, dst *[]string) {
		// An empty value cannot be distinguished from unset; use "off" to
		// disable a listener.
		if v := getenv(key); v != "" {
			*dst = splitList(v)
			for role, env := range ListenerEnv {
				if env == key {
					c.ListenerLock[role] = key
				}
			}
		}
	}
	str("PICACHE_DATA_DIR", &c.DataDir)
	str("PICACHE_CACHE_DIR", &c.CacheDir)
	list("PICACHE_DNS_LISTEN", &c.DNSListen)
	list("PICACHE_CACHE_LISTEN", &c.CacheListen)
	list("PICACHE_SNI_LISTEN", &c.SNIListen)
	list("PICACHE_WEB_LISTEN", &c.WebListen)
	list("PICACHE_WEB_TLS_LISTEN", &c.WebTLSListen)
	list("PICACHE_DOT_LISTEN", &c.DoTListen)
	list("PICACHE_DOH_LISTEN", &c.DoHListen)
	list("PICACHE_NTP_LISTEN", &c.NTPListen)
	str("PICACHE_LOG_FILE", &c.LogFile)
	str("PICACHE_INITIAL_CONFIG", &c.InitialConfig)
	if v := strings.TrimSpace(getenv("PICACHE_LOG_SYSLOG")); v != "" {
		network, addr, err := ParseSyslogURL(v)
		if err != nil {
			return fmt.Errorf("PICACHE_LOG_SYSLOG: %w", err)
		}
		c.LogSyslogNetwork, c.LogSyslogAddr = network, addr
	}
	if v := getenv("PICACHE_PPROF"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "on":
			c.PProf = true
		case "off":
		default:
			return fmt.Errorf("PICACHE_PPROF must be on or off, got %q", v)
		}
	}
	str("PICACHE_WEB_TLS_CERT", &c.WebTLSCertFile)
	str("PICACHE_WEB_TLS_KEY", &c.WebTLSKeyFile)
	list("PICACHE_WEB_HOSTS", &c.WebHosts)
	str("PICACHE_RUN_AS", &c.RunAs)
	str("PICACHE_LOG_FORMAT", &c.LogFormat)
	str("PICACHE_ADMIN_USER", &c.AdminUser)
	str("PICACHE_MASTER_KEY_FILE", &c.MasterKeyFile)
	str("PICACHE_MOUNT_ROOT", &c.MountRoot)

	if v := getenv("PICACHE_LOG_LEVEL"); v != "" {
		lvl, err := parseLevel(v)
		if err != nil {
			return err
		}
		c.LogLevel = lvl
	}
	for key, dst := range map[string]*bool{"PICACHE_DEV": &c.Dev, "PICACHE_WEB_SECURE_COOKIES": &c.WebSecureCookies,
		"PICACHE_CONFIG_LOCKED": &c.ConfigLocked, "PICACHE_DESTRUCTIVE_API": &c.DestructiveAPI} {
		if v := getenv(key); v != "" {
			b, err := parseSwitch(v)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dst = b
		}
	}
	if v := getenv("PICACHE_DHCP"); v != "" {
		on, err := parseSwitch(v)
		if err != nil {
			return fmt.Errorf("PICACHE_DHCP: %w (unset it to allow the DHCP server, off to prevent it)", err)
		}
		c.DHCP = DHCPOff
		if on {
			c.DHCP = DHCPOn
		}
	}

	// Secrets: prefer *_FILE (Docker secrets) over plain env; read only
	// for serve (LoadWithoutSecrets).
	if !secrets {
		return nil
	}
	if f := getenv("PICACHE_ADMIN_PASSWORD_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("PICACHE_ADMIN_PASSWORD_FILE: %w", err)
		}
		c.AdminPassword = strings.TrimRight(string(b), "\r\n")
	} else if v := getenv("PICACHE_ADMIN_PASSWORD"); v != "" {
		c.AdminPassword = v
		c.AdminPasswordFromEnv = true
	}
	return nil
}

func (c *Config) validate() error {
	var errs []error
	if c.DataDir == "" {
		errs = append(errs, errors.New("data dir must not be empty"))
	}
	if c.CacheDir == "" {
		errs = append(errs, errors.New("cache dir must not be empty"))
	}
	if c.LogFormat != "text" && c.LogFormat != "json" {
		errs = append(errs, fmt.Errorf("log format must be text or json, got %q", c.LogFormat))
	}
	if len(c.DNSListen) == 0 {
		errs = append(errs, errors.New("at least one DNS listen address is required"))
	}
	if len(c.WebListen) == 0 && len(c.WebTLSListen) == 0 {
		errs = append(errs, errors.New("at least one web listen address is required"))
	}
	if (c.WebTLSCertFile == "") != (c.WebTLSKeyFile == "") {
		errs = append(errs, errors.New("PICACHE_WEB_TLS_CERT and PICACHE_WEB_TLS_KEY must be set together"))
	}
	if c.RunAs != "" {
		if _, _, err := ParseRunAs(c.RunAs); err != nil {
			errs = append(errs, err)
		}
	}
	if c.LogFile != "" && !LogFileAllowed(c.LogFile, c.DataDir) {
		errs = append(errs, fmt.Errorf("PICACHE_LOG_FILE must be a <name>.log file below %s, directly in %s or below %s/logs", LogDir, c.DataDir, c.DataDir))
	}
	for _, addr := range c.NTPListen {
		if _, err := ParseListenerAddr(addr); err != nil {
			errs = append(errs, fmt.Errorf("PICACHE_NTP_LISTEN %q: %w", addr, err))
		}
	}
	if c.AdminPassword != "" && len(c.AdminPassword) < 10 {
		errs = append(errs, errors.New("PICACHE_ADMIN_PASSWORD must be at least 10 characters"))
	}
	return errors.Join(errs...)
}

// ParseRunAs parses "uid:gid" (numeric).
func ParseRunAs(s string) (uid, gid int, err error) {
	u, g, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, fmt.Errorf("PICACHE_RUN_AS must be uid:gid, got %q", s)
	}
	uid, err1 := strconv.Atoi(u)
	gid, err2 := strconv.Atoi(g)
	if err1 != nil || err2 != nil || uid <= 0 || gid <= 0 {
		return 0, 0, fmt.Errorf("PICACHE_RUN_AS must be numeric non-root uid:gid, got %q", s)
	}
	return uid, gid, nil
}

// parseSwitch parses a boolean variable: on/off, yes/no and everything
// strconv.ParseBool accepts (1, true, 0, false, …).
func parseSwitch(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "yes":
		return true, nil
	case "off", "no":
		return false, nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(s))
	if err != nil {
		return false, fmt.Errorf("must be on or off, got %q", s)
	}
	return b, nil
}

func parseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(strings.TrimSpace(s)))); err != nil {
		return 0, fmt.Errorf("invalid log level %q", s)
	}
	return l, nil
}

// splitList splits a comma-separated list; "off"/"none"/"-" yield an empty list.
func splitList(s string) []string {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "off", "none", "-":
		return nil
	}
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// LogDir is the log directory of the systemd unit (LogsDirectory=picache).
const LogDir = "/var/log/picache"

// LogFileAllowed reports whether path may be PICACHE_LOG_FILE: an
// absolute, clean path of a file named <name>.log below /var/log/picache,
// directly in the data directory or below <data dir>/logs. The sink
// appends to the file, renames it away and compresses it: every other
// file of the data directory is state (the databases, keys/master.key,
// tls/, the listeners files, the root helper's request directories), and
// none of it is named *.log.
func LogFileAllowed(path, dataDir string) bool {
	if !filepath.IsAbs(path) && !strings.HasPrefix(path, "/") {
		return false
	}
	p := filepath.Clean(path)
	if p != path && filepath.ToSlash(p) != path {
		return false
	}
	if name := filepath.Base(p); len(name) <= len(".log") || !strings.HasSuffix(name, ".log") {
		return false
	}
	if below(LogDir, p) {
		return true
	}
	if dataDir == "" {
		return false
	}
	d := filepath.Clean(dataDir)
	return filepath.Dir(p) == d || below(filepath.Join(d, "logs"), p)
}

// below reports whether the clean path p is strictly below the directory
// base.
func below(base, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(base), p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(rel)
}

// ParseSyslogURL parses PICACHE_LOG_SYSLOG: udp://host:port or
// tcp://host:port (host an IP literal or a name; no other scheme, no
// path, user info, query or fragment).
func ParseSyslogURL(s string) (network, addr string, err error) {
	const form = "must be udp://host:port or tcp://host:port"
	scheme, rest, ok := strings.Cut(s, "://")
	scheme = strings.ToLower(scheme)
	if !ok || (scheme != "udp" && scheme != "tcp") || rest == "" || strings.ContainsAny(rest, "/?#@ ") {
		return "", "", errors.New(form)
	}
	host, port, err := net.SplitHostPort(rest)
	if err != nil || host == "" {
		return "", "", errors.New(form)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", "", errors.New(form)
	}
	if strings.Contains(host, "%") {
		return "", "", errors.New(form)
	}
	return scheme, net.JoinHostPort(host, port), nil
}
