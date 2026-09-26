package dnsserver

import (
	"context"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

func wantField(t *testing.T, err error, kind apperr.Kind, field string) {
	t.Helper()
	if e, ok := apperr.As(err); !ok || e.Kind != kind || (field != "" && e.Field != field) {
		t.Fatalf("err = %v, want %v with field %q", err, kind, field)
	}
}

// Several domains per forwarder: domains wins, domain alone means
// [domain], a domain belongs to one forwarder, the domains are matched
// like single-domain forwarders and go with a deleted forwarder.
func TestForwarderDomains(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	f, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domains: []string{"Corp.Example.", "*.corp.net", "10.in-addr.arpa"}, Upstreams: []string{"10.9.9.9"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if f.Domain != "corp.example" || !slices.Equal(f.Domains, []string{"corp.example", "*.corp.net", "10.in-addr.arpa"}) {
		t.Fatalf("created %+v", f)
	}
	tbl := e.srv.fwd.Load()
	for name, want := range map[string]string{"a.corp.example": "corp.example", "x.corp.net": "*.corp.net", "corp.net": "", "5.1.2.10.in-addr.arpa": "10.in-addr.arpa"} {
		got := ""
		if m := tbl.match(name, false); m != nil {
			got = m.domain
		}
		if got != want {
			t.Errorf("match(%s) = %q, want %q", name, got, want)
		}
	}
	for _, tc := range []struct {
		in    ForwarderInput
		kind  apperr.Kind
		field string
	}{
		{ForwarderInput{Domain: "corp.net", Domains: []string{"other.example"}, Upstreams: []string{"1.1.1.1"}}, apperr.KindInvalid, "domain"},
		{ForwarderInput{Domains: []string{"a.example", "A.example."}, Upstreams: []string{"1.1.1.1"}}, apperr.KindInvalid, "domains[1]"},
		{ForwarderInput{Domains: []string{"a.example", "bad name"}, Upstreams: []string{"1.1.1.1"}}, apperr.KindInvalid, "domains[1]"},
		{ForwarderInput{Domains: make([]string, 17), Upstreams: []string{"1.1.1.1"}}, apperr.KindInvalid, "domains"},
		{ForwarderInput{Upstreams: []string{"1.1.1.1"}}, apperr.KindInvalid, "domain"},
		{ForwarderInput{Domain: "bad name", Upstreams: []string{"1.1.1.1"}}, apperr.KindInvalid, "domain"},
		{ForwarderInput{Domains: []string{"x.example", "*.corp.net"}, Upstreams: []string{"1.1.1.1"}}, apperr.KindConflict, ""},
		{ForwarderInput{Domain: "10.in-addr.arpa", Upstreams: []string{"1.1.1.1"}}, apperr.KindConflict, ""},
	} {
		_, err := e.srv.CreateForwarder(ctx, tc.in)
		wantField(t, err, tc.kind, tc.field)
	}
	if _, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domain: "10.in-addr.arpa", Upstreams: []string{"1.1.1.1"}}); err == nil ||
		!strings.Contains(err.Error(), "a forwarder for 10.in-addr.arpa already exists") {
		t.Errorf("conflict message: %v", err)
	}
	// Both given and equal: fine; an update may keep its own domains.
	if _, err := e.srv.UpdateForwarder(ctx, f.ID, ForwarderInput{Domain: "corp.example", Domains: []string{"corp.example", "corp.org"}, Upstreams: []string{"10.9.9.9"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if e.srv.fwd.Load().match("x.corp.net", false) != nil || e.srv.fwd.Load().match("x.corp.org", false) == nil {
		t.Error("the domains were not replaced")
	}
	// A released domain can be used by another forwarder.
	if _, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domain: "*.corp.net", Upstreams: []string{"10.9.9.8"}, Enabled: true}); err != nil {
		t.Fatalf("released domain: %v", err)
	}
	if err := e.srv.DeleteForwarder(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.srv.d.DB.R.QueryRow(`SELECT COUNT(*) FROM dns_forwarder_domains WHERE forwarder_id = ?`, f.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("domains of the deleted forwarder: %d %v", n, err)
	}
	list, _ := e.srv.Forwarders(ctx)
	if len(list) != 1 || list[0].Domain != "*.corp.net" || !slices.Equal(list[0].Domains, []string{"*.corp.net"}) {
		t.Errorf("list %+v", list)
	}
}

// The dns migration v2 back-fills the domains of forwarders stored before
// (a restored older backup is converted at the start that applies it).
func TestForwarderDomainsMigration(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "picache.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(ctx, "dns", migrations[:1]); err != nil {
		t.Fatal(err)
	}
	for i, dom := range []string{"fritz.box", "*.corp.example"} {
		if _, err := d.W.ExecContext(ctx, `INSERT INTO dns_forwarders (domain, upstreams, enabled, comment, created_at, updated_at)
			VALUES (?, '["192.168.178.1"]', 1, '', ?, ?)`, dom, i, i); err != nil {
			t.Fatal(err)
		}
	}
	set, err := settings.Open(ctx, d, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(ctx, Deps{DB: d, Settings: set, Log: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	list, err := srv.Forwarders(ctx)
	if err != nil || len(list) != 2 || !slices.Equal(list[0].Domains, []string{"*.corp.example"}) || !slices.Equal(list[1].Domains, []string{"fritz.box"}) {
		t.Fatalf("forwarders %+v %v", list, err)
	}
	if srv.fwd.Load().match("nas.fritz.box", false) == nil {
		t.Error("migrated forwarder not matched")
	}
}

// The target "default": the only target, refused for names that never
// reach the default upstreams, skipped by step 6 when a later settings
// change makes its domain local, and the step-13 path at step 12.
func TestDefaultForwarder(t *testing.T) {
	e := newEnv(t, func(a *settings.All) { a.DNS.PrivateReverseNetworks = []string{"8.8.8.0/24"} })
	e.srv.host.Store(&hostInfo{search: []string{"corp.search"}})
	ctx := context.Background()
	for _, tc := range []struct {
		domain string
		ups    []string
		field  string
		msg    string
	}{
		{"x.example", []string{"default", "1.1.1.1"}, "upstreams[0]", "default must be the only target"},
		{"x.example", []string{"1.1.1.1", "DEFAULT"}, "upstreams[1]", "default must be the only target"},
		{"10.in-addr.arpa", []string{"default"}, "upstreams[0]", "private reverse zones"},
		{"1.8.8.8.in-addr.arpa", []string{"default"}, "upstreams[0]", "dns.privateReverseNetworks"},
		{"nas.lan", []string{"default"}, "upstreams[0]", "the local domain"},
		{"*.lan", []string{"default"}, "upstreams[0]", "the local domain"},
		{"router.home.arpa", []string{"default"}, "upstreams[0]", "home.arpa"},
		{"x.corp.search", []string{"default"}, "upstreams[0]", "search domains"},
		{"printer.local", []string{"default"}, "upstreams[0]", "special-use"},
		{"x.test", []string{"default"}, "upstreams[0]", "special-use"},
		{Unqualified, []string{"default"}, "upstreams[0]", "single-label"},
	} {
		_, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domain: tc.domain, Upstreams: tc.ups, Enabled: true})
		if ae, ok := apperr.As(err); !ok || ae.Field != tc.field || !strings.Contains(ae.Message, tc.msg) {
			t.Errorf("%s %v: %v", tc.domain, tc.ups, err)
		}
	}
	if _, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domain: "corp.example", Upstreams: []string{"10.9.9.9"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	f, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domain: "public.corp.example", Upstreams: []string{" Default "}, Enabled: true})
	if err != nil || !slices.Equal(f.Upstreams, []string{"default"}) {
		t.Fatalf("default forwarder: %+v %v", f, err)
	}
	e.serve()
	e.query("udp", "www.public.corp.example", dns.TypeA)
	e.query("udp", "intranet.corp.example", dns.TypeA)
	if c := e.up.callsFor("www.public.corp.example"); len(c) != 1 || c[0].via != nil {
		t.Errorf("default forwarder: %v", c)
	}
	if c := e.up.callsFor("intranet.corp.example"); len(c) != 1 || !slices.Equal(c[0].via, []string{"10.9.9.9"}) {
		t.Errorf("explicit forwarder: %v", c)
	}
	res, _ := e.srv.Lookup(ctx, LookupRequest{Name: "www.public.corp.example"}, e.srv.host.Load().primary4)
	if !slices.ContainsFunc(res.Steps, func(s string) bool {
		return strings.Contains(s, "conditional forwarder public.corp.example: default upstreams")
	}) {
		t.Errorf("lookup steps %v", res.Steps)
	}
	// A local CNAME into the default forwarder's zone: its target goes to
	// the default upstreams, never to the parent zone's forwarder.
	e.addRecord("app.lan", "CNAME", "www3.public.corp.example")
	e.query("udp", "app.lan", dns.TypeA)
	if c := e.up.callsFor("www3.public.corp.example"); len(c) != 1 || c[0].via != nil {
		t.Errorf("CNAME target of a local record: %v", c)
	}
	// The local domain becomes corp.example: step 6 skips the default
	// forwarder (the explicit one answers), nothing reaches the default
	// upstreams.
	e.update(func(a *settings.All) { a.DNS.LocalDomain = "corp.example" })
	e.query("udp", "www2.public.corp.example", dns.TypeA)
	if c := e.up.callsFor("www2.public.corp.example"); len(c) != 1 || !slices.Equal(c[0].via, []string{"10.9.9.9"}) {
		t.Errorf("step 6 used a default forwarder: %v", c)
	}
}

// Targets given by name: plain ones must be public names (not below the
// local domain or a search domain); every name needs bootstrap servers.
func TestForwarderNamedTargets(t *testing.T) {
	e := newEnv(t, nil)
	e.srv.host.Store(&hostInfo{search: []string{"corp.search"}})
	ctx := context.Background()
	for _, tc := range []struct {
		target string
		ok     bool
	}{
		{"dns.example.com", true},
		{"tcp://dns.example.com:5353", true},
		{"udp://unbound", false},
		{"dns.lan", false},
		{"dns.corp.search", false},
		{"tls://dns.corp.search", true}, // DoT names are verified by TLS
	} {
		_, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domain: "d" + strconv.Itoa(len(tc.target)) + strings.NewReplacer(":", "", "/", "", ".", "").Replace(tc.target) + ".example",
			Upstreams: []string{tc.target}, Enabled: true})
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.target, err)
		}
		if !tc.ok {
			if ae, _ := apperr.As(err); ae == nil || ae.Field != "upstreams[0]" || ae.Message != settings.ErrPlainUpstreamName {
				t.Errorf("%s: %v", tc.target, err)
			}
		}
	}
	e.update(func(a *settings.All) {
		a.DNS.Bootstrap = nil
		a.DNS.Upstreams = []string{"9.9.9.9"}
		a.DNS.FallbackUpstreams = nil
	})
	_, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domain: "named.example", Upstreams: []string{"https://dns.example/dns-query"}, Enabled: true})
	if ae, _ := apperr.As(err); ae == nil || ae.Field != "upstreams[0]" || ae.Message != "a host name needs dns.bootstrap servers" {
		t.Errorf("without bootstrap: %v", err)
	}
}

