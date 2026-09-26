package logs

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// The upstream EDE and the client's subnet are stored, read back and sent
// to the live feed; the EDE text is bounded like the resolver bounds it
// and the subnet is anonymised with the client addresses.
func TestQueryEventEDEAndECS(t *testing.T) {
	s, set := newTestStore(t)
	qch, cancel, err := s.SubscribeQueries(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	now := time.Now()
	e := query(now, "192.168.1.7", "blocked.example", "blocked-upstream")
	e.Reason = "dns.quad9.net: ede"
	e.UpstreamEDE = &UpstreamEDE{Code: 15, Text: "Blocked ‮by\x00 policy" + strings.Repeat("x", 300)}
	e.ECS = "203.0.113.77/24"
	plain := query(now, "192.168.1.7", "plain.example", "forwarded")
	s.w.addQuery(e)
	s.w.addQuery(plain)
	s.w.flush(now)

	live := <-qch
	if live.UpstreamEDE == nil || live.UpstreamEDE.Code != 15 || !strings.HasPrefix(live.UpstreamEDE.Text, "Blocked by policyxx") ||
		len(live.UpstreamEDE.Text) > 200 || live.ECS != "203.0.113.0/24" {
		t.Fatalf("live event %+v %+v", live, live.UpstreamEDE)
	}
	if e.UpstreamEDE.Text[8] != 0xe2 {
		t.Error("the producer's event was modified")
	}
	page, err := s.QueryLog(context.Background(), QueryFilter{From: now.Add(-time.Minute), To: now.Add(time.Minute)})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("query log %+v %v", page.Items, err)
	}
	for _, got := range page.Items {
		switch got.QName {
		case "blocked.example":
			if got.UpstreamEDE == nil || *got.UpstreamEDE != *live.UpstreamEDE || got.ECS != "203.0.113.0/24" || got.Status != "blocked-upstream" {
				t.Errorf("stored %+v %+v", got, got.UpstreamEDE)
			}
		case "plain.example":
			if got.UpstreamEDE != nil || got.ECS != "" {
				t.Errorf("an event without EDE/ECS read back %+v %q", got.UpstreamEDE, got.ECS)
			}
		}
	}
	// Anonymised: the subnet keeps at most /16 (IPv4) or /48 (IPv6).
	updateLogs(t, set, func(l *settings.Logs) { l.AnonymizeClientIPs = true })
	for in, want := range map[string]string{
		"203.0.113.0/24":         "203.0.0.0/16",
		"10.0.0.0/8":             "10.0.0.0/8",
		"2001:db8:1:2::/56":      "2001:db8:1::/48",
		"2001:db8::/32":          "2001:db8::/32",
		"::ffff:203.0.113.0/120": "203.0.0.0/16",
		"garbage":                "",
	} {
		ev := query(now, "192.168.1.7", "a.example", "forwarded")
		ev.ECS = in
		if got := cleanQuery(ev, true, now).ECS; got != want {
			t.Errorf("anonymised %q = %q, want %q", in, got, want)
		}
	}
}

// Logs v3 adds the columns without touching stored rows; rows written
// before read back without EDE and subnet.
func TestMigrateEDEColumns(t *testing.T) {
	ctx := context.Background()
	ldb, cdb, set := openTestDBs(t, t.TempDir())
	defer cdb.Close()
	defer ldb.Close()
	if err := ldb.Migrate(ctx, "logs", migrations[:2]); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(-time.Minute).UnixMilli()
	if _, err := ldb.W.ExecContext(ctx, `INSERT INTO logs_queries (ts, client_ip, qname, qtype, status) VALUES (`+
		strconv.FormatInt(ts, 10)+`, '10.0.0.2', 'old.example', 'A', 'forwarded')`); err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, ldb, set, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	page, err := s.QueryLog(ctx, QueryFilter{From: time.Now().Add(-time.Hour)})
	if err != nil || len(page.Items) != 1 || page.Items[0].UpstreamEDE != nil || page.Items[0].ECS != "" {
		t.Fatalf("old row %+v %v", page.Items, err)
	}
}

// blocked-upstream and blocked-rebind are blocked statuses: accepted by the
// status filter, in the class "blocked" and counted as blocked.
func TestNewBlockedStatuses(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Now()
	for _, st := range []string{"blocked-upstream", "blocked-rebind", "forwarded"} {
		s.w.addQuery(query(now, "192.168.1.7", st+".example", st))
	}
	s.w.flush(now)
	for _, filter := range [][]string{{"blocked-upstream"}, {"blocked-rebind"}, {"blocked"}} {
		page, err := s.QueryLog(context.Background(), QueryFilter{From: now.Add(-time.Minute), To: now.Add(time.Minute), Status: filter})
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if filter[0] == "blocked" {
			want = 2
		}
		if len(page.Items) != want {
			t.Errorf("filter %v: %d rows, want %d", filter, len(page.Items), want)
		}
	}
	sum, err := s.Summary(context.Background(), now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil || sum.DNSBlocked != 2 || sum.DNSQueries != 3 {
		t.Fatalf("summary %+v %v", sum, err)
	}
	if m, err := QueryMatcher(nil, []string{"blocked-rebind"}); err != nil || m == nil || !m(QueryEvent{Status: "blocked-rebind"}) {
		t.Errorf("live filter: %v", err)
	}
}
