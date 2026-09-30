package dnsserver

import (
	"context"
	"database/sql"
	"log/slog"
	"slices"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// storedTargets returns the targets of forwarder id as stored.
func storedTargets(t *testing.T, e *testEnv, id int64) string {
	t.Helper()
	var ups string
	if err := e.srv.d.DB.R.QueryRow(`SELECT upstreams FROM dns_forwarders WHERE id = ?`, id).Scan(&ups); err != nil {
		t.Fatal(err)
	}
	return ups
}

// A target given as host#port is stored as host:port on every save path
// (create, update, sync; the import in TestImportForwardersHashPort): a
// version before 1.0.0 ignores "#port" after going back, so
// 127.0.0.1#5335 would reach 127.0.0.1:53. A target that 1.0.0-rc.2
// stored as host#port keeps working with its port and is stored as
// host:port by the next save.
func TestForwarderTargetsStoredAsHostPort(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	f, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domain: "corp.example", Enabled: true,
		Upstreams: []string{"10.0.0.53#5353", "[fd00::53]#5335", "tls://dns.example#8853", "UDP://10.0.0.54#53"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.53:5353", "[fd00::53]:5335", "tls://dns.example:8853", "UDP://10.0.0.54:53"}
	if !slices.Equal(f.Upstreams, want) || storedTargets(t, e, f.ID) != `["10.0.0.53:5353","[fd00::53]:5335","tls://dns.example:8853","UDP://10.0.0.54:53"]` {
		t.Fatalf("created %v, stored %s", f.Upstreams, storedTargets(t, e, f.ID))
	}
	f, err = e.srv.UpdateForwarder(ctx, f.ID, ForwarderInput{Domain: "corp.example", Enabled: true, Upstreams: []string{"127.0.0.2#5335"}})
	if err != nil || storedTargets(t, e, f.ID) != `["127.0.0.2:5335"]` {
		t.Fatalf("updated %+v %v, stored %s", f, err, storedTargets(t, e, f.ID))
	}

	// Stored by 1.0.0-rc.2 as typed: read as it is, with its port.
	if _, err := e.srv.d.DB.W.ExecContext(ctx, `UPDATE dns_forwarders SET upstreams = '["127.0.0.2#5335"]' WHERE id = ?`, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.ReloadRecords(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := e.srv.Forwarders(ctx)
	if err != nil || len(list) != 1 || !slices.Equal(list[0].Upstreams, []string{"127.0.0.2#5335"}) {
		t.Fatalf("stored #port: %+v %v", list, err)
	}
	if spec, err := settings.ParseUpstream(list[0].Upstreams[0]); err != nil || spec.Addr() != "127.0.0.2:5335" {
		t.Fatalf("stored #port asks %s (%v)", spec.Addr(), err)
	}
	if _, err := e.srv.UpdateForwarder(ctx, f.ID, ForwarderInput{Domain: "corp.example", Enabled: true, Upstreams: list[0].Upstreams}); err != nil ||
		storedTargets(t, e, f.ID) != `["127.0.0.2:5335"]` {
		t.Fatalf("the next save stored %s (%v)", storedTargets(t, e, f.ID), err)
	}

	// The follower sync validates the primary's forwarders like a save.
	synced, err := e.srv.ValidateSync(nil, []Forwarder{{ID: 7, Domain: "sync.example", Domains: []string{"sync.example"},
		Upstreams: []string{"10.0.0.53#5353"}, Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.srv.d.DB.Tx(ctx, func(tx *sql.Tx) error { return ReplaceSynced(ctx, tx, synced) }); err != nil {
		t.Fatal(err)
	}
	if got := storedTargets(t, e, 7); got != `["10.0.0.53:5353"]` {
		t.Fatalf("synced %s", got)
	}
}

// A target an earlier version stored with other text after "#" is read
// without it and logged once, naming the forwarder, when the forwarders
// are loaded (at the start, after a sync or restore); a reload after
// another change does not log it again, and it is logged again when a
// restore brings it back after it was fixed.
func TestLegacyForwarderTargetsLogged(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	logs := &captureLog{}
	e.srv.log = slog.New(logs)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.srv.d.DB.W.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO dns_forwarders (id, domain, upstreams, enabled, validate, comment, created_at, updated_at)
		VALUES (5, 'corp.example', '["192.168.1.1#corp","127.0.0.1#5335"]', 1, 0, '', 1, 1)`)
	exec(`INSERT INTO dns_forwarder_domains (forwarder_id, position, domain) VALUES (5, 0, 'corp.example')`)
	const msg = `an upstream saved by an earlier version has text after "#"`
	if err := e.srv.ReloadRecords(ctx); err != nil {
		t.Fatal(err)
	}
	if n := logs.count(msg); n != 1 || logs.count(`forwarder=corp.example stored="192.168.1.1#corp" used="192.168.1.1"`) != 1 ||
		logs.count("fix it under Local DNS → Conditional forwarders") != 1 {
		t.Fatalf("%d warnings: %v", n, logs.msgs)
	}
	list, err := e.srv.Forwarders(ctx)
	if err != nil || len(list) != 1 || !slices.Equal(list[0].Upstreams, []string{"192.168.1.1", "127.0.0.1#5335"}) {
		t.Fatalf("read as %+v %v", list, err)
	}
	// Another change reloads the forwarders: no second warning.
	if _, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domain: "other.example", Upstreams: []string{"10.0.0.1"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if n := logs.count(msg); n != 1 {
		t.Fatalf("warned again after another change: %v", logs.msgs)
	}
	// Fixed by a save, then brought back (a restore): warned again.
	if _, err := e.srv.UpdateForwarder(ctx, 5, ForwarderInput{Domain: "corp.example", Upstreams: list[0].Upstreams, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if got := storedTargets(t, e, 5); got != `["192.168.1.1","127.0.0.1:5335"]` {
		t.Fatalf("the save stored %s", got)
	}
	exec(`UPDATE dns_forwarders SET upstreams = '["192.168.1.1#corp"]' WHERE id = 5`)
	if err := e.srv.ReloadRecords(ctx); err != nil {
		t.Fatal(err)
	}
	if n := logs.count(msg); n != 2 {
		t.Fatalf("%d warnings after the entry came back: %v", n, logs.msgs)
	}
}
