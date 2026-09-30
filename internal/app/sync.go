package app

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/parental"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/version"
)

// The follower sync (docs/ARCHITECTURE.md 15.4): a follower pulls the
// exportable sections of a primary (GET <source>/api/v1/system/export with
// a sync token) and applies them live. The runs start 1 minute after the
// start and 1 minute after a change of the mode, the primary, the
// sections, the trust anchor or the token, then every
// sync.intervalMinutes (±10 %), plus POST /system/sync/run; one run at a
// time. A run validates everything first
// with the API's validators and limits, replaces the synced tables in one
// transaction (each domain package keeps its SQL), commits, then applies
// the synced settings and reloads the components. An error before the
// commit keeps nothing of the run; an error after it (the settings, a
// reload) fails the run too, so its state is not stored and the next run
// applies the export again.
const (
	syncStateKey     = "sync.state" // app_meta: the last applied export
	syncFirstDelay   = time.Minute
	syncRunTimeout   = 30 * time.Second // the request and the response
	syncApplyTimeout = 2 * time.Minute
	syncRunGap       = 30 * time.Second // POST /system/sync/run: 429 within this after a start
	syncStaleRuns    = 3                // the health check warns after this many intervals without success
	syncErrorLen     = 300              // a message of the primary is cut to this
)

// syncState is the app_meta document of the last applied export: a run
// whose content, source and sections are unchanged applies nothing.
type syncState struct {
	ContentSHA256  string    `json:"contentSha256"`
	PrimaryVersion string    `json:"primaryVersion"`
	Origin         string    `json:"origin"`
	Sections       []string  `json:"sections"`
	AppliedAt      time.Time `json:"appliedAt"`
}

// syncer runs the follower sync; it implements api.SyncManager.
type syncer struct {
	a    *App
	log  *slog.Logger
	kick chan struct{} // POST /system/sync/run
	cfg  chan struct{} // the sync settings changed
	now  func() time.Time
	// dial replaces the transport's dialer (tests; nil: the host resolver
	// and netutil.SafeDialer with private destinations allowed).
	dial func(ctx context.Context, network, addr string) (net.Conn, error)

	mu          sync.Mutex
	running     bool
	lastStart   time.Time
	lastRun     time.Time
	lastSuccess time.Time
	lastError   string
	nextRun     time.Time
	state       syncState
	started     time.Time
}

func newSyncer(a *App) *syncer {
	s := &syncer{a: a, log: a.log.With(slog.String("component", "sync")), kick: make(chan struct{}, 1),
		cfg: make(chan struct{}, 1), now: time.Now}
	s.started = s.now()
	return s
}

