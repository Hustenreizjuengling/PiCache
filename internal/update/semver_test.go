package update

import (
	"strings"
	"testing"
)

func mustVersion(t *testing.T, s string) Version {
	t.Helper()
	v, err := ParseVersion(s)
	if err != nil {
		t.Fatalf("ParseVersion(%q): %v", s, err)
	}
	return v
}

// The precedence example of SemVer 2.0 §11, plus numbers beyond one digit
// and beyond uint64 in pre-release identifiers.
func TestVersionPrecedence(t *testing.T) {
	ordered := []string{
		"v0.9.0", "v0.9.1", "v0.10.0",
		"v1.0.0-0", "v1.0.0-9", "v1.0.0-10", "v1.0.0-99999999999999999999999",
		"v1.0.0-alpha", "v1.0.0-alpha.1", "v1.0.0-alpha.beta", "v1.0.0-beta", "v1.0.0-beta.2", "v1.0.0-beta.11",
		"v1.0.0-rc.1", "v1.0.0", "v1.0.1", "v1.1.0-rc.1", "v1.1.0", "v2.0.0", "v10.0.0",
	}
	for i := range ordered {
		for j := range ordered {
			a, b := mustVersion(t, ordered[i]), mustVersion(t, ordered[j])
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got := Compare(a, b); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
	if s := mustVersion(t, "v1.2.3-rc.1").String(); s != "v1.2.3-rc.1" {
		t.Errorf("String() = %q", s)
	}
}

func TestParseVersionRejects(t *testing.T) {
	for _, s := range []string{
		"", "1.2.3", "v1.2", "v1.2.3.4", "v01.2.3", "v1.02.3", "v1.2.03", "v1.2.3-", "v1.2.3-rc..1", "v1.2.3-01",
		"v1.2.3+build", "v1.2.3-rc.1+build", "V1.2.3", " v1.2.3", "v1.2.3\n", "v1.2.3/../x", "v1.2.3;rm", "latest",
		"v99999999999999999999.0.0", "v1.2.3-" + strings.Repeat("a", 200),
	} {
		if _, err := ParseVersion(s); err == nil {
			t.Errorf("ParseVersion(%q) succeeded", s)
		}
	}
	if v := mustVersion(t, "v1.2.3-rc-1.x-y"); len(v.Pre) != 2 || !v.Prerelease() {
		t.Errorf("hyphens in identifiers: %+v", v)
	}
}

func TestParseRunning(t *testing.T) {
	for _, tc := range []struct {
		in   string
		dev  bool
		base string // "" = no base
	}{
		{"v1.2.3", false, "v1.2.3"},
		{"v1.2.3-rc.1", false, "v1.2.3-rc.1"},
		{"v1.2.3-4-gabc1234", true, "v1.2.3"},
		{"v1.2.3-4-gabc1234-dirty", true, "v1.2.3"},
		{"v1.2.3-dirty", true, "v1.2.3"},
		{"v1.2.3-rc.1-12-g0123456789ab", true, "v1.2.3-rc.1"},
		{"dev", true, ""},
		{"unknown", true, ""},
		{"", true, ""},
		{"abc1234", true, ""},
		{"abc1234-dirty", true, ""},
		{"1.2.3", true, ""},
	} {
		r := ParseRunning(tc.in)
		base := ""
		if r.Base != nil {
			base = r.Base.String()
		}
		if r.Dev != tc.dev || base != tc.base || r.Raw != tc.in {
			t.Errorf("ParseRunning(%q) = dev %v base %q, want dev %v base %q", tc.in, r.Dev, base, tc.dev, tc.base)
		}
	}
}

// An update is a release greater than the running release or the base of
// a development build (which counts as newer than its base); a development
// build without a base takes any release.
func TestRunningAccepts(t *testing.T) {
	for _, tc := range []struct {
		running, target string
		want            bool
	}{
		{"v1.2.3", "v1.2.4", true},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.3", "v1.2.2", false},
		{"v1.2.3", "v1.3.0-rc.1", true},
		{"v1.2.3-rc.1", "v1.2.3", true},
		{"v1.2.3-rc.1", "v1.2.3-rc.2", true},
		{"v1.2.3-rc.2", "v1.2.3-rc.1", false},
		{"v1.2.3-4-gabc1234", "v1.2.3", false},
		{"v1.2.3-4-gabc1234-dirty", "v1.2.4", true},
		{"v1.2.3-dirty", "v1.2.3", false},
		{"dev", "v0.0.1", true},
		{"abc1234", "v1.0.0-rc.1", true},
	} {
		if got := ParseRunning(tc.running).Accepts(mustVersion(t, tc.target)); got != tc.want {
			t.Errorf("%s accepts %s = %v, want %v", tc.running, tc.target, got, tc.want)
		}
	}
	if err := checkNewer("v1.2.3", mustVersion(t, "v1.2.0"), false); err == nil || !strings.Contains(err.Error(), "--allow-downgrade") {
		t.Errorf("downgrade: %v", err)
	}
	if err := checkNewer("v1.2.3", mustVersion(t, "v1.2.0"), true); err != nil {
		t.Errorf("allowed downgrade: %v", err)
	}
}

func TestNewOverview(t *testing.T) {
	rc := &Release{Version: "v1.3.0-rc.1", Prerelease: true}
	stable := &Release{Version: "v1.2.4"}
	for _, tc := range []struct {
		name       string
		current    string
		includePre bool
		latest     *Release
		mode       string
		wantLatest bool
		available  bool
		cli        string
	}{
		{"update", "v1.2.3", false, stable, ModeHelper, true, true, "sudo picache update --version v1.2.4"},
		{"up to date", "v1.2.4", false, stable, ModeManual, true, false, "sudo picache update"},
		{"pre-release allowed", "v1.2.3", true, rc, ModeHelper, true, true, "sudo picache update --version v1.3.0-rc.1"},
		{"pre-release no longer allowed", "v1.2.3", false, rc, ModeHelper, false, false, "sudo picache update"},
		{"dev build", "dev", false, stable, ModeDocker, true, true, "sudo picache update --version v1.2.4"},
		{"nothing found", "v1.2.3", false, nil, ModeManual, false, false, "sudo picache update"},
	} {
		o := NewOverview(tc.current, tc.mode, true, tc.includePre, CheckResult{Latest: tc.latest}, nil)
		if (o.Latest != nil) != tc.wantLatest || o.UpdateAvailable != tc.available || o.Commands.CLI != tc.cli ||
			o.Current.Version != tc.current || o.CurrentIsDevBuild != (tc.current == "dev") || o.Mode != tc.mode ||
			(o.Commands.Docker != "") != (tc.mode == ModeDocker) {
			t.Errorf("%s: %+v", tc.name, o)
		}
	}
}
