package prefill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/hustenreizjuengling/picache/internal/prefill/battlenet"
	"github.com/hustenreizjuengling/picache/internal/prefill/prefillapi"
)

// The Battle.net part of the service (docs/ARCHITECTURE.md 20.8): no
// sign-in (Blizzard's builds are public). For each chosen product the
// current build in the chosen region is read from Blizzard's version
// service, and its files for a Windows install in the chosen language go
// through the cache.

const (
	bnetVersionsMaxAge = 12 * time.Hour
	bnetUserAgent      = "" // the Battle.net agent sends none
)

// bnetConnector supplies the client options (tests: a fake).
type bnetConnector interface {
	options(used func(host, protocol, purpose string), log *slog.Logger) battlenet.Options
}

type liveBattleNet struct{}

func (liveBattleNet) options(used func(host, protocol, purpose string), log *slog.Logger) battlenet.Options {
	return battlenet.Options{Used: used, Log: log}
}

type bnetService struct {
	d    *Daemon
	conn bnetConnector

	mu   sync.Mutex
	vers bnetVersions

	listMu sync.Mutex // one reading of the versions at a time
}

func newBattleNetService(d *Daemon, conn bnetConnector) *bnetService {
	b := &bnetService{d: d, conn: conn}
	if err := d.st.load(fileBattleNet, &b.vers); err != nil {
		b.vers = bnetVersions{}
	}
	return b
}

func (b *bnetService) client() *battlenet.Client {
	return battlenet.New(b.conn.options(b.d.usedHost, b.d.log))
}

func (b *bnetService) label() string { return "Battle.net" }
func (b *bnetService) idle() bool    { return true }

func (b *bnetService) ready(set prefillapi.Settings) bool { return len(set.BattleNet.Products) > 0 }

func bnetKey(code string) string { return "battlenet:" + code }

// bnetHash identifies what a prefill downloaded: the build, and the
// product config, region and language that chose its files.
func bnetHash(buildConfig, productConfig, region, locale string) string {
	h := sha256.Sum256([]byte("1|" + productConfig + "|" + region + "|" + locale))
	return buildConfig + "/" + hex.EncodeToString(h[:6])
}

// products lists the games with their current versions, read again when
// refresh is set, the list is old or the region changed.
func (b *bnetService) products(ctx context.Context, refresh bool) (prefillapi.BattleNetProducts, error) {
	set := b.d.currentSettings()
	b.mu.Lock()
	stale := b.vers.Updated.IsZero() || b.d.now().Sub(b.vers.Updated) > bnetVersionsMaxAge || b.vers.Region != set.BattleNet.Region
	b.mu.Unlock()
	var listErr string
	if refresh || stale {
		vctx, cancel := context.WithTimeout(ctx, appsTimeout)
		defer cancel()
		if err := b.readVersions(vctx, set.BattleNet.Region); err != nil {
			listErr = err.Error()
		}
	}
	out := b.list(set)
	out.Error = listErr
	return out, nil
}

// readVersions asks the version service for every product.
func (b *bnetService) readVersions(ctx context.Context, region string) error {
	b.listMu.Lock()
	defer b.listMu.Unlock()
	c := b.client()
	recs := make([]bnetVersionRec, len(battlenet.Products))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(6)
	for i, p := range battlenet.Products {
		g.Go(func() error {
			recs[i] = bnetVersionRec{Code: p.Code}
			v, err := c.Version(gctx, p.Code, region)
			if err != nil {
				recs[i].Error = bnetError(err)
				return nil
			}
			recs[i].Version, recs[i].BuildConfig, recs[i].ProductConfig = v.Name, v.BuildConfig.String(), v.ProductConfig.String()
			return nil
		})
	}
	_ = g.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	failed := 0
	for _, r := range recs {
		if r.Error != "" {
			failed++
		}
	}
	if failed == len(recs) {
		return fmt.Errorf("Blizzard's version service did not answer: %s", recs[0].Error)
	}
	v := bnetVersions{Updated: b.d.now(), Region: region, Products: recs}
	b.mu.Lock()
	b.vers = v
	b.mu.Unlock()
	return b.d.st.save(fileBattleNet, v)
}

func bnetError(err error) string {
	switch {
	case errors.Is(err, battlenet.ErrEncrypted):
		return "encrypted: this game cannot be prefilled"
	case errors.Is(err, battlenet.ErrUnknown):
		return "not offered in this region"
	}
	return err.Error()
}

// updateVersion stores a version read for a job.
func (b *bnetService) updateVersion(region string, r bnetVersionRec) {
	b.mu.Lock()
	if b.vers.Region != region {
		b.mu.Unlock()
		return
	}
	i := slices.IndexFunc(b.vers.Products, func(x bnetVersionRec) bool { return x.Code == r.Code })
	if i < 0 {
		b.mu.Unlock()
		return
	}
	b.vers.Products[i] = r
	cp := bnetVersions{Updated: b.vers.Updated, Region: b.vers.Region, Products: slices.Clone(b.vers.Products)}
	b.mu.Unlock()
	_ = b.d.st.save(fileBattleNet, cp)
}