// load reads the last applied export from app_meta.
func (s *syncer) load(ctx context.Context) {
	var doc string
	if err := s.a.cdb.R.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key = ?`, syncStateKey).Scan(&doc); err != nil {
		return
	}
	var st syncState
	if err := json.Unmarshal([]byte(doc), &st); err != nil {
		s.log.Warn("ignoring the stored state of the follower sync", slog.Any("err", err))
		return
	}
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
}

// forget removes the last applied export (a restore changed the synced
// tables: the next run applies the primary's configuration again).
func (s *syncer) forget(ctx context.Context) {
	if _, err := s.a.cdb.W.ExecContext(ctx, `DELETE FROM app_meta WHERE key = ?`, syncStateKey); err != nil {
		s.log.Warn("cannot reset the state of the follower sync", slog.Any("err", err))
	}
	s.mu.Lock()
	s.state = syncState{}
	s.mu.Unlock()
}

// settingsChanged wakes the loop after every settings write of a follower
// (or one that switches the mode): a replaced token leaves the document as
// it is, so the loop compares the token itself (rescheduled).
func (s *syncer) settingsChanged(o, n *settings.All) {
	if o.Sync.Mode != settings.SyncFollower && n.Sync.Mode != settings.SyncFollower {
		return
	}
	select {
	case s.cfg <- struct{}{}:
	default:
	}
}

// rescheduled reports whether a change of the sync settings of a follower
// brings the next run forward to a minute from now: the mode switched on,
// another primary, other sections, another trust anchor or another token
// (tok and prevTok: tokenPrint), e.g. after a run failed with a wrong
// token or a missing CA certificate.
func rescheduled(prev, cur settings.Sync, prevTok, tok [sha256.Size]byte) bool {
	return cur.Mode == settings.SyncFollower && (prev.Mode != settings.SyncFollower || prev.Source != cur.Source ||
		!slices.Equal(prev.Sections, cur.Sections) || prev.CAPEM != cur.CAPEM || prevTok != tok)
}

// tokenPrint is a fingerprint of the stored sync token (the hash of ""
// when none is stored or it cannot be read).
func (s *syncer) tokenPrint(ctx context.Context) [sha256.Size]byte {
	tok, _ := s.a.set.Secret(ctx, settings.SecretSyncToken)
	return sha256.Sum256([]byte(tok))
}

// interval returns the next wait: sync.intervalMinutes ±10 %.
func interval(minutes int) time.Duration {
	d := time.Duration(minutes) * time.Minute
	return time.Duration(float64(d) * (0.9 + 0.2*rand.Float64()))
}

// Start runs the loop until ctx ends.
func (s *syncer) Start(ctx context.Context) {
	next := s.now().Add(syncFirstDelay)
	prev, prevTok := s.a.set.Get().Sync, s.tokenPrint(ctx)
	for {
		cur := s.a.set.Get().Sync
		follower := cur.Mode == settings.SyncFollower
		tok := prevTok
		if follower {
			tok = s.tokenPrint(ctx)
		}
		if rescheduled(prev, cur, prevTok, tok) {
			next = s.now().Add(syncFirstDelay) // switched on, pointed elsewhere or new credentials
		} else if follower && cur.IntervalMinutes < prev.IntervalMinutes {
			if soon := s.now().Add(interval(cur.IntervalMinutes)); soon.Before(next) {
				next = soon
			}
		}
		prev, prevTok = cur, tok
		s.mu.Lock()
		if follower {
			s.nextRun = next
		} else {
			s.nextRun = time.Time{}
		}
		s.mu.Unlock()
		var timer <-chan time.Time
		var t *time.Timer
		if follower {
			t = time.NewTimer(max(next.Sub(s.now()), 0))
			timer = t.C
		}
		run := false
		select {
		case <-ctx.Done():
		case <-s.cfg:
		case <-s.kick:
			run = true // Run marked it running
		case <-timer:
			run = s.begin()
			if !run {
				next = s.now().Add(interval(cur.IntervalMinutes))
			}
		}
		if t != nil {
			t.Stop()
		}
		if ctx.Err() != nil {
			return
		}
		if run {
			s.runOnce(ctx)
			next = s.now().Add(interval(s.a.set.Get().Sync.IntervalMinutes))
		}
	}
}

// begin marks a run as started (false: one is running).
func (s *syncer) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}
	s.running, s.lastStart = true, s.now()
	return true
}

// Run starts a run now (POST /system/sync/run).
func (s *syncer) Run() error {
	if s.a.set.Get().Sync.Mode != settings.SyncFollower {
		return apperr.Conflict("sync is off")
	}
	s.mu.Lock()
	switch {
	case s.running:
		s.mu.Unlock()
		return apperr.Conflict("a sync is running")
	case !s.lastStart.IsZero() && s.now().Sub(s.lastStart) < syncRunGap:
		s.mu.Unlock()
		return apperr.TooMany("a sync was started less than 30 seconds ago")
	}
	s.running, s.lastStart = true, s.now()
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
	return nil
}

// Status is GET /system/sync.
func (s *syncer) Status() api.SyncStatus {
	cfg := s.a.set.Get().Sync
	st := api.SyncStatus{Mode: cfg.Mode, Sections: slices.Clone(cfg.Sections), IntervalMinutes: cfg.IntervalMinutes}
	if st.Sections == nil {
		st.Sections = []string{}
	}
	if origin, msg := settings.SyncOrigin(cfg.Source); msg == "" {
		st.Source = origin
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st.Running, st.LastRun, st.LastSuccess, st.LastError = s.running, s.lastRun, s.lastSuccess, s.lastError
	st.LastAppliedSHA256, st.PrimaryVersion = s.state.ContentSHA256, s.state.PrimaryVersion
	if cfg.Mode == settings.SyncFollower {
		st.NextRun = s.nextRun
	}
	return st
}

// ConfigSchema returns the schema version of every picache.db component.
func (s *syncer) ConfigSchema() map[string]int { return ConfigSchemaVersions() }

// errNoSyncToken: a follower without a stored sync token.
var errNoSyncToken = errors.New("no sync token is stored: enter a sync token of the primary")

// health evaluates the check "sync" (present while the mode is follower):
// no sync token is stored, the last run failed, or no run succeeded for 3
// intervals.
func (s *syncer) health() (status, msg, hint string, show bool) {
	cfg := s.a.set.Get().Sync
	if cfg.Mode != settings.SyncFollower {
		return "", "", "", false
	}
	origin, _ := settings.SyncOrigin(cfg.Source)
	s.mu.Lock()
	defer s.mu.Unlock()
	hint = "check the address of the primary, the sync token and the trust anchor in the sync settings"
	if !cfg.TokenSet { // e.g. a backup restored without secrets
		return "warn", fmt.Sprintf("sync from %s failed: %s", origin, errNoSyncToken), hint, true
	}
	if s.lastError != "" {
		return "warn", fmt.Sprintf("sync from %s failed: %s", origin, s.lastError), hint, true
	}
	since := s.lastSuccess
	if since.IsZero() {
		since = s.started
	}
	if stale := time.Duration(syncStaleRuns*cfg.IntervalMinutes) * time.Minute; s.now().Sub(since) > stale+syncFirstDelay {
		return "warn", fmt.Sprintf("no sync from %s succeeded for %d minutes", origin, syncStaleRuns*cfg.IntervalMinutes), hint, true
	}
	return "ok", "", "", true
}

// runOnce fetches and applies an export (begin was called).
func (s *syncer) runOnce(ctx context.Context) {
	err := s.syncNow(ctx)
	s.mu.Lock()
	s.running, s.lastRun = false, s.now()
	if err != nil {
		s.lastError = err.Error()
	} else {
		s.lastError, s.lastSuccess = "", s.lastRun
	}
	s.mu.Unlock()
	if err != nil {
		s.log.Warn("follower sync failed", slog.Any("err", err))
	}
}

// syncNow is one run.
func (s *syncer) syncNow(ctx context.Context) error {
	cfg := s.a.set.Get().Sync
	if cfg.Mode != settings.SyncFollower {
		return errors.New("sync is off")
	}
	origin, msg := settings.SyncOrigin(cfg.Source)
	if msg != "" {
		return errors.New("the address of the primary is not valid")
	}
	token, err := s.a.set.Secret(ctx, settings.SecretSyncToken)
	if err != nil || token == "" {
		return errNoSyncToken
	}
	fctx, cancel := context.WithTimeout(ctx, syncRunTimeout)
	exp, err := s.fetch(fctx, origin, cfg, token)
	cancel()
	if err != nil {
		return err
	}
	if err := checkExport(exp, cfg.Sections); err != nil {
		return err
	}
	s.mu.Lock()
	st := s.state
	s.mu.Unlock()
	if st.ContentSHA256 == exp.ContentSHA256 && st.Origin == origin && slices.Equal(st.Sections, cfg.Sections) {
		s.mu.Lock()
		s.state.PrimaryVersion = exp.Version
		s.mu.Unlock()
		return nil // unchanged
	}
	actx, cancel := context.WithTimeout(ctx, syncApplyTimeout)
	defer cancel()
	if err := s.a.applySync(actx, exp, cfg.Sections); err != nil {
		return err
	}
	st = syncState{ContentSHA256: exp.ContentSHA256, PrimaryVersion: exp.Version, Origin: origin,
		Sections: slices.Clone(cfg.Sections), AppliedAt: s.now().UTC()}
	if doc, err := json.Marshal(st); err == nil {
		if _, err := s.a.cdb.W.ExecContext(actx, `INSERT INTO app_meta (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, syncStateKey, string(doc)); err != nil {
			s.log.Warn("cannot store the state of the follower sync", slog.Any("err", err))
		}
	}
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
	if s.a.auth != nil {
		s.a.auth.Audit(actx, &auth.Principal{Username: "sync"}, "", "sync.applied", origin,
			map[string]any{"sections": cfg.Sections, "contentSha256": exp.ContentSHA256, "primaryVersion": exp.Version})
	}
	s.log.Info("configuration synced from the primary", slog.String("source", origin), slog.Any("sections", cfg.Sections),
		slog.String("primary_version", exp.Version))
	return nil
}

