package clients

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
)

// lockedBuffer is a log sink safe for concurrent writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), sub)
}

// storedGroupUpstreams returns the upstreams of group id as stored.
func storedGroupUpstreams(t *testing.T, r *Registry, id int64) string {
	t.Helper()
	var ups string
	if err := r.db.R.QueryRow(`SELECT upstreams FROM client_groups WHERE id = ?`, id).Scan(&ups); err != nil {
		t.Fatal(err)
	}
	return ups
}

// A group upstream given as host#port is stored as host:port on every
// save path (create, update, PUT upstreams, sync; duplicates of the same
// address removed): a version before 1.0.0 ignores "#port" after going
// back. A list that 1.0.0-rc.2 stored as typed keeps working and is stored
// as host:port by the next save of the group, even one that keeps it.
func TestGroupUpstreamsStoredAsHostPort(t *testing.T) {
	r := newTestRegistry(t, false)
	ctx := context.Background()
	g, err := r.CreateGroup(ctx, GroupInput{Name: "Kids", Enabled: true,
		Upstreams: []string{"10.0.0.53#5353", "10.0.0.53:5353", "[fd00::53]#5335", "tls://dns.example#8853"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.53:5353", "[fd00::53]:5335", "tls://dns.example:8853"}
	if !slices.Equal(g.Upstreams, want) || storedGroupUpstreams(t, r, g.ID) != `["10.0.0.53:5353","[fd00::53]:5335","tls://dns.example:8853"]` {
		t.Fatalf("created %v, stored %s", g.Upstreams, storedGroupUpstreams(t, r, g.ID))
	}
	if _, err := r.UpdateGroup(ctx, g.ID, GroupInput{Name: "Kids", Enabled: true, Upstreams: []string{"127.0.0.2#5335"}}); err != nil ||
		storedGroupUpstreams(t, r, g.ID) != `["127.0.0.2:5335"]` {
		t.Fatalf("update: stored %s (%v)", storedGroupUpstreams(t, r, g.ID), err)
	}
	if _, err := r.SetGroupUpstreams(ctx, g.ID, []string{"quic://dns.example#8853"}, ""); err != nil ||
		storedGroupUpstreams(t, r, g.ID) != `["quic://dns.example:8853"]` {
		t.Fatalf("set upstreams: stored %s (%v)", storedGroupUpstreams(t, r, g.ID), err)
	}

	// Stored by 1.0.0-rc.2 as typed: read as it is; a save that keeps the
	// list (upstreams left out) stores it as host:port.
	if _, err := r.db.W.Exec(`UPDATE client_groups SET upstreams = '["127.0.0.2#5335"]' WHERE id = ?`, g.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Groups(ctx); err != nil || !slices.Equal(got[len(got)-1].Upstreams, []string{"127.0.0.2#5335"}) {
		t.Fatalf("stored #port read as %+v %v", got, err)
	}
	if _, err := r.UpdateGroup(ctx, g.ID, GroupInput{Name: "Kids 2", Enabled: true}); err != nil ||
		storedGroupUpstreams(t, r, g.ID) != `["127.0.0.2:5335"]` {
		t.Fatalf("a save keeping the list stored %s (%v)", storedGroupUpstreams(t, r, g.ID), err)
	}

	// The follower sync validates the primary's groups like a save.
	groups, err := r.Groups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range groups {
		if groups[i].ID == g.ID {
			groups[i].Upstreams = []string{"10.0.0.53#5353"}
		}
	}
	synced, err := ValidateSync(groups, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.db.Tx(ctx, func(tx *sql.Tx) error { return ReplaceSynced(ctx, tx, synced) }); err != nil {
		t.Fatal(err)
	}
	if got := storedGroupUpstreams(t, r, g.ID); got != `["10.0.0.53:5353"]` {
		t.Fatalf("synced %s", got)
	}
}

// A group upstream an earlier version stored with other text after "#" is
// read without it and logged once, naming the group, when the groups are
// loaded (at the start, after a sync or restore); a reload after another
// change does not log it again, and it is logged again when a restore
// brings it back after it was fixed.
func TestLegacyGroupUpstreamsLogged(t *testing.T) {
	r := newTestRegistry(t, false)
	ctx := context.Background()
	logs := &lockedBuffer{}
	r.log = slog.New(slog.NewTextHandler(logs, nil))
	g := mustGroup(t, r, "Kids", true)
	if _, err := r.db.W.Exec(`UPDATE client_groups SET upstreams = '["1.1.1.1#family","127.0.0.1#5335"]' WHERE id = ?`, g.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	const msg = `an upstream saved by an earlier version has text after \"#\"`
	if n := logs.count(msg); n != 1 || logs.count(`group=Kids stored="\"1.1.1.1#family\"" used="\"1.1.1.1\""`) != 1 ||
		logs.count("fix it under Clients & groups") != 1 {
		t.Fatalf("%d warnings: %s", n, logs.b.String())
	}
	if got, err := r.group(ctx, g.ID); err != nil || !slices.Equal(got.Upstreams, []string{"1.1.1.1", "127.0.0.1#5335"}) {
		t.Fatalf("read as %+v %v", got, err)
	}
	// Another change reloads the groups: no second warning.
	mustGroup(t, r, "Guests", true)
	if n := logs.count(msg); n != 1 {
		t.Fatalf("warned again after another change: %s", logs.b.String())
	}
	// Fixed by a save, then brought back (a restore): warned again.
	if _, err := r.UpdateGroup(ctx, g.ID, GroupInput{Name: "Kids", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if got := storedGroupUpstreams(t, r, g.ID); got != `["1.1.1.1","127.0.0.1:5335"]` {
		t.Fatalf("the save stored %s", got)
	}
	if _, err := r.db.W.Exec(`UPDATE client_groups SET upstreams = '["1.1.1.1#family"]' WHERE id = ?`, g.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if n := logs.count(msg); n != 2 {
		t.Fatalf("%d warnings after the entry came back: %s", n, logs.b.String())
	}
}
