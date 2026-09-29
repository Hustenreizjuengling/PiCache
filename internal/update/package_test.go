package update

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestBuildGOARM(t *testing.T) {
	for _, tc := range []struct {
		settings []debug.BuildSetting
		want     string
	}{
		{nil, ""},
		{[]debug.BuildSetting{{Key: "GOARCH", Value: "amd64"}, {Key: "GOAMD64", Value: "v1"}}, ""},
		{[]debug.BuildSetting{{Key: "GOARCH", Value: "arm"}, {Key: "GOARM", Value: "7"}}, "7"},
		{[]debug.BuildSetting{{Key: "GOARM", Value: "6"}}, "6"},
		{[]debug.BuildSetting{{Key: "GOARM", Value: "6,softfloat"}}, "6"},
		{[]debug.BuildSetting{{Key: "GOARM", Value: "7,hardfloat"}}, "7"},
	} {
		if got := goarmSetting(tc.settings); got != tc.want {
			t.Errorf("goarmSetting(%v) = %q, want %q", tc.settings, got, tc.want)
		}
	}
	// The test binary itself: no GOARM outside arm.
	if bi, ok := debug.ReadBuildInfo(); ok {
		arm := false
		for _, s := range bi.Settings {
			if s.Key == "GOARCH" && s.Value == "arm" {
				arm = true
			}
		}
		if got := BuildGOARM(); !arm && got != "" {
			t.Errorf("BuildGOARM() = %q on a non-arm build", got)
		}
	}
}

// The package marker counts only as a regular file owned by root whose
// content is "deb" (plus a newline).
func TestPackageMarker(t *testing.T) {
	root := true
	old := ownedByRoot
	ownedByRoot = func(os.FileInfo) bool { return root }
	t.Cleanup(func() { ownedByRoot = old })
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		content string
		want    bool
	}{
		{"deb\n", true},
		{"deb", true},
		{"deb\n\n", false},
		{"rpm\n", false},
		{"", false},
		{"deb \n", false},
		{"DEB\n", false},
		{strings.Repeat("deb\n", 100), false},
	} {
		if got := PackageInstalledAt(write("m", tc.content)); got != tc.want {
			t.Errorf("content %q: %v, want %v", tc.content, got, tc.want)
		}
	}
	marker := write("marker", "deb\n")
	root = false
	if PackageInstalledAt(marker) {
		t.Error("a marker of another owner counts")
	}
	root = true
	if PackageInstalledAt(filepath.Join(dir, "missing")) {
		t.Error("a missing marker counts")
	}
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if PackageInstalledAt(filepath.Join(dir, "d")) {
		t.Error("a directory counts")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(marker, link); err == nil && PackageInstalledAt(link) {
		t.Error("a symbolic link counts")
	}
	if !PackageInstalledAt(marker) {
		t.Error("the marker does not count")
	}
	if PackageMarker != "/usr/lib/picache/packaged" {
		t.Errorf("PackageMarker %q", PackageMarker)
	}
}

// In package mode a release needs its .deb for this architecture as well;
// Release (the root helper, --version) likewise.
func TestLatestPackageMode(t *testing.T) {
	g := newFakeGitHub(t)
	g.releases = []ghRel{
		{TagName: "v0.17.0", Assets: stdAssets()}, // no .deb
		{TagName: "v0.16.1", Assets: append(stdAssets(), ghAsset{Name: "picache_0.16.1_amd64.deb"}, ghAsset{Name: "picache_0.16.1_armhf.deb"})},
		{TagName: "v0.16.2-rc.1", Prerelease: true, Assets: append(stdAssets(), ghAsset{Name: "picache_0.16.2-rc.1_amd64.deb"})},
		{TagName: "v0.16.0", Assets: append(stdAssets(), ghAsset{Name: "picache_v0.16.0_amd64.deb"})}, // wrong name
	}
	c := g.client()
	if rel, err := c.Latest(t.Context(), ChannelStable); err != nil || rel == nil || rel.Version != "v0.17.0" {
		t.Fatalf("helper mode: %+v %v", rel, err)
	}
	c.Package = true
	if rel, err := c.Latest(t.Context(), ChannelStable); err != nil || rel == nil || rel.Version != "v0.16.1" {
		t.Fatalf("package mode: %+v %v", rel, err)
	}
	if rel, err := c.Latest(t.Context(), ChannelBeta); err != nil || rel == nil || rel.Version != "v0.16.2-rc.1" {
		t.Fatalf("package mode, beta: %+v %v", rel, err)
	}
	c.Arch, c.GOARM = "arm", "7" // armhf
	if rel, err := c.Latest(t.Context(), ChannelBeta); err != nil || rel == nil || rel.Version != "v0.16.1" {
		t.Fatalf("package mode, armhf: %+v %v", rel, err)
	}
	c.Arch, c.GOARM = "arm64", ""
	if rel, err := c.Latest(t.Context(), ChannelBeta); err != nil || rel != nil {
		t.Fatalf("package mode, no arm64 .deb: %+v %v", rel, err)
	}
	c.Arch = "amd64"
	if _, err := c.Release(t.Context(), "v0.17.0"); err == nil || !strings.Contains(err.Error(), "release v0.17.0 has no picache_0.17.0_amd64.deb") {
		t.Fatalf("Release without the .deb: %v", err)
	}
}

