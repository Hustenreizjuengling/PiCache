package notify

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/secrets"
)

// newTestService opens a service on a fresh picache.db with a zero master key.
func newTestService(t *testing.T) (*Service, *db.DB) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "picache.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	s, err := New(context.Background(), d, testBox(t), Options{InstanceID: "picache-0123456789ab", Hostname: "pi", Version: "v0.4.0"},
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return s, d
}

func testBox(t *testing.T) *secrets.Box {
	t.Helper()
	box, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func strp(s string) *string { return &s }

func wantInvalid(t *testing.T, what string, err error, field string) {
	t.Helper()
	if e, ok := apperr.As(err); !ok || e.Kind != apperr.KindInvalid || e.Field != field {
		t.Fatalf("%s: err = %v, want invalid %q", what, err, field)
	}
}

func TestChannelValidation(t *testing.T) {
	s, _ := newTestService(t)
	ok := ChannelInput{Name: "Home Assistant", Kind: KindWebhook, URL: "http://192.168.1.20:8123/api/webhook/picache", Enabled: true}
	for _, tc := range []struct {
		name  string
		fn    func(*ChannelInput)
		field string
	}{
		{"no name", func(in *ChannelInput) { in.Name = "  " }, "name"},
		{"long name", func(in *ChannelInput) { in.Name = strings.Repeat("ä", 65) }, "name"},
		{"control in name", func(in *ChannelInput) { in.Name = "a\x07b" }, "name"},
		{"kind", func(in *ChannelInput) { in.Kind = "email" }, "kind"},
		{"no url", func(in *ChannelInput) { in.URL = "" }, "url"},
		{"scheme", func(in *ChannelInput) { in.URL = "ftp://192.168.1.20/x" }, "url"},
		{"file", func(in *ChannelInput) { in.URL = "file:///etc/passwd" }, "url"},
		{"userinfo", func(in *ChannelInput) { in.URL = "https://user:pw@ntfy.example/t" }, "url"},
		{"user only", func(in *ChannelInput) { in.URL = "https://user@ntfy.example/t" }, "url"},
		{"fragment", func(in *ChannelInput) { in.URL = "https://ntfy.example/t#x" }, "url"},
		{"space", func(in *ChannelInput) { in.URL = "https://ntfy.example/a b" }, "url"},
		{"non-ascii", func(in *ChannelInput) { in.URL = "https://ntfy.example/äö" }, "url"},
		{"opaque", func(in *ChannelInput) { in.URL = "http:ntfy.example" }, "url"},
		{"port", func(in *ChannelInput) { in.URL = "http://ntfy.example:0/t" }, "url"},
		{"host", func(in *ChannelInput) { in.URL = "http://bad_host!/t" }, "url"},
		{"metadata", func(in *ChannelInput) { in.URL = "http://169.254.169.254/latest" }, "url"},
		{"link-local v6", func(in *ChannelInput) { in.URL = "http://[fe80::1]/x" }, "url"},
		{"multicast", func(in *ChannelInput) { in.URL = "http://239.1.2.3/x" }, "url"},
		{"unspecified", func(in *ChannelInput) { in.URL = "http://0.0.0.0:8080/x" }, "url"},
		{"nat64 metadata", func(in *ChannelInput) { in.URL = "http://[64:ff9b::a9fe:a9fe]/x" }, "url"},
		{"long url", func(in *ChannelInput) { in.URL = "https://ntfy.example/" + strings.Repeat("a", maxURL) }, "url"},
		{"severity", func(in *ChannelInput) { in.MinSeverity = "debug" }, "minSeverity"},
		{"event", func(in *ChannelInput) { in.Events = []string{"health.failed", "dns.down"} }, "events"},
		{"summary event", func(in *ChannelInput) { in.Events = []string{EventDropped} }, "events"},
		{"secret control", func(in *ChannelInput) { in.Secret = strp("Bearer a\nb") }, "secret"},
		{"secret long", func(in *ChannelInput) { in.Secret = strp(strings.Repeat("x", maxSecret+1)) }, "secret"},
		{"gotify without token", func(in *ChannelInput) { in.Kind = KindGotify; in.URL = "https://gotify.lan" }, "secret"},
		{"gotify empty token", func(in *ChannelInput) {
			in.Kind, in.URL, in.Secret = KindGotify, "https://gotify.lan", strp(" ")
		}, "secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := ok
			tc.fn(&in)
			_, err := s.Create(t.Context(), in)
			wantInvalid(t, tc.name, err, tc.field)
		})
	}
	if n := len(s.Channels()); n != 0 {
		t.Fatalf("%d channels after invalid input", n)
	}

	// Accepted forms, normalised.
	for _, in := range []ChannelInput{
		ok,
		{Name: " ntfy ", Kind: " NTFY ", URL: " https://ntfy.sh/picache-alerts ", MinSeverity: "Error", Events: []string{"health.failed", "health.failed", " backup.failed "}},
		{Name: "loopback", Kind: KindWebhook, URL: "http://127.0.0.1:8123/hook?x=1"},
		{Name: "lan name", Kind: KindWebhook, URL: "https://homeassistant.local/api/webhook/abc"},
		{Name: "ula", Kind: KindWebhook, URL: "http://[fd00::20]:8123/api/webhook/abc"},
		{Name: "gotify", Kind: KindGotify, URL: "https://gotify.lan/", Secret: strp("AbC.123")},
	} {
		c, err := s.Create(t.Context(), in)
		if err != nil {
			t.Fatalf("%+v: %v", in, err)
		}
		if len(c.ID) != 32 || c.Events == nil || c.MinSeverity.rank() < 0 || c.CreatedAt.IsZero() {
			t.Fatalf("created %+v", c)
		}
		if c.Name == "ntfy" && (c.Kind != KindNtfy || c.URL != "https://ntfy.sh/picache-alerts" || c.MinSeverity != SeverityError ||
			strings.Join(c.Events, ",") != "health.failed,backup.failed") {
			t.Fatalf("not normalised: %+v", c)
		}
		if c.Name == "Home Assistant" && c.MinSeverity != SeverityWarning {
			t.Fatalf("default severity: %+v", c)
		}
	}
	for len(s.Channels()) < maxChannels {
		if _, err := s.Create(t.Context(), ok); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Create(t.Context(), ok); apperr.KindOf(err) != apperr.KindConflict {
		t.Fatalf("channel %d: %v", maxChannels+1, err)
	}
}

// The secret is write-only: sealed with the master key and the channel id,
// never returned, kept when absent, removed with "", and not kept when the
// kind or the URL's server changes (a changed URL must never receive it).
func TestChannelSecret(t *testing.T) {
	s, d := newTestService(t)
	const secret = "Bearer s3cr3t-webhook-token"
	in := ChannelInput{Name: "HA", Kind: KindWebhook, URL: "https://ha.lan:8123/api/webhook/one", Secret: strp(secret), Enabled: true}
	c, err := s.Create(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(s.Channels())
	if !c.HasSecret || strings.Contains(string(b), "s3cr3t") || !strings.Contains(string(b), `"hasSecret":true`) {
		t.Fatalf("channel JSON %s", b)
	}
	sealed := func() string {
		t.Helper()
		var v string
		if err := d.R.QueryRow(`SELECT COALESCE(secret_sealed, '') FROM notify_channels WHERE id = ?`, c.ID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	stored := sealed()
	if !strings.HasPrefix(stored, "v1:") || strings.Contains(stored, "s3cr3t") {
		t.Fatalf("stored %q", stored)
	}
	if pt, err := testBox(t).Open(stored, secretAAD(c.ID)); err != nil || string(pt) != secret {
		t.Fatalf("open = %q, %v", pt, err)
	}
	if _, err := testBox(t).Open(stored, secretAAD(newID())); err == nil {
		t.Fatal("the secret must be bound to its channel")
	}

	// Absent keeps it, also with another path, name or filter.
	in.Secret, in.URL, in.Name, in.MinSeverity = nil, "https://HA.lan:8123/api/webhook/two?x=1", "HA 2", SeverityInfo
	if c, err = s.Update(t.Context(), c.ID, in); err != nil || !c.HasSecret || sealed() != stored {
		t.Fatalf("keep: %+v, %v", c, err)
	}
	// Another host, port, scheme or kind needs the secret again.
	for _, change := range []func(*ChannelInput){
		func(in *ChannelInput) { in.URL = "https://evil.example:8123/api/webhook/two" },
		func(in *ChannelInput) { in.URL = "https://ha.lan:8124/api/webhook/two" },
		func(in *ChannelInput) { in.URL = "http://ha.lan:8123/api/webhook/two" },
		func(in *ChannelInput) { in.Kind = KindNtfy },
	} {
		next := in
		change(&next)
		_, err := s.Update(t.Context(), c.ID, next)
		wantInvalid(t, next.URL+" "+string(next.Kind), err, "secret")
		if sealed() != stored {
			t.Fatal("a refused update changed the secret")
		}
	}
	// With a new secret the server can change.
	next := in
	next.URL, next.Secret = "https://other.example/hook", strp("Bearer new")
	if c2, err := s.Update(t.Context(), c.ID, next); err != nil || !c2.HasSecret || sealed() == stored || sealed() == "" {
		t.Fatalf("with a new secret: %+v, %v", c2, err)
	}
	// "" removes it.
	in.Secret = strp("")
	if c, err = s.Update(t.Context(), c.ID, in); err != nil || c.HasSecret || sealed() != "" {
		t.Fatalf("remove: %+v, %v", c, err)
	}
	// Without a stored secret, the server may change freely.
	in.Secret, in.URL = nil, "https://other.lan/hook"
	if _, err := s.Update(t.Context(), c.ID, in); err != nil {
		t.Fatal(err)
	}

	// Gotify requires its token on create and keeps it on update.
	g, err := s.Create(t.Context(), ChannelInput{Name: "g", Kind: KindGotify, URL: "https://gotify.lan", Secret: strp("AppToken1")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(t.Context(), g.ID, ChannelInput{Name: "g2", Kind: KindGotify, URL: "https://gotify.lan/"}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Update(t.Context(), g.ID, ChannelInput{Name: "g", Kind: KindGotify, URL: "https://gotify.lan", Secret: strp("")})
	wantInvalid(t, "remove gotify token", err, "secret")

	// ntfy: "Bearer " is added when sending, a pasted one is removed.
	n, err := s.Create(t.Context(), ChannelInput{Name: "n", Kind: KindNtfy, URL: "https://ntfy.sh/t", Secret: strp("Bearer tk_abc")})
	if err != nil {
		t.Fatal(err)
	}
	_, ns := s.worker(n.ID).snapshot()
	if pt, _ := testBox(t).Open(ns, secretAAD(n.ID)); string(pt) != "tk_abc" {
		t.Fatalf("ntfy token %q", pt)
	}

	// Everything survives a restart; unknown ids are 404.
	s2, err := New(t.Context(), d, testBox(t), Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s2.Channel(g.ID); err != nil || !got.HasSecret || got.Name != "g2" || got.URL != "https://gotify.lan/" {
		t.Fatalf("after restart %+v, %v", got, err)
	}
	if err := s2.Delete(t.Context(), g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Channel(g.ID); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("deleted: %v", err)
	}
	if err := s2.Delete(t.Context(), g.ID); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("delete twice: %v", err)
	}
	if _, err := s2.Update(t.Context(), newID(), in); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("update unknown: %v", err)
	}
}

// A row whose event list cannot be read is loaded disabled instead of
// matching every event.
func TestChannelLoadTamperedEvents(t *testing.T) {
	s, d := newTestService(t)
	c, err := s.Create(t.Context(), ChannelInput{Name: "x", Kind: KindWebhook, URL: "http://10.0.0.2/h", Enabled: true,
		Events: []string{EventBackupFailed}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`UPDATE notify_channels SET events = 'not json' WHERE id = ?`, c.ID); err != nil {
		t.Fatal(err)
	}
	s2, err := New(t.Context(), d, testBox(t), Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s2.Channel(c.ID); got.Enabled || got.Events == nil {
		t.Fatalf("tampered row %+v", got)
	}
}

func TestDisplayURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://ntfy.sh/topic?auth=tk_secret": "https://ntfy.sh/topic",
		"http://10.0.0.2:8123/api/webhook/x":   "http://10.0.0.2:8123/api/webhook/x",
		"https://gotify.lan/?token=abc#frag":   "https://gotify.lan/",
		"%zz":                                  "",
	} {
		if got := DisplayURL(in); got != want {
			t.Errorf("DisplayURL(%q) = %q, want %q", in, got, want)
		}
	}
	c := Channel{URL: "https://ntfy.sh/t?auth=x", Events: []string{"a"}}
	if r := c.Redacted(); r.URL != "https://ntfy.sh/t" || c.URL != "https://ntfy.sh/t?auth=x" {
		t.Fatalf("redacted %+v", r)
	}
}
