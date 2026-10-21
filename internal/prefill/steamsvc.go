package prefill

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/prefill/prefillapi"
	"github.com/hustenreizjuengling/picache/internal/prefill/steam"
)

// The Steam part of the service: the QR sign-in, the account's games and
// the prefill of the selected ones.

const (
	loginTimeout    = 10 * time.Minute
	connectTimeout  = 30 * time.Second
	appsTimeout     = 5 * time.Minute
	appsMaxAge      = 12 * time.Hour
	chunkAttempts   = 3
	manifestAttempt = 3
)

// steamConnector supplies the connection options (tests: a fake CM).
type steamConnector interface {
	options(used func(host, protocol, purpose string), log *slog.Logger) steam.Options
}

type liveSteam struct{}

func (liveSteam) options(used func(host, protocol, purpose string), log *slog.Logger) steam.Options {
	return steam.Options{Used: used, Log: log}
}

type steamService struct {
	d    *Daemon
	conn steamConnector

	mu    sync.Mutex
	acct  *steamAccount
	login *loginRun
	games steamApps
	pre   prefilledRec

	appsMu sync.Mutex // one refresh of the games at a time
}

type loginRun struct {
	st     prefillapi.SteamLogin
	cancel context.CancelFunc
	done   chan struct{}
}

func newSteamService(d *Daemon, conn steamConnector) *steamService {
	s := &steamService{d: d, conn: conn, pre: prefilledRec{}}
	var a steamAccount
	if err := d.st.load(fileSteam, &a); err != nil {
		d.log.Warn("steam.json is unreadable: signed out of Steam", "err", err)
	} else if a.RefreshToken != "" {
		s.acct = &a
	}
	if err := d.st.load(fileSteamApps, &s.games); err != nil {
		d.log.Warn("steam-apps.json is unreadable: the games are read again", "err", err)
		s.games = steamApps{}
	}
	if err := d.st.load(filePrefilled, &s.pre); err != nil || s.pre == nil {
		s.pre = prefilledRec{}
	}
	return s
}

func (s *steamService) options() steam.Options {
	return s.conn.options(s.d.usedHost, s.d.log)
}

// signedIn reports a stored sign-in (valid or not).
func (s *steamService) signedIn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acct != nil
}

// idle reports that no sign-in waits.
func (s *steamService) idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.login == nil || s.login.st.State != "waiting"
}

func (s *steamService) shutdown() {
	s.mu.Lock()
	l := s.login
	s.mu.Unlock()
	if l != nil {
		l.cancel()
		<-l.done
	}
}

func (s *steamService) status() prefillapi.Steam {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := prefillapi.Steam{DeviceName: s.d.cfg.DeviceName}
	if a := s.acct; a != nil {
		acc := &prefillapi.SteamAccount{
			Name: a.Name, SteamID: strconv.FormatUint(a.SteamID, 10), DeviceName: s.d.cfg.DeviceName,
			Since: a.Since, Error: a.Error,
		}
		if !a.Expires.IsZero() {
			e := a.Expires
			acc.Expires = &e
		}
		st.Account = acc
	}
	if s.login != nil {
		l := s.login.st
		st.Login = &l
	}
	return st
}

