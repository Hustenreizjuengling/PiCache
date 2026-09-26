package dnsserver

import (
	"context"
	"encoding/json/v2"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

func strp(s string) *string { return &s }

// groups adds client groups (the records per group reference them).
func (e *testEnv) groups(ids ...int64) {
	e.t.Helper()
	for _, id := range ids {
		if _, err := e.srv.d.DB.W.Exec(`INSERT INTO client_groups (id, name, created_at) VALUES (?, ?, 0)`, id, "g"+string(rune('0'+id))); err != nil {
			e.t.Fatal(err)
		}
	}
}

// client makes ip a client of the groups.
func (e *testEnv) client(ip string, groups ...int64) {
	e.cl.set(&clients.Identity{IP: netip.MustParseAddr(ip), ClientID: 1, GroupIDs: groups})
}

// lookupAs asks as the client ip.
func (e *testEnv) lookupAs(name, typ, ip string) LookupResult {
	e.t.Helper()
	res, err := e.srv.Lookup(context.Background(), LookupRequest{Name: name, Type: typ, ClientIP: ip}, netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

// values returns the data of the answers (the part after the type).
func values(res LookupResult) []string {
	var out []string
	for _, a := range res.Answers {
		rr, err := dns.NewRR(a)
		if err != nil {
			out = append(out, a)
			continue
		}
		out = append(out, strings.TrimPrefix(rr.String(), rr.Header().String()))
	}
	return out
}

func (e *testEnv) mustRecord(in RecordInput) Record {
	e.t.Helper()
	in.Enabled = true
	r, err := e.srv.CreateRecord(context.Background(), in)
	if err != nil {
		e.t.Fatalf("create %+v: %v", in, err)
	}
	return r
}

// SRV, MX, PTR, HTTPS and SVCB records: value or data in, both out, the
// canonical value stored, answered as configured.
func TestTypedRecords(t *testing.T) {
	e := newEnv(t, func(a *settings.All) { a.DNS.RefuseANY = false })
	srv := e.mustRecord(RecordInput{Name: "_sip._tcp.example.lan", Type: "srv", Value: " 10 5  5060 SIP.Example.Lan. "})
	if srv.Value != "10 5 5060 sip.example.lan" {
		t.Fatalf("SRV %+v", srv)
	}
	if d, ok := srv.Data.(*SRVData); !ok || *d != (SRVData{Priority: 10, Weight: 5, Port: 5060, Target: "sip.example.lan"}) {
		t.Fatalf("SRV data %#v", srv.Data)
	}
	mx := e.mustRecord(RecordInput{Name: "example.lan", Type: "MX", Data: []byte(`{"preference":0,"host":"."}`)})
	if mx.Value != "0 ." {
		t.Fatalf("MX %+v", mx)
	}
	ptr := e.mustRecord(RecordInput{Name: "5.1.168.192.in-addr.arpa", Type: "PTR", Value: "nas.lan."})
	if ptr.Value != "nas.lan" {
		t.Fatalf("PTR %+v", ptr)
	}
	https := e.mustRecord(RecordInput{Name: "web.lan", Type: "HTTPS", Data: []byte(
		`{"priority":1,"target":".","alpn":["h3","h2"],"port":8443,"ipv4hint":["192.168.1.9"],"ipv6hint":["fd00::9"]}`)})
	if https.Value != "1 . alpn=h3,h2 port=8443 ipv4hint=192.168.1.9 ipv6hint=fd00::9" {
		t.Fatalf("HTTPS %+v", https)
	}
	svcb := e.mustRecord(RecordInput{Name: "_dns.resolver.example.lan", Type: "SVCB", Value: "0 alias.example.lan"})
	b, _ := json.Marshal(svcb)
	if !strings.Contains(string(b), `"data":{"priority":0,"target":"alias.example.lan","alpn":[],"ipv4hint":[],"ipv6hint":[]}`) {
		t.Fatalf("SVCB JSON %s", b)
	}
	a := e.mustRecord(RecordInput{Name: "plain.lan", Type: "A", Value: "192.168.1.8"})
	if b, _ := json.Marshal(a); strings.Contains(string(b), `"data"`) || !strings.Contains(string(b), `"scope":"all","groupIds":[],"otherFamily":"nodata"`) {
		t.Fatalf("A JSON %s", b)
	}

	for _, c := range []struct{ name, typ, want string }{
		{"_sip._tcp.example.lan", "SRV", "10 5 5060 sip.example.lan."},
		{"example.lan", "MX", "0 ."},
		{"5.1.168.192.in-addr.arpa", "PTR", "nas.lan."},
		{"web.lan", "HTTPS", `1 . alpn="h3,h2" port="8443" ipv4hint="192.168.1.9" ipv6hint="fd00::9"`},
		{"_dns.resolver.example.lan", "SVCB", "0 alias.example.lan."},
	} {
		res := e.lookupAs(c.name, c.typ, "192.168.1.50")
		if got := values(res); len(got) != 1 || strings.TrimSpace(got[0]) != c.want || res.Status != StatusLocal {
			t.Errorf("%s %s: %v (%s), want %q", c.name, c.typ, got, res.Status, c.want)
		}
	}
	if res := e.lookupAs("web.lan", "A", "192.168.1.50"); len(res.Answers) != 0 || res.RCode != "NOERROR" {
		t.Errorf("other type: %+v", res)
	}
	if res := e.lookupAs("web.lan", "ANY", "192.168.1.50"); len(res.Answers) != 1 {
		t.Errorf("ANY: %+v", res)
	}

	bad := []struct {
		in    RecordInput
		field string
	}{
		{RecordInput{Name: "s.lan", Type: "SRV", Data: []byte(`{"priority":70000,"weight":0,"port":1,"target":"x.lan"}`)}, "data.priority"},
		{RecordInput{Name: "s.lan", Type: "SRV", Value: "1 2 3"}, "value"},
		{RecordInput{Name: "s.lan", Type: "SRV", Value: "1 2 70000 x.lan"}, "value"},
		{RecordInput{Name: "s.lan", Type: "SRV", Data: []byte(`{"priority":1,"weight":0,"port":1,"target":"bad..name"}`)}, "data.target"},
		{RecordInput{Name: "s.lan", Type: "SRV", Data: []byte(`{"priority":1,"extra":true}`)}, "data"},
		{RecordInput{Name: "m.lan", Type: "MX", Value: "10 ."}, "value"},
		{RecordInput{Name: "m.lan", Type: "MX", Data: []byte(`{"preference":10,"host":"."}`)}, "data.host"},
		{RecordInput{Name: "x.lan", Type: "PTR", Value: "nas.lan"}, "name"},
		{RecordInput{Name: "1.168.192.in-addr.arpa", Type: "PTR", Value: "nas.lan"}, "name"},
		{RecordInput{Name: "*.1.168.192.in-addr.arpa", Type: "PTR", Value: "nas.lan"}, "name"},
		{RecordInput{Name: "6.1.168.192.in-addr.arpa", Type: "PTR", Value: "."}, "value"},
		{RecordInput{Name: "h.lan", Type: "HTTPS", Value: "0 . alpn=h2"}, "value"},
		{RecordInput{Name: "h.lan", Type: "HTTPS", Data: []byte(`{"priority":0,"target":".","port":443}`)}, "data.priority"},
		{RecordInput{Name: "h.lan", Type: "HTTPS", Value: "1 . mandatory=alpn"}, "value"},
		{RecordInput{Name: "h.lan", Type: "HTTPS", Value: "1 . ech=AAAA"}, "value"},
		{RecordInput{Name: "h.lan", Type: "HTTPS", Value: "1 . alpn=h2 alpn=h3"}, "value"},
		{RecordInput{Name: "h.lan", Type: "HTTPS", Data: []byte(`{"priority":1,"target":".","alpn":["H2"]}`)}, "data.alpn"},
		{RecordInput{Name: "h.lan", Type: "HTTPS", Data: []byte(`{"priority":1,"target":".","port":0}`)}, "data.port"},
		{RecordInput{Name: "h.lan", Type: "HTTPS", Data: []byte(`{"priority":1,"target":".","ipv4hint":["fd00::1"]}`)}, "data.ipv4hint"},
		{RecordInput{Name: "h.lan", Type: "HTTPS", Data: []byte(`{"priority":1,"target":".","ipv6hint":["::ffff:1.2.3.4"]}`)}, "data.ipv6hint"},
		{RecordInput{Name: "h.lan", Type: "HTTPS", Data: []byte(`{"priority":1,"target":".","ipv4hint":["0.0.0.0"]}`)}, "data.ipv4hint"},
		{RecordInput{Name: "h.lan", Type: "HTTPS", Data: []byte(`{"priority":1,"target":".","ipv6hint":["fd00::1","fd00::2","fd00::3","fd00::4","fd00::5","fd00::6","fd00::7","fd00::8","fd00::9"]}`)}, "data.ipv6hint"},
		{RecordInput{Name: "a.lan", Type: "A", Value: "192.168.1.1", Data: []byte(`{"target":"x"}`)}, "data"},
		{RecordInput{Name: "a.lan", Type: "NAPTR", Value: "x"}, "type"},
	}
	for _, c := range bad {
		_, err := e.srv.CreateRecord(context.Background(), c.in)
		wantField(t, err, apperr.KindInvalid, c.field)
	}
	// A null data member is absent.
	if r, err := e.srv.CreateRecord(context.Background(), RecordInput{Name: "n.lan", Type: "MX", Value: "5 mail.lan", Data: []byte("null")}); err != nil || r.Value != "5 mail.lan" {
		t.Fatalf("null data %+v %v", r, err)
	}
}

// An explicit PTR record beats the automatic PTR of the same reverse name
// in the same scope and this server's own PTR.
func TestExplicitPTR(t *testing.T) {
	e := newEnv(t, nil)
	e.groups(2)
	e.client("192.168.1.60", 2)
	e.mustRecord(RecordInput{Name: "nas.lan", Type: "A", Value: "192.168.1.5"})
	e.mustRecord(RecordInput{Name: "files.lan", Type: "A", Value: "192.168.1.5"})
	rev := "5.1.168.192.in-addr.arpa"
	if got := values(e.lookupAs(rev, "PTR", "192.168.1.50")); len(got) != 2 {
		t.Fatalf("two automatic PTRs: %v", got)
	}
	e.mustRecord(RecordInput{Name: rev, Type: "PTR", Value: "storage.lan", Scope: strp("groups"), GroupIDs: []int64{2}})
	if got := values(e.lookupAs(rev, "PTR", "192.168.1.60")); !slices.Equal(got, []string{"storage.lan."}) {
		t.Fatalf("group 2: %v", got)
	}
	if got := values(e.lookupAs(rev, "PTR", "192.168.1.50")); len(got) != 2 {
		t.Fatalf("everyone else keeps the automatic PTRs: %v", got)
	}
	e.mustRecord(RecordInput{Name: rev, Type: "PTR", Value: "main.lan"})
	if got := values(e.lookupAs(rev, "PTR", "192.168.1.50")); !slices.Equal(got, []string{"main.lan."}) {
		t.Fatalf("explicit PTR of scope all: %v", got)
	}
	// This server's own address (192.168.1.10) is answered from the record.
	e.mustRecord(RecordInput{Name: "10.1.168.192.in-addr.arpa", Type: "PTR", Value: "gateway.lan"})
	if got := values(e.lookupAs("10.1.168.192.in-addr.arpa", "PTR", "192.168.1.50")); !slices.Equal(got, []string{"gateway.lan."}) {
		t.Fatalf("own address: %v", got)
	}
}

// The lookup order of records per group: the exact name (the client's
// lowest-id group with records, then scope all), then the wildcards from
// the most specific base (each: group, then all); answer sets are never
// merged; automatic PTRs inherit the scope; a scoped record hides no lease
// name from other clients.
func TestSplitHorizon(t *testing.T) {
	e := newEnv(t, nil)
	e.groups(2, 3)
	e.client("192.168.1.60", 2, 3)
	e.client("192.168.1.70", 3)
	e.mustRecord(RecordInput{Name: "app.lan", Type: "A", Value: "10.0.0.1"})
	e.mustRecord(RecordInput{Name: "app.lan", Type: "AAAA", Value: "fd00::1"})
	e.mustRecord(RecordInput{Name: "app.lan", Type: "A", Value: "10.0.0.2", Scope: strp("groups"), GroupIDs: []int64{2}})
	e.mustRecord(RecordInput{Name: "app.lan", Type: "A", Value: "10.0.0.3", Scope: strp("groups"), GroupIDs: []int64{3}})
	e.mustRecord(RecordInput{Name: "*.svc.lan", Type: "A", Value: "10.1.0.1"})
	e.mustRecord(RecordInput{Name: "*.svc.lan", Type: "A", Value: "10.1.0.2", Scope: strp("groups"), GroupIDs: []int64{3}})
	e.mustRecord(RecordInput{Name: "x.svc.lan", Type: "A", Value: "10.1.0.9"})
	e.mustRecord(RecordInput{Name: "staff.lan", Type: "A", Value: "10.2.0.1", Scope: strp("groups"), GroupIDs: []int64{3}})
	cases := []struct{ name, typ, client, want string }{
		{"app.lan", "A", "192.168.1.60", "10.0.0.2"},
		{"app.lan", "A", "192.168.1.70", "10.0.0.3"},
		{"app.lan", "A", "192.168.1.50", "10.0.0.1"},
		{"app.lan", "AAAA", "192.168.1.60", ""}, // the group's set has no AAAA: NODATA, never merged
		{"app.lan", "AAAA", "192.168.1.50", "fd00::1"},
		{"a.svc.lan", "A", "192.168.1.70", "10.1.0.2"},
		{"a.svc.lan", "A", "192.168.1.50", "10.1.0.1"},
		{"x.svc.lan", "A", "192.168.1.70", "10.1.0.9"}, // the exact name before a group's wildcard
		{"staff.lan", "A", "192.168.1.70", "10.2.0.1"},
		{"1.0.2.10.in-addr.arpa", "PTR", "192.168.1.70", "staff.lan."},
	}
	for _, c := range cases {
		got := values(e.lookupAs(c.name, c.typ, c.client))
		if want := []string{c.want}; c.want == "" && len(got) != 0 || c.want != "" && !slices.Equal(got, want) {
			t.Errorf("%s %s as %s: %v, want %q", c.name, c.typ, c.client, got, c.want)
		}
	}
	// Other clients: the scoped name is not local for them (the local
	// domain: NXDOMAIN), and so is its reverse name.
	if res := e.lookupAs("staff.lan", "A", "192.168.1.50"); res.RCode != "NXDOMAIN" {
		t.Errorf("staff.lan for others: %+v", res)
	}
	if res := e.lookupAs("1.0.2.10.in-addr.arpa", "PTR", "192.168.1.50"); res.RCode != "NXDOMAIN" {
		t.Errorf("reverse for others: %+v", res)
	}
	// A lease name loses only to a record the client's own lookup finds.
	e.srv.d.Leases = mapLeases(map[string]netip.Addr{"staff.lan": netip.MustParseAddr("192.168.1.99")})
	if got := values(e.lookupAs("staff.lan", "A", "192.168.1.50")); !slices.Equal(got, []string{"192.168.1.99"}) {
		t.Errorf("lease name for others: %v", got)
	}
	if got := values(e.lookupAs("staff.lan", "A", "192.168.1.70")); !slices.Equal(got, []string{"10.2.0.1"}) {
		t.Errorf("staff keep the record: %v", got)
	}
}

// Scope, groups and other family: validation, the defaults, a body of 0.12
// keeps them, a stored member that no longer fits is reset unless named.
func TestRecordScopeValidation(t *testing.T) {
	e := newEnv(t, nil)
	e.groups(2, 3)
	ctx := context.Background()
	var many []int64
	for i := range 65 {
		many = append(many, int64(i+10))
	}
	for _, c := range []struct {
		in    RecordInput
		field string
	}{
		{RecordInput{Name: "a.lan", Type: "A", Value: "10.0.0.1", GroupIDs: []int64{2}}, "groupIds"},
		{RecordInput{Name: "a.lan", Type: "A", Value: "10.0.0.1", Scope: strp("groups"), GroupIDs: []int64{9}}, "groupIds"},
		{RecordInput{Name: "a.lan", Type: "A", Value: "10.0.0.1", Scope: strp("groups"), GroupIDs: many}, "groupIds"},
		{RecordInput{Name: "a.lan", Type: "A", Value: "10.0.0.1", Scope: strp("some")}, "scope"},
		{RecordInput{Name: "a.lan", Type: "A", Value: "10.0.0.1", OtherFamily: strp("maybe")}, "otherFamily"},
		{RecordInput{Name: "a.lan", Type: "TXT", Value: "x", OtherFamily: strp("forward")}, "otherFamily"},
	} {
		_, err := e.srv.CreateRecord(ctx, c.in)
		wantField(t, err, apperr.KindInvalid, c.field)
	}
	r := e.mustRecord(RecordInput{Name: "a.lan", Type: "A", Value: "10.0.0.1", Scope: strp("groups"), GroupIDs: []int64{3, 2, 2},
		OtherFamily: strp("forward")})
	if r.Scope != ScopeGroups || !slices.Equal(r.GroupIDs, []int64{2, 3}) || r.OtherFamily != FamilyForward {
		t.Fatalf("created %+v", r)
	}
	var old RecordInput
	if err := json.Unmarshal([]byte(`{"name":"a.lan","type":"A","value":"10.0.0.2","ttl":60,"enabled":true,"comment":"c"}`), &old); err != nil {
		t.Fatal(err)
	}
	u, err := e.srv.UpdateRecord(ctx, r.ID, old)
	if err != nil || u.Scope != ScopeGroups || !slices.Equal(u.GroupIDs, []int64{2, 3}) || u.OtherFamily != FamilyForward || u.Value != "10.0.0.2" {
		t.Fatalf("a 0.12 body changed the scope: %+v %v", u, err)
	}
	u, err = e.srv.UpdateRecord(ctx, r.ID, RecordInput{Name: "a.lan", Type: "A", Value: "10.0.0.2", Scope: strp("all")})
	if err != nil || u.Scope != ScopeAll || len(u.GroupIDs) != 0 {
		t.Fatalf("scope all resets the groups: %+v %v", u, err)
	}
	_, err = e.srv.UpdateRecord(ctx, r.ID, RecordInput{Name: "a.lan", Type: "A", Value: "10.0.0.2", GroupIDs: []int64{2}})
	wantField(t, err, apperr.KindInvalid, "groupIds")
	u, err = e.srv.UpdateRecord(ctx, r.ID, RecordInput{Name: "a.lan", Type: "TXT", Value: "now text"})
	if err != nil || u.OtherFamily != FamilyNoData {
		t.Fatalf("forward reset on TXT: %+v %v", u, err)
	}
}

// CNAME exclusivity holds within a scope only; the loop check follows the
// CNAMEs of every scope.
func TestCNAMEScopes(t *testing.T) {
	e := newEnv(t, nil)
	e.groups(2, 3)
	ctx := context.Background()
	e.mustRecord(RecordInput{Name: "x.lan", Type: "CNAME", Value: "guest.lan", Scope: strp("groups"), GroupIDs: []int64{3}})
	e.mustRecord(RecordInput{Name: "x.lan", Type: "A", Value: "10.0.0.1", Scope: strp("groups"), GroupIDs: []int64{2}})
	e.mustRecord(RecordInput{Name: "x.lan", Type: "A", Value: "10.0.0.2"})
	_, err := e.srv.CreateRecord(ctx, RecordInput{Name: "x.lan", Type: "A", Value: "10.0.0.3", Scope: strp("groups"), GroupIDs: []int64{3}})
	wantField(t, err, apperr.KindConflict, "")
	_, err = e.srv.CreateRecord(ctx, RecordInput{Name: "x.lan", Type: "CNAME", Value: "other.lan"})
	wantField(t, err, apperr.KindConflict, "")
	// "CNAME for guests, A for staff" never reaches a client merged.
	e.client("192.168.1.70", 3)
	if got := values(e.lookupAs("x.lan", "CNAME", "192.168.1.70")); !slices.Equal(got, []string{"guest.lan."}) {
		t.Fatalf("guests %v", got)
	}
	e.mustRecord(RecordInput{Name: "a.lan", Type: "CNAME", Value: "b.lan", Scope: strp("groups"), GroupIDs: []int64{2}})
	_, err = e.srv.CreateRecord(ctx, RecordInput{Name: "b.lan", Type: "CNAME", Value: "a.lan", Scope: strp("groups"), GroupIDs: []int64{3}})
	wantField(t, err, apperr.KindConflict, "")
	if err == nil || !strings.Contains(err.Error(), "loop") {
		t.Fatalf("loop across scopes: %v", err)
	}
}

// Deleting the only group of a scoped record never makes it answer other
// clients: the record serves nobody (single and batch deletes).
func TestDeletedGroupRecords(t *testing.T) {
	e := newEnv(t, nil)
	e.groups(2, 3)
	ctx := context.Background()
	r := e.mustRecord(RecordInput{Name: "secret.example.org", Type: "A", Value: "10.0.0.9", Scope: strp("groups"), GroupIDs: []int64{2}})
	e.mustRecord(RecordInput{Name: "both.example.org", Type: "A", Value: "10.0.0.8", Scope: strp("groups"), GroupIDs: []int64{2, 3}})
	if _, err := e.srv.d.DB.W.Exec(`DELETE FROM client_groups WHERE id IN (2, 3)`); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.ReloadRecords(ctx); err != nil {
		t.Fatal(err)
	}
	recs, _ := e.srv.Records(ctx)
	for _, x := range recs {
		if x.Scope != ScopeGroups || len(x.GroupIDs) != 0 {
			t.Fatalf("record %+v", x)
		}
	}
	for _, ip := range []string{"192.168.1.50", "192.168.1.60"} {
		if res := e.lookupAs("secret.example.org", "A", ip); res.Status == StatusLocal || slices.Contains(values(res), "10.0.0.9") {
			t.Fatalf("a record of a deleted group answers %s: %+v", ip, res)
		}
	}
	if r.ID == 0 {
		t.Fatal("no record")
	}
}

// otherFamily forward: an A query for a name with only an AAAA record
// continues as if no local record matched (step 7: the upstreams; step 6:
// never the default upstreams; step 11a: its next options; a CNAME target
// likewise); nodata keeps NODATA; dns.localRecordsEnabled switches the
// records off.
func TestOtherFamilyForward(t *testing.T) {
	e := newEnv(t, nil)
	e.mustRecord(RecordInput{Name: "v6.example.org", Type: "AAAA", Value: "fd00::6", OtherFamily: strp("forward")})
	e.mustRecord(RecordInput{Name: "v6only.example.org", Type: "AAAA", Value: "fd00::7"})
	e.mustRecord(RecordInput{Name: "v6.lan", Type: "AAAA", Value: "fd00::8", OtherFamily: strp("forward")})
	e.mustRecord(RecordInput{Name: "c.example.org", Type: "CNAME", Value: "v6.example.org"})
	res := e.lookupAs("v6.example.org", "A", "192.168.1.50")
	if res.Status != StatusForwarded || !slices.Equal(values(res), []string{"198.51.100.7"}) ||
		!slices.ContainsFunc(res.Steps, func(s string) bool { return strings.Contains(s, "continuing (other family: forward)") }) {
		t.Fatalf("step 7: %+v", res)
	}
	if got := values(e.lookupAs("v6.example.org", "AAAA", "192.168.1.50")); !slices.Equal(got, []string{"fd00::6"}) {
		t.Fatalf("own family %v", got)
	}
	if res := e.lookupAs("v6only.example.org", "A", "192.168.1.50"); res.Status != StatusLocal || len(res.Answers) != 0 {
		t.Fatalf("nodata %+v", res)
	}
	if res := e.lookupAs("v6.example.org", "MX", "192.168.1.50"); res.Status != StatusLocal || len(res.Answers) != 0 {
		t.Fatalf("other types keep NODATA: %+v", res)
	}
	before := len(e.up.callsFor("v6.lan"))
	if res := e.lookupAs("v6.lan", "A", "192.168.1.50"); res.RCode != "NXDOMAIN" || len(e.up.callsFor("v6.lan")) != before {
		t.Fatalf("a local-domain name reached the upstreams: %+v", res)
	}
	res = e.lookupAs("c.example.org", "A", "192.168.1.50")
	if got := values(res); len(got) != 2 || got[1] != "198.51.100.7" {
		t.Fatalf("CNAME target %+v", res)
	}
	if res := e.lookupAs("v6", "A", "192.168.1.50"); res.Status != StatusSpecial || res.RCode != "NXDOMAIN" {
		t.Fatalf("single label: %+v", res)
	}
	if got := values(e.lookupAs("v6", "AAAA", "192.168.1.50")); !slices.Equal(got, []string{"fd00::8"}) {
		t.Fatalf("single label AAAA: %v", got)
	}
	// The switch: records and their automatic PTRs are off, lease names stay.
	e.srv.d.Leases = mapLeases(map[string]netip.Addr{"pc.lan": netip.MustParseAddr("192.168.1.77")})
	e.mustRecord(RecordInput{Name: "nas.lan", Type: "A", Value: "192.168.1.5"})
	e.update(func(a *settings.All) { a.DNS.LocalRecordsEnabled = false })
	if res := e.lookupAs("nas.lan", "A", "192.168.1.50"); res.Status == StatusLocal {
		t.Fatalf("records answered while switched off: %+v", res)
	}
	if res := e.lookupAs("5.1.168.192.in-addr.arpa", "PTR", "192.168.1.50"); res.Status == StatusLocal {
		t.Fatalf("automatic PTR while switched off: %+v", res)
	}
	if got := values(e.lookupAs("pc.lan", "A", "192.168.1.50")); !slices.Equal(got, []string{"192.168.1.77"}) {
		t.Fatalf("lease names stay: %v", got)
	}
}

// dns.localizeRecords orders the addresses of a multi-address answer by the
// client's network; without a local address the stored order stays.
func TestLocalizeRecords(t *testing.T) {
	e := newEnv(t, nil) // eth0: 192.168.1.10/24 and fd00::10/64
	for _, v := range []string{"10.9.9.9", "192.168.1.20", "172.16.0.1", "192.168.1.21"} {
		e.mustRecord(RecordInput{Name: "multi.lan", Type: "A", Value: v})
	}
	for _, v := range []string{"2001:db8::20", "fd00::20"} {
		e.mustRecord(RecordInput{Name: "multi.lan", Type: "AAAA", Value: v})
	}
	stored := []string{"10.9.9.9", "192.168.1.20", "172.16.0.1", "192.168.1.21"}
	cases := []struct {
		mode, client, typ string
		want              []string
	}{
		{"first", "192.168.1.50", "A", []string{"192.168.1.20", "192.168.1.21", "10.9.9.9", "172.16.0.1"}},
		{"only", "192.168.1.50", "A", []string{"192.168.1.20", "192.168.1.21"}},
		{"off", "192.168.1.50", "A", stored},
		{"first", "10.8.0.5", "A", stored},
		{"only", "10.8.0.5", "A", stored},
		{"first", "192.168.1.50", "AAAA", []string{"fd00::20", "2001:db8::20"}}, // the other family on the client's interface
		{"only", "fd00::99", "AAAA", []string{"fd00::20"}},
		{"only", "fd00::99", "A", []string{"192.168.1.20", "192.168.1.21"}},
	}
	for _, c := range cases {
		e.update(func(a *settings.All) { a.DNS.LocalizeRecords = c.mode })
		if got := values(e.lookupAs("multi.lan", c.typ, c.client)); !slices.Equal(got, c.want) {
			t.Errorf("%s %s %s: %v, want %v", c.mode, c.client, c.typ, got, c.want)
		}
	}
}

// The hosts import: all or nothing, the skipped lines, the errors of a
// pasted blocklist, duplicates and CNAME conflicts, identical records
// unchanged, the scope of the import.
func TestImportHosts(t *testing.T) {
	e := newEnv(t, nil)
	e.groups(2)
	ctx := context.Background()
	e.mustRecord(RecordInput{Name: "old.lan", Type: "A", Value: "192.168.1.30", Scope: strp("groups"), GroupIDs: []int64{2}})
	e.mustRecord(RecordInput{Name: "alias.lan", Type: "CNAME", Value: "old.lan"})
	text := "# hosts\n192.168.1.2 nas.lan nas # 02:00:00:00:00:02\n127.0.1.1 myhost\n::1 localhost ip6-localhost\n" +
		"192.168.1.30 old.lan\nfd00::5 printer.lan\n255.255.255.255 broadcasthost\n\n"
	res, err := e.srv.ImportRecords(ctx, RecordImport{Format: "hosts", Text: text, DryRun: true})
	if err != nil || res.Applied || res.Added != 3 || res.Unchanged != 1 || res.Skipped != 5 || res.ErrorCount != 0 {
		t.Fatalf("dry run %+v %v", res, err)
	}
	if recs, _ := e.srv.Records(ctx); len(recs) != 2 {
		t.Fatal("a dry run wrote records")
	}
	// A header line of junk names is skipped whatever its address (the
	// "fe80::1%lo0 localhost" of older macOS hosts files).
	res, err = e.srv.ImportRecords(ctx, RecordImport{Format: "hosts", Text: "fe80::1%lo0 localhost\n192.168.1.3 tv.lan\n", DryRun: true})
	if err != nil || res.ErrorCount != 0 || res.Skipped != 1 || res.Added != 1 {
		t.Fatalf("zone header %+v %v", res, err)
	}
	blocklist := text + "0.0.0.0 ads.example\n:: ads.example\n192.168.1.40 bad..name\n192.168.1.41 *.wild.lan\n" +
		"224.0.0.1 mcast.lan\nfe80::1%eth0 zone.lan\n192.168.1.2 nas.lan\n192.168.1.42 alias.lan\nnot-an-address x.lan\n"
	res, err = e.srv.ImportRecords(ctx, RecordImport{Format: "hosts", Text: blocklist})
	if err != nil || res.Applied || res.ErrorCount != 9 {
		t.Fatalf("errors %+v %v", res, err)
	}
	wantFields := []string{"syntax", "syntax", "name", "name", "syntax", "syntax", "syntax", "name", "syntax"}
	for i, w := range wantFields {
		if res.Errors[i].Field != w || res.Errors[i].Line != 9+i {
			t.Errorf("error %d = %+v, want line %d field %s", i, res.Errors[i], 9+i, w)
		}
	}
	if !strings.Contains(res.Errors[0].Message, "looks like a blocklist entry") {
		t.Errorf("blocklist message %q", res.Errors[0].Message)
	}
	if recs, _ := e.srv.Records(ctx); len(recs) != 2 {
		t.Fatal("a failed import wrote records")
	}
	res, err = e.srv.ImportRecords(ctx, RecordImport{Format: "hosts", Text: text, Scope: strp("groups"), GroupIDs: []int64{2}})
	if err != nil || !res.Applied || res.Added != 3 {
		t.Fatalf("import %+v %v", res, err)
	}
	recs, _ := e.srv.Records(ctx)
	for _, r := range recs {
		if r.Name == "nas" || r.Name == "nas.lan" || r.Name == "printer.lan" {
			if r.Scope != ScopeGroups || !slices.Equal(r.GroupIDs, []int64{2}) || r.TTL != 300 || !r.Enabled {
				t.Errorf("imported %+v", r)
			}
		}
	}
	_, err = e.srv.ImportRecords(ctx, RecordImport{Format: "csv", Text: "x"})
	wantField(t, err, apperr.KindInvalid, "format")
	_, err = e.srv.ImportRecords(ctx, RecordImport{Format: "hosts", Text: "x", GroupIDs: []int64{2}})
	wantField(t, err, apperr.KindInvalid, "groupIds")
	_, err = e.srv.ImportRecords(ctx, RecordImport{Format: "hosts", Text: strings.Repeat("192.168.1.2 a.lan\n", maxRecordImportLines+1)})
	wantField(t, err, apperr.KindInvalid, "text")
}

// Record and forwarder batches: all or nothing, the zone reloaded once.
func TestBatchRecordsAndForwarders(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	a := e.mustRecord(RecordInput{Name: "a.lan", Type: "A", Value: "192.168.1.2"})
	b := e.mustRecord(RecordInput{Name: "b.lan", Type: "A", Value: "192.168.1.3"})
	_, err := e.srv.BatchRecords(ctx, "disable", []int64{a.ID, 999})
	wantField(t, err, apperr.KindNotFound, "")
	if n, err := e.srv.BatchRecords(ctx, "disable", []int64{a.ID, b.ID}); err != nil || n != 2 {
		t.Fatalf("disable %d %v", n, err)
	}
	if res := e.lookupAs("a.lan", "A", "192.168.1.50"); res.Status == StatusLocal {
		t.Fatal("a disabled record answers")
	}
	if n, _ := e.srv.BatchRecords(ctx, "enable", []int64{a.ID}); n != 1 {
		t.Fatalf("enable %d", n)
	}
	if n, _ := e.srv.BatchRecords(ctx, "delete", []int64{a.ID, b.ID}); n != 2 {
		t.Fatalf("delete %d", n)
	}
	f, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domain: "corp.example", Upstreams: []string{"10.9.9.9"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := e.srv.BatchForwarders(ctx, "disable", []int64{f.ID}); err != nil || n != 1 || e.srv.fwd.Load().match("x.corp.example", false) != nil {
		t.Fatalf("forwarder disable %d %v", n, err)
	}
	if n, _ := e.srv.BatchForwarders(ctx, "delete", []int64{f.ID}); n != 1 {
		t.Fatalf("forwarder delete %d", n)
	}
}

// The hosts line parser reads untrusted text: it never panics, and every
// record it makes is a valid A or AAAA record of a unicast, non-loopback
// address.
func FuzzHostsLine(f *testing.F) {
	for _, s := range []string{"192.168.1.2 nas.lan nas", "0.0.0.0 ads.example", "::1 localhost", "fe80::1%eth0 x", "# c",
		"192.168.1.2 *.x", "10.0.0.1 a.lan # comment", "::ffff:10.0.0.1 m.lan", "1.2.3.4 localhost broadcasthost"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		specs, skip, lerr := parseHostsLine(line, ScopeAll, []int64{})
		if (skip || lerr != nil) && len(specs) != 0 {
			t.Fatalf("%q: records with skip or error", line)
		}
		for _, sp := range specs {
			ip, err := netip.ParseAddr(sp.Value)
			if err != nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || ip.Is4In6() ||
				(sp.Type == "A") != ip.Is4() || strings.Contains(sp.Name, "*") || !validDomain(sp.Name) {
				t.Fatalf("%q: bad record %+v", line, sp)
			}
		}
	})
}

// mapLeases answers the lease names of the map (TTL 120, no PTRs).
type mapLeases map[string]netip.Addr

func (m mapLeases) LeaseAddr(name string) (netip.Addr, uint32, bool) {
	ip, ok := m[name]
	return ip, 120, ok
}

func (m mapLeases) LeasePTR(netip.Addr) (string, uint32, bool) { return "", 0, false }

var _ = upstream.Info{}