// fetch requests the export: https only, TLS 1.2+, the chain and the host
// name verified against sync.caPem (trust anchors; a self-signed leaf in
// them is trusted as itself) or the system roots; the token is sent only
// after the handshake verified; no redirects, no proxy; the host resolved
// by the host resolver, then link-local, multicast, unspecified, loopback
// and this machine's addresses refused (private addresses allowed); at
// most 64 MiB, decoded strictly.
func (s *syncer) fetch(ctx context.Context, origin string, cfg settings.Sync, token string) (*api.ConfigExport, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CAPEM != "" {
		certs, err := settings.ParseCAPEM(cfg.CAPEM)
		if err != nil {
			return nil, errors.New("the trust anchor (caPem) cannot be read")
		}
		pool := x509.NewCertPool()
		for _, c := range certs {
			pool.AddCert(c)
		}
		tlsCfg.RootCAs = pool
	}
	dial := s.dial
	if dial == nil {
		resolve := func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
		d := &netutil.SafeDialer{Resolve: resolve, AllowPrivate: func(context.Context) bool { return true }, Timeout: 10 * time.Second}
		dial = d.DialContext
	}
	c := &http.Client{
		Transport: &http.Transport{Proxy: nil, DialContext: dial, TLSClientConfig: tlsCfg, ForceAttemptHTTP2: true,
			TLSHandshakeTimeout: 10 * time.Second, DisableKeepAlives: true, MaxResponseHeaderBytes: 64 << 10},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	u := origin + "/api/v1/system/export?sections=" + url.QueryEscape(strings.Join(cfg.Sections, ","))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "PiCache/"+version.Version)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, transportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, primaryError(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, api.MaxExportBytes+1))
	if err != nil {
		return nil, transportError(err)
	}
	if len(body) > api.MaxExportBytes {
		return nil, errors.New("the export of the primary is larger than 64 MiB")
	}
	var exp api.ConfigExport
	if err := json.Unmarshal(body, &exp, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("the export of the primary cannot be read: %s", cleanMessage(err.Error()))
	}
	return &exp, nil
}