// beginLogin starts a QR sign-in (or returns the one that waits).
func (s *steamService) beginLogin(ctx context.Context) (prefillapi.SteamLogin, error) {
	s.mu.Lock()
	if s.acct != nil && s.acct.Error == "" {
		name := s.acct.Name
		s.mu.Unlock()
		return prefillapi.SteamLogin{}, conflict(fmt.Sprintf("already signed in to Steam as %s: sign out first", name))
	}
	if s.login != nil && s.login.st.State == "waiting" {
		l := s.login.st
		s.mu.Unlock()
		return l, nil
	}
	s.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	c, err := steam.Connect(cctx, s.options())
	if err != nil {
		return prefillapi.SteamLogin{}, unavailable("Steam is not reachable: " + err.Error())
	}
	q, err := c.BeginQR(cctx, s.d.cfg.DeviceName)
	if err != nil {
		c.Close()
		return prefillapi.SteamLogin{}, unavailable("Steam did not start the sign-in: " + err.Error())
	}
	lctx, lcancel := context.WithTimeout(context.Background(), loginTimeout)
	run := &loginRun{
		st:     prefillapi.SteamLogin{State: "waiting", ChallengeURL: q.URL(), Started: s.d.now()},
		cancel: lcancel,
		done:   make(chan struct{}),
	}
	s.mu.Lock()
	if old := s.login; old != nil {
		old.cancel()
	}
	s.login = run
	s.mu.Unlock()
	s.d.log.Info("steam sign-in started: waiting for the QR code to be confirmed in the Steam app")
	go s.pollLogin(lctx, run, c, q)
	return run.st, nil
}

func (s *steamService) pollLogin(ctx context.Context, run *loginRun, c *steam.Conn, q *steam.QR) {
	defer close(run.done)
	defer run.cancel()
	defer c.Close()
	set := func(f func(*prefillapi.SteamLogin)) {
		s.mu.Lock()
		f(&run.st)
		if run.st.State != "waiting" {
			run.st.ChallengeURL, run.st.Scanned = "", false
		}
		s.mu.Unlock()
	}
	t := time.NewTicker(q.Interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			set(func(l *prefillapi.SteamLogin) {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					l.State, l.Error = "expired", "the QR code expired"
				} else {
					l.State = "cancelled"
				}
			})
			return
		case <-t.C:
		}
		res, err := q.Poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				continue
			}
			set(func(l *prefillapi.SteamLogin) {
				l.State, l.Error = "failed", err.Error()
				var se *steam.Error
				if errors.As(err, &se) && se.Result == 27 { // Expired
					l.State, l.Error = "expired", "the QR code expired"
				}
			})
			s.d.log.Info("steam sign-in ended", "err", err)
			return
		}
		if res == nil {
			set(func(l *prefillapi.SteamLogin) {
				l.ChallengeURL = q.URL()
				l.Scanned = q.Scanned()
			})
			continue
		}
		seed := make([]byte, 16)
		_, _ = rand.Read(seed)
		var lid [4]byte
		_, _ = rand.Read(lid[:])
		loginID := binary.LittleEndian.Uint32(lid[:])
		s.mu.Lock()
		if s.acct != nil && s.acct.MachineID != "" {
			seed, _ = hex.DecodeString(s.acct.MachineID)
		}
		if s.acct != nil && s.acct.LoginID != 0 {
			loginID = s.acct.LoginID
		}
		s.mu.Unlock()
		a := &steamAccount{
			Name: res.AccountName, SteamID: res.SteamID, RefreshToken: res.RefreshToken,
			Since: s.d.now(), Expires: res.Expires, MachineID: hex.EncodeToString(seed), LoginID: max(loginID, 1),
		}
		if err := s.d.st.save(fileSteam, a); err != nil {
			set(func(l *prefillapi.SteamLogin) {
				l.State, l.Error = "failed", "the sign-in could not be stored: "+err.Error()
			})
			return
		}
		s.mu.Lock()
		s.acct = a
		run.st.State, run.st.ChallengeURL = "done", ""
		s.games = steamApps{}
		s.mu.Unlock()
		_ = s.d.st.remove(fileSteamApps)
		s.d.log.Info("signed in to Steam", "account", a.Name)
		go func() {
			actx, cancel := context.WithTimeout(context.Background(), appsTimeout)
			defer cancel()
			if _, err := s.refreshApps(actx); err != nil {
				s.d.log.Warn("the Steam games could not be read", "err", err)
			}
		}()
		return
	}
}

