package logs

import (
	"cmp"
	"context"
	"database/sql"
	"slices"
	"sync"
	"sync/atomic"
)

// topKeysPerHour is the number of keys kept per hour and kind (and per day
// and kind in the daily top tables).
const topKeysPerHour = 1000

// DNS top kinds (logs_dns_top_hourly.kind).
type dnsKind int

const (
	dnsTopDomain   dnsKind = iota // allowed (not blocked) queries per domain
	dnsTopBlocked                 // blocked queries per domain
	dnsTopClient                  // queries (and blocked) per client
	dnsTopUpstream                // queries and duration per upstream
	dnsTopPurpose                 // blocked and safe-search queries per purpose (QueryEvent.Purpose)
	dnsTopQType                   // queries per query type (at most maxQTypeKeys types, the others under qtypeOther)
	numDNSKinds
)

var (
	// dnsKindNames are stored in logs_dns_top_hourly.kind; a version that
	// does not know a kind ignores its rows.
	dnsKindNames = [numDNSKinds]string{"domain", "blocked", "client", "upstream", "purpose", "qtype"}
	// dnsKindCaps bound the distinct keys counted per hour in memory
	// (qtype: maxQTypeKeys types plus qtypeOther).
	dnsKindCaps = [numDNSKinds]int{16384, 16384, 4096, 256, 64, maxQTypeKeys + 1}
)

// Kind "unique" of the DNS top tables holds the hour's (day's) sketch of
// the distinct query names (hll.go); it has one row with key "".
const uniqueKind = "unique"

// Query types: at most maxQTypeKeys distinct types per hour (and per range
// in GET /stats/qtypes); further types are counted under qtypeOther.
const (
	maxQTypeKeys = 32
	qtypeOther   = "OTHER"
)

// Cache top kinds (logs_cache_top_hourly.kind).
type cacheKind int

const (
	cacheTopClient  cacheKind = iota // per client
	cacheTopContent                  // per service + content group
	numCacheKinds
)

var (
	cacheKindNames = [numCacheKinds]string{"client", "content"}
	cacheKindCaps  = [numCacheKinds]int{4096, 8192}
)

// dnsTopRow is one DNS top entry (JSON field names are used by the queries
// that merge the in-memory hour through json_each).
type dnsTopRow struct {
	Key        string `json:"k"`
	Label      string `json:"l"`
	Count      int64  `json:"c"`
	Blocked    int64  `json:"b"`
	DurationUs int64  `json:"d"`
	LastSeen   int64  `json:"t"`
}

// cacheTopRow is one cache top entry.
type cacheTopRow struct {
	Service  string `json:"s"`
	Key      string `json:"k"`
	Label    string `json:"l"`
	Requests int64  `json:"r"`
	Sent     int64  `json:"b"`
	Hit      int64  `json:"h"`
	WAN      int64  `json:"w"`
	LastSeen int64  `json:"t"`
}

type contentKey struct{ service, key string }

// topSet holds the top counters of the current hour. The writer adds to it;
// queries take snapshots. Keys beyond the per-kind caps are not counted
// (overflow).
type topSet struct {
	mu       sync.Mutex
	hour     int64 // bucket (unix ms) the counters belong to
	dns      [numDNSKinds]map[string]*dnsTopRow
	cache    [numCacheKinds]map[contentKey]*cacheTopRow
	uniq     *hll // distinct query names of the hour
	overflow atomic.Uint64
	changed  bool // counted since the last checkpoint
}

