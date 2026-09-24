package proxy

import (
	"cmp"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	rateWindowSecs   = 10               // rate window of live transfers and downloads
	downloadLinger   = 30 * time.Second // aggregated downloads stay this long after their last request
	maxDownloads     = 4096
	maxTransfers     = 8192 // live requests tracked (the listener caps connections at 4096)
	maxSteamRefused  = 20
	maxTransferLabel = 256
)

// counters are the live statistics.
type counters struct {
	requests, bytesHit, bytesWAN, refused, errors, activeFills atomic.Int64

	mu    sync.Mutex
	steam []string // recent hosts refused although the Steam User-Agent was used, newest first
}

func (c *counters) steamRefused(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.steam = slices.DeleteFunc(c.steam, func(h string) bool { return h == host })
	c.steam = slices.Insert(c.steam, 0, host)
	if len(c.steam) > maxSteamRefused {
		c.steam = c.steam[:maxSteamRefused]
	}
}

func (c *counters) steamHosts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.steam)
}

// rateWindow sums bytes per second over the last rateWindowSecs seconds.
type rateWindow struct {
	sec [rateWindowSecs]int64
	n   [rateWindowSecs]int64
}

func (w *rateWindow) add(sec, n int64) {
	i := sec % rateWindowSecs
	if w.sec[i] != sec {
		w.sec[i], w.n[i] = sec, 0
	}
	w.n[i] += n
}

// rate is the average over the window (or since started, if shorter).
func (w *rateWindow) rate(now int64, started time.Time) float64 {
	var sum int64
	for i := range rateWindowSecs {
		if s := w.sec[i]; s > now-rateWindowSecs && s <= now {
			sum += w.n[i]
		}
	}
	span := min(rateWindowSecs, max(1, now-started.Unix()))
	return float64(sum) / float64(span)
}

// transfer is a live request.
type transfer struct {
	id    uint64
	info  Transfer
	acct  *acct
	total atomic.Int64
	dl    *download // nil if the download table was full
	// folded byte counts (under liveState.mu)
	sent, hit, wan int64
	win            rateWindow
}

type downloadKey struct{ client, service, group string }

// download aggregates the requests of one client for one content group.
type download struct {
	info ActiveDownload
	win  rateWindow
}

// liveState tracks live transfers and aggregated downloads. Byte counts are
// folded in by tick (every second) and when a request ends.
type liveState struct {
	mu        sync.Mutex
	nextID    uint64
	transfers map[uint64]*transfer      // ≤ maxTransfers
	downloads map[downloadKey]*download // ≤ maxDownloads
}

func (l *liveState) begin(rq *request) *transfer {
	now := rq.start
	t := &transfer{acct: rq.acct}
	t.info = Transfer{
		ClientIP:   rq.ip.String(),
		ClientName: rq.clientName(),
		Service:    rq.service,
		Host:       rq.host,
		Path:       clip(rq.path, maxLogPath),
		GroupKey:   rq.group.Key,
		Label:      clip(rq.label, maxTransferLabel),
		Started:    now.UTC(),
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.transfers == nil {
		l.transfers = make(map[uint64]*transfer)
		l.downloads = make(map[downloadKey]*download)
	}
	l.nextID++
	t.id = l.nextID
	t.info.ID = strconv.FormatUint(t.id, 10)
	if len(l.transfers) < maxTransfers {
		l.transfers[t.id] = t
	}
	k := downloadKey{client: t.info.ClientIP, service: t.info.Service, group: t.info.GroupKey}
	d := l.downloads[k]
	if d == nil && (len(l.downloads) < maxDownloads || l.evictIdleLocked()) {
		d = &download{info: ActiveDownload{
			ClientIP: t.info.ClientIP, ClientName: t.info.ClientName, Service: t.info.Service,
			GroupKey: t.info.GroupKey, Label: t.info.Label, Started: t.info.Started,
		}}
		l.downloads[k] = d
	}
	if d != nil {
		d.info.InFlight++
		d.info.LastSeen = t.info.Started
		t.dl = d
	}
	return t
}

// evictIdleLocked removes the idle download seen least recently.
func (l *liveState) evictIdleLocked() bool {
	var victim downloadKey
	var found *download
	for k, d := range l.downloads {
		if d.info.InFlight == 0 && (found == nil || d.info.LastSeen.Before(found.info.LastSeen)) {
			victim, found = k, d
		}
	}
	if found == nil {
		return false
	}
	delete(l.downloads, victim)
	return true
}

func (l *liveState) end(t *transfer, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.foldLocked(t, now.Unix())
	delete(l.transfers, t.id)
	if d := t.dl; d != nil {
		d.info.InFlight--
		d.info.LastSeen = now.UTC()
	}
}

// foldLocked adds the bytes t moved since the last fold to its windows and
// its download.
func (l *liveState) foldLocked(t *transfer, sec int64) {
	sent, hit, wan := t.acct.sent.Load(), t.acct.hit.Load(), t.acct.wan.Load()
	ds, dh, dw := sent-t.sent, hit-t.hit, wan-t.wan
	t.sent, t.hit, t.wan = sent, hit, wan
	t.win.add(sec, ds)
	if d := t.dl; d != nil {
		d.info.BytesSent += ds
		d.info.BytesHit += dh
		d.info.BytesWAN += dw
		d.win.add(sec, ds)
	}
}

// tick folds the byte counters and expires idle downloads.
func (l *liveState) tick(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	sec := now.Unix()
	for _, t := range l.transfers {
		l.foldLocked(t, sec)
	}
	for k, d := range l.downloads {
		if d.info.InFlight == 0 && now.Sub(d.info.LastSeen) > downloadLinger {
			delete(l.downloads, k)
		}
	}
}

func (l *liveState) transfersSnapshot(now time.Time) []Transfer {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Transfer, 0, len(l.transfers))
	for _, t := range l.transfers {
		x := t.info
		x.BytesSent, x.BytesHit, x.BytesWAN = t.acct.sent.Load(), t.acct.hit.Load(), t.acct.wan.Load()
		x.Total = t.total.Load()
		x.RateBps = t.win.rate(now.Unix(), x.Started)
		out = append(out, x)
	}
	slices.SortFunc(out, func(a, b Transfer) int {
		return cmp.Or(a.Started.Compare(b.Started), cmp.Compare(a.ID, b.ID))
	})
	return out
}

func (l *liveState) downloadsSnapshot(now time.Time) []ActiveDownload {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]ActiveDownload, 0, len(l.downloads))
	for _, d := range l.downloads {
		if d.info.InFlight == 0 && now.Sub(d.info.LastSeen) > downloadLinger {
			continue
		}
		x := d.info
		x.RateBps = d.win.rate(now.Unix(), x.Started)
		out = append(out, x)
	}
	slices.SortFunc(out, func(a, b ActiveDownload) int {
		return cmp.Or(cmp.Compare(b.RateBps, a.RateBps), b.LastSeen.Compare(a.LastSeen), cmp.Compare(a.ClientIP, b.ClientIP))
	})
	return out
}

// activeClients counts distinct clients with requests in flight.
func (l *liveState) activeClients() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	seen := make(map[string]struct{}, len(l.transfers))
	for _, t := range l.transfers {
		seen[t.info.ClientIP] = struct{}{}
	}
	return len(seen)
}
