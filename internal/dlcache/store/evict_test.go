package cachestore

import (
	"context"
	"errors"
	"maps"
	"strconv"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

const day = 24 * time.Hour

// setLastAccess flushes and then backdates an object's last access.
func setLastAccess(t *testing.T, s *Store, id string, at time.Time) {
	t.Helper()
	mustFlush(t, s)
	if _, err := s.db.W.Exec(`UPDATE store_objects SET last_access = ? WHERE id = ?`, at.UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
}

// lruObjects creates n single-slice objects with increasing last access.
func lruObjects(t *testing.T, s *Store, n int) []string {
	t.Helper()
	base := time.Now().Add(-time.Hour)
	ids := make([]string, n)
	for i := range ids {
		ids[i], _ = putObject(t, s, "steam", "/lru/"+strconv.Itoa(i), "steam:lru", testSlice)
		setLastAccess(t, s, ids[i], base.Add(time.Duration(i)*time.Minute))
	}
	return ids
}

func present(t *testing.T, s *Store, id string) bool {
	t.Helper()
	_, ok, err := s.Head(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func wantReasons(t *testing.T, res EvictResult, want map[string]int64) {
	t.Helper()
	if !maps.Equal(res.Reasons, want) {
		t.Fatalf("reasons = %v, want %v", res.Reasons, want)
	}
}

func TestEvictInactive(t *testing.T) {
	s, e := newStore(t)
	ctx := context.Background()
	old, _ := putObject(t, s, "steam", "/old", "steam:a", 1000)
	oldPinned, _ := putObject(t, s, "steam", "/old-pinned", "steam:a", 1000)
	oldTouched, _ := putObject(t, s, "steam", "/old-touched", "steam:a", 1000)
	recent, _ := putObject(t, s, "steam", "/recent", "steam:a", 1000)
	for _, id := range []string{old, oldPinned, oldTouched} {
		setLastAccess(t, s, id, time.Now().Add(-400*day))
	}
	if err := s.SetPinned(ctx, oldPinned, true); err != nil {
		t.Fatal(err)
	}
	s.Touch(oldTouched, 10) // an access since the last statistics flush counts
	var reasons []string
	s.OnEvict(func(o Object, reason string) { reasons = append(reasons, o.Path+" "+reason) })

	res, err := s.Evict(ctx, Policy{MaxAge: 365 * day})
	if err != nil {
		t.Fatal(err)
	}
	wantReasons(t, res, map[string]int64{ReasonInactive: 1})
	if res.Objects != 1 || res.Bytes != 1000 || res.Full {
		t.Fatalf("result %+v", res)
	}
	if present(t, s, old) || !present(t, s, oldPinned) || !present(t, s, oldTouched) || !present(t, s, recent) {
		t.Fatal("wrong objects evicted")
	}
	if fileExists(t, e, old, 0) {
		t.Fatal("evicted file still on disk")
	}
	if len(reasons) != 1 || reasons[0] != "/old inactive" {
		t.Fatalf("OnEvict: %v", reasons)
	}
	checkAggregates(t, s)
}

func TestEvictSize(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ids := lruObjects(t, s, 4)
	// cached = 4 S > max = 2.5 S → free down to 95 % of max: deficit 1.625 S → the two oldest go.
	res, err := s.Evict(ctx, Policy{MaxBytes: 5 * testSlice / 2})
	if err != nil {
		t.Fatal(err)
	}
	wantReasons(t, res, map[string]int64{ReasonSize: 2})
	if res.Full || res.Bytes != 2*testSlice {
		t.Fatalf("result %+v", res)
	}
	if present(t, s, ids[0]) || present(t, s, ids[1]) || !present(t, s, ids[2]) || !present(t, s, ids[3]) {
		t.Fatal("eviction did not follow LRU order")
	}
	// Under the limit: nothing happens.
	if res, err := s.Evict(ctx, Policy{MaxBytes: 5 * testSlice / 2}); err != nil || res.Objects != 0 {
		t.Fatalf("second run: %+v %v", res, err)
	}
	// Everything left pinned: the store is full.
	for _, id := range ids[2:] {
		if err := s.SetPinned(ctx, id, true); err != nil {
			t.Fatal(err)
		}
	}
	res, err = s.Evict(ctx, Policy{MaxBytes: testSlice})
	if err != nil || !res.Full || res.Objects != 0 {
		t.Fatalf("pinned store: %+v %v", res, err)
	}
	checkAggregates(t, s)
}

func TestEvictMinFree(t *testing.T) {
	const S = testSlice
	tests := []struct {
		name    string
		policy  Policy
		evicted int // oldest objects expected to be gone
		reasons map[string]int64
		full    bool
	}{
		{
			// deficit = 4 S·1.05 − 3.5 S = 0.7 S → one object
			name:    "deficit",
			policy:  Policy{MinFreeBytes: 4 * S, FreeBytes: func() (uint64, error) { return 7 * S / 2, nil }},
			evicted: 1, reasons: map[string]int64{ReasonMinFree: 1},
		},
		{
			// deficit = 4 S·1.05 − 2.5 S = 1.7 S → two objects
			name:    "larger deficit",
			policy:  Policy{MinFreeBytes: 4 * S, FreeBytes: func() (uint64, error) { return 5 * S / 2, nil }},
			evicted: 2, reasons: map[string]int64{ReasonMinFree: 2},
		},
		{
			name:    "enough free space",
			policy:  Policy{MinFreeBytes: 4 * S, FreeBytes: func() (uint64, error) { return 4 * S, nil }},
			reasons: map[string]int64{},
		},
		{
			name:    "free space unknown",
			policy:  Policy{MinFreeBytes: 4 * S, FreeBytes: func() (uint64, error) { return 0, errors.New("no sample") }},
			reasons: map[string]int64{},
		},
		{
			// size frees 1 S first; the stale sample's deficit (0.7 S) is covered by it
			name: "size eviction counts towards the deficit",
			policy: Policy{MaxBytes: 7 * S / 2, MinFreeBytes: 4 * S,
				FreeBytes: func() (uint64, error) { return 7 * S / 2, nil }},
			evicted: 1, reasons: map[string]int64{ReasonSize: 1},
		},
		{
			// deficit 4.2 S·… larger than everything cached
			name:    "cannot be met",
			policy:  Policy{MinFreeBytes: 100 * S, FreeBytes: func() (uint64, error) { return 0, nil }},
			evicted: 4, reasons: map[string]int64{ReasonMinFree: 4}, full: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newStore(t)
			ids := lruObjects(t, s, 4)
			res, err := s.Evict(context.Background(), tt.policy)
			if err != nil {
				t.Fatal(err)
			}
			wantReasons(t, res, tt.reasons)
			if res.Full != tt.full || res.Objects != int64(tt.evicted) {
				t.Fatalf("result %+v", res)
			}
			for i, id := range ids {
				if present(t, s, id) == (i < tt.evicted) {
					t.Fatalf("object %d present=%v", i, !(i < tt.evicted))
				}
			}
			checkAggregates(t, s)
		})
	}
}

func TestPinnedGroups(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	// Pinned before any object exists: new objects inherit the pin.
	if err := s.SetGroupPinned(ctx, "steam", "steam:pinned", true); err != nil {
		t.Fatal(err)
	}
	p1, _ := putObject(t, s, "steam", "/p1", "steam:pinned", 1000)
	u1, _ := putObject(t, s, "steam", "/u1", "steam:free", 1000)
	later, _ := putObject(t, s, "steam", "/l1", "steam:later", 1000)
	if err := s.SetGroupPinned(ctx, "steam", "steam:later", true); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{p1, u1, later} {
		setLastAccess(t, s, id, time.Now().Add(-400*day))
	}
	objs, err := s.Objects(ctx, ObjectQuery{Sort: "path", Retention: 365 * day})
	if err != nil {
		t.Fatal(err)
	}
	pinned := map[string]bool{}
	for _, o := range objs.Items {
		pinned[o.Path] = o.Pinned
		if o.Pinned != o.ExpiresAt.IsZero() {
			t.Fatalf("%s: pinned=%v expiresAt=%v", o.Path, o.Pinned, o.ExpiresAt)
		}
	}
	if !pinned["/p1"] || pinned["/u1"] || !pinned["/l1"] {
		t.Fatalf("pinned flags %v", pinned)
	}
	g, err := s.Groups(ctx, GroupQuery{GroupKey: "steam:pinned"})
	if err != nil || len(g.Items) != 1 || !g.Items[0].Pinned {
		t.Fatalf("group view: %+v %v", g, err)
	}

	policy := Policy{MaxAge: 365 * day, MaxBytes: 1}
	res, err := s.Evict(ctx, policy)
	if err != nil {
		t.Fatal(err)
	}
	if res.Objects != 1 || !res.Full || present(t, s, u1) || !present(t, s, p1) || !present(t, s, later) {
		t.Fatalf("result %+v", res)
	}
	// Unpinning the object alone does not override the group pin.
	if err := s.SetPinned(ctx, p1, false); err != nil {
		t.Fatal(err)
	}
	if res, _ := s.Evict(ctx, policy); res.Objects != 0 {
		t.Fatalf("group-pinned object evicted: %+v", res)
	}
	if err := s.SetGroupPinned(ctx, "steam", "steam:pinned", false); err != nil {
		t.Fatal(err)
	}
	if res, _ := s.Evict(ctx, policy); res.Objects != 1 || present(t, s, p1) {
		t.Fatalf("after unpinning the group: %+v", res)
	}
	if err := s.SetPinned(ctx, ObjectID("steam", "/nope"), true); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("SetPinned unknown: %v", err)
	}
	if err := s.SetGroupPinned(ctx, "Bad Service", "x", true); apperr.KindOf(err) != apperr.KindInvalid {
		t.Fatalf("SetGroupPinned invalid: %v", err)
	}
	checkAggregates(t, s)
}

func TestDeleteAndPurge(t *testing.T) {
	s, e := newStore(t)
	ctx := context.Background()
	a1, _ := putObject(t, s, "steam", "/a1", "steam:a", 2*testSlice+1)
	a2, _ := putObject(t, s, "steam", "/a2", "steam:a", 100)
	b1, _ := putObject(t, s, "steam", "/b1", "steam:b", 200)
	c1, _ := putObject(t, s, "blizzard", "/c1", "blizzard:c", 300)
	var removed []string
	s.OnEvict(func(o Object, reason string) { removed = append(removed, reason) })

	if err := s.DeleteObject(ctx, "nope"); apperr.KindOf(err) != apperr.KindInvalid {
		t.Fatalf("malformed id: %v", err)
	}
	if err := s.DeleteObject(ctx, ObjectID("steam", "/unknown")); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("unknown id: %v", err)
	}
	freed, err := s.DeleteGroup(ctx, "steam", "steam:a")
	if err != nil || freed != 2*testSlice+1+100 {
		t.Fatalf("DeleteGroup: freed=%d err=%v", freed, err)
	}
	if present(t, s, a1) || present(t, s, a2) || !present(t, s, b1) || fileExists(t, e, a1, 2) {
		t.Fatal("DeleteGroup removed the wrong objects")
	}
	freed, err = s.DeleteService(ctx, "steam")
	if err != nil || freed != 200 || present(t, s, b1) || !present(t, s, c1) {
		t.Fatalf("DeleteService: freed=%d err=%v", freed, err)
	}
	if err := s.DeleteObject(ctx, c1); err != nil || present(t, s, c1) {
		t.Fatalf("DeleteObject: %v", err)
	}
	if len(removed) != 4 || removed[0] != ReasonManual {
		t.Fatalf("OnEvict reasons %v", removed)
	}
	if _, err := s.DeleteGroup(ctx, "steam", ""); apperr.KindOf(err) != apperr.KindInvalid {
		t.Fatalf("empty group key: %v", err)
	}
	if u := s.Usage(); u.Objects != 0 || u.CachedBytes != 0 || u.Slices != 0 {
		t.Fatalf("usage %+v", u)
	}
	checkAggregates(t, s)
}

// TestRemovalOfOpenFile covers Windows semantics: an open slice file cannot
// be deleted; the index entry is dropped first and the next Evict retries.
func TestRemovalOfOpenFile(t *testing.T) {
	s, e := newStore(t)
	ctx := context.Background()
	id, gen := putObject(t, s, "steam", "/open", "steam:o", 1000)
	r, err := s.ReadSlice(ctx, id, gen, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteObject(ctx, id); err != nil {
		t.Fatal(err)
	}
	if present(t, s, id) {
		t.Fatal("index entry must be gone immediately")
	}
	buf := make([]byte, 10)
	if _, err := r.ReadAt(buf, 0); err != nil {
		t.Fatalf("open reader must stay readable: %v", err)
	}
	r.Close()
	if _, err := s.Evict(ctx, Policy{}); err != nil {
		t.Fatal(err)
	}
	if fileExists(t, e, id, 0) || len(s.retry) != 0 {
		t.Fatal("file not removed by the retry")
	}
}

// TestRetryKeepsRewrittenSlice: a pending removal must not delete a slice
// that has been written again in the meantime.
func TestRetryKeepsRewrittenSlice(t *testing.T) {
	s, e := newStore(t)
	ctx := context.Background()
	id, gen := putObject(t, s, "steam", "/rewritten", "steam:o", 1000)
	s.addRetry(retryRemoval{id: id, idx: 0})
	if _, err := s.Evict(ctx, Policy{}); err != nil {
		t.Fatal(err)
	}
	if !fileExists(t, e, id, 0) || !s.HasSlice(ctx, id, gen, 0) {
		t.Fatal("live slice removed by a stale retry")
	}
}