// replace installs the counters of hour (loaded from the database; uniq
// nil = none yet).
func (t *topSet) replace(hour int64, dns [numDNSKinds]map[string]*dnsTopRow, cache [numCacheKinds]map[contentKey]*cacheTopRow, uniq *hll) {
	if uniq == nil {
		uniq = &hll{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.hour, t.dns, t.cache, t.uniq, t.changed = hour, dns, cache, uniq, false
}

// setChanged records whether the counters differ from the stored rows and
// returns the previous state.
func (t *topSet) setChanged(v bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev := t.changed
	t.changed = v
	return prev
}

func (t *topSet) currentHour() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.hour
}

func (t *topSet) dnsEntry(k dnsKind, key string) *dnsTopRow {
	if key == "" {
		return nil
	}
	m := t.dns[k]
	e := m[key]
	if e == nil {
		if len(m) >= dnsKindCaps[k] {
			t.overflow.Add(1)
			return nil
		}
		e = &dnsTopRow{Key: key}
		m[key] = e
	}
	return e
}

func (t *topSet) cacheEntry(k cacheKind, key contentKey) *cacheTopRow {
	if key.key == "" {
		return nil
	}
	m := t.cache[k]
	e := m[key]
	if e == nil {
		if len(m) >= cacheKindCaps[k] {
			t.overflow.Add(1)
			return nil
		}
		e = &cacheTopRow{Service: key.service, Key: key.key}
		m[key] = e
	}
	return e
}

// addQuery counts a query in the current hour: per domain (unless the
// domains are hidden: domains false), client, upstream and purpose, and in
// the sketch of distinct names (name: the normalised query name, "" = not
// counted). Query types are counted by addQType.
func (t *topSet) addQuery(e *QueryEvent, name string, domains bool) {
	ts := e.Time.UnixMilli()
	blocked := isBlocked(e.Status)
	var h uint64
	if name != "" {
		h = hashName(name)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.changed = true
	if name != "" {
		t.uniq.add(h)
	}
	kind := dnsTopDomain
	if blocked {
		kind = dnsTopBlocked
	}
	if domains {
		if d := t.dnsEntry(kind, e.QName); d != nil {
			d.Count++
			d.LastSeen = max(d.LastSeen, ts)
		}
	}
	if c := t.dnsEntry(dnsTopClient, e.ClientIP); c != nil {
		c.Count++
		if blocked {
			c.Blocked++
		}
		if e.ClientName != "" {
			c.Label = e.ClientName
		}
		c.LastSeen = max(c.LastSeen, ts)
	}
	if u := t.dnsEntry(dnsTopUpstream, e.Upstream); u != nil {
		u.Count++
		u.DurationUs += e.DurationUs
		u.LastSeen = max(u.LastSeen, ts)
	}
	if p := t.dnsEntry(dnsTopPurpose, e.Purpose); p != nil {
		p.Count++
		p.LastSeen = max(p.LastSeen, ts)
	}
}

// addQType counts a query by its type; beyond maxQTypeKeys types of the
// hour it is counted under qtypeOther.
func (t *topSet) addQType(e *QueryEvent) {
	key := e.QType
	if key == "" {
		key = qtypeOther
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.changed = true
	m := t.dns[dnsTopQType]
	if _, ok := m[key]; !ok && key != qtypeOther {
		n := len(m)
		if _, other := m[qtypeOther]; other {
			n--
		}
		if n >= maxQTypeKeys {
			key = qtypeOther
		}
	}
	if q := t.dnsEntry(dnsTopQType, key); q != nil {
		q.Count++
		q.LastSeen = max(q.LastSeen, e.Time.UnixMilli())
	}
}

// uniqSnapshot returns the current hour and a copy of its sketch.
func (t *topSet) uniqSnapshot() (int64, hll) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.uniq == nil { // the Discard store
		return t.hour, hll{}
	}
	return t.hour, *t.uniq
}

// addCache counts a cache request in the current hour.
func (t *topSet) addCache(e *CacheEvent) {
	last := e.Time.UnixMilli() + e.DurationMs
	t.mu.Lock()
	defer t.mu.Unlock()
	t.changed = true
	add := func(r *cacheTopRow, label string) {
		if r == nil {
			return
		}
		r.Requests++
		r.Sent += e.BytesSent
		r.Hit += e.BytesHit
		r.WAN += e.BytesWAN
		if label != "" {
			r.Label = label
		}
		r.LastSeen = max(r.LastSeen, last)
	}
	add(t.cacheEntry(cacheTopClient, contentKey{"", e.ClientIP}), e.ClientName)
	add(t.cacheEntry(cacheTopContent, contentKey{e.Service, e.GroupKey}), e.Label)
}

// dnsSnapshot returns the current hour and its top rows of kind k (at most
// topKeysPerHour, highest count first).
func (t *topSet) dnsSnapshot(k dnsKind) (int64, []dnsTopRow) {
	t.mu.Lock()
	hour := t.hour
	rows := make([]dnsTopRow, 0, len(t.dns[k]))
	for _, e := range t.dns[k] {
		rows = append(rows, *e)
	}
	t.mu.Unlock()
	return hour, topDNS(rows)
}

// cacheSnapshot returns the current hour and its top rows of kind k (at
// most topKeysPerHour, most bytes sent first).
func (t *topSet) cacheSnapshot(k cacheKind) (int64, []cacheTopRow) {
	t.mu.Lock()
	hour := t.hour
	rows := make([]cacheTopRow, 0, len(t.cache[k]))
	for _, e := range t.cache[k] {
		rows = append(rows, *e)
	}
	t.mu.Unlock()
	return hour, topCache(rows)
}

func topDNS(rows []dnsTopRow) []dnsTopRow {
	slices.SortFunc(rows, func(a, b dnsTopRow) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Key, b.Key))
	})
	return rows[:min(len(rows), topKeysPerHour)]
}

