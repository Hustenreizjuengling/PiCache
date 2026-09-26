package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/logs"
)

// A device key resolves to the addresses the device had in the range, at
// most 256, the most recently seen first; the series sums them.
func TestClientSeriesDeviceKey(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
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
	c, err := reg.CreateClient(ctx, clients.ClientInput{Name: "Lab", Identifiers: []string{"10.9.0.0/16"}})
	if err != nil {
		t.Fatal(err)
	}
	// 300 addresses of the client (the later, the more recently seen) and
	// one other address, in a stored hour.
	hour := time.Now().Truncate(time.Hour).Add(-2 * time.Hour).UnixMilli()
	tx, err := ldb.W.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range 300 {
		if _, err := tx.Exec(`INSERT INTO logs_dns_top_hourly (bucket, kind, key, count, blocked, last_seen) VALUES (?, 'client', ?, 1, 0, ?)`,
			hour, fmt.Sprintf("10.9.%d.%d", i/200, i%200+1), hour+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO logs_dns_top_hourly (bucket, kind, key, count, blocked, last_seen) VALUES (?, 'client', '192.168.1.2', 50, 0, ?)`,
		hour, hour+1000); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s := &Server{d: Deps{Logs: st, Clients: reg, Log: log}, log: log}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/stats/clients/x/series?range=24h", nil)
	r.SetPathValue("key", "client:"+strconv.FormatInt(c.ID, 10)) // the handler is called without the mux
	rec := httptest.NewRecorder()
	if err := s.logsClientSeries(rec, r); err != nil {
		t.Fatal(err)
	}
	ser := logsDecode[logs.ClientSeries](t, rec)
	if len(ser.Addresses) != logs.MaxSeriesAddresses || ser.Addresses[0] != "10.9.1.100" {
		t.Fatalf("%d addresses, first %v", len(ser.Addresses), ser.Addresses[:1])
	}
	var sum float64
	for _, v := range ser.Values["allowed"] {
		sum += v
	}
	if sum != logs.MaxSeriesAddresses {
		t.Fatalf("allowed %v", sum)
	}
}