// transportError drops the URL (it carries no token, but the query) from
// an error of the HTTP client.
func transportError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("the primary did not answer within 30 seconds")
	}
	return fmt.Errorf("cannot reach the primary: %s", cleanMessage(err.Error()))
}

// primaryError describes an answer other than 200 (the primary's message,
// cleaned and cut).
func primaryError(resp *http.Response) error {
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = json.Unmarshal(b, &body)
	msg := cleanMessage(body.Error.Message)
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return errors.New("the primary refused the sync token (401): create a new sync token on the primary")
	case http.StatusForbidden:
		if msg == "" {
			msg = "forbidden"
		}
		return fmt.Errorf("the primary refused the export (403): %s", msg)
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return fmt.Errorf("the primary answered %d: %s", resp.StatusCode, msg)
}

// cleanMessage removes control characters and cuts a message of the
// primary (it is shown in the UI and the health check).
func cleanMessage(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > syncErrorLen {
		s = string(r[:syncErrorLen]) + "…"
	}
	return s
}

// checkExport refuses an export this binary cannot apply: another format,
// an unknown format version, a component schema newer than this binary's,
// a requested section missing or a content hash that does not match.
func checkExport(exp *api.ConfigExport, sections []string) error {
	newer := func() error {
		return fmt.Errorf("the primary runs PiCache %s with a newer configuration schema: update this follower", cleanMessage(exp.Version))
	}
	if exp.Format != api.ExportFormat {
		return errors.New("the answer of the primary is no PiCache export")
	}
	if exp.FormatVersion != api.ExportFormatVersion {
		if exp.FormatVersion > api.ExportFormatVersion {
			return newer()
		}
		return fmt.Errorf("the export format %d of the primary is not supported", exp.FormatVersion)
	}
	own := ConfigSchemaVersions()
	for comp, v := range exp.Schema {
		if have, ok := own[comp]; !ok || v > have {
			return newer()
		}
	}
	for _, sec := range sections {
		if _, ok := exp.Sections[sec]; !ok {
			return fmt.Errorf("the export of the primary lacks the section %s", sec)
		}
	}
	sha, err := api.ExportContentSHA256(exp.Sections)
	if err != nil || sha != exp.ContentSHA256 {
		return errors.New("the content hash of the export does not match its sections")
	}
	return nil
}

