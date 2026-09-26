package auth

import (
	"maps"
	"slices"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/applog"
)

// The application log redacts the same names as the audit log.
func TestRedactedNamesMatchApplog(t *testing.T) {
	if got, want := applog.RedactedNames(), slices.Sorted(maps.Keys(redactedNames)); !slices.Equal(got, want) {
		t.Fatalf("applog redacts %v, the audit log %v", got, want)
	}
}
