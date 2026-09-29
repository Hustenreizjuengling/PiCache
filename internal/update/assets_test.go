package update

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The release assets are named in several places: the Makefile (BINARIES,
// DEB_ARCHES), both release workflows (job env), scripts/get-picache.sh
// (arch), scripts/build-deb.sh (the binary of each package) and the
// updater (AssetName, DebianArch). They must agree.

// buildTargets are the GOARCH/GOARM pairs PiCache is built for.
var buildTargets = [][2]string{{"386", ""}, {"amd64", ""}, {"arm64", ""}, {"arm", "6"}, {"arm", "7"}, {"riscv64", ""}}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// makeList returns the words of a "NAME := …" line of the Makefile.
func makeList(t *testing.T, mk, name string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + name + ` := (.+)$`).FindStringSubmatch(mk)
	if m == nil {
		t.Fatalf("the Makefile has no line %s := …", name)
	}
	return strings.Fields(m[1])
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

func TestReleaseAssetNamesAgree(t *testing.T) {
	mk := readRepoFile(t, "Makefile")
	binaries := makeList(t, mk, "BINARIES")
	debArches := makeList(t, mk, "DEB_ARCHES")
	if !slices.IsSorted(binaries) || !slices.IsSorted(debArches) {
		t.Errorf("BINARIES %v and DEB_ARCHES %v are kept sorted", binaries, debArches)
	}

	// The updater: every build target has a binary and a package.
	var assets, debs []string
	for _, bt := range buildTargets {
		a, ok := AssetName(bt[0], bt[1])
		if !ok {
			t.Fatalf("AssetName(%s, %q) has no asset", bt[0], bt[1])
		}
		assets = append(assets, a)
		d, ok := DebianArch(bt[0], bt[1])
		if !ok {
			t.Fatalf("DebianArch(%s, %q) has no architecture", bt[0], bt[1])
		}
		if !slices.Contains(debs, d) {
			debs = append(debs, d)
		}
	}
	if !slices.Equal(sorted(assets), binaries) {
		t.Errorf("AssetName gives %v, the Makefile's BINARIES %v", sorted(assets), binaries)
	}
	if !slices.Equal(sorted(debs), debArches) {
		t.Errorf("DebianArch gives %v, the Makefile's DEB_ARCHES %v", sorted(debs), debArches)
	}

	// Both workflows repeat the values in their job env.
	for _, wf := range []string{".github/workflows/release.yml", ".github/workflows/nightly.yml"} {
		text := readRepoFile(t, wf)
		for name, want := range map[string][]string{"BINARIES": binaries, "DEB_ARCHES": debArches} {
			lines := regexp.MustCompile(`(?m)^\s+`+name+`: (.+)$`).FindAllStringSubmatch(text, -1)
			if len(lines) < 2 {
				t.Errorf("%s: %d %s env lines, want the build and the release job", wf, len(lines), name)
			}
			for _, l := range lines {
				if got := strings.Fields(l[1]); !slices.Equal(got, want) {
					t.Errorf("%s: %s %v, the Makefile %v", wf, name, got, want)
				}
			}
		}
	}

	// get-picache.sh: the names arch() prints are the binaries.
	gp := readRepoFile(t, "scripts/get-picache.sh")
	body := gp[strings.Index(gp, "\narch() {"):]
	body = body[:strings.Index(body, "\n}\n")]
	var names []string
	for _, m := range regexp.MustCompile(`(?m)^\s+[^\s)][^)]*\) echo (\S+) ;;$`).FindAllStringSubmatch(body, -1) {
		names = append(names, "picache-linux-"+m[1])
	}
	if !slices.Equal(sorted(names), binaries) {
		t.Errorf("get-picache.sh arch() prints %v, the Makefile's BINARIES %v", sorted(names), binaries)
	}
	// Its message names every build.
	for _, bt := range buildTargets {
		a, _ := AssetName(bt[0], bt[1])
		if !strings.Contains(body, strings.TrimPrefix(a, "picache-linux-")) {
			t.Errorf("get-picache.sh arch() does not mention %s", a)
		}
	}

	// build-deb.sh packages for each Debian architecture the binary whose
	// target DebianArch maps to it (armhf: the armv6 build).
	bd := readRepoFile(t, "scripts/build-deb.sh")
	for _, d := range debArches {
		var bin string
		for _, m := range regexp.MustCompile(`(?m)^([a-z0-9 |]+)\) bin=(\S+) ;;$`).FindAllStringSubmatch(bd, -1) {
			for _, alt := range strings.Split(m[1], "|") {
				if strings.TrimSpace(alt) == d {
					bin = strings.ReplaceAll(m[2], "$arch", d)
				}
			}
		}
		if bin == "" {
			t.Errorf("build-deb.sh has no binary for %s", d)
			continue
		}
		ok := false
		for _, bt := range buildTargets {
			a, _ := AssetName(bt[0], bt[1])
			if da, _ := DebianArch(bt[0], bt[1]); a == bin && da == d {
				ok = true
			}
		}
		if !ok {
			t.Errorf("build-deb.sh packages %s for %s, which is not a build of that Debian architecture", bin, d)
		}
	}
	if !strings.Contains(bd, "armhf) bin=picache-linux-armv6 ;;") {
		t.Error("armhf must package the armv6 build (it runs on ARMv6 and ARMv7)")
	}
}