// syncSections are the decoded sections of an export (nil: not synced).
type syncSections struct {
	cg  *api.ExportClientsGroups
	lr  *api.ExportListsRules
	ld  *api.ExportLocalDNS
	pa  *api.ExportParental
	dns jsontext.Value
}

// decodeSections decodes the synced sections strictly.
func decodeSections(exp *api.ConfigExport, sections []string) (*syncSections, error) {
	out := &syncSections{}
	dec := func(sec string, v any) error {
		if err := json.Unmarshal(exp.Sections[sec], v, json.RejectUnknownMembers(true)); err != nil {
			return fmt.Errorf("the section %s of the export cannot be read: %s", sec, cleanMessage(err.Error()))
		}
		return nil
	}
	for _, sec := range sections {
		var err error
		switch sec {
		case settings.SectionClientsGroups:
			out.cg = &api.ExportClientsGroups{}
			err = dec(sec, out.cg)
		case settings.SectionListsRules:
			out.lr = &api.ExportListsRules{}
			err = dec(sec, out.lr)
		case settings.SectionLocalDNS:
			out.ld = &api.ExportLocalDNS{}
			err = dec(sec, out.ld)
		case settings.SectionParental:
			out.pa = &api.ExportParental{}
			err = dec(sec, out.pa)
		case settings.SectionDNSSettings:
			out.dns = exp.Sections[sec]
		default:
			err = fmt.Errorf("the section %s cannot be synced", sec)
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// legacyUpstream is an upstream of a primary before 1.0.0 with text after
// "#" that this version refuses, read as that version did
// (settings.LegacyUpstream).
type legacyUpstream struct{ where, sent, used string }

// legacySyncedUpstreams reads the group upstreams and forwarder targets of
// the synced sections as a follower reads its own stored ones
// (settings.LegacyUpstreams): a primary before 1.0.0 exports them as it
// stored them, and such an entry failed the whole run with a validation
// error. It returns the changed entries.
func legacySyncedUpstreams(sec *syncSections) []legacyUpstream {
	var out []legacyUpstream
	fix := func(where string, list []string) []string {
		for _, u := range list {
			if used, ok := settings.LegacyUpstream(u); ok {
				out = append(out, legacyUpstream{where, u, used})
			}
		}
		return settings.LegacyUpstreams(list)
	}
	if sec.cg != nil {
		for i := range sec.cg.Groups {
			g := &sec.cg.Groups[i]
			g.Upstreams = fix("clients-and-groups: group "+g.Name, g.Upstreams)
		}
	}
	if sec.ld != nil {
		for i := range sec.ld.Forwarders {
			f := &sec.ld.Forwarders[i]
			f.Upstreams = fix("local-dns: forwarder "+f.Domain, f.Upstreams)
		}
	}
	return out
}

// groupMapper maps the primary's group ids onto this follower's: the
// identity with clients-and-groups (the groups are replaced too), else by
// name (case-insensitive; the Default group is 1 everywhere).
type groupMapper struct {
	identity bool
	primary  map[int64]string // the primary's name table
	follower map[string]int64 // this follower's groups by lower-case name
}

func (m *groupMapper) id(id int64) (int64, error) {
	if m.identity || id == clients.DefaultGroupID {
		return id, nil
	}
	name, ok := m.primary[id]
	if !ok {
		return 0, fmt.Errorf("the export refers to group %d, which is not in its group table", id)
	}
	fid, ok := m.follower[strings.ToLower(name)]
	if !ok {
		return 0, fmt.Errorf("the group %q of the primary does not exist here: create it or also sync clients-and-groups", name)
	}
	return fid, nil
}

func (m *groupMapper) ids(in []int64) ([]int64, error) {
	out := make([]int64, 0, len(in))
	for _, id := range in {
		fid, err := m.id(id)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, fid) {
			out = append(out, fid)
		}
	}
	return out, nil
}

// applySync validates and applies the synced sections of exp (checked by
// checkExport): one transaction for the tables, then the settings, then
// the reloads. An error of the settings or a reload after the commit is
// returned too, so the run is not recorded as applied.
func (a *App) applySync(ctx context.Context, exp *api.ConfigExport, sections []string) error {
	sec, err := decodeSections(exp, sections)
	if err != nil {
		return err
	}
	// Upstreams a primary before 1.0.0 exports with text after "#" keep
	// that version's meaning (as stored ones do), logged once the run is
	// applied.
	legacy := legacySyncedUpstreams(sec)
	// The synced settings are checked first (without storing): the group
	// upstreams below are checked against them.
	var syncedSettings func(*settings.All) error
	var legacyDNS [][2]string
	nextDNS := a.set.Get().DNS
	if sec.dns != nil {
		syncedSettings = func(s *settings.All) error {
			old := a.set.Get()
			changed, err := settings.ApplySyncable(s, sec.dns)
			if err != nil {
				return err
			}
			legacyDNS = changed
			return a.checkSyncedSettings(old, s)
		}
		next, err := a.set.DryRun(ctx, syncedSettings)
		if err != nil {
			return fmt.Errorf("dns-settings: %w", err)
		}
		nextDNS = next.DNS
	}
	var (
		cg     *clients.SyncedClients
		lr     *filter.SyncedLists
		ld     *dnsserver.SyncedLocalDNS
		pa     []parental.SyncGroup
		change *filter.SyncChange
	)
	primary := make(map[int64]string, len(exp.Groups))
	for _, g := range exp.Groups {
		primary[g.ID] = g.Name
	}
	err = a.cdb.Tx(ctx, func(tx *sql.Tx) error {
		m := &groupMapper{identity: sec.cg != nil, primary: primary}
		var err error
		if !m.identity {
			if m.follower, err = clients.GroupNames(ctx, tx); err != nil {
				return err
			}
		}
		// 1. Validate everything with the API's rules and limits.
		groups := []clients.Group{}
		if sec.cg != nil {
			if cg, err = clients.ValidateSync(sec.cg.Groups, sec.cg.Clients); err != nil {
				return fmt.Errorf("clients-and-groups: %w", err)
			}
			groups = sec.cg.Groups
		} else if sec.dns != nil {
			if groups, err = a.clients.Groups(ctx); err != nil {
				return err
			}
		}
		if err := checkSyncedGroupUpstreams(groups, &nextDNS); err != nil {
			return err
		}
		if sec.lr != nil {
			if lr, err = a.syncedLists(m, sec.lr); err != nil {
				return fmt.Errorf("lists-and-rules: %w", err)
			}
		}
		if sec.ld != nil {
			records := slices.Clone(sec.ld.Records)
			for i := range records {
				if records[i].GroupIDs, err = m.ids(records[i].GroupIDs); err != nil {
					return fmt.Errorf("local-dns: %w", err)
				}
			}
			if ld, err = a.dns.ValidateSync(records, sec.ld.Forwarders); err != nil {
				return fmt.Errorf("local-dns: %w", err)
			}
		}
		if sec.pa != nil {
			in := make([]parental.SyncGroup, 0, len(sec.pa.Groups))
			for _, g := range sec.pa.Groups {
				id, err := m.id(g.GroupID)
				if err != nil {
					return fmt.Errorf("parental: %w", err)
				}
				in = append(in, parental.SyncGroup{GroupID: id, Config: parental.Config{BlockedServices: g.BlockedServices,
					Schedules: g.Schedules, SafeSearch: g.SafeSearch}})
			}
			if pa, err = parental.ValidateSync(in); err != nil {
				return fmt.Errorf("parental: %w", err)
			}
		}
		// 2. Replace the tables (the overrides and pauses of the parental
		// controls are read first: replacing the groups cascades).
		var kept map[string]parental.SyncKept
		if sec.pa != nil {
			if kept, err = parental.KeptByGroupName(ctx, tx); err != nil {
				return err
			}
		}
		if cg != nil {
			if err := clients.ReplaceSynced(ctx, tx, cg); err != nil {
				return fmt.Errorf("clients-and-groups: %w", err)
			}
		}
		if lr != nil {
			if change, err = a.filter.ReplaceSynced(ctx, tx, lr); err != nil {
				return fmt.Errorf("lists-and-rules: %w", err)
			}
		}
		if ld != nil {
			if err := dnsserver.ReplaceSynced(ctx, tx, ld); err != nil {
				return fmt.Errorf("local-dns: %w", err)
			}
		}
		if sec.pa != nil {
			names, err := clients.GroupNames(ctx, tx)
			if err != nil {
				return err
			}
			keep := map[int64]parental.SyncKept{}
			for name, k := range kept {
				if id, ok := names[name]; ok {
					keep[id] = k
				}
			}
			if err := parental.ReplaceSynced(ctx, tx, pa, keep); err != nil {
				return fmt.Errorf("parental: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// 3. The settings (only the syncable members), after the commit.
	if syncedSettings != nil {
		if _, err := a.set.Update(ctx, syncedSettings); err != nil {
			return fmt.Errorf("dns-settings: %w", err)
		}
		for _, c := range legacyDNS {
			legacy = append(legacy, legacyUpstream{"dns-settings", c[0], c[1]})
		}
	}
	for _, l := range legacy {
		a.log.Warn(`the primary sent an upstream with text after "#" that this version refuses (saved by a version before 1.0.0); `+
			`it is used without that text, as that version did (fix it on the primary)`, slog.String("component", "sync"),
			slog.String("where", l.where), slog.String("sent", strconv.QuoteToASCII(l.sent)), slog.String("used", strconv.QuoteToASCII(l.used)))
	}
	// 4. The reloads (the clients' OnChange reloads the filter's groups,
	// the parental controls, the records and the group upstream sets).
	var errs []error
	if cg != nil {
		errs = append(errs, a.clients.Reload(ctx))
	}
	if change != nil {
		errs = append(errs, a.filter.SyncApplied(ctx, change))
	}
	if ld != nil {
		errs = append(errs, a.dns.ReloadRecords(ctx))
	}
	if sec.pa != nil {
		errs = append(errs, a.parental.Reload(ctx))
	}
	// A failed reload fails the run: its state is not stored, so the next
	// run applies the export again instead of leaving the components on the
	// previous configuration while the tables hold the new one.
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("reload after the sync: %w", err)
	}
	return nil
}

// syncedLists maps the group ids of the lists, rules and IP rules and
// validates them.
func (a *App) syncedLists(m *groupMapper, lr *api.ExportListsRules) (*filter.SyncedLists, error) {
	lists, rules, ipRules := slices.Clone(lr.Lists), slices.Clone(lr.Rules), slices.Clone(lr.IPRules)
	var err error
	for i := range lists {
		if lists[i].GroupIDs, err = m.ids(lists[i].GroupIDs); err != nil {
			return nil, err
		}
	}
	for i := range rules {
		if rules[i].GroupIDs, err = m.ids(rules[i].GroupIDs); err != nil {
			return nil, err
		}
	}
	for i := range ipRules {
		if ipRules[i].GroupIDs, err = m.ids(ipRules[i].GroupIDs); err != nil {
			return nil, err
		}
	}
	return a.filter.ValidateSync(lists, rules, ipRules)
}

// checkSyncedSettings applies the checks of PUT /settings that concern
// syncable members: the dns.blockedClients safety net (no entry that
// blocks this machine, its router, the container gateway or a trusted
// forwarder) and a public custom ECS subnet.
func (a *App) checkSyncedSettings(old, next *settings.All) error {
	list := settings.NormalizeBlockedClients(next.DNS.BlockedClients)
	if a.dns != nil && !slices.Equal(settings.NormalizeBlockedClients(old.DNS.BlockedClients), list) {
		protected := a.dns.ProtectedClients(next.DNS.EDNSClientTrusted, a.clients.NeighbourMAC)
		for i, entry := range list {
			if why := dnsserver.BlockedClientLockout(entry, protected); why != "" {
				return apperr.Invalid(fmt.Sprintf("dns.blockedClients[%d]", i), "%s", why)
			}
		}
	}
	if cur := strings.TrimSpace(next.DNS.ECS.CustomSubnet); cur != "" {
		if p, err := netip.ParsePrefix(cur); err == nil && !netutil.IsPublicUnicast(p.Masked().Addr()) {
			return apperr.Invalid("dns.ecs.customSubnet", "must be a public network (not private, carrier-grade NAT, link-local, documentation or another special-use range)")
		}
	}
	return nil
}

// checkSyncedGroupUpstreams applies the rules of the DNS settings to the
// upstreams of the groups (like the API when a group or the DNS settings
// are saved): no upstream that is PiCache itself, a plain upstream given
// by name must be a public name, and names (and presets) need
// dns.bootstrap servers.
func checkSyncedGroupUpstreams(groups []clients.Group, d *settings.DNS) error {
	for _, g := range groups {
		if g.UpstreamPreset != "" && len(d.Bootstrap) == 0 {
			return fmt.Errorf("group %s: a preset (DNS over HTTPS by name) needs dns.bootstrap servers", g.Name)
		}
		for _, u := range g.Upstreams {
			spec, err := settings.ParseUpstream(u)
			if err != nil {
				continue // the clients package reports a syntax error
			}
			if settings.SelfUpstream(spec, d) {
				return fmt.Errorf("group %s: %s", g.Name, settings.ErrSelfUpstream)
			}
			if !spec.NeedsBootstrap() {
				continue
			}
			if (spec.Proto == "udp" || spec.Proto == "tcp") && !settings.PublicUpstreamName(spec.Host, d.LocalDomain) {
				return fmt.Errorf("group %s: %s", g.Name, settings.ErrPlainUpstreamName)
			}
			if len(d.Bootstrap) == 0 {
				return fmt.Errorf("group %s: a host name needs dns.bootstrap servers", g.Name)
			}
		}
	}
	return nil
}