func TestPackageInfo(t *testing.T) {
	if f := PackageFile("v0.16.0-rc.1", "armhf"); f != "picache_0.16.0-rc.1_armhf.deb" {
		t.Fatalf("PackageFile %q", f)
	}
	rel := &Release{Version: "v0.16.1", URL: "https://github.com/" + Repository + "/releases/tag/v0.16.1"}
	p := NewPackageInfo("amd64", rel)
	b, err := json.Marshal(p)
	want := `{"format":"deb","arch":"amd64","file":"picache_0.16.1_amd64.deb","url":"https://github.com/Hustenreizjuengling/PiCache/releases/download/v0.16.1/picache_0.16.1_amd64.deb"}`
	if err != nil || string(b) != want {
		t.Fatalf("PackageInfo %s %v", b, err)
	}
	b, _ = json.Marshal(NewPackageInfo("armhf", nil))
	if string(b) != `{"format":"deb","arch":"armhf"}` {
		t.Fatalf("without latest: %s", b)
	}
	// A release page of another base (tests, mirrors) keeps its base.
	p = NewPackageInfo("i386", &Release{Version: "v1.0.0", URL: "http://127.0.0.1:9/" + Repository + "/releases/tag/v1.0.0"})
	if p.URL != "http://127.0.0.1:9/"+Repository+"/releases/download/v1.0.0/picache_1.0.0_i386.deb" {
		t.Fatalf("URL %s", p.URL)
	}
	if h := PackageUpdateHint("", "arm64"); h != `PiCache was installed as a Debian package: download picache_<version>_arm64.deb from the release, `+
		`verify it (docs/DEPLOYMENT.md "Debian package") and install it with sudo apt install ./picache_<version>_arm64.deb` {
		t.Fatalf("hint %q", h)
	}
	if h := PackageUpdateHint("v0.16.1", "amd64"); !strings.HasSuffix(h, "sudo apt install ./picache_0.16.1_amd64.deb") {
		t.Fatalf("hint %q", h)
	}
	o := NewOverview("v0.16.0", ModePackage, true, ChannelStable, CheckResult{Latest: rel}, nil)
	if o.Mode != "package" || !o.UpdateAvailable || o.Package != nil {
		t.Fatalf("NewOverview leaves Package to the caller: %+v", o)
	}
}

// Apply selects the binary by the injected GOARCH and GOARM, like the
// client.
func TestApplyAssetByGOARM(t *testing.T) {
	host := Host{Systemctl: func(context.Context, ...string) error { return nil }, Health: func(context.Context) error { return nil }}
	for _, tc := range []struct{ arch, goarm, want string }{
		{"arm", "6", "picache-linux-armv6"},
		{"arm", "7", "picache-linux-armv7"},
		{"386", "", "picache-linux-386"},
		{"riscv64", "", "picache-linux-riscv64"},
	} {
		a, err := newApplier(Options{BinPath: "/usr/bin/picache", DataDir: t.TempDir(), Files: DirFiles(t.TempDir()), Host: host,
			Arch: tc.arch, GOARM: tc.goarm})
		if err != nil || a.asset != tc.want {
			t.Errorf("%s/%s: %v %v", tc.arch, tc.goarm, a, err)
		}
	}
	_, err := newApplier(Options{BinPath: "/usr/bin/picache", DataDir: t.TempDir(), Files: DirFiles(t.TempDir()), Host: host,
		Arch: "arm", GOARM: "5"})
	if err == nil || !strings.Contains(err.Error(), "linux/arm (GOARM=5)") {
		t.Errorf("GOARM=5: %v", err)
	}
}

// bodyTransport answers every request with one body (fuzzing the release
// list without a server).
type bodyTransport []byte

func (b bodyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(b)), Header: http.Header{}, Request: r}, nil
}

// The release list is GitHub's answer (untrusted): any body gives an error
// or an eligible release (a valid version with the binary, the sums, the
// signature and in package mode the .deb of this architecture), never a
// panic.
func FuzzLatestReleaseList(f *testing.F) {
	f.Add([]byte(`[{"tag_name":"v0.16.1","assets":[{"name":"picache-linux-amd64"},{"name":"SHA256SUMS"},{"name":"SHA256SUMS.sig"},{"name":"picache_0.16.1_amd64.deb"}]}]`), true)
	f.Add([]byte(`[{"tag_name":"v0.16.1-rc.1","prerelease":true,"assets":[{"name":"picache-linux-amd64"},{"name":"SHA256SUMS"},{"name":"SHA256SUMS.sig"}]}]`), false)
	f.Add([]byte(`[{"tag_name":"v1.0.0-nightly.20261001.1","draft":false,"assets":[]}]`), false)
	f.Add([]byte(`{}`), true)
	f.Add([]byte(`[{"tag_name":"v99999999999999999999.0.0","assets":[{"name":"picache_99999999999999999999.0.0_amd64.deb"}]}]`), true)
	f.Fuzz(func(t *testing.T, body []byte, pkg bool) {
		c := &Client{HTTP: &http.Client{Transport: bodyTransport(body)}, APIBase: "https://api.invalid", Arch: "amd64", GOARM: "", Package: pkg}
		for _, ch := range []string{ChannelStable, ChannelBeta, ChannelNightly} {
			rel, err := c.Latest(context.Background(), ch)
			if err != nil || rel == nil {
				continue
			}
			v, err := ParseVersion(rel.Version)
			if err != nil || !Offered(ch, v) {
				t.Fatalf("offered %q on %s: %v", rel.Version, ch, err)
			}
			if !bytes.Contains(body, []byte(`"`+assetAmd64()+`"`)) {
				t.Fatalf("release %s without the binary", rel.Version)
			}
			if pkg && !bytes.Contains(body, []byte(`"`+PackageFile(rel.Version, "amd64")+`"`)) {
				t.Fatalf("release %s without the package", rel.Version)
			}
		}
	})
}

// assetAmd64 is the amd64 binary (the fuzz test's architecture).
func assetAmd64() string {
	a, _ := AssetName("amd64", "")
	return a
}
