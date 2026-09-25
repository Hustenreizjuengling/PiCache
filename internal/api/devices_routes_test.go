package api

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/logs"
)

// Statistics grouped by device: a configured client (two addresses), a
// device known by its MAC (seen data) and a lone address; the ungrouped
// rows keep their shape plus clientId/mac/addresses. The query log takes
// repeated client values (ORed).
func TestStatsGroupedByDevice(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		log := slog.New(slog.DiscardHandler)
		ldb, err := db.Open(filepath.Join(dir, "logs.db"), 2)
		if err != nil {
			t.Fatal(err)
		}
		defer ldb.Close()
		cdb, err := db.Open(filepath.Join(dir, "picache.db"), 2)
		if err != nil {
			t.Fatal(err)
		}
		defer cdb.Close()
		st, err := logs.New(ctx, ldb, nil, log)
		if err != nil {
			t.Fatal(err)
		}
		reg, err := clients.New(ctx, cdb, ldb, log)
		if err != nil {
			t.Fatal(err)
		}
		laptop, err := reg.CreateClient(ctx, clients.ClientInput{Name: "Laptop", Identifiers: []string{"192.168.1.5", "fd00::5"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ldb.W.ExecContext(ctx, `INSERT INTO clients_seen (ip, mac, hostname, first_seen, last_seen, queries)
			VALUES ('192.168.1.8', 'aa:00:00:00:00:08', 'tv.lan', 1, 2, 1), ('fd00::8', 'aa:00:00:00:00:08', '', 1, 2, 1)`); err != nil {
			t.Fatal(err)
		}
		s := &Server{d: Deps{Logs: st, Clients: reg, Log: log}, log: log}
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { st.Start(runCtx); close(done) }()
		defer func() { cancel(); <-done }()

		now := time.Now()
		for _, ev := range []struct {
			ip, name string
			n        int
			at       time.Duration
		}{
			{"192.168.1.5", "Laptop", 3, 0},
			{"fd00::5", "Laptop", 2, 2 * time.Second},
			{"192.168.1.8", "tv.lan", 1, time.Second},
			{"fd00::8", "", 4, 3 * time.Second},
			{"10.0.0.9", "", 5, 0},
		} {
			for range ev.n {
				st.LogQuery(logs.QueryEvent{Time: now.Add(ev.at), ClientIP: ev.ip, ClientName: ev.name, QName: "a.example",
					QType: "A", Status: "forwarded"})
			}
		}
		st.LogCache(logs.CacheEvent{Time: now, ClientIP: "192.168.1.8", Service: "steam", Host: "cdn.example", Path: "/depot/1/chunk/a",
			Method: "GET", Status: 200, CacheStatus: "HIT", BytesSent: 700, BytesHit: 700, GroupKey: "steam:depot:1"})
		st.LogCache(logs.CacheEvent{Time: now, ClientIP: "fd00::8", Service: "steam", Host: "cdn.example", Path: "/depot/1/chunk/b",
			Method: "GET", Status: 200, CacheStatus: "HIT", BytesSent: 300, BytesHit: 300, GroupKey: "steam:depot:1"})
		time.Sleep(6 * time.Second) // one flush
		synctest.Wait()

		// Ungrouped: one row per address, as before, plus clientId, mac and addresses.
		rows := logsDecode[[]logs.ClientStat](t, logsGet(s, s.logsClientStats, "/api/v1/stats/clients?range=1h"))
		if len(rows) != 5 {
			t.Fatalf("ungrouped rows %+v", rows)
		}
		byIP := map[string]logs.ClientStat{}
		for _, r := range rows {
			byIP[r.ClientIP] = r
			if !slices.Equal(r.Addresses, []string{r.ClientIP}) {
				t.Errorf("ungrouped addresses %v", r.Addresses)
			}
		}
		if r := byIP["fd00::5"]; r.ClientID != laptop.ID || r.Queries != 2 {
			t.Errorf("laptop row %+v", r)
		}
		if r := byIP["fd00::8"]; r.MAC != "aa:00:00:00:00:08" || r.ClientID != 0 || r.Queries != 4 {
			t.Errorf("tv row %+v", r)
		}
		if r := byIP["10.0.0.9"]; r.MAC != "" || r.ClientID != 0 {
			t.Errorf("lone row %+v", r)
		}

		// Grouped by device.
		rows = logsDecode[[]logs.ClientStat](t, logsGet(s, s.logsClientStats, "/api/v1/stats/clients?range=1h&group=device"))
		var got []string
		for _, r := range rows {
			got = append(got, r.ClientIP+" "+r.ClientName+" "+strconv.FormatInt(r.Queries, 10)+" "+r.MAC+" "+
				strconv.FormatInt(r.ClientID, 10)+" "+strconv.FormatInt(r.CacheBytes, 10)+" "+r.Addresses[0])
		}
		want := []string{
			"fd00::8 tv.lan 5 aa:00:00:00:00:08 0 1000 fd00::8",
			"10.0.0.9  5  0 0 10.0.0.9", // ties: by address
			"fd00::5 Laptop 5  " + strconv.FormatInt(laptop.ID, 10) + " 0 fd00::5",
		}
		if !slices.Equal(got, want) {
			t.Fatalf("grouped rows\n%q\nwant\n%q", got, want)
		}
		if !slices.Equal(rows[0].Addresses, []string{"fd00::8", "192.168.1.8"}) || !slices.Equal(rows[2].Addresses, []string{"fd00::5", "192.168.1.5"}) {
			t.Errorf("addresses %v / %v, want most recent first", rows[0].Addresses, rows[1].Addresses)
		}
		if !rows[0].LastSeen.Equal(byIP["fd00::8"].LastSeen) || !rows[0].LastSeen.After(byIP["192.168.1.8"].LastSeen) {
			t.Errorf("lastSeen %v, want the latest of the device", rows[0].LastSeen)
		}

		// Top clients grouped by device: summed, key = the most active address.
		top := logsDecode[[]logs.TopItem](t, logsGet(s, s.logsTop, "/api/v1/stats/top?kind=clients&range=1h&group=device&limit=2"))
		if len(top) != 2 || top[0].Count != 5 || top[1].Count != 5 {
			t.Fatalf("grouped top %+v", top)
		}
		for _, it := range top {
			switch it.Label {
			case "tv.lan":
				if it.Key != "fd00::8" || !slices.Equal(it.Addresses, []string{"fd00::8", "192.168.1.8"}) {
					t.Errorf("tv %+v", it)
				}
			case "Laptop":
				if it.Key != "192.168.1.5" || !slices.Equal(it.Addresses, []string{"192.168.1.5", "fd00::5"}) {
					t.Errorf("laptop %+v", it)
				}
			default:
				if it.Key != "10.0.0.9" || !slices.Equal(it.Addresses, []string{"10.0.0.9"}) {
					t.Errorf("lone %+v", it)
				}
			}
		}
		cache := logsDecode[[]logs.TopItem](t, logsGet(s, s.logsTop, "/api/v1/stats/top?kind=cache-clients&range=1h&group=device"))
		if len(cache) != 1 || cache[0].Bytes != 1000 || cache[0].Count != 2 || cache[0].Key != "192.168.1.8" || cache[0].Label != "tv.lan" {
			t.Errorf("grouped cache clients %+v", cache)
		}
		plain := logsDecode[[]logs.TopItem](t, logsGet(s, s.logsTop, "/api/v1/stats/top?kind=clients&range=1h"))
		if len(plain) != 5 || plain[0].Addresses != nil {
			t.Errorf("ungrouped top %+v", plain)
		}
		domains := logsDecode[[]logs.TopItem](t, logsGet(s, s.logsTop, "/api/v1/stats/top?kind=domains&range=1h&group=device"))
		if len(domains) != 1 || domains[0].Addresses != nil {
			t.Errorf("group=device does not apply to domains: %+v", domains)
		}
		if w := logsGet(s, s.logsClientStats, "/api/v1/stats/clients?group=mac"); w.Code != 400 ||
			logsDecode[errorBody](t, w).Error.Field != "group" {
			t.Errorf("bad group: %d %s", w.Code, w.Body)
		}

		// The query log ORs repeated client values: all addresses of a device.
		page := logsDecode[logs.QueryPage](t, logsGet(s, s.logsQueries, "/api/v1/logs/queries?client=192.168.1.5&client=fd00::5&limit=100"))
		if len(page.Items) != 5 {
			t.Errorf("laptop queries %d, want 5", len(page.Items))
		}
		page = logsDecode[logs.QueryPage](t, logsGet(s, s.logsQueries, "/api/v1/logs/queries?client=10.0.0.9&client=tv.l&limit=100"))
		if len(page.Items) != 6 {
			t.Errorf("address or name: %d, want 6", len(page.Items))
		}
		if w := logsGet(s, s.logsQueries, "/api/v1/logs/queries?client=10.0.0.9&client=ab"); w.Code != 400 {
			t.Errorf("a short name among the values: %d", w.Code)
		}
	})
}
