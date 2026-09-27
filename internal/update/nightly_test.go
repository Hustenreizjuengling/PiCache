package update

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// nightlyTestKey signs the fake nightly builds; useNightlyKey makes it the
// only nightly key.
var nightlyTestKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))

func useNightlyKey(t *testing.T) {
	t.Helper()
	old := nightlyKeys
	nightlyKeys = []string{base64.StdEncoding.EncodeToString(nightlyTestKey.Public().(ed25519.PublicKey))}
	t.Cleanup(func() { nightlyKeys = old })
}

// nightlyFiles is releaseFiles signed with the nightly key.
func nightlyFiles(bin []byte) map[string][]byte {
	files := releaseFiles(bin)
	files[SigFile] = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(nightlyTestKey, files[SumsFile])) + "\n")
	return files
}

// Nightly builds sort above everything published before them and below
// the next release (candidate) of their core.
func TestNightlyOrdering(t *testing.T) {
	order := []string{
		"v0.15.0", "v0.15.1-rc.1", "v0.15.1-rc.2", "v0.15.1", "v0.15.2-nightly.20261001.1", "v0.15.2-nightly.20261001.2",
		"v0.15.2-nightly.20261001.10", "v0.15.2-nightly.20261002.1", "v0.15.2-rc.1", "v0.15.2", "v0.16.1-nightly.20261101.1",
	}
	for i := range order {
		for j := range order {
			a, b := mustVersion(t, order[i]), mustVersion(t, order[j])
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got := Compare(a, b); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
}

func TestChannels(t *testing.T) {
	for _, tc := range []struct {
		version               string
		stable, beta, nightly bool
	}{
		{"v0.15.0", true, true, true},
		{"v0.15.1-rc.1", false, true, true},
		{"v0.15.2-nightly.20261001.1", false, false, true},
	} {
		v := mustVersion(t, tc.version)
		if Offered(ChannelStable, v) != tc.stable || Offered(ChannelBeta, v) != tc.beta || Offered(ChannelNightly, v) != tc.nightly {
			t.Errorf("%s: %v %v %v", tc.version, Offered(ChannelStable, v), Offered(ChannelBeta, v), Offered(ChannelNightly, v))
		}
	}
	g := newFakeGitHub(t)
	for _, v := range []string{"v0.15.0", "v0.15.1-rc.1", "v0.15.2-nightly.20261001.1"} {
		g.addRelease(v, releaseFiles(fakeBinary(v)))
	}
	for ch, want := range map[string]string{ChannelStable: "v0.15.0", ChannelBeta: "v0.15.1-rc.1", ChannelNightly: "v0.15.2-nightly.20261001.1"} {
		if rel, err := g.client().Latest(t.Context(), ch); err != nil || rel == nil || rel.Version != want {
			t.Errorf("channel %s: %+v %v", ch, rel, err)
		}
	}
	// The overview offers only what the channel offers.
	nightly := &Release{Version: "v0.15.2-nightly.20261001.1", Prerelease: true}
	if o := NewOverview("v0.15.0", ModeHelper, true, ChannelBeta, CheckResult{Latest: nightly}, nil); o.Latest != nil || o.UpdateAvailable {
		t.Fatalf("a nightly offered on beta: %+v", o)
	}
	if o := NewOverview("v0.15.0", ModeHelper, true, ChannelNightly, CheckResult{Latest: nightly}, nil); !o.UpdateAvailable || o.Channel != ChannelNightly || !o.IncludePrereleases {
		t.Fatalf("nightly: %+v", o)
	}
}

// A nightly version verifies only against the nightly keys, every other
// version only against the release keys.
func TestKeySelection(t *testing.T) {
	useTestKey(t)
	useNightlyKey(t)
	if keysFor("v0.15.2-nightly.20261001.1")[0] != nightlyKeys[0] || keysFor("v0.15.1-rc.1")[0] != trustedKeys[0] {
		t.Fatal("keysFor")
	}
	for _, k := range trustedKeys {
		for _, n := range nightlyKeys {
			if k == n {
				t.Fatal("the release list contains a nightly key")
			}
		}
	}
	nv := "v0.15.2-nightly.20261001.1"
	for _, tc := range []struct {
		name, version string
		files         map[string][]byte
		ok            bool
	}{
		{"nightly with the nightly key", nv, nightlyFiles(fakeBinary(nv)), true},
		{"nightly with the release key", nv, releaseFiles(fakeBinary(nv)), false},
		{"release with the nightly key", "v0.15.1", nightlyFiles(fakeBinary("v0.15.1")), false},
		{"release with the release key", "v0.15.1", releaseFiles(fakeBinary("v0.15.1")), true},
	} {
		g := newFakeGitHub(t)
		g.addRelease(tc.version, tc.files)
		e := newInstallEnv(t, "v0.15.0")
		res, err := Apply(t.Context(), e.options("v0.15.0", tc.version, g.client().Files(tc.version)))
		if (err == nil) != tc.ok {
			t.Errorf("%s: %+v %v", tc.name, res, err)
		}
		// From a directory without a version: the key must match the
		// version the binary reports.
		dir := t.TempDir()
		for name, b := range tc.files {
			writeFile(t, filepath.Join(dir, name), b)
		}
		e = newInstallEnv(t, "v0.15.0")
		if res, err := Apply(t.Context(), e.options("v0.15.0", "", DirFiles(dir))); (err == nil) != tc.ok {
			t.Errorf("%s from a directory: %+v %v", tc.name, res, err)
		}
	}
}

// The root helper installs a nightly build only while the marker is a
// root-owned regular file.
func TestApplyPendingNightlyMarker(t *testing.T) {
	useTestKey(t)
	useNightlyKey(t)
	nv := "v0.15.2-nightly.20261001.1"
	g := newFakeGitHub(t)
	g.addRelease(nv, nightlyFiles(fakeBinary(nv)))
	marker := filepath.Join(t.TempDir(), "nightly.enabled")
	old := ownedByRoot
	root := true
	ownedByRoot = func(os.FileInfo) bool { return root }
	t.Cleanup(func() { ownedByRoot = old })

	run := func() (Status, error) {
		e := newInstallEnv(t, "v0.15.0")
		if err := QueueRequest(e.dataDir, Request{Version: nv, RequestedAt: time.Now().UTC(), RequestedBy: "admin"}); err != nil {
			t.Fatal(err)
		}
		o := e.helperOptions("v0.15.0")
		o.NightlyMarker = marker
		err := ApplyPending(t.Context(), g.client(), o)
		return readStatusFile(t, e.dataDir), err
	}
	st, err := run()
	if err == nil || st.State != StateFailed || st.Message != "nightly builds are not enabled on this host (install.sh --nightly)" {
		t.Fatalf("without the marker: %+v %v", st, err)
	}
	writeFile(t, marker, nil)
	root = false
	if st, err := run(); err == nil || st.State != StateFailed {
		t.Fatalf("a marker not owned by root: %+v %v", st, err)
	}
	root = true
	if st, err := run(); err != nil || st.State != StateSucceeded {
		t.Fatalf("with the marker: %+v %v", st, err)
	}
	os.Remove(marker)
	if err := os.Symlink(t.TempDir(), marker); err == nil {
		if st, err := run(); err == nil || st.State != StateFailed {
			t.Fatalf("a link as the marker: %+v %v", st, err)
		}
	}
}

func TestParseUpdateProxy(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"http://proxy.lan:3128", "http://proxy.lan:3128"},
		{"SOCKS5://10.0.0.1:1080/", "socks5://10.0.0.1:1080"},
		{"socks5://[fd00::1]:1080", "socks5://[fd00::1]:1080"},
	} {
		p, err := ParseUpdateProxy(tc.in)
		got := ""
		if p != nil {
			got = p.Origin()
		}
		if err != nil || got != tc.want {
			t.Errorf("%q: %q %v", tc.in, got, err)
		}
	}
	for _, bad := range []string{"https://proxy.lan:3128", "http://u:p@proxy.lan:3128", "http://proxy.lan", "http://proxy.lan:3128/x",
		"socks5h://proxy.lan:1080", "http://proxy.lan:0", "proxy.lan:3128", "http://proxy.lan:3128?x=1"} {
		if _, err := ParseUpdateProxy(bad); err == nil || !strings.Contains(err.Error(), "PICACHE_UPDATE_PROXY") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
