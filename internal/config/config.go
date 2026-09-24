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

	DNSListen      []string // PICACHE_DNS_LISTEN, default ":53"
	CacheListen    []string // PICACHE_CACHE_LISTEN, default ":80"
	SNIListen      []string // PICACHE_SNI_LISTEN, default ":443" (empty disables)
	WebListen      []string // PICACHE_WEB_LISTEN, default ":8080"
	WebTLSListen   []string // PICACHE_WEB_TLS_LISTEN, default ":8443" (empty disables)
	WebTLSCertFile string   // PICACHE_WEB_TLS_CERT (optional, else self-signed)
	WebTLSKeyFile  string   // PICACHE_WEB_TLS_KEY
	WebHosts       []string // PICACHE_WEB_HOSTS: extra allowed Host names for the UI
	// WebSecureCookies (PICACHE_WEB_SECURE_COOKIES) sets the Secure flag on
	// the session and device cookies for plain-HTTP requests too: for a
	// TLS-terminating reverse proxy in front of the HTTP listener. Requests
	// over TLS always get it.
	WebSecureCookies bool

	RunAs string // PICACHE_RUN_AS "uid:gid": drop privileges after binding when started as root

	LogLevel  slog.Level // PICACHE_LOG_LEVEL: debug|info|warn|error
	LogFormat string     // PICACHE_LOG_FORMAT: text|json

	AdminUser string // PICACHE_ADMIN_USER (default "admin"), used with AdminPassword
	// AdminPassword (PICACHE_ADMIN_PASSWORD_FILE preferred, or PICACHE_ADMIN_PASSWORD)
	// provisions the first admin. The app clears it after provisioning.
	AdminPassword        string `json:"-"`
	AdminPasswordFromEnv bool   `json:"-"` // true if the plain env var was used (warn)

	MasterKeyFile string // PICACHE_MASTER_KEY_FILE: optional external master key

	MountRoot string // PICACHE_MOUNT_ROOT: the only place NAS stores may live, default /srv/picache

	Dev bool // PICACHE_DEV: development mode (relaxed platform checks, verbose errors in log)
}

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
		LogLevel:     slog.LevelInfo,
		LogFormat:    "text",
		AdminUser:    "admin",
		MountRoot:    "/srv/picache",
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
	c := defaults()
	if err := c.applyEnv(getenv); err != nil {
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

func (c *Config) applyEnv(getenv func(string) string) error {
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
		}
	}
	str("PICACHE_DATA_DIR", &c.DataDir)
	str("PICACHE_CACHE_DIR", &c.CacheDir)
	list("PICACHE_DNS_LISTEN", &c.DNSListen)
	list("PICACHE_CACHE_LISTEN", &c.CacheListen)
	list("PICACHE_SNI_LISTEN", &c.SNIListen)
	list("PICACHE_WEB_LISTEN", &c.WebListen)
	list("PICACHE_WEB_TLS_LISTEN", &c.WebTLSListen)
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
	for key, dst := range map[string]*bool{"PICACHE_DEV": &c.Dev, "PICACHE_WEB_SECURE_COOKIES": &c.WebSecureCookies} {
		if v := getenv(key); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dst = b
		}
	}

	// Secrets: prefer *_FILE (Docker secrets) over plain env.
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
