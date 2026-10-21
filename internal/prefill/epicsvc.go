package prefill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path"
	"slices"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/prefill/epic"
	"github.com/hustenreizjuengling/picache/internal/prefill/prefillapi"
)

// The Epic part of the service (docs/ARCHITECTURE.md 20.9): a device-code
// sign-in (the QR code opens Epic's activation page; nothing is typed into
// PiCache), the launcher session kept alive with its rolling refresh
// token, the account's games, and per chosen game the manifest of its
// current Windows build and every chunk through the cache.

const (
	epicRefreshAhead = 10 * time.Minute // renew the access token this early
	epicKeepAlive    = 24 * time.Hour   // renew the session at least daily
	epicAppsMaxAge   = 12 * time.Hour   // read the games again after this
	epicLoginTimeout = 10 * time.Minute // a device code lives 600 s
	epicMaxManifest  = 64 << 20         // one manifest download
)

// epicConnector supplies the client options (tests: a fake).
type epicConnector interface {
	options(used func(host, protocol, purpose string), log *slog.Logger) epic.Options
}

type liveEpic struct{}

func (liveEpic) options(used func(host, protocol, purpose string), log *slog.Logger) epic.Options {
	return epic.Options{Used: used, Log: log}
}

type epicService struct {
	d    *Daemon
	conn epicConnector

	mu    sync.Mutex
	acct  *epicAccount
	login *epicLoginRun
	games epicGames

	sessMu sync.Mutex // one renewal at a time
	appsMu sync.Mutex // one reading of the games at a time
}

type epicLoginRun struct {
	st     prefillapi.EpicLogin
	cancel context.CancelFunc
	done   chan struct{}
}

func newEpicService(d *Daemon, conn epicConnector) *epicService {
	e := &epicService{d: d, conn: conn}
	var a epicAccount
	if err := d.st.load(fileEpic, &a); err != nil {
		d.log.Warn("epic.json is unreadable: signed out of Epic", "err", err)
	} else if a.RefreshToken != "" {
		e.acct = &a
	}
	if err := d.st.load(fileEpicApps, &e.games); err != nil {
		e.games = epicGames{}
	}
	return e
}

func (e *epicService) client() *epic.Client { return epic.New(e.conn.options(e.d.usedHost, e.d.log)) }

func (e *epicService) label() string { return "Epic" }

func (e *epicService) idle() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.login == nil || e.login.st.State != "waiting"
}

func (e *epicService) signedIn() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.acct != nil
}

func (e *epicService) ready(set prefillapi.Settings) bool {
	return len(set.Epic.Apps) > 0 && e.signedIn()
}

func (e *epicService) status() prefillapi.Epic {
	e.mu.Lock()
	defer e.mu.Unlock()
	var st prefillapi.Epic
	if a := e.acct; a != nil {
		acc := &prefillapi.EpicAccount{Name: a.Name, AccountID: a.AccountID, Since: a.Since, Error: a.Error}
		if !a.RefreshExpires.IsZero() {
			t := a.RefreshExpires
			acc.Expires = &t
		}
		st.Account = acc
	}
	if e.login != nil {
		l := e.login.st
		st.Login = &l
	}
	return st
}

func (e *epicService) shutdown() {
	e.mu.Lock()
	l := e.login
	e.mu.Unlock()
	if l != nil {
		l.cancel()
		<-l.done
	}
}

