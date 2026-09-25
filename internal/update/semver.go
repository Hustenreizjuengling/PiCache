package update

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// versionRE is the shape of every version PiCache installs: a SemVer 2.0
// version with a "v" prefix and without build metadata. The root helper
// accepts nothing else from a request.
var versionRE = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// describeTailRE is the suffix `git describe` adds after the last tag.
var describeTailRE = regexp.MustCompile(`-\d+-g[0-9a-f]{4,40}$`)

const maxVersionLen = 128

// Version is a release version: vMAJOR.MINOR.PATCH[-PRERELEASE].
type Version struct {
	Major, Minor, Patch uint64
	Pre                 []string // pre-release identifiers (empty for a stable release)
}

// ParseVersion parses a release version (SemVer 2.0 with a "v" prefix, no
// build metadata; numbers without leading zeros).
func ParseVersion(s string) (Version, error) {
	var v Version
	if len(s) > maxVersionLen || !versionRE.MatchString(s) {
		return v, fmt.Errorf("invalid version %q (expected vX.Y.Z or vX.Y.Z-pre)", truncate(s, 40))
	}
	core, pre, hasPre := strings.Cut(s[1:], "-")
	nums := strings.Split(core, ".")
	for i, dst := range []*uint64{&v.Major, &v.Minor, &v.Patch} {
		if len(nums[i]) > 1 && nums[i][0] == '0' {
			return Version{}, fmt.Errorf("invalid version %q: leading zero", s)
		}
		n, err := strconv.ParseUint(nums[i], 10, 64)
		if err != nil {
			return Version{}, fmt.Errorf("invalid version %q: %w", s, err)
		}
		*dst = n
	}
	if hasPre {
		v.Pre = strings.Split(pre, ".")
		for _, id := range v.Pre {
			if id == "" {
				return Version{}, fmt.Errorf("invalid version %q: empty pre-release identifier", s)
			}
			if isNumeric(id) && len(id) > 1 && id[0] == '0' {
				return Version{}, fmt.Errorf("invalid version %q: leading zero in %q", s, id)
			}
		}
	}
	return v, nil
}

// String returns the canonical form with the "v" prefix.
func (v Version) String() string {
	s := fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	return s
}

// Prerelease reports whether v has pre-release identifiers.
func (v Version) Prerelease() bool { return len(v.Pre) > 0 }

// Compare returns -1, 0 or +1 by SemVer precedence.
func Compare(a, b Version) int {
	for _, p := range [][2]uint64{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		switch {
		case p[0] < p[1]:
			return -1
		case p[0] > p[1]:
			return 1
		}
	}
	switch {
	case len(a.Pre) == 0 && len(b.Pre) == 0:
		return 0
	case len(a.Pre) == 0: // a release is greater than its pre-releases
		return 1
	case len(b.Pre) == 0:
		return -1
	}
	for i := 0; i < len(a.Pre) && i < len(b.Pre); i++ {
		if c := compareIdent(a.Pre[i], b.Pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.Pre) < len(b.Pre):
		return -1
	case len(a.Pre) > len(b.Pre):
		return 1
	}
	return 0
}

// compareIdent compares two pre-release identifiers: numeric ones
// numerically (by length first: no leading zeros, any size), numeric below
// alphanumeric, alphanumeric ones in ASCII order.
func compareIdent(a, b string) int {
	an, bn := isNumeric(a), isNumeric(b)
	switch {
	case an && bn:
		if len(a) != len(b) {
			if len(a) < len(b) {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	case an:
		return -1
	case bn:
		return 1
	}
	return strings.Compare(a, b)
}

func isNumeric(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// Running describes the version of a binary (version.Version).
type Running struct {
	Raw string
	// Dev: a development build ("dev", a commit hash or a `git describe`
	// string); release builds report a plain release version.
	Dev bool
	// Base: the release version, or the tag a `git describe` build is based
	// on; nil for development builds without one.
	Base *Version
}

// ParseRunning classifies a version string. `git describe --tags --always
// --dirty` yields v1.2.3 (a release build), v1.2.3-4-gabc1234[-dirty] and
// v1.2.3-dirty (development builds based on v1.2.3), or a commit hash.
func ParseRunning(s string) Running {
	r := Running{Raw: s, Dev: true}
	base, dirty := strings.CutSuffix(s, "-dirty")
	described := false
	if loc := describeTailRE.FindStringIndex(base); loc != nil {
		base, described = base[:loc[0]], true
	}
	if v, err := ParseVersion(base); err == nil {
		r.Base = &v
		r.Dev = dirty || described
	}
	return r
}

// Accepts reports whether installing v is an update: v is greater than the
// release version (or the base of a development build, which counts as
// newer than its base). A development build without a base accepts any
// release.
func (r Running) Accepts(v Version) bool {
	return r.Base == nil || Compare(v, *r.Base) > 0
}

// errNotNewer refuses a downgrade or a reinstall of the running version.
var errNotNewer = errors.New("not newer than the installed version")

// checkNewer returns an error unless v is an update for current (or
// allowDowngrade is set).
func checkNewer(current string, v Version, allowDowngrade bool) error {
	r := ParseRunning(current)
	if allowDowngrade || r.Accepts(v) {
		return nil
	}
	return fmt.Errorf("%s is %w %s (use --allow-downgrade to install it anyway)", v, errNotNewer, current)
}

// truncate shortens s to at most n bytes for messages.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