func topCache(rows []cacheTopRow) []cacheTopRow {
	slices.SortFunc(rows, func(a, b cacheTopRow) int {
		return cmp.Or(cmp.Compare(b.Sent, a.Sent), cmp.Compare(b.Requests, a.Requests),
			cmp.Compare(a.Service, b.Service), cmp.Compare(a.Key, b.Key))
	})
	return rows[:min(len(rows), topKeysPerHour)]
}

func emptyTopMaps() (dns [numDNSKinds]map[string]*dnsTopRow, cache [numCacheKinds]map[contentKey]*cacheTopRow) {
	for i := range dns {
		dns[i] = map[string]*dnsTopRow{}
	}
	for i := range cache {
		cache[i] = map[contentKey]*cacheTopRow{}
	}
	return dns, cache
}

// loadTop makes hour the current top hour, starting from the rows already
// stored for it (a checkpoint before a restart).
func (s *Store) loadTop(ctx context.Context, hour int64) error {
	dns, cache := emptyTopMaps()
	var uniq *hll
	rows, err := s.d.R.QueryContext(ctx, `SELECT kind, key, label, count, blocked, duration_us, last_seen
		FROM logs_dns_top_hourly WHERE bucket = ?`, hour)
	if err != nil {
		return err
	}
	for rows.Next() {
		var kind string
		var r dnsTopRow
		if err := rows.Scan(&kind, &r.Key, &r.Label, &r.Count, &r.Blocked, &r.DurationUs, &r.LastSeen); err != nil {
			rows.Close()
			return err
		}
		if k := slices.Index(dnsKindNames[:], kind); k >= 0 {
			dns[k][r.Key] = &r
		} else if kind == uniqueKind && r.Key == "" {
			uniq, _ = decodeHLL(r.Label)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	rows, err = s.d.R.QueryContext(ctx, `SELECT kind, service, key, label, requests, bytes_sent, bytes_hit, bytes_wan, last_seen
		FROM logs_cache_top_hourly WHERE bucket = ?`, hour)
	if err != nil {
		return err
	}
	for rows.Next() {
		var kind string
		var r cacheTopRow
		if err := rows.Scan(&kind, &r.Service, &r.Key, &r.Label, &r.Requests, &r.Sent, &r.Hit, &r.WAN, &r.LastSeen); err != nil {
			rows.Close()
			return err
		}
		if k := slices.Index(cacheKindNames[:], kind); k >= 0 {
			cache[k][contentKey{r.Service, r.Key}] = &r
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	s.top.replace(hour, dns, cache, uniq)
	return nil
}

// writeTop replaces the stored top rows of the current hour with the top
// topKeysPerHour of every kind (skipped when nothing was counted since the
// last write).
func (s *Store) writeTop(ctx context.Context) error {
	if !s.top.setChanged(false) {
		return nil
	}
	var hour int64
	var dns [numDNSKinds][]dnsTopRow
	var cache [numCacheKinds][]cacheTopRow
	for k := range dns {
		hour, dns[k] = s.top.dnsSnapshot(dnsKind(k))
	}
	for k := range cache {
		_, cache[k] = s.top.cacheSnapshot(cacheKind(k))
	}
	_, uniq := s.top.uniqSnapshot()
	err := s.d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM logs_dns_top_hourly WHERE bucket = ?`, hour); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM logs_cache_top_hourly WHERE bucket = ?`, hour); err != nil {
			return err
		}
		ins, err := tx.PrepareContext(ctx, `INSERT INTO logs_dns_top_hourly
			(bucket, kind, key, label, count, blocked, duration_us, last_seen) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer ins.Close()
		for k, rows := range dns {
			for _, r := range rows {
				if _, err := ins.ExecContext(ctx, hour, dnsKindNames[k], r.Key, r.Label, r.Count, r.Blocked, r.DurationUs, r.LastSeen); err != nil {
					return err
				}
			}
		}
		if !uniq.empty() {
			if _, err := ins.ExecContext(ctx, hour, uniqueKind, "", uniq.encode(), uniq.estimate(), 0, 0, 0); err != nil {
				return err
			}
		}
		insC, err := tx.PrepareContext(ctx, `INSERT INTO logs_cache_top_hourly
			(bucket, kind, service, key, label, requests, bytes_sent, bytes_hit, bytes_wan, last_seen)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer insC.Close()
		for k, rows := range cache {
			for _, r := range rows {
				if _, err := insC.ExecContext(ctx, hour, cacheKindNames[k], r.Service, r.Key, r.Label, r.Requests, r.Sent, r.Hit, r.WAN, r.LastSeen); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		s.top.setChanged(true) // retry at the next checkpoint
	}
	return err
}