func (s *steamService) cancelLogin() {
	s.mu.Lock()
	l := s.login
	s.mu.Unlock()
	if l != nil {
		l.cancel()
		<-l.done
	}
}

// logout revokes the refresh token at Steam (best effort) and deletes it.
func (s *steamService) logout(ctx context.Context) error {
	s.mu.Lock()
	a := s.acct
	s.mu.Unlock()
	if a == nil {
		return notFound("not signed in to Steam")
	}
	revoked := false
	if a.Error == "" {
		rctx, cancel := context.WithTimeout(ctx, connectTimeout)
		if sess, err := s.signIn(rctx, a); err == nil {
			if err := sess.RevokeToken(rctx, a.RefreshToken); err == nil {
				revoked = true
			} else {
				s.d.log.Warn("the Steam sign-in could not be revoked at Steam", "err", err)
			}
			sess.Close()
		} else {
			s.d.log.Warn("the Steam sign-in could not be revoked at Steam", "err", err)
		}
		cancel()
	}
	if err := s.d.st.remove(fileSteam); err != nil {
		return err
	}
	_ = s.d.st.remove(fileSteamApps)
	s.mu.Lock()
	s.acct = nil
	s.games = steamApps{}
	s.mu.Unlock()
	s.d.log.Info("signed out of Steam", "account", a.Name, "revokedAtSteam", revoked)
	return nil
}

