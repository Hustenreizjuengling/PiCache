package prefill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/prefill/prefillapi"
)

// A run that left its marker is reported: with Go's crash output as a
// crash, without as killed; a run that ends by itself leaves none.
func TestPreviousRun(t *testing.T) {
	marker := `{"started":"2026-10-21T20:00:00Z","pid":42,"version":"v1.5.0"}`
	for _, tc := range []struct {
		name, crash, kind string
	}{
		{"crash", "panic: runtime error: index out of range [3] with length 3\n\ngoroutine 7 [running]:\nmain.f()", prefillapi.ExitCrash},
		{"killed", "", prefillapi.ExitKilled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stateDir string
			e := newEnvWith(t, func(c *Config) {
				stateDir = c.StateDir
				if err := os.MkdirAll(stateDir, 0o700); err != nil {
					t.Fatal(err)
				}
				os.WriteFile(filepath.Join(stateDir, fileRunning), []byte(marker), 0o600)
				if tc.crash != "" {
					os.WriteFile(filepath.Join(stateDir, fileCrashPrev), []byte(tc.crash), 0o600)
				}
			})
			st := e.status()
			if len(st.Exits) != 1 || st.Exits[0].Kind != tc.kind || st.Exits[0].Version != "v1.5.0" ||
				!st.Exits[0].Started.Equal(time.Date(2026, 10, 21, 20, 0, 0, 0, time.UTC)) {
				t.Fatalf("exits %+v", st.Exits)
			}
			if tc.crash != "" && !strings.HasPrefix(st.Exits[0].Detail, "panic: runtime error") {
				t.Errorf("detail %q", st.Exits[0].Detail)
			}
			var lg prefillapi.Log
			e.call("GET", "/v1/log?lines=50", nil, &lg, 200)
			if len(lg.Lines) == 0 || !strings.Contains(strings.Join(lg.Lines, "\n"), "the last run of the prefill service") {
				t.Errorf("log %q", lg.Lines)
			}
			// This run's marker, gone after a clean stop.
			if _, err := os.Stat(filepath.Join(stateDir, fileRunning)); err != nil {
				t.Errorf("marker: %v", err)
			}
			if _, err := os.Stat(filepath.Join(stateDir, fileCrashPrev)); !os.IsNotExist(err) {
				t.Errorf("crash.prev left: %v", err)
			}
			e.stop()
			if _, err := os.Stat(filepath.Join(stateDir, fileRunning)); !os.IsNotExist(err) {
				t.Errorf("marker after a clean stop: %v", err)
			}
		})
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"refresh failed: eg1~eyJhbGciOiJIUzI1NiJ9.abc.def":                  "refresh failed: eg1~ey…",
		"key 88d5e8ef05ec5a8446cdec93d142d7c6 stays":                        "key 88d5e8ef05ec5a8446cdec93d142d7c6 stays",
		"jwt eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiI3NjU2MTE5ODAwMDAwMDAwMSJ9.sig": "jwt eyJhbG…",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}