// beginLogin starts a device-code sign-in (or returns the one that waits).
func (e *epicService) beginLogin(ctx context.Context) (prefillapi.EpicLogin, error) {
	e.mu.Lock()
	if e.acct != nil && e.acct.Error == "" {
		name := e.acct.Name
		e.mu.Unlock()
		return prefillapi.EpicLogin{}, conflict(fmt.Sprintf("already signed in to Epic as %s: sign out first", name))
	}
	if e.login != nil && e.login.st.State == "waiting" {
		l := e.login.st
		e.mu.Unlock()
		return l, nil
	}
	e.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	dl, err := e.client().StartDeviceLogin(cctx)
	if err != nil {
		return prefillapi.EpicLogin{}, unavailable("Epic did not start the sign-in: " + err.Error())
	}
	lctx, lcancel := context.WithTimeout(context.Background(), epicLoginTimeout)
	exp := dl.ExpiresAt
	run := &epicLoginRun{
		st:     prefillapi.EpicLogin{State: "waiting", VerificationURL: dl.VerificationURL, UserCode: dl.UserCode, Started: e.d.now(), Expires: &exp},
		cancel: lcancel,
		done:   make(chan struct{}),
	}
	e.mu.Lock()
	if old := e.login; old != nil {
		old.cancel()
	}
	e.login = run
	e.mu.Unlock()
	e.d.log.Info("epic sign-in started: waiting for the code to be confirmed at Epic")
	go e.pollLogin(lctx, run, dl)
	return run.st, nil
}

func (e *epicService) pollLogin(ctx context.Context, run *epicLoginRun, dl *epic.DeviceLogin) {
	defer close(run.done)
	defer run.cancel()
	set := func(state, msg string) {
		e.mu.Lock()
		run.st.State, run.st.Error = state, msg
		if state != "waiting" {
			run.st.VerificationURL, run.st.UserCode = "", ""
		}
		e.mu.Unlock()
	}
	for {
		t := time.NewTimer(dl.Interval())
		select {
		case <-ctx.Done():
			t.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				set("expired", "the code expired")
			} else {
				set("cancelled", "")
			}
			return
		case <-t.C:
		}
		sess, err := dl.Poll(ctx)
		if errors.Is(err, epic.ErrExpired) {
			set("expired", "the code expired")
			return
		}
		if err != nil {
			if ctx.Err() != nil {
				continue
			}
			set("failed", err.Error())
			e.d.log.Info("epic sign-in ended", "err", err)
			return
		}
		if sess == nil {
			continue
		}
		a := &epicAccount{Name: sess.DisplayName, AccountID: sess.AccountID, AccessToken: sess.AccessToken, AccessExpires: sess.ExpiresAt,
			RefreshToken: sess.RefreshToken, RefreshExpires: sess.RefreshExpiresAt, Since: e.d.now(), Refreshed: e.d.now()}
		if err := e.d.st.save(fileEpic, a); err != nil {
			set("failed", "the sign-in could not be stored: "+err.Error())
			return
		}
		e.mu.Lock()
		e.acct = a
		run.st.State = "done"
		run.st.VerificationURL, run.st.UserCode = "", ""
		e.games = epicGames{}
		e.mu.Unlock()
		_ = e.d.st.remove(fileEpicApps)
		e.d.log.Info("signed in to Epic", "account", a.Name)
		go func() {
			actx, cancel := context.WithTimeout(context.Background(), appsTimeout)
			defer cancel()
			if _, err := e.refreshApps(actx); err != nil {
				e.d.log.Warn("the Epic games could not be read", "err", err)
			}
		}()
		return
	}
}

func (e *epicService) cancelLogin() {
	e.mu.Lock()
	l := e.login
	e.mu.Unlock()
	if l != nil {
		l.cancel()
		<-l.done
	}
}

// logout ends the session at Epic (best effort) and deletes it.
func (e *epicService) logout(ctx context.Context) error {
	e.mu.Lock()
	a := e.acct
	e.mu.Unlock()
	if a == nil {
		return notFound("not signed in to Epic")
	}
	if a.Error == "" {
		kctx, cancel := context.WithTimeout(ctx, connectTimeout)
		if sess, err := e.session(kctx); err == nil {
			if err := e.client().Kill(kctx, sess.AccessToken); err != nil {
				e.d.log.Warn("the Epic session could not be ended at Epic", "err", err)
			}
		}
		cancel()
	}
	if err := e.d.st.remove(fileEpic); err != nil {
		return err
	}
	_ = e.d.st.remove(fileEpicApps)
	e.mu.Lock()
	e.acct = nil
	e.games = epicGames{}
	e.mu.Unlock()
	e.d.log.Info("signed out of Epic", "account", a.Name)
	return nil
}