// signIn connects and logs on with the stored token, on another server
// when Steam asks for it. A token Steam no longer accepts is marked on the
// account.
func (s *steamService) signIn(ctx context.Context, a *steamAccount) (*steam.Session, error) {
	var last error
	for attempt := range 3 {
		if attempt > 0 {
			if err := sleepCtx(ctx, time.Duration(attempt)*2*time.Second); err != nil {
				return nil, err
			}
		}
		c, err := steam.Connect(ctx, s.options())
		if err != nil {
			last = fmt.Errorf("Steam is not reachable: %w", err)
			continue
		}
		sess, err := c.SignIn(ctx, steam.LogOn{
			AccountName: a.Name, SteamID: a.SteamID, RefreshToken: a.RefreshToken, LoginID: a.LoginID,
			MachineID: a.MachineID, MachineName: s.d.cfg.DeviceName,
		})
		if err == nil {
			if a.Error != "" {
				s.markAccount(a, "")
			}
			return sess, nil
		}
		c.Close()
		last = err
		var se *steam.Error
		if errors.As(err, &se) {
			if steam.TokenRejected(se.Result) {
				s.markAccount(a, "Steam no longer accepts the sign-in ("+se.Error()+"): sign in again")
				return nil, err
			}
			if !steam.Transient(se.Result) {
				return nil, err
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, last
}

func (s *steamService) markAccount(a *steamAccount, msg string) {
	s.mu.Lock()
	if s.acct == nil || s.acct.RefreshToken != a.RefreshToken {
		s.mu.Unlock()
		return
	}
	s.acct.Error = msg
	cp := *s.acct
	s.mu.Unlock()
	if err := s.d.st.save(fileSteam, &cp); err != nil {
		s.d.log.Warn("steam.json could not be written", "err", err)
	}
}

func (s *steamService) account() (*steamAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acct == nil {
		return nil, conflict("sign in to Steam first")
	}
	cp := *s.acct
	return &cp, nil
}

// filterKey identifies the depot choice a game list was read with.
func filterKey(set prefillapi.Settings) string {
	return strings.Join(set.Steam.OS, ",") + "|" + strings.Join(set.Steam.Languages, ",")
}

func filterOf(set prefillapi.Settings) steam.Filter {
	return steam.Filter{OS: set.Steam.OS, Languages: set.Steam.Languages}
}

// gameList returns the account's games, read again when refresh is set, when
// the list is older than appsMaxAge or was read under other settings.
func (s *steamService) gameList(ctx context.Context, refresh bool) (prefillapi.SteamApps, error) {
	if _, err := s.account(); err != nil {
		return prefillapi.SteamApps{Apps: []prefillapi.SteamApp{}}, err
	}
	key := filterKey(s.d.currentSettings())
	s.mu.Lock()
	stale := s.games.Updated.IsZero() || s.d.now().Sub(s.games.Updated) > appsMaxAge || s.games.Filter != key
	s.mu.Unlock()
	if refresh || stale {
		actx, cancel := context.WithTimeout(ctx, appsTimeout)
		defer cancel()
		if _, err := s.refreshApps(actx); err != nil {
			out := s.appList()
			out.Error = err.Error()
			return out, nil
		}
	}
	return s.appList(), nil
}

// refreshApps reads the games from Steam and stores them.
func (s *steamService) refreshApps(ctx context.Context) (steamApps, error) {
	s.appsMu.Lock()
	defer s.appsMu.Unlock()
	a, err := s.account()
	if err != nil {
		return steamApps{}, err
	}
	sess, err := s.signIn(ctx, a)
	if err != nil {
		return steamApps{}, err
	}
	defer sess.LogOff()
	set := s.d.currentSettings()
	games, err := sess.OwnedGames(ctx, filterOf(set))
	if err != nil {
		return steamApps{}, err
	}
	rec := steamApps{Updated: s.d.now(), Filter: filterKey(set), Apps: make([]steamAppRec, 0, len(games))}
	for _, g := range games {
		rec.Apps = append(rec.Apps, appRec(g))
	}
	if err := s.d.st.save(fileSteamApps, rec); err != nil {
		return rec, err
	}
	s.mu.Lock()
	s.games = rec
	s.mu.Unlock()
	return rec, nil
}

func appRec(g steam.Game) steamAppRec {
	r := steamAppRec{ID: g.ID, Name: g.Name, Depots: make([]steamDepotRec, 0, len(g.Depots))}
	for _, d := range g.Depots {
		dr := steamDepotRec{ID: d.ID, Manifest: d.Manifest, Bytes: d.Bytes}
		if d.App != g.ID {
			dr.App = d.App
		}
		r.Depots = append(r.Depots, dr)
	}
	return r
}

func (s *steamService) appList() prefillapi.SteamApps {
	set := s.d.currentSettings()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := prefillapi.SteamApps{Apps: make([]prefillapi.SteamApp, 0, len(s.games.Apps))}
	if !s.games.Updated.IsZero() {
		u := s.games.Updated
		out.Updated = &u
	}
	for _, r := range s.games.Apps {
		a := prefillapi.SteamApp{ID: r.ID, Name: r.Name, Selected: slices.Contains(set.Steam.Apps, r.ID)}
		for _, d := range r.Depots {
			a.Bytes += d.Bytes
		}
		if p, ok := s.pre[preKey(r.ID)]; ok {
			at := p.At
			a.Prefilled = &at
			a.Current = maps.Equal(p.Manifests, manifestsOf(r))
		}
		out.Apps = append(out.Apps, a)
	}
	return out
}

func preKey(app uint32) string { return "steam:" + strconv.FormatUint(uint64(app), 10) }

func manifestsOf(r steamAppRec) map[string]uint64 {
	m := make(map[string]uint64, len(r.Depots))
	for _, d := range r.Depots {
		m[strconv.FormatUint(uint64(d.ID), 10)] = d.Manifest
	}
	return m
}

// prefill downloads the selected games through the cache.
func (s *steamService) prefill(ctx context.Context, r *jobRun, set prefillapi.Settings) error {
	s.mu.Lock()
	a := s.acct
	s.mu.Unlock()
	if a == nil || len(set.Steam.Apps) == 0 {
		return nil
	}
	s.d.setCurrent(r, "Steam: signing in")
	sess, err := s.signIn(ctx, a)
	if err != nil {
		return fmt.Errorf("Steam: %w", err)
	}
	defer func() { sess.LogOff() }()
	s.d.setCurrent(r, "Steam: reading the selected games")
	games, err := sess.Games(ctx, set.Steam.Apps, filterOf(set))
	if err != nil {
		return fmt.Errorf("Steam: %w", err)
	}
	s.updateAppRecs(games)
	servers, err := sess.ContentServers(ctx)
	if err != nil {
		return fmt.Errorf("Steam content servers: %w", err)
	}
	hosts := serverHosts(servers)
	if len(hosts) == 0 {
		return errors.New("Steam named no content server")
	}
	apps := make([]prefillapi.JobApp, 0, len(games))
	for _, g := range games {
		apps = append(apps, prefillapi.JobApp{Service: "steam", ID: strconv.FormatUint(uint64(g.ID), 10), Name: g.Name,
			State: prefillapi.AppPending, Total: g.Bytes()})
	}
	s.d.addApps(r, apps)
	s.d.mu.Lock()
	for _, a := range apps {
		r.job.Total += a.Total
	}
	s.d.mu.Unlock()
	for _, g := range games {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		id := strconv.FormatUint(uint64(g.ID), 10)
		rec := appRec(g)
		s.mu.Lock()
		p, done := s.pre[preKey(g.ID)]
		s.mu.Unlock()
		if done && !r.force && maps.Equal(p.Manifests, manifestsOf(rec)) {
			s.d.updateApp(r, "steam", id, func(ja *prefillapi.JobApp) { ja.State = prefillapi.AppCurrent; ja.Total = 0 })
			continue
		}
		s.d.updateApp(r, "steam", id, func(ja *prefillapi.JobApp) { ja.State = prefillapi.AppRunning })
		s.d.setCurrent(r, "Steam: "+g.Name)
		sess, err = s.ensureSession(ctx, sess, a)
		if err != nil {
			return fmt.Errorf("Steam: %w", err)
		}
		if err := s.prefillGame(ctx, r, sess, hosts, g); err != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			s.d.log.Warn("steam prefill of a game failed", "app", g.ID, "name", g.Name, "err", err)
			s.d.updateApp(r, "steam", id, func(ja *prefillapi.JobApp) { ja.State, ja.Error = prefillapi.AppFailed, err.Error() })
			continue
		}
		s.mu.Lock()
		s.pre[preKey(g.ID)] = prefilledApp{At: s.d.now(), Manifests: manifestsOf(rec)}
		pre := maps.Clone(s.pre)
		s.mu.Unlock()
		if err := s.d.st.save(filePrefilled, pre); err != nil {
			s.d.log.Warn("prefilled.json could not be written", "err", err)
		}
		s.d.updateApp(r, "steam", id, func(ja *prefillapi.JobApp) { ja.State = prefillapi.AppDone })
	}
	return nil
}

// ensureSession reconnects when the CM closed the connection during a
// long job.
func (s *steamService) ensureSession(ctx context.Context, sess *steam.Session, a *steamAccount) (*steam.Session, error) {
	select {
	case <-sess.Done():
	default:
		return sess, nil
	}
	return s.signIn(ctx, a)
}

// updateAppRecs stores the current depots of games read for a job.
func (s *steamService) updateAppRecs(games []steam.Game) {
	s.mu.Lock()
	for _, g := range games {
		rec := appRec(g)
		if i := slices.IndexFunc(s.games.Apps, func(x steamAppRec) bool { return x.ID == g.ID }); i >= 0 {
			s.games.Apps[i] = rec
		}
	}
	cp := steamApps{Updated: s.games.Updated, Filter: s.games.Filter, Apps: slices.Clone(s.games.Apps)}
	s.mu.Unlock()
	if !cp.Updated.IsZero() {
		_ = s.d.st.save(fileSteamApps, cp)
	}
}

// serverHosts orders the content servers whose names the requests carry
// (the Host header, like SteamKit and SteamPrefill in lancache mode): CDN
// and Steam cache servers for every app, least loaded first, those that
// want HTTPS last (the cache fetches from them itself).
func serverHosts(servers []steam.ContentServer) []string {
	var ok []steam.ContentServer
	for _, sv := range servers {
		if (sv.Type == "CDN" || sv.Type == "SteamCache") && !sv.Proxy && len(sv.AppIDs) == 0 && sv.Host != "" {
			ok = append(ok, sv)
		}
	}
	slices.SortStableFunc(ok, func(a, b steam.ContentServer) int {
		return cmp.Or(cmp.Compare(boolInt(a.HTTPS == "mandatory"), boolInt(b.HTTPS == "mandatory")), cmp.Compare(a.Load, b.Load))
	})
	var out []string
	for _, sv := range ok {
		if !slices.Contains(out, sv.Host) {
			out = append(out, sv.Host)
		}
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// forbidden reports a 403 of the content server (it does not serve the
// depot: the next server is tried).
func forbidden(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.code == 403
}

// prefillGame downloads every depot of g: the manifest (through the
// cache, like the client), then all of its chunks. A server that refuses
// a depot (403) is replaced by the next one for that depot.
func (s *steamService) prefillGame(ctx context.Context, r *jobRun, sess *steam.Session, hosts []string, g steam.Game) error {
	id := strconv.FormatUint(uint64(g.ID), 10)
	for _, d := range g.Depots {
		var err error
		planned := false
		for _, host := range hosts {
			s.d.usedHost(host, "http", "cache")
			err = s.prefillDepot(ctx, r, sess, host, d, id, &planned)
			if err == nil || !forbidden(err) {
				break
			}
			s.d.log.Debug("steam content server refused a depot: trying the next", "depot", d.ID, "host", host)
		}
		if err != nil {
			return fmt.Errorf("depot %d: %w", d.ID, err)
		}
	}
	return nil
}

func (s *steamService) prefillDepot(ctx context.Context, r *jobRun, sess *steam.Session, host string, d steam.Depot, id string, planned *bool) error {
	m, err := s.manifest(ctx, r, sess, host, d, id)
	if err != nil {
		return err
	}
	// The plan follows the manifest: what Steam's app info said is replaced
	// by the depot's real download size (once, whichever server serves it).
	if !*planned {
		*planned = true
		s.d.updateApp(r, "steam", id, func(ja *prefillapi.JobApp) { ja.Total += m.Bytes - d.Bytes })
	}
	paths := make([]string, len(m.Chunks))
	for i, c := range m.Chunks {
		paths[i] = c.Path(d.ID)
	}
	return s.d.downloadAll(ctx, r, host, steam.UserAgent, "steam", id, paths)
}

func (s *steamService) manifest(ctx context.Context, r *jobRun, sess *steam.Session, vhost string, d steam.Depot, id string) (*steam.Manifest, error) {
	var last error
	for attempt := range manifestAttempt {
		if attempt > 0 {
			if err := sleepCtx(ctx, time.Duration(attempt)*5*time.Second); err != nil {
				return nil, err
			}
		}
		code, err := sess.ManifestRequestCode(ctx, d.App, d.ID, d.Manifest, "public")
		if err != nil {
			if errors.Is(err, steam.ErrNoAccess) {
				return nil, err
			}
			last = err
			continue
		}
		res, body, err := s.d.cache.get(ctx, vhost, steam.ManifestPath(d.ID, d.Manifest, code), steam.UserAgent, nil, true, steam.MaxManifest)
		if err != nil {
			last = err
			if errors.Is(err, errCacheRefused) || forbidden(err) {
				return nil, err
			}
			continue
		}
		s.d.addBytes(r, "steam", id, res.bytes, res.hit)
		m, err := steam.ParseManifest(body)
		if err != nil {
			return nil, err
		}
		return m, nil
	}
	return nil, last
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
		return nil
	}
}
