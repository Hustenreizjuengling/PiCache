package settings

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// fakeSealer seals by prefixing the AAD (tests only).
func fakeSealer(s *Store) {
	s.SetSealer(func(p []byte, aad string) (string, error) { return aad + "|" + string(p), nil },
		func(sealed, aad string) ([]byte, error) {
			v, ok := strings.CutPrefix(sealed, aad+"|")
			if !ok {
				return nil, errors.New("wrong aad")
			}
			return []byte(v), nil
		})
}

func openTestStore(t *testing.T) (*Store, *db.DB) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	s, err := Open(context.Background(), d, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	fakeSealer(s)
	return s, d
}

func wantInvalid(t *testing.T, err error, field, msg string) {
	t.Helper()
	e, ok := apperr.As(err)
	if !ok || e.Kind != apperr.KindInvalid || e.Field != field || (msg != "" && !strings.Contains(e.Message, msg)) {
		t.Fatalf("got %v, want invalid %s %q", err, field, msg)
	}
}

func ptr(s string) *string { return &s }

// A document without the members of 0.15.0 decodes them from the defaults.
func TestK1DefaultsForOlderDocuments(t *testing.T) {
	ctx := context.Background()
	s, d := openTestStore(t)
	for _, m := range []string{"$.clients", "$.sync", "$.network", "$.ntp", "$.logs.seenRetentionDays"} {
		if _, err := d.W.ExecContext(ctx, `UPDATE settings SET doc = json_remove(doc, ?)`, m); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(ctx, d, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	a := s.Get()
	if a.Clients.NameSources != (NameSources{PTR: true, DHCP: true}) || a.Sync.Mode != SyncOff || a.Sync.IntervalMinutes != 15 ||
		a.Sync.Sections == nil || a.NTP != (NTP{Stratum: 3}) || a.Logs.SeenRetentionDays != 30 || a.Network.ProxyFor.Any() ||
		a.Updates.Channel != ChannelStable {
		t.Fatalf("defaults of an older document: %+v", a)
	}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
}

// Settings v6 creates settings_secrets and derives updates.channel from
// includePrereleases in documents without it (a restored older backup
// too).
func TestMigrateChannelV6(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, updates string
		want          string
		incl          bool
	}{
		{"prereleases", `{"checkEnabled":true,"includePrereleases":true}`, ChannelBeta, true},
		{"releases", `{"checkEnabled":false,"includePrereleases":false}`, ChannelStable, false},
		{"no member", `{"checkEnabled":true}`, ChannelStable, false},
		{"null", `null`, ChannelStable, false},
		{"kept", `{"checkEnabled":true,"includePrereleases":true,"channel":"nightly"}`, ChannelNightly, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := db.Open(filepath.Join(t.TempDir(), "s.db"), 1)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err := d.Migrate(ctx, "settings", migrations[:5]); err != nil {
				t.Fatal(err)
			}
			if _, err := d.W.ExecContext(ctx, `INSERT INTO settings (id, doc, updated_at)
				VALUES (1, json_set('{"dns":{"upstreams":["9.9.9.9"]}}', '$.updates', json(?)), 0)`, tc.updates); err != nil {
				t.Fatal(err)
			}
			s, err := Open(ctx, d, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			if u := s.Get().Updates; u.Channel != tc.want {
				t.Fatalf("channel %q, want %q", u.Channel, tc.want)
			}
			var n int
			if err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings_secrets`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("settings_secrets: %d, %v", n, err)
			}
			// DecodeStored judges an older staged document the same way.
			a, err := DecodeStored([]byte(`{"updates":` + tc.updates + `}`))
			if err != nil || a.Updates.Channel != tc.want {
				t.Fatalf("DecodeStored: %+v, %v", a, err)
			}
		})
	}
}

func TestChannelCompatibility(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	set := func(fn func(u *Updates)) (Updates, error) {
		a, err := s.Update(ctx, func(a *All) error { fn(&a.Updates); return nil })
		if err != nil {
			return Updates{}, err
		}
		return a.Updates, nil
	}
	// includePrereleases alone: stable → beta, beta and nightly stay, false → stable.
	if u, err := set(func(u *Updates) { u.IncludePrereleases = true }); err != nil || u.Channel != ChannelBeta || !u.IncludePrereleases {
		t.Fatalf("%+v %v", u, err)
	}
	if u, err := set(func(u *Updates) { u.Channel = "Nightly" }); err != nil || u.Channel != ChannelNightly || !u.IncludePrereleases {
		t.Fatalf("%+v %v", u, err)
	}
	if u, err := set(func(u *Updates) { u.CheckEnabled = false }); err != nil || u.Channel != ChannelNightly {
		t.Fatalf("an unrelated change keeps the channel: %+v %v", u, err)
	}
	if u, err := set(func(u *Updates) { u.IncludePrereleases = false }); err != nil || u.Channel != ChannelStable || u.IncludePrereleases {
		t.Fatalf("%+v %v", u, err)
	}
	// Both changed and disagreeing.
	_, err := set(func(u *Updates) { u.Channel, u.IncludePrereleases = ChannelStable, true })
	if err != nil {
		t.Fatalf("stable+true when only includePrereleases changed: %v", err)
	}
	// Now beta: a new channel with a changed includePrereleases that
	// disagrees is refused; one that agrees is accepted.
	_, err = set(func(u *Updates) { u.Channel, u.IncludePrereleases = ChannelNightly, false })
	wantInvalid(t, err, "updates.channel", "includePrereleases contradicts channel")
	if u, err := set(func(u *Updates) { u.Channel, u.IncludePrereleases = ChannelStable, false }); err != nil || u.Channel != ChannelStable {
		t.Fatalf("agreeing change refused: %+v %v", u, err)
	}
	_, err = set(func(u *Updates) { u.Channel = "weekly" })
	wantInvalid(t, err, "updates.channel", "must be stable, beta or nightly")
}

func TestK1Validation(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	ca := testCertPEM(t)
	for _, tc := range []struct {
		name         string
		fn           func(a *All)
		field, msg   string
		wantAccepted bool
	}{
		{"seen retention low", func(a *All) { a.Logs.SeenRetentionDays = 6 }, "logs.seenRetentionDays", "must be between 7 and 365", false},
		{"seen retention high", func(a *All) { a.Logs.SeenRetentionDays = 366 }, "logs.seenRetentionDays", "", false},
		{"seen retention ok", func(a *All) { a.Logs.SeenRetentionDays = 365 }, "", "", true},
		{"stratum", func(a *All) { a.NTP.Stratum = 1 }, "ntp.stratum", "must be between 2 and 15", false},
		{"stratum ok", func(a *All) { a.NTP = NTP{Enabled: true, Stratum: 15} }, "", "", true},
		{"sync mode", func(a *All) { a.Sync.Mode = "leader" }, "sync.mode", "must be off or follower", false},
		{"follower without source", func(a *All) { a.Sync.Mode = SyncFollower }, "sync.source", "enter the address of the primary", false},
		{"http source", func(a *All) { a.Sync.Source = "http://primary.lan" }, "sync.source", "must be an https URL", false},
		{"source path", func(a *All) { a.Sync.Source = "https://primary.lan/api" }, "sync.source", "must be an https URL", false},
		{"source user", func(a *All) { a.Sync.Source = "https://u:p@primary.lan" }, "sync.source", "user name, password, query or fragment", false},
		{"source query", func(a *All) { a.Sync.Source = "https://primary.lan/?x=1" }, "sync.source", "user name, password, query or fragment", false},
		{"source loopback", func(a *All) { a.Sync.Source = "https://127.0.0.1:8443" }, "sync.source", "loopback", false},
		{"source link-local", func(a *All) { a.Sync.Source = "https://[fe80::1]" }, "sync.source", "link-local", false},
		{"source too long", func(a *All) { a.Sync.Source = "https://" + strings.Repeat("a", 2050) + ".lan" }, "sync.source", "must be an https URL", false},
		{"follower without token", func(a *All) {
			a.Sync.Mode, a.Sync.Source, a.Sync.Sections = SyncFollower, "https://primary.lan", []string{"local-dns"}
		}, "sync.token", "enter a sync token of the primary", false},
		{"token syntax", func(a *All) { a.Sync.Source, a.Sync.Token = "https://primary.lan", ptr("bad token") }, "sync.token", "must be an API token", false},
		{"token without source", func(a *All) { a.Sync.Token = ptr("pc_abc") }, "sync.source", "enter the address of the primary", false},
		{"ca", func(a *All) { a.Sync.CAPEM = "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----" }, "sync.caPem", "must be 1 to 4 PEM certificates", false},
		{"ca five", func(a *All) { a.Sync.CAPEM = strings.Repeat(ca, 5) }, "sync.caPem", "", false},
		{"ca key", func(a *All) { a.Sync.CAPEM = "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n" }, "sync.caPem", "", false},
		{"ca ok", func(a *All) { a.Sync.CAPEM = ca + ca }, "", "", true},
		{"interval", func(a *All) { a.Sync.IntervalMinutes = 4 }, "sync.intervalMinutes", "must be between 5 and 1440", false},
		{"sections unknown", func(a *All) { a.Sync.Sections = []string{"local-dns", "foo"} }, "sync.sections", "unknown section foo", false},
		{"sections not syncable", func(a *All) { a.Sync.Sections = []string{"dhcp"} }, "sync.sections", "dhcp cannot be synced", false},
		{"sections dependency", func(a *All) { a.Sync.Sections = []string{"clients-and-groups", "local-dns"} }, "sync.sections",
			"also sync lists-and-rules, local-dns and parental", false},
		{"follower sections", func(a *All) {
			a.Sync.Mode, a.Sync.Source, a.Sync.Token = SyncFollower, "https://primary.lan", ptr("pc_x")
		}, "sync.sections", "choose at least one section", false},
		{"proxy scheme", func(a *All) { a.Network.Proxy.URL = "https://proxy.lan:3128" }, "network.proxy.url", "must be http://host:port or socks5://host:port", false},
		{"proxy no port", func(a *All) { a.Network.Proxy.URL = "http://proxy.lan" }, "network.proxy.url", "must be http", false},
		{"proxy path", func(a *All) { a.Network.Proxy.URL = "http://proxy.lan:3128/x" }, "network.proxy.url", "must be http", false},
		{"proxy user info", func(a *All) { a.Network.Proxy.URL = "socks5://u:p@proxy.lan:1080" }, "network.proxy.url", "their own fields", false},
		{"proxy link-local", func(a *All) { a.Network.Proxy.URL = "http://169.254.1.1:3128" }, "network.proxy.url", "link-local", false},
		{"proxy long", func(a *All) { a.Network.Proxy.URL = "http://" + strings.Repeat("a", 250) + ".lan:1" }, "network.proxy.url", "", false},
		{"proxy for without url", func(a *All) { a.Network.ProxyFor.Lists = true }, "network.proxy.url", "set a proxy first", false},
		{"proxy username", func(a *All) { a.Network.Proxy.Username = "a\x01b" }, "network.proxy.username", "at most 255 printable characters", false},
		{"proxy password long", func(a *All) {
			a.Network.Proxy.URL, a.Network.Proxy.Password = "http://proxy.lan:3128", ptr(strings.Repeat("x", 256))
		}, "network.proxy.password", "at most 255 printable characters", false},
		{"proxy ok", func(a *All) {
			a.Network.Proxy.URL, a.Network.ProxyFor = "SOCKS5://Proxy.LAN:1080", ProxyFor{Lists: true, Notifications: true}
		}, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := s.Get()
			_, err := s.Update(ctx, func(a *All) error { tc.fn(a); return nil })
			if tc.wantAccepted {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			wantInvalid(t, err, tc.field, tc.msg)
			if s.Get() != before {
				t.Fatal("a refused update changed the settings")
			}
		})
	}
	if u := s.Get().Network.Proxy.URL; u != "socks5://proxy.lan:1080" {
		t.Fatalf("proxy URL normalised to %q", u)
	}
}

// A follower whose token is gone (a backup restored without secrets) keeps
// accepting writes that leave the sync section as it is; a change of the
// sync section needs the token again.
func TestFollowerWithoutStoredToken(t *testing.T) {
	ctx := context.Background()
	s, d := openTestStore(t)
	if _, err := s.Update(ctx, func(a *All) error {
		a.Sync = Sync{Mode: SyncFollower, Source: "https://primary.lan", Token: ptr("pc_x"), IntervalMinutes: 15,
			Sections: []string{"local-dns"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`DELETE FROM settings_secrets`); err != nil { // scrubBackup
		t.Fatal(err)
	}
	s, err := Open(ctx, d, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	fakeSealer(s)
	if a := s.Get(); a.Sync.Mode != SyncFollower || a.Sync.TokenSet {
		t.Fatalf("restored %+v", a.Sync)
	}
	// Unrelated writes (pausing blocking, a DNS setting) are accepted.
	if _, err := s.Update(ctx, func(a *All) error {
		until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		a.Filter.PausedUntil = &until
		a.DNS.RefuseANY = !a.DNS.RefuseANY
		return nil
	}); err != nil {
		t.Fatalf("unrelated write refused: %v", err)
	}
	// Changing the sync section needs the token.
	_, err = s.Update(ctx, func(a *All) error { a.Sync.IntervalMinutes = 30; return nil })
	wantInvalid(t, err, "sync.token", "enter a sync token of the primary")
	if a, err := s.Update(ctx, func(a *All) error { a.Sync.Token = ptr("pc_y"); return nil }); err != nil || !a.Sync.TokenSet {
		t.Fatalf("token entered again: %+v %v", a, err)
	}
	// Removing the token of a follower is refused.
	_, err = s.Update(ctx, func(a *All) error { a.Sync.Token = ptr(""); return nil })
	wantInvalid(t, err, "sync.token", "enter a sync token of the primary")
}

// Secrets: absent or null keeps, "" removes, a value replaces; a stored
// secret is kept only while its origin matches; never in the document, a
// snapshot or a subscriber's view.
func TestSettingsSecrets(t *testing.T) {
	ctx := context.Background()
	s, d := openTestStore(t)
	var seen []*All
	s.Subscribe(func(_, n *All) { seen = append(seen, n) })
	update := func(fn func(a *All)) (*All, error) {
		return s.Update(ctx, func(a *All) error { fn(a); return nil })
	}
	a, err := update(func(a *All) {
		a.Sync.Mode, a.Sync.Source, a.Sync.Sections = SyncFollower, "https://Primary.LAN:8443/", []string{"dns-settings"}
		a.Sync.Token = ptr("pc_secret1")
		a.Sync.TokenSet = false // ignored
	})
	if err != nil {
		t.Fatal(err)
	}
	if !a.Sync.TokenSet || a.Sync.Token != nil || a.Sync.Source != "https://primary.lan:8443" {
		t.Fatalf("after storing: %+v", a.Sync)
	}
	if v, err := s.Secret(ctx, SecretSyncToken); err != nil || v != "pc_secret1" {
		t.Fatalf("Secret = %q, %v", v, err)
	}
	// Keep: absent; a changed interval keeps it.
	if a, err = update(func(a *All) { a.Sync.IntervalMinutes = 30; a.Sync.TokenSet = false }); err != nil || !a.Sync.TokenSet {
		t.Fatalf("keep: %+v %v", a, err)
	}
	// Replace.
	if _, err = update(func(a *All) { a.Sync.Token = ptr("pc_secret2") }); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Secret(ctx, SecretSyncToken); v != "pc_secret2" {
		t.Fatalf("replaced: %q", v)
	}
	// A changed origin needs the token again.
	_, err = update(func(a *All) { a.Sync.Source = "https://other.lan:8443" })
	wantInvalid(t, err, "sync.token", "enter the token again: the primary's address changed")
	if a, err = update(func(a *All) { a.Sync.Source, a.Sync.Token = "https://other.lan:8443", ptr("pc_secret3") }); err != nil || !a.Sync.TokenSet {
		t.Fatalf("new origin with a token: %+v %v", a, err)
	}
	// Remove (and switch off).
	if a, err = update(func(a *All) { a.Sync.Mode, a.Sync.Token = SyncOff, ptr("") }); err != nil || a.Sync.TokenSet {
		t.Fatalf("remove: %+v %v", a, err)
	}
	if v, _ := s.Secret(ctx, SecretSyncToken); v != "" {
		t.Fatalf("removed secret still readable: %q", v)
	}
	// Proxy password bound to the origin and the user name; clearing the
	// URL removes it (nothing it could be sent to).
	if a, err = update(func(a *All) {
		a.Network.Proxy = Proxy{URL: "http://proxy.lan:3128", Username: "joe", Password: ptr("pw one")}
	}); err != nil || !a.Network.Proxy.PasswordSet {
		t.Fatalf("proxy: %+v %v", a, err)
	}
	_, err = update(func(a *All) { a.Network.Proxy.Username = "ann" })
	wantInvalid(t, err, "network.proxy.password", "enter the password again: the proxy changed")
	_, err = update(func(a *All) { a.Network.Proxy.URL = "http://proxy.lan:8080" })
	wantInvalid(t, err, "network.proxy.password", "enter the password again")
	if v, _ := s.Secret(ctx, SecretProxyPassword); v != "pw one" {
		t.Fatalf("proxy password %q", v)
	}
	if a, err = update(func(a *All) { a.Network.Proxy.URL, a.Network.Proxy.Username = "", "" }); err != nil || a.Network.Proxy.PasswordSet {
		t.Fatalf("clearing the proxy: %+v %v", a, err)
	}
	var n int
	if err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings_secrets`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("secrets left: %d %v", n, err)
	}
	// Never in the stored document, a snapshot or a subscriber's view.
	if _, err = update(func(a *All) {
		a.Sync.Source, a.Sync.Token = "https://primary.lan", ptr("pc_topsecret")
		a.Network.Proxy = Proxy{URL: "http://proxy.lan:3128", Password: ptr("hunter2hunter2")}
	}); err != nil {
		t.Fatal(err)
	}
	var doc string
	if err := d.R.QueryRowContext(ctx, `SELECT doc FROM settings`).Scan(&doc); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(s.Get())
	for _, x := range append([]*All{s.Get()}, seen...) {
		if x.Sync.Token != nil || x.Network.Proxy.Password != nil {
			t.Fatal("a snapshot carries a secret")
		}
	}
	for _, text := range []string{doc, string(b)} {
		if strings.Contains(text, "pc_topsecret") || strings.Contains(text, "hunter2") || strings.Contains(text, `"token"`) ||
			strings.Contains(text, `"password"`) {
			t.Fatalf("secret in %s", text)
		}
	}
	// A new Open reads the flags from the table.
	s2, err := Open(ctx, d, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if a := s2.Get(); !a.Sync.TokenSet || !a.Network.Proxy.PasswordSet {
		t.Fatalf("flags after Open: %+v %+v", a.Sync, a.Network)
	}
	// A secret of another origin (e.g. a restored document) does not count.
	if _, err := d.W.ExecContext(ctx, `UPDATE settings_secrets SET bound = 'https://elsewhere.lan' WHERE name = ?`, SecretSyncToken); err != nil {
		t.Fatal(err)
	}
	s3, err := Open(ctx, d, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	fakeSealer(s3)
	if s3.Get().Sync.TokenSet {
		t.Fatal("a secret of another origin counts")
	}
	if v, _ := s3.Secret(ctx, SecretSyncToken); v != "" {
		t.Fatalf("a secret of another origin is returned: %q", v)
	}
}

// Every leaf of All has a sync decision, and every decision a leaf.
func TestSyncClassificationCoversEveryLeaf(t *testing.T) {
	a := Defaults()
	until := time.Now()
	a.Filter.PausedUntil = &until
	leaves := Leaves(&a)
	for l := range leaves {
		if !SyncRuleKnown(l) {
			t.Errorf("settings member %s has no sync decision (syncRules)", l)
		}
	}
	for p := range syncRules {
		if _, ok := leaves[p]; !ok {
			t.Errorf("sync decision for %s, which is no settings member", p)
		}
	}
	for _, p := range []string{"dns.allowedNetworks", "dns.allowAllNetworks", "dns.plainDns", "dns.encrypted.dot", "web.restrictToNetworks",
		"filter.enabled", "filter.pausedUntil", "sync.mode", "logs.queryLogEnabled"} {
		if Syncable(p) {
			t.Errorf("%s must never be synced", p)
		}
	}
	for _, p := range []string{"dns.upstreams", "dns.blockedClients", "filter.blockingMode", "dns.ecs.mode"} {
		if !Syncable(p) {
			t.Errorf("%s must be synced", p)
		}
	}
}

func TestSyncableSettingsRoundTrip(t *testing.T) {
	prim := Defaults()
	prim.DNS.Upstreams = []string{"tls://dns.example.net"}
	prim.DNS.ECS = ECS{Mode: ECSClient}
	prim.DNS.AllowAllNetworks = true
	prim.DNS.Encrypted.ServerName = "dns.primary.example"
	prim.Filter.BlockingMode = "nxdomain"
	prim.Filter.Enabled = false
	raw, err := SyncableSettings(&prim)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]map[string]jsontext.Value
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"allowAllNetworks", "encrypted", "serverNames", "plainDns", "routerResolver"} {
		if _, ok := probe["dns"][m]; ok {
			t.Fatalf("dns.%s exported", m)
		}
	}
	if _, ok := probe["filter"]["enabled"]; ok {
		t.Fatal("filter.enabled exported")
	}
	fol := Defaults()
	fol.DNS.ServerNames = []string{"follower"}
	if legacy, err := ApplySyncable(&fol, raw); err != nil || legacy != nil {
		t.Fatal(legacy, err)
	}
	if !slices.Equal(fol.DNS.Upstreams, prim.DNS.Upstreams) || fol.DNS.ECS.Mode != ECSClient || fol.Filter.BlockingMode != "nxdomain" {
		t.Fatalf("synced members not applied: %+v", fol.DNS)
	}
	if fol.DNS.AllowAllNetworks || !fol.Filter.Enabled || fol.DNS.Encrypted.ServerName != "" || !slices.Equal(fol.DNS.ServerNames, []string{"follower"}) {
		t.Fatalf("follower members changed: %+v", fol)
	}
	if got := SyncedChange(&defaults0, &fol); got == "" {
		t.Fatal("SyncedChange found nothing")
	}
	// A member that is not synced, or unknown, is refused.
	for _, bad := range []string{`{"dns":{"allowAllNetworks":true}}`, `{"dns":{"newThing":1}}`, `{"web":{}}`,
		`{"dns":{"encrypted":{"dot":true}}}`} {
		f := Defaults()
		if _, err := ApplySyncable(&f, jsontext.Value(bad)); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
}

// REV-3: a primary before 1.0.0 exports upstreams with text after "#" that
// this version refuses (it ignored that text); the follower reads them with
// that meaning, as its own stored settings, and reports each one. The
// result validates, so the sync is not refused.
func TestApplySyncableLegacyUpstreams(t *testing.T) {
	raw := jsontext.Value(`{"dns":{"upstreams":["9.9.9.9#dns.quad9.net","tls://1.1.1.1#cloudflare-dns.com","9.9.9.9"],` +
		`"fallbackUpstreams":["8.8.8.8#google"],"localPtrUpstreams":["192.168.1.1#router"]},"filter":{}}`)
	fol := Defaults()
	legacy, err := ApplySyncable(&fol, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fol.DNS.Upstreams, []string{"9.9.9.9", "tls://1.1.1.1"}) || !slices.Equal(fol.DNS.FallbackUpstreams, []string{"8.8.8.8"}) ||
		!slices.Equal(fol.DNS.LocalPTRUpstreams, []string{"192.168.1.1"}) {
		t.Fatalf("read as: %v %v %v", fol.DNS.Upstreams, fol.DNS.FallbackUpstreams, fol.DNS.LocalPTRUpstreams)
	}
	want := [][2]string{{"9.9.9.9#dns.quad9.net", "9.9.9.9"}, {"tls://1.1.1.1#cloudflare-dns.com", "tls://1.1.1.1"},
		{"8.8.8.8#google", "8.8.8.8"}, {"192.168.1.1#router", "192.168.1.1"}}
	if !slices.Equal(legacy, want) {
		t.Fatalf("legacy %v", legacy)
	}
	if err := fol.Validate(); err != nil {
		t.Fatalf("the synced settings do not validate: %v", err)
	}
	// A port after "#" is this version's meaning and is kept as it is.
	fol = Defaults()
	if legacy, err := ApplySyncable(&fol, jsontext.Value(`{"dns":{"upstreams":["10.0.0.53#5353"]}}`)); err != nil || legacy != nil ||
		!slices.Equal(fol.DNS.Upstreams, []string{"10.0.0.53#5353"}) {
		t.Fatalf("a port after #: %v %v %v", fol.DNS.Upstreams, legacy, err)
	}
}

var defaults0 = Defaults()

func TestCheckSections(t *testing.T) {
	got, err := CheckSections("sections", []string{" Local-DNS", "settings", "local-dns"}, RestoreSections, "restore")
	if err != nil || !slices.Equal(got, []string{"settings", "local-dns"}) {
		t.Fatalf("%v %v", got, err)
	}
	_, err = CheckSections("sections", nil, RestoreSections, "restore")
	wantInvalid(t, err, "sections", "choose at least one section")
	_, err = CheckSections("sections", []string{"x"}, RestoreSections, "restore")
	wantInvalid(t, err, "sections", "unknown section x")
	_, err = CheckSections("sections", []string{"dhcp"}, ExportSections, "export")
	wantInvalid(t, err, "sections", "dhcp cannot be exported")
	_, err = CheckSections("sections", []string{"clients-and-groups", "lists-and-rules", "parental"}, RestoreSections, "restore")
	wantInvalid(t, err, "sections", "also restore lists-and-rules, local-dns and parental")
	// The export itself carries any exportable selection.
	if _, err = CheckSections("sections", []string{"clients-and-groups"}, ExportSections, "export"); err != nil {
		t.Fatal(err)
	}
}

func testCertPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, NotBefore: time.Now(),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