// session returns a valid launcher session, renewing it when its access
// token runs out (the new refresh token is stored first). A refresh token
// Epic no longer accepts marks the account.
func (e *epicService) session(ctx context.Context) (*epic.Session, error) {
	e.sessMu.Lock()
	defer e.sessMu.Unlock()
	e.mu.Lock()
	a := e.acct
	var cp epicAccount
	if a != nil {
		cp = *a
	}
	e.mu.Unlock()
	if a == nil {
		return nil, errors.New("not signed in to Epic")
	}
	now := e.d.now()
	if cp.Error == "" && cp.AccessExpires.Sub(now) > epicRefreshAhead && now.Sub(cp.Refreshed) < epicKeepAlive {
		return &epic.Session{AccessToken: cp.AccessToken, ExpiresAt: cp.AccessExpires, RefreshToken: cp.RefreshToken,
			RefreshExpiresAt: cp.RefreshExpires, AccountID: cp.AccountID, DisplayName: cp.Name}, nil
	}
	sess, err := e.client().Refresh(ctx, cp.RefreshToken)
	if err != nil {
		if epic.Rejected(err) {
			e.markAccount(cp.RefreshToken, "Epic no longer accepts the sign-in ("+err.Error()+"): sign in again")
		}
		return nil, err
	}
	cp.AccessToken, cp.AccessExpires = sess.AccessToken, sess.ExpiresAt
	cp.RefreshToken, cp.RefreshExpires = sess.RefreshToken, sess.RefreshExpiresAt
	cp.Refreshed, cp.Error = now, ""
	if sess.DisplayName != "" {
		cp.Name = sess.DisplayName
	}
	if err := e.d.st.save(fileEpic, &cp); err != nil {
		return nil, fmt.Errorf("epic.json: %w", err)
	}
	e.mu.Lock()
	if e.acct != nil && e.acct.AccountID == cp.AccountID {
		e.acct = &cp
	}
	e.mu.Unlock()
	return sess, nil
}

func (e *epicService) markAccount(refreshToken, msg string) {
	e.mu.Lock()
	if e.acct == nil || e.acct.RefreshToken != refreshToken {
		e.mu.Unlock()
		return
	}
	e.acct.Error = msg
	cp := *e.acct
	e.mu.Unlock()
	if err := e.d.st.save(fileEpic, &cp); err != nil {
		e.d.log.Warn("epic.json could not be written", "err", err)
	}
}

