package storage

import (
	"errors"
	"net/netip"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/config"
	cachestore "github.com/hustenreizjuengling/picache/internal/dlcache/store"
)

var (
	targetIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
	shareRE    = regexp.MustCompile(`^[A-Za-z0-9._$-]{1,80}$`)
	exportRE   = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,255}$`)
	accountRE  = regexp.MustCompile(`^[A-Za-z0-9._@-]{0,64}$`)
	segmentRE  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	// pathRE is the charset of every non-built-in path (after removing a
	// Windows volume name): it keeps paths safe to paste into fstab lines,
	// systemd units, YAML and shell snippets.
	pathRE = regexp.MustCompile(`^/[A-Za-z0-9._/-]*$`)
)

var (
	smbVersions = []string{"3.1.1", "3.0", "3"}    // first = default
	nfsVersions = []string{"4.2", "4.1", "4", "3"} // first = default
	netFSKinds  = []Kind{KindSMB, KindNFS}         // kinds that are network mounts
)

const (
	maxSubdirs  = 8    // path segments in Subdir
	maxNConnect = 16   // kernel NFS_MAX_CONNECTIONS
	defNConnect = 4    // default nconnect
	maxNameLen  = 64   // runes in Name
	maxPassword = 256  // bytes in a NAS password
	maxPathLen  = 1024 // bytes in a target path
)

// ValidTargetID reports whether id is "local" or 32 lower-case hex characters.
func ValidTargetID(id string) bool { return id == LocalTargetID || targetIDRE.MatchString(id) }

// ValidateTarget checks every field of t with strict allowlists (id, IP
// literal, share/export/username/domain charsets, versions, relative subdir,
// path below cfg.MountRoot and not inside cfg.DataDir).
func ValidateTarget(t Target, cfg *config.Config) error {
	if cfg == nil {
		return errors.New("storage: no configuration")
	}
	if !ValidTargetID(t.ID) {
		return apperr.Invalid("id", "must be %q or 32 lower-case hex characters", LocalTargetID)
	}
	if err := validateName(t.Name); err != nil {
		return err
	}
	if t.StoreID != "" && !cachestore.ValidStoreID(t.StoreID) {
		return apperr.Invalid("storeId", "must be 32 lower-case hex characters")
	}
	if t.ID == LocalTargetID {
		return validateBuiltin(t, cfg)
	}
	var err error
	switch t.Kind {
	case KindLocal:
		err = validateLocal(t)
	case KindSMB:
		err = validateSMB(t)
	case KindNFS:
		err = validateNFS(t)
	default:
		err = apperr.Invalid("kind", "must be local, smb or nfs")
	}
	if err != nil {
		return err
	}
	if err := validateSubdir(t.Subdir); err != nil {
		return err
	}
	switch t.Mode {
	case ModeExternal:
	case ModeHostApply:
		if !slices.Contains(netFSKinds, t.Kind) {
			return apperr.Invalid("mode", "host-apply is only available for SMB and NFS shares")
		}
		if want := hostApplyPath(cfg, t.ID); t.Path != want {
			return apperr.Invalid("path", "host-apply targets are always mounted at %s", want)
		}
	default:
		return apperr.Invalid("mode", "must be external or host-apply")
	}
	return validatePath(t.Path, cfg)
}

func validateName(name string) error {
	if name == "" || strings.TrimSpace(name) != name {
		return apperr.Invalid("name", "required, without leading or trailing spaces")
	}
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxNameLen {
		return apperr.Invalid("name", "at most %d characters", maxNameLen)
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return apperr.Invalid("name", "must not contain control characters")
		}
	}
	return nil
}

func validateBuiltin(t Target, cfg *config.Config) error {
	switch {
	case t.Kind != KindLocal || t.Mode != ModeExternal:
		return apperr.Invalid("kind", "the built-in target is a local directory")
	case t.Path != localPath(cfg):
		return apperr.Invalid("path", "the built-in target always uses PICACHE_CACHE_DIR (%s)", localPath(cfg))
	case t.Subdir != "" || t.RequireMountpoint:
		return apperr.Invalid("subdir", "the built-in target has no options")
	}
	return validateNoNetworkFields(t)
}

func validateLocal(t Target) error {
	if t.Mode != ModeExternal {
		return apperr.Invalid("mode", "local directories are always external")
	}
	return validateNoNetworkFields(t)
}

func validateNoNetworkFields(t Target) error {
	switch {
	case t.Server != "":
		return apperr.Invalid("server", "only used for SMB and NFS")
	case t.Share != "" || t.Username != "" || t.Domain != "" || t.HasPassword || t.SMBVersion != "" || t.SMBSeal:
		return apperr.Invalid("share", "only used for SMB")
	case t.Export != "" || t.NFSVersion != "" || t.NFSNConnect != 0:
		return apperr.Invalid("export", "only used for NFS")
	}
	return nil
}

func validateSMB(t Target) error {
	if err := validateServer(t.Server); err != nil {
		return err
	}
	switch {
	case !shareRE.MatchString(t.Share) || t.Share == "." || t.Share == "..":
		return apperr.Invalid("share", "1–80 characters: letters, digits and . _ $ -")
	case !accountRE.MatchString(t.Username):
		return apperr.Invalid("username", "at most 64 characters: letters, digits and . _ @ -")
	case !accountRE.MatchString(t.Domain):
		return apperr.Invalid("domain", "at most 64 characters: letters, digits and . _ @ -")
	case t.HasPassword && t.Username == "":
		return apperr.Invalid("password", "a password requires a username")
	case !slices.Contains(smbVersions, t.SMBVersion):
		return apperr.Invalid("smbVersion", "must be one of %s", strings.Join(smbVersions, ", "))
	case t.Export != "" || t.NFSVersion != "" || t.NFSNConnect != 0:
		return apperr.Invalid("export", "only used for NFS")
	case !t.RequireMountpoint:
		return apperr.Invalid("requireMountpoint", "network shares must always be mount points")
	}
	return nil
}

func validateNFS(t Target) error {
	if err := validateServer(t.Server); err != nil {
		return err
	}
	switch {
	case !exportRE.MatchString(t.Export) || strings.Contains(t.Export, ".."):
		return apperr.Invalid("export", "an absolute path of letters, digits and . _ - / (no ..)")
	case !slices.Contains(nfsVersions, t.NFSVersion):
		return apperr.Invalid("nfsVersion", "must be one of %s", strings.Join(nfsVersions, ", "))
	case t.NFSNConnect < 1 || t.NFSNConnect > maxNConnect:
		return apperr.Invalid("nfsNconnect", "must be between 1 and %d", maxNConnect)
	case t.Share != "" || t.Username != "" || t.Domain != "" || t.HasPassword || t.SMBVersion != "" || t.SMBSeal:
		return apperr.Invalid("share", "only used for SMB")
	case !t.RequireMountpoint:
		return apperr.Invalid("requireMountpoint", "network shares must always be mount points")
	}
	return nil
}

// validateServer accepts only canonical IP literals: the kernel does not
// resolve host names when mounting, and PiCache may itself be the DNS server.
func validateServer(s string) error {
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" || a.Is4In6() || a.String() != s ||
		a.IsUnspecified() || a.IsMulticast() || (a.Is6() && a.IsLinkLocalUnicast()) {
		return apperr.Invalid("server", "must be the IP address of the NAS (host names are not resolved when mounting)")
	}
	return nil
}

// validateSubdir accepts "" or a clean relative slash path.
func validateSubdir(sub string) error {
	if sub == "" {
		return nil
	}
	segs := strings.Split(sub, "/")
	if len(segs) > maxSubdirs || path.Clean(sub) != sub || !filepath.IsLocal(filepath.FromSlash(sub)) {
		return apperr.Invalid("subdir", "must be a relative path inside the share")
	}
	for _, s := range segs {
		if s == "." || s == ".." || !segmentRE.MatchString(s) {
			return apperr.Invalid("subdir", "may only contain letters, digits and . _ - /")
		}
	}
	return nil
}

// validatePath checks a non-built-in path: absolute, clean, safe charset,
// strictly below MountRoot and neither inside nor containing the data dir.
func validatePath(p string, cfg *config.Config) error {
	if p == "" {
		return apperr.Invalid("path", "required")
	}
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return apperr.Invalid("path", "must be an absolute, clean path")
	}
	if len(p) > maxPathLen || !pathRE.MatchString(filepath.ToSlash(strings.TrimPrefix(p, filepath.VolumeName(p)))) {
		return apperr.Invalid("path", "may only contain letters, digits and . _ - /")
	}
	root := cfg.MountRoot
	if !filepath.IsAbs(root) {
		return apperr.Invalid("path", "PICACHE_MOUNT_ROOT must be an absolute path")
	}
	root = filepath.Clean(root)
	if !within(root, p) {
		return apperr.Invalid("path", "must be below %s (PICACHE_MOUNT_ROOT)", root)
	}
	data := filepath.Clean(cfg.DataDir)
	if abs, err := filepath.Abs(data); err == nil {
		data = abs
	}
	if p == data || within(data, p) || within(p, data) {
		return apperr.Invalid("path", "must not be inside or contain the data directory (%s)", data)
	}
	return nil
}

// within reports whether p is strictly below parent (lexically).
func within(parent, p string) bool {
	rel, err := filepath.Rel(parent, p)
	return err == nil && rel != "." && filepath.IsLocal(rel)
}

// overlaps reports whether two store roots are the same or nested.
func overlaps(a, b string) bool { return a == b || within(a, b) || within(b, a) }

// validatePassword rejects passwords that could break the line-based
// credentials file (control characters) or are unreasonably long.
func validatePassword(pw string) error { return validatePasswordBytes([]byte(pw)) }

// validatePasswordBytes is validatePassword without copying into a string.
func validatePasswordBytes(pw []byte) error {
	if len(pw) > maxPassword || !utf8.Valid(pw) {
		return apperr.Invalid("password", "at most %d bytes of valid UTF-8", maxPassword)
	}
	for len(pw) > 0 {
		r, n := utf8.DecodeRune(pw)
		if unicode.IsControl(r) {
			return apperr.Invalid("password", "must not contain control characters or line breaks")
		}
		pw = pw[n:]
	}
	return nil
}