// (unqualified) matches single-label names with qtype A, AAAA, HTTPS,
// SVCB or ANY only, never the root.
func TestUnqualifiedForwarder(t *testing.T) {
	e := newEnv(t, func(a *settings.All) { a.DNS.DomainNeeded = false; a.DNS.RefuseANY = false })
	if _, err := e.srv.CreateForwarder(context.Background(), ForwarderInput{Domain: "(Unqualified)", Upstreams: []string{"10.8.8.8"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	e.serve()
	for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeHTTPS, dns.TypeSVCB, dns.TypeANY, dns.TypeMX, dns.TypeTXT} {
		e.query("tcp", "host", qtype)
	}
	for _, c := range e.up.callsFor("host") {
		want := singleLabelType(c.qtype)
		if got := slices.Equal(c.via, []string{"10.8.8.8"}); got != want {
			t.Errorf("%s via %v", dns.TypeToString[c.qtype], c.via)
		}
	}
	e.query("udp", "host.example", dns.TypeA)
	e.query("udp", ".", dns.TypeA)
	for _, n := range []string{"host.example", ""} {
		if c := e.up.callsFor(n); len(c) != 1 || c[0].via != nil {
			t.Errorf("%q: %v", n, c)
		}
	}
}

// POST /dns/forwarders/import: dnsmasq-like lines, all or nothing.
func TestImportForwarders(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	existing, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domains: []string{"fritz.box", "178.168.192.in-addr.arpa"}, Upstreams: []string{"192.168.178.1"}, Comment: "router"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domain: "same.example", Upstreams: []string{"10.1.1.1"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	text := "# comment\r\n" +
		"\r\n" +
		"[/fritz.box/178.168.192.in-addr.arpa/box.lan/]192.168.178.1 192.168.178.2\r\n" + // update
		"[/same.example/]10.1.1.1\n" + // unchanged
		"[/corp.example/*.corp.net/]10.9.9.9\n" + // add
		"[/public.corp.example/]#\n" + // add, default
		"[//]10.8.8.8\n" // add, (unqualified)
	res, err := e.srv.ImportForwarders(ctx, ForwarderImport{Text: text, DryRun: true})
	if err != nil || res.Applied || res.Added != 3 || res.Updated != 1 || res.Unchanged != 1 || len(res.Errors) != 0 {
		t.Fatalf("dry run %+v %v", res, err)
	}
	if list, _ := e.srv.Forwarders(ctx); len(list) != 2 {
		t.Fatalf("a dry run wrote %d forwarders", len(list))
	}
	res, err = e.srv.ImportForwarders(ctx, ForwarderImport{Text: text})
	if err != nil || !res.Applied || res.Added != 3 || res.Updated != 1 || res.Unchanged != 1 {
		t.Fatalf("import %+v %v", res, err)
	}
	f, err := e.srv.forwarder(ctx, existing.ID)
	if err != nil || !slices.Equal(f.Domains, []string{"fritz.box", "178.168.192.in-addr.arpa", "box.lan"}) ||
		!slices.Equal(f.Upstreams, []string{"192.168.178.1", "192.168.178.2"}) || f.Enabled || f.Comment != "router" {
		t.Errorf("updated %+v %v (enabled and comment kept)", f, err)
	}
	tbl := e.srv.fwd.Load()
	if m := tbl.match("x.public.corp.example", false); m == nil || !m.def {
		t.Error("# is the default target")
	}
	if tbl.unqualified == nil || !slices.Equal(tbl.unqualified.upstreams, []string{"10.8.8.8"}) {
		t.Error("[//] is (unqualified)")
	}
	// Errors: nothing is written, the first error of each line is listed.
	bad := "[/new1.example/]10.2.2.2\n" +
		"new2.example 10.2.2.3\n" + // no [/…/]
		"[/new3.example/]\n" + // no target
		"[/new4.example/fritz.box/]10.2.2.4\n" + // fritz.box belongs to another forwarder
		"[/new1.example/]10.2.2.5\n" + // repeated in the import
		"[/bad name/]10.2.2.6\n" +
		"[/new7.example/]default 1.1.1.1\n" +
		"[/]\n"
	res, err = e.srv.ImportForwarders(ctx, ForwarderImport{Text: bad})
	if err != nil || res.Applied || res.Added != 1 {
		t.Fatalf("bad import %+v %v", res, err)
	}
	want := []ForwarderImportError{{2, "syntax", ""}, {3, "upstreams", ""}, {4, "domains", "fritz.box belongs to the forwarder for fritz.box"},
		{5, "domains", "new1.example is on line 1 already"}, {6, "domains", ""}, {7, "upstreams", "default must be the only target"}, {8, "syntax", ""}}
	if len(res.Errors) != len(want) {
		t.Fatalf("errors %+v", res.Errors)
	}
	for i, w := range want {
		g := res.Errors[i]
		if g.Line != w.Line || g.Field != w.Field || (w.Message != "" && g.Message != w.Message) {
			t.Errorf("error %d: %+v, want %+v", i, g, w)
		}
	}
	if list, _ := e.srv.Forwarders(ctx); len(list) != 5 {
		t.Errorf("a failed import wrote: %d forwarders", len(list))
	}
	// Bounds: 1024 lines, 256 KiB, 256 forwarders in the end.
	_, err = e.srv.ImportForwarders(ctx, ForwarderImport{Text: strings.Repeat("\n", 1024) + "x"})
	wantField(t, err, apperr.KindInvalid, "text")
	_, err = e.srv.ImportForwarders(ctx, ForwarderImport{Text: strings.Repeat("#", 256<<10+1)})
	wantField(t, err, apperr.KindInvalid, "text")
	var many strings.Builder
	for i := range 252 {
		many.WriteString("[/m" + strconv.Itoa(i) + ".example/]10.3.3.3\n")
	}
	res, err = e.srv.ImportForwarders(ctx, ForwarderImport{Text: many.String()})
	if err != nil || res.Applied || len(res.Errors) != 1 || res.Errors[0].Line != 0 || res.Errors[0].Field != "text" {
		t.Errorf("more than 256: %+v %v", res.Errors, err)
	}
}