// maintain keeps the sign-in alive (the scheduler calls it every minute):
// a refresh token lives about 23 days, renewed with every refresh.
func (e *epicService) maintain(ctx context.Context) {
	e.mu.Lock()
	a := e.acct
	due := a != nil && a.Error == "" && e.d.now().Sub(a.Refreshed) >= epicKeepAlive
	e.mu.Unlock()
	if !due {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if _, err := e.session(rctx); err != nil {
		e.d.log.Warn("the Epic sign-in could not be renewed", "err", err)
	}
}

func (e *epicService) account() error {
	if !e.signedIn() {
		return conflict("sign in to Epic first")
	}
	return nil
}

// gameList returns the account's games, read again when refresh is set or
// the list is older than epicAppsMaxAge.
func (e *epicService) gameList(ctx context.Context, refresh bool) (prefillapi.EpicApps, error) {
	if err := e.account(); err != nil {
		return prefillapi.EpicApps{Apps: []prefillapi.EpicApp{}}, err
	}
	e.mu.Lock()
	stale := e.games.Updated.IsZero() || e.d.now().Sub(e.games.Updated) > epicAppsMaxAge
	e.mu.Unlock()
	if refresh || stale {
		actx, cancel := context.WithTimeout(ctx, appsTimeout)
		defer cancel()
		if _, err := e.refreshApps(actx); err != nil {
			out := e.appList()
			out.Error = err.Error()
			return out, nil
		}
	}
	return e.appList(), nil
}

func (e *epicService) refreshApps(ctx context.Context) (epicGames, error) {
	e.appsMu.Lock()
	defer e.appsMu.Unlock()
	sess, err := e.session(ctx)
	if err != nil {
		return epicGames{}, err
	}
	games, err := e.client().Games(ctx, sess)
	if err != nil {
		if epic.Rejected(err) {
			e.markAccount(sess.RefreshToken, "Epic no longer accepts the sign-in ("+err.Error()+"): sign in again")
		}
		return epicGames{}, err
	}
	rec := epicGames{Updated: e.d.now(), Apps: make([]epicAppRec, 0, len(games))}
	for _, g := range games {
		rec.Apps = append(rec.Apps, epicAppRec{AppName: g.AppName, Name: g.Name, Namespace: g.Namespace, CatalogItemID: g.CatalogItemID, BuildVersion: g.BuildVersion})
	}
	if err := e.d.st.save(fileEpicApps, rec); err != nil {
		return rec, err
	}
	e.mu.Lock()
	e.games = rec
	e.mu.Unlock()
	return rec, nil
}

func epicKey(app string) string { return "epic:" + app }

func (e *epicService) appList() prefillapi.EpicApps {
	set := e.d.currentSettings()
	pre := e.d.prefilled()
	e.mu.Lock()
	defer e.mu.Unlock()
	out := prefillapi.EpicApps{Apps: make([]prefillapi.EpicApp, 0, len(e.games.Apps))}
	if !e.games.Updated.IsZero() {
		u := e.games.Updated
		out.Updated = &u
	}
	for _, g := range e.games.Apps {
		a := prefillapi.EpicApp{ID: g.AppName, Name: g.Name, Version: g.BuildVersion, Selected: slices.Contains(set.Epic.Apps, g.AppName)}
		if p, ok := pre[epicKey(g.AppName)]; ok {
			at := p.At
			a.Prefilled = &at
			a.Current = p.Version == g.BuildVersion
		}
		out.Apps = append(out.Apps, a)
	}
	return out
}

// prefill downloads the chosen games through the cache.
func (e *epicService) prefill(ctx context.Context, r *jobRun, set prefillapi.Settings) error {
	if !e.signedIn() || len(set.Epic.Apps) == 0 {
		return nil
	}
	e.d.setCurrent(r, "Epic: signing in")
	sess, err := e.session(ctx)
	if err != nil {
		return fmt.Errorf("Epic: %w", err)
	}
	e.d.setCurrent(r, "Epic: reading the selected games")
	recs, err := e.refreshApps(ctx)
	if err != nil {
		return fmt.Errorf("Epic: %w", err)
	}
	var games []epicAppRec
	for _, g := range recs.Apps {
		if slices.Contains(set.Epic.Apps, g.AppName) {
			games = append(games, g)
		}
	}
	apps := make([]prefillapi.JobApp, 0, len(games))
	for _, g := range games {
		apps = append(apps, prefillapi.JobApp{Service: "epic", ID: g.AppName, Name: g.Name, State: prefillapi.AppPending})
	}
	e.d.addApps(r, apps)
	cl := e.client()
	for _, g := range games {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		if sess, err = e.session(ctx); err != nil {
			return fmt.Errorf("Epic: %w", err)
		}
		game := epic.Game{AppName: g.AppName, Name: g.Name, Namespace: g.Namespace, CatalogItemID: g.CatalogItemID, BuildVersion: g.BuildVersion}
		mi, err := cl.Manifest(ctx, sess, game)
		if err != nil {
			e.d.updateApp(r, "epic", g.AppName, func(ja *prefillapi.JobApp) { ja.State, ja.Error = prefillapi.AppFailed, err.Error() })
			continue
		}
		if p, ok := e.d.prefilled()[epicKey(g.AppName)]; ok && !r.force && p.Hash != "" && p.Hash == mi.Hash {
			e.d.updateApp(r, "epic", g.AppName, func(ja *prefillapi.JobApp) { ja.State = prefillapi.AppCurrent })
			continue
		}
		e.d.updateApp(r, "epic", g.AppName, func(ja *prefillapi.JobApp) { ja.State = prefillapi.AppRunning })
		e.d.setCurrent(r, "Epic: "+g.Name)
		if err := e.prefillGame(ctx, r, cl, sess, game, mi); err != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			e.d.log.Warn("epic prefill of a game failed", "app", g.AppName, "err", err)
			e.d.updateApp(r, "epic", g.AppName, func(ja *prefillapi.JobApp) { ja.State, ja.Error = prefillapi.AppFailed, err.Error() })
			continue
		}
		e.d.markPrefilled(epicKey(g.AppName), prefilledApp{At: e.d.now(), Version: mi.BuildVersion, Hash: mi.Hash})
		e.d.updateApp(r, "epic", g.AppName, func(ja *prefillapi.JobApp) { ja.State = prefillapi.AppDone })
	}
	return nil
}