func (b *bnetService) list(set prefillapi.Settings) prefillapi.BattleNetProducts {
	pre := b.d.prefilled()
	b.mu.Lock()
	defer b.mu.Unlock()
	out := prefillapi.BattleNetProducts{Products: make([]prefillapi.BattleNetProduct, 0, len(battlenet.Products))}
	if !b.vers.Updated.IsZero() && b.vers.Region == set.BattleNet.Region {
		u := b.vers.Updated
		out.Updated = &u
	}
	for _, p := range battlenet.Products {
		bp := prefillapi.BattleNetProduct{Code: p.Code, Name: p.Name, Selected: slices.Contains(set.BattleNet.Products, p.Code)}
		var rec bnetVersionRec
		if b.vers.Region == set.BattleNet.Region {
			if i := slices.IndexFunc(b.vers.Products, func(x bnetVersionRec) bool { return x.Code == p.Code }); i >= 0 {
				rec = b.vers.Products[i]
			}
		}
		bp.Version, bp.Error = rec.Version, rec.Error
		if pr, ok := pre[bnetKey(p.Code)]; ok {
			at := pr.At
			bp.Prefilled = &at
			bp.Current = rec.BuildConfig != "" && pr.Hash == bnetHash(rec.BuildConfig, rec.ProductConfig, set.BattleNet.Region, set.BattleNet.Locale)
		}
		out.Products = append(out.Products, bp)
	}
	return out
}

// prefill downloads the chosen products through the cache.
func (b *bnetService) prefill(ctx context.Context, r *jobRun, set prefillapi.Settings) error {
	codes := set.BattleNet.Products
	if len(codes) == 0 {
		return nil
	}
	region, locale := set.BattleNet.Region, set.BattleNet.Locale
	apps := make([]prefillapi.JobApp, 0, len(codes))
	for _, code := range codes {
		apps = append(apps, prefillapi.JobApp{Service: "battlenet", ID: code, Name: battlenet.ProductName(code), State: prefillapi.AppPending})
	}
	b.d.addApps(r, apps)
	c := b.client()
	for _, code := range codes {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		name := battlenet.ProductName(code)
		fail := func(err error) {
			b.d.log.Warn("battle.net prefill of a game failed", "product", code, "err", err)
			b.d.updateApp(r, "battlenet", code, func(ja *prefillapi.JobApp) { ja.State, ja.Error = prefillapi.AppFailed, bnetError(err) })
		}
		b.d.setCurrent(r, "Battle.net: reading the version of "+name)
		v, err := c.Version(ctx, code, region)
		if err != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			fail(err)
			continue
		}
		b.updateVersion(region, bnetVersionRec{Code: code, Version: v.Name, BuildConfig: v.BuildConfig.String(), ProductConfig: v.ProductConfig.String()})
		hash := bnetHash(v.BuildConfig.String(), v.ProductConfig.String(), region, locale)
		if p, ok := b.d.prefilled()[bnetKey(code)]; ok && !r.force && p.Hash == hash {
			b.d.updateApp(r, "battlenet", code, func(ja *prefillapi.JobApp) { ja.State = prefillapi.AppCurrent })
			continue
		}
		b.d.updateApp(r, "battlenet", code, func(ja *prefillapi.JobApp) { ja.State = prefillapi.AppRunning })
		b.d.setCurrent(r, "Battle.net: "+name)
		f := &bnetFetcher{d: b.d, r: r, hosts: v.Hosts, id: code}
		res, err := c.Prefill(ctx, v, locale, r.conns, f)
		if err != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			fail(err)
			continue
		}
		b.d.log.Info("battle.net prefill of a game finished", "product", code, "version", v.Name, "files", res.Files,
			"bytes", res.Bytes, "fileBytes", res.FileBytes, "tags", res.Query, "superset", res.Superset, "missing", f.missing, "versionSource", v.Source)
		b.d.markPrefilled(bnetKey(code), prefilledApp{At: b.d.now(), Version: v.Name, Hash: hash})
		b.d.updateApp(r, "battlenet", code, func(ja *prefillapi.JobApp) { ja.State = prefillapi.AppDone })
	}
	return nil
}

// bnetFetcher downloads a product's files through the cache, trying the
// CDN hosts in turn (some files are not on every CDN).
type bnetFetcher struct {
	d     *Daemon
	r     *jobRun
	hosts []string
	id    string

	mu      sync.Mutex
	missing int
}

func notFoundErr(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.code == 404
}

func (f *bnetFetcher) Fetch(ctx context.Context, path, rng string, read func(io.Reader) error) error {
	var last error
	for attempt := range chunkAttempts {
		if attempt > 0 {
			if err := sleepCtx(ctx, time.Duration(attempt*attempt)*2*time.Second); err != nil {
				return err
			}
		}
		allMissing := true
		for i := range f.hosts {
			h := f.hosts[(attempt+i)%len(f.hosts)]
			f.d.usedHost(h, "http", "cache")
			res, err := f.d.cache.stream(ctx, fetchReq{host: h, path: path, userAgent: bnetUserAgent, rng: rng, lim: f.r.lim}, read)
			if res.bytes > 0 {
				f.d.addBytes(f.r, "battlenet", f.id, res.bytes, res.hit)
			}
			if err == nil {
				return nil
			}
			last = err
			if errors.Is(err, errCacheRefused) || errors.Is(err, battlenet.ErrEncrypted) || ctx.Err() != nil {
				return err
			}
			if !notFoundErr(err) {
				allMissing = false
			}
		}
		if allMissing {
			break
		}
	}
	return fmt.Errorf("%s: %w", path, last)
}

func (f *bnetFetcher) FetchAll(ctx context.Context, reqs []battlenet.Request) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(f.r.conns, 1))
	for _, q := range reqs {
		if gctx.Err() != nil {
			break
		}
		g.Go(func() error {
			err := f.Fetch(gctx, q.Path, q.Range, nil)
			if err != nil && q.Optional && notFoundErr(err) {
				f.mu.Lock()
				f.missing++
				f.mu.Unlock()
				return nil
			}
			return err
		})
	}
	err := g.Wait()
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return err
}

func (f *bnetFetcher) Planned(n int64) {
	f.d.updateApp(f.r, "battlenet", f.id, func(ja *prefillapi.JobApp) { ja.Total = ja.Bytes + n })
}
