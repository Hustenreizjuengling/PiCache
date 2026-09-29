package settings

import (
	"os"
	"regexp"
	"slices"
	"testing"
)

// Languages equals the ids of LOCALES in the web UI (same ids, same order):
// web.language accepts exactly the languages the UI can load.
func TestLanguagesMatchWebLocales(t *testing.T) {
	b, err := os.ReadFile("../../web/src/i18n/locales.ts")
	if err != nil {
		t.Fatalf("the web UI's language list: %v", err)
	}
	// One entry per line: { id: 'en', label: 'English', tag: 'en-US' },
	re := regexp.MustCompile(`(?m)^\s*\{\s*id:\s*'([^']+)'\s*,\s*label:\s*'[^']+'\s*,\s*tag:\s*'[^']+'\s*\}\s*,?\s*$`)
	var ids []string
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		ids = append(ids, m[1])
	}
	if !slices.Equal(ids, Languages) {
		t.Fatalf("web/src/i18n/locales.ts lists %v, settings.Languages %v", ids, Languages)
	}
}
