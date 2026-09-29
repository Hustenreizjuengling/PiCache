package settings

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/db"
)

// web.language accepts "" and the ids of Languages, nothing else.
func TestWebLanguageValues(t *testing.T) {
	for _, l := range append([]string{""}, Languages...) {
		a := Defaults()
		a.Web.Language = l
		if err := a.Validate(); err != nil {
			t.Errorf("language %q: %v", l, err)
		}
	}
	for _, l := range []string{"EN", "pt", "pt-br", "de-DE", "xx", " en", "zh"} {
		a := Defaults()
		a.Web.Language = l
		wantInvalid(t, a.Validate(), "web.language", "must be empty or one of en, de")
	}
	if len(Languages) != 2 || Languages[0] != "en" || Languages[1] != "de" {
		t.Fatalf("Languages = %v", Languages)
	}
}

// A stored web.language of a later version is read as "" with one WARN
// (Open) and silently by DecodeStored; a write of such a value still
// answers 400.
func TestUnknownStoredLanguage(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	if _, err := Open(ctx, d, log); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, `UPDATE settings SET doc = json_set(doc, '$.web.language', 'ja')`); err != nil {
		t.Fatal(err)
	}
	logBuf.Reset()
	s, err := Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Get().Web.Language; got != "" {
		t.Fatalf("unknown language loaded as %q", got)
	}
	if n := strings.Count(logBuf.String(), "level=WARN"); n != 1 || !strings.Contains(logBuf.String(), `language="\"ja\""`) {
		t.Fatalf("want one WARN naming the language, got:\n%s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "stored settings are invalid") {
		t.Fatal("the unknown language left the document invalid")
	}
	_, err = s.Update(ctx, func(a *All) error { a.Web.Language = "ja"; return nil })
	wantInvalid(t, err, "web.language", "must be empty or one of")
	if _, err := s.Update(ctx, func(a *All) error { a.Web.Language = "de"; return nil }); err != nil {
		t.Fatal(err)
	}
	if s.Get().Web.Language != "de" {
		t.Fatal("de not stored")
	}

	a, err := DecodeStored([]byte(`{"web":{"language":"xx-unknown"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.Web.Language != "" {
		t.Fatalf("DecodeStored kept %q", a.Web.Language)
	}
	if a, err = DecodeStored([]byte(`{"web":{"language":"de"}}`)); err != nil || a.Web.Language != "de" {
		t.Fatalf("DecodeStored de: %v %q", err, a.Web.Language)
	}
}

// web.onboardingDone: true by default, so a document without it (before
// 0.16.0) reads true in Open and DecodeStored; a stored false stays.
func TestOnboardingDoneDefaults(t *testing.T) {
	if !Defaults().Web.OnboardingDone {
		t.Fatal("Defaults must give onboardingDone true")
	}
	ctx := context.Background()
	s, d := openTestStore(t)
	if !s.Get().Web.OnboardingDone {
		t.Fatal("a created document must hold the default true (the app stores false on a fresh start)")
	}
	if _, err := d.W.ExecContext(ctx, `UPDATE settings SET doc = json_remove(doc, '$.web.onboardingDone')`); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, d, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Get().Web.OnboardingDone {
		t.Fatal("a document without the member must read true")
	}
	fakeSealer(s2)
	if _, err := s2.Update(ctx, func(a *All) error { a.Web.OnboardingDone = false; return nil }); err != nil {
		t.Fatal(err)
	}
	s3, err := Open(ctx, d, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if s3.Get().Web.OnboardingDone {
		t.Fatal("a stored false was not kept")
	}
	var doc string
	if err := d.R.QueryRowContext(ctx, `SELECT doc FROM settings WHERE id = 1`).Scan(&doc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, `"onboardingDone":false`) {
		t.Fatalf("stored document lacks the member: %s", doc)
	}

	for _, tc := range []struct {
		doc  string
		want bool
	}{
		{`{"web":{"language":"de"}}`, true},
		{`{}`, true},
		{`{"web":{"onboardingDone":false}}`, false},
		{`{"web":{"onboardingDone":true}}`, true},
	} {
		a, err := DecodeStored([]byte(tc.doc))
		if err != nil {
			t.Fatal(err)
		}
		if a.Web.OnboardingDone != tc.want {
			t.Errorf("DecodeStored(%s): onboardingDone %v, want %v", tc.doc, a.Web.OnboardingDone, tc.want)
		}
	}
	// No 400s: both values are valid.
	for _, v := range []bool{true, false} {
		a := Defaults()
		a.Web.OnboardingDone = v
		if err := a.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	b, err := json.Marshal(Defaults().Web)
	if err != nil || !strings.Contains(string(b), `"onboardingDone":true`) {
		t.Fatalf("JSON name: %s %v", b, err)
	}
}

// web.onboardingDone is never synced (every web member is this machine's).
func TestOnboardingDoneNotSynced(t *testing.T) {
	if !SyncRuleKnown("web.onboardingDone") || Syncable("web.onboardingDone") {
		t.Fatal("web.onboardingDone must have the decision never synced")
	}
	a := Defaults()
	a.Web.OnboardingDone = false
	v, err := SyncableSettings(&a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(v), "onboardingDone") {
		t.Fatalf("dns-settings carry the member: %s", v)
	}
}

// A stored document (also a restored backup) is untrusted: after
// DecodeStored web.language is always one this version knows.
func FuzzDecodeStoredLanguage(f *testing.F) {
	for _, s := range []string{`{"web":{"language":"de"}}`, `{"web":{"language":"ja"}}`, `{"web":{"language":""}}`,
		`{"web":{"language":"sv","onboardingDone":false}}`, `{"web":null}`, `{"web":{"language":1}}`, `[]`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, doc []byte) {
		a, err := DecodeStored(doc)
		if err != nil {
			return
		}
		if !KnownLanguage(a.Web.Language) {
			t.Fatalf("DecodeStored kept the language %q", a.Web.Language)
		}
	})
}