// prefillGame fetches the manifest through the cache (the first of Epic's
// addresses the cache serves), then every chunk; signed builds, and a
// build whose plain chunk addresses are refused, use signed addresses.
func (e *epicService) prefillGame(ctx context.Context, r *jobRun, cl *epic.Client, sess *epic.Session, g epic.Game, mi *epic.ManifestInfo) error {
	var m *epic.Manifest
	var host, base string
	var last error
	for _, u := range mi.URLs {
		h := u.Host
		e.d.usedHost(h, "http", "cache")
		res, body, err := e.d.cache.get(ctx, h, u.RequestURI(), epic.UserAgent, nil, true, epicMaxManifest)
		if err != nil {
			last = err
			continue
		}
		e.d.addBytes(r, "epic", g.AppName, res.bytes, res.hit)
		if m, err = epic.ParseManifest(body, mi.Hash); err != nil {
			return err
		}
		host, base = h, path.Dir(u.EscapedPath())
		break
	}
	if m == nil {
		return fmt.Errorf("no manifest address answered: %w", last)
	}
	e.d.updateApp(r, "epic", g.AppName, func(ja *prefillapi.JobApp) { ja.Total = ja.Bytes + m.Bytes })
	rel := make([]string, len(m.Chunks))
	for i, c := range m.Chunks {
		rel[i] = c.Path(m.FeatureLevel)
	}
	if !mi.UseSignedURL {
		paths := make([]string, len(rel))
		for i, p := range rel {
			paths[i] = base + "/" + p
		}
		err := e.d.downloadAll(ctx, r, host, epic.UserAgent, "epic", g.AppName, paths)
		if err == nil || !forbidden(err) {
			return err
		}
		e.d.log.Debug("epic chunks refused without signed addresses: asking for them", "app", g.AppName)
	}
	signed, err := cl.SignedChunkURLs(ctx, sess, g, mi.BuildVersion, rel)
	if err != nil {
		return fmt.Errorf("signed addresses: %w", err)
	}
	byHost := map[string][]string{}
	for _, p := range rel {
		u, ok := signed[p]
		if !ok {
			return fmt.Errorf("no signed address for %s", p)
		}
		byHost[u.Host] = append(byHost[u.Host], u.RequestURI())
	}
	for _, h := range slices.Sorted(maps.Keys(byHost)) {
		e.d.usedHost(h, "http", "cache")
		if err := e.d.downloadAll(ctx, r, h, epic.UserAgent, "epic", g.AppName, byHost[h]); err != nil {
			return err
		}
	}
	return nil
}
