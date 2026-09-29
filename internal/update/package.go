package update

import (
	"bytes"
	"io"
	"net/url"
	"os"
	"runtime/debug"
	"strings"
)

// The release assets per architecture (docs/ARCHITECTURE.md 14.1) and the
// package mode (14.4): a Debian package is updated with apt, never by the
// update helper or `picache update`.

// PackageMarker exists when the Debian package installed PiCache
// (/usr/bin/picache). It counts only as a regular file (not a link) owned
// by root whose content is "deb" with an optional newline, so the service
// cannot switch the mode itself.
const PackageMarker = "/usr/lib/picache/packaged"

// AssetName returns the release binary for a GOARCH and, for arm, the
// GOARM of the running binary (BuildGOARM): amd64, arm64, 386 and riscv64
// → picache-linux-<goarch>; arm with GOARM 6 → picache-linux-armv6, 7 or
// none → picache-linux-armv7. The binary's own build setting decides, never
// the CPU, so an armv7 installation stays on armv7.
func AssetName(goarch, goarm string) (string, bool) {
	switch goarch {
	case "amd64", "arm64", "386", "riscv64":
		return "picache-linux-" + goarch, true
	case "arm":
		switch goarm {
		case "6":
			return "picache-linux-armv6", true
		case "7", "":
			return "picache-linux-armv7", true
		}
	}
	return "", false
}

// DebianArch returns the Debian architecture of the package for a GOARCH
// (and GOARM, as AssetName): amd64, arm64, armhf (arm: the GOARM=6 build,
// which runs on ARMv6 and ARMv7), i386 (386) and riscv64.
func DebianArch(goarch, goarm string) (string, bool) {
	switch goarch {
	case "amd64", "arm64", "riscv64":
		return goarch, true
	case "386":
		return "i386", true
	case "arm":
		if goarm == "6" || goarm == "7" || goarm == "" {
			return "armhf", true
		}
	}
	return "", false
}

// archLabel names an architecture in messages (arm with its GOARM).
func archLabel(goarch, goarm string) string {
	if goarch == "arm" && goarm != "" {
		return "arm (GOARM=" + goarm + ")"
	}
	return goarch
}

// BuildGOARM returns the GOARM this binary was built with (the digits
// before any ",", e.g. 6 of "6,softfloat"; "" for other architectures or
// without build information).
func BuildGOARM() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return goarmSetting(bi.Settings)
}

func goarmSetting(settings []debug.BuildSetting) string {
	for _, s := range settings {
		if s.Key == "GOARM" {
			v, _, _ := strings.Cut(s.Value, ",")
			return v
		}
	}
	return ""
}

// PackageFile is the name of the Debian package of a release: the tag
// without its v, then the Debian architecture
// (picache_0.16.0-rc.1_armhf.deb).
func PackageFile(version, debArch string) string {
	return "picache_" + strings.TrimPrefix(version, "v") + "_" + debArch + ".deb"
}

// PackageInstalled reports whether PiCache runs from the Debian package
// (PackageMarker).
func PackageInstalled() bool { return PackageInstalledAt(PackageMarker) }

// PackageInstalledAt checks the package marker at path: a regular file
// (never a link) owned by root whose content is "deb" or "deb\n".
func PackageInstalledAt(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || !ownedByRoot(fi) {
		return false
	}
	f, err := os.OpenFile(path, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 8))
	if err != nil {
		return false
	}
	return bytes.Equal(b, []byte("deb")) || bytes.Equal(b, []byte("deb\n"))
}

// NewPackageInfo describes the package of this host for GET /system/update
// (mode package): the Debian architecture and, with a latest release, its
// .deb and download URL.
func NewPackageInfo(debArch string, latest *Release) *PackageInfo {
	p := &PackageInfo{Format: "deb", Arch: debArch}
	if latest != nil {
		p.File = PackageFile(latest.Version, debArch)
		p.URL = releaseAssetURL(latest, p.File)
	}
	return p
}

// releaseAssetURL returns the download URL of a file of a release: next to
// its release page (…/releases/tag/<v> → …/releases/download/<v>/<name>).
func releaseAssetURL(rel *Release, name string) string {
	base, ok := strings.CutSuffix(rel.URL, "/releases/tag/"+rel.Version)
	if !ok {
		base = DefaultDownloadBase + "/" + Repository
	}
	return base + "/releases/download/" + url.PathEscape(rel.Version) + "/" + url.PathEscape(name)
}

// PackageUpdateHint is what `picache update` says in package mode: the
// .deb of version (a placeholder while none is known) for the Debian
// architecture of this binary.
func PackageUpdateHint(version, debArch string) string {
	file := "picache_<version>_" + debArch + ".deb"
	if version != "" {
		file = PackageFile(version, debArch)
	}
	return "PiCache was installed as a Debian package: download " + file + " from the release, verify it " +
		`(docs/DEPLOYMENT.md "Debian package") and install it with sudo apt install ./` + file
}
