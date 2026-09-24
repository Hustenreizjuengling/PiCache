package storage

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	cachestore "github.com/hustenreizjuengling/picache/internal/lancache/store"
)

// statfs(2) f_type magics (compared as uint32: the field is int32 on 32-bit targets).
const (
	magicCIFS  uint32 = 0xff534d42
	magicSMB2  uint32 = 0xfe534d42
	magicNFS   uint32 = 0x6969
	magicZFS   uint32 = 0x2fc12fc1 // not in x/sys
	magicExt4  uint32 = 0xef53     // ext2/3/4
	magicXFS   uint32 = 0x58465342
	magicBtrfs uint32 = 0x9123683e
	magicTmpfs uint32 = 0x01021994
	magicOvl   uint32 = 0x794c7630
	magicFuse  uint32 = 0x65735546
	magicVFAT  uint32 = 0x4d44
	magicExFAT uint32 = 0x2011bab0
	magicNTFS  uint32 = 0x5346544e
	magicF2FS  uint32 = 0xf2f52010
)

var fsNames = map[uint32]string{
	magicCIFS: "cifs", magicSMB2: "smb3", magicNFS: "nfs", magicZFS: "zfs", magicExt4: "ext4",
	magicXFS: "xfs", magicBtrfs: "btrfs", magicTmpfs: "tmpfs", magicOvl: "overlay", magicFuse: "fuse",
	magicVFAT: "vfat", magicExFAT: "exfat", magicNTFS: "ntfs", magicF2FS: "f2fs",
}

// fsTypeName names a statfs magic ("0x…" if unknown).
func fsTypeName(magic uint32) string {
	if n, ok := fsNames[magic]; ok {
		return n
	}
	return fmt.Sprintf("0x%x", magic)
}

func isNetworkFS(magic uint32) bool {
	return magic == magicCIFS || magic == magicSMB2 || magic == magicNFS
}

// fsInfo is the result of statfs.
type fsInfo struct {
	typ         uint32
	total, free uint64
}

// errUnsupported: the platform cannot report this (non-Linux development builds).
var errUnsupported = errors.New("not supported on this platform")

// probePrefix names the files of the write test.
const probePrefix = ".picache-probe-"

// probe is the mount guard: it checks a target from configuration to write
// test, stopping at the first problem. It runs in a check goroutine, never on
// a caller's path, and never writes unless the location is verified (exists,
// is a mount point when required, has the expected file system).
func (m *Manager) probe(t Target) checkResult {
	var res checkResult
	st := &res.st
	st.StoreRoot = storeRootPath(t)
	st.CheckedAt = time.Now().UTC()
	step := func(format string, a ...any) { res.steps = append(res.steps, fmt.Sprintf(format, a...)) }
	fail := func(reason, hint string) checkResult {
		st.Online, st.Reason, st.Hint = false, reason, hint
		step("Problem: %s", reason)
		return res
	}
	builtin := t.ID == LocalTargetID

	if err := ValidateTarget(t, m.cfg); err != nil {
		return fail("invalid storage configuration: "+errMessage(err), "Edit the storage target and save it again.")
	}
	step("Configuration is valid (%s, %s)", t.Kind, t.Mode)
	if !builtin {
		if err := noSymlinks(m.cfg.MountRoot, t.Path); err != nil {
			return fail(err.Error(), "Symbolic links are not allowed below the mount root; configure the real path.")
		}
	}
	fi, err := os.Lstat(t.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fail(t.Path+" does not exist", m.missingHint(t))
	case err != nil:
		return fail("cannot access "+t.Path+": "+errText(err), m.accessHint())
	case fi.Mode()&fs.ModeSymlink != 0 && !builtin:
		return fail(t.Path+" is a symbolic link", "Configure the real path.")
	}
	dir, err := os.OpenRoot(t.Path)
	if err != nil {
		return fail("cannot open "+t.Path+": "+errText(err), m.accessHint())
	}
	defer dir.Close()
	dfi, err := dir.Stat(".")
	if err != nil || !dfi.IsDir() {
		return fail(t.Path+" is not a directory", "")
	}
	step("%s exists", t.Path)

	// The handle pins what is mounted now: all later operations go through it.
	mounted, supported := mountState(t.Path, dfi)
	st.Mounted = mounted
	if t.RequireMountpoint {
		if !supported {
			return fail("cannot verify that "+t.Path+" is a mount point on this platform", "")
		}
		if !mounted {
			res.notMounted = true
			return fail("nothing is mounted at "+t.Path, m.notMountedHint(t))
		}
		step("%s is a mount point", t.Path)
	}

	info, err := statDir(dir)
	known := err == nil
	switch {
	case known:
		st.FSType, st.TotalBytes, st.FreeBytes = fsTypeName(info.typ), info.total, info.free
		step("File system %s, %s free of %s", st.FSType, formatBytes(info.free), formatBytes(info.total))
	case errors.Is(err, errUnsupported):
		step("File system type and free space are not available on this platform")
	default:
		return fail("cannot read file system information: "+errText(err), m.accessHint())
	}
	if reason := fsMismatch(t, info, known); reason != "" {
		return fail(reason, "Check what is mounted at "+t.Path+" and the storage kind.")
	}
	res.located = true

	root := dir
	if t.Subdir != "" {
		sub := filepath.FromSlash(t.Subdir)
		sfi, err := dir.Lstat(sub)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			res.rootMissing = true
			return fail("the store directory "+st.StoreRoot+" does not exist", "Initialise the store to create it.")
		case err != nil:
			return fail("cannot access "+st.StoreRoot+": "+errText(err), m.accessHint())
		case !sfi.IsDir():
			return fail(st.StoreRoot+" is not a directory", "")
		}
		if root, err = dir.OpenRoot(sub); err != nil {
			return fail("cannot open "+st.StoreRoot+": "+errText(err), m.accessHint())
		}
		defer root.Close()
	}
	if rfi, err := root.Stat("."); err == nil {
		m.deviceInfo(st, rfi, info, known)
	}

	mc := checkMarker(t, st, step)
	st.Initialised = mc.valid && mc.reason == ""
	probeDir := "."
	if mc.valid {
		probeDir = "tmp" // the store's own temp directory
	}
	lat, err := writeProbe(root, probeDir)
	if err != nil {
		return fail("write test failed: "+errText(err), m.writeHint(err))
	}
	res.writable, st.Writable = true, true
	st.LatencyMs = math.Round(float64(lat.Microseconds())/10) / 100
	step("Write, rename, read and delete test passed (%.2f ms)", st.LatencyMs)
	if mc.reason != "" {
		res.uninit = mc.uninit
		return fail(mc.reason, mc.hint)
	}
	st.Online = true
	step("Online")
	return res
}

// markerCheck is the result of comparing the store marker with the target.
type markerCheck struct {
	reason, hint string // why the store is not usable ("" = usable)
	valid        bool   // a valid marker exists
	uninit       bool   // no store recorded yet: initialise or adopt
}

func checkMarker(t Target, st *Status, step func(string, ...any)) markerCheck {
	mk, err := cachestore.ReadMarker(st.StoreRoot)
	switch {
	case errors.Is(err, cachestore.ErrNoMarker):
		step("No PiCache store marker found")
		if t.StoreID == "" {
			return markerCheck{reason: ErrNotInitialised.Message, uninit: true,
				hint: "Initialise the store (the directory must be empty) or adopt an existing one."}
		}
		return markerCheck{reason: "the store marker (.picache-store) is missing",
			hint: "Check that the right share is mounted. To start over with an empty store, initialise it again."}
	case err != nil:
		return markerCheck{reason: "invalid store marker: " + errText(err),
			hint: "Check the share; to start over, empty the directory and initialise it."}
	}
	st.StoreID = mk.StoreID
	switch t.StoreID {
	case mk.StoreID:
		step("Store marker found (store %s)", mk.StoreID)
		return markerCheck{valid: true}
	case "":
		step("An existing store was found (store %s)", mk.StoreID)
		return markerCheck{reason: ErrNotInitialised.Message, valid: true, uninit: true,
			hint: "An existing PiCache store (" + mk.StoreID + ") was found here: adopt it."}
	}
	step("A different store was found (store %s)", mk.StoreID)
	return markerCheck{reason: fmt.Sprintf("a different cache store (%s) was found (expected %s)", mk.StoreID, t.StoreID),
		hint: "Adopt it to use it with this target, or check what is mounted.", valid: true}
}

// deviceInfo fills SameFSAsData, Device and SDCard.
func (m *Manager) deviceInfo(st *Status, rfi os.FileInfo, info fsInfo, known bool) {
	if dfi, err := os.Stat(m.cfg.DataDir); err == nil {
		st.SameFSAsData, _ = sameDevice(rfi, dfi)
	}
	if known && isNetworkFS(info.typ) {
		return
	}
	st.Device = sanitizeDevice(blockDevice(rfi))
	st.SDCard = strings.HasPrefix(st.Device, "mmcblk")
}

// fsMismatch returns why the file system does not fit the kind ("" = fine).
func fsMismatch(t Target, info fsInfo, known bool) string {
	switch t.Kind {
	case KindSMB:
		if !known {
			return "cannot verify the file system type on this platform"
		}
		if info.typ != magicCIFS && info.typ != magicSMB2 {
			return fmt.Sprintf("expected an SMB/CIFS file system at %s, found %s", t.Path, fsTypeName(info.typ))
		}
	case KindNFS:
		if !known {
			return "cannot verify the file system type on this platform"
		}
		if info.typ != magicNFS {
			return fmt.Sprintf("expected an NFS file system at %s, found %s", t.Path, fsTypeName(info.typ))
		}
	case KindLocal:
		if t.ID != LocalTargetID && known && isNetworkFS(info.typ) {
			return fmt.Sprintf("%s is a network file system (%s); add it as an SMB or NFS target", t.Path, fsTypeName(info.typ))
		}
	}
	return ""
}

// writeProbe writes, renames, reads back and deletes a small file in dir
// (relative to r) and returns how long that took.
func writeProbe(r *os.Root, dir string) (time.Duration, error) {
	if dir != "." {
		if err := r.MkdirAll(dir, 0o750); err != nil {
			return 0, err
		}
	}
	var id [8]byte
	rand.Read(id[:])
	name := path.Join(dir, probePrefix+hex.EncodeToString(id[:]))
	data := make([]byte, 4096)
	rand.Read(data)
	start := time.Now()
	if err := r.WriteFile(name, data, 0o640); err != nil {
		_ = r.Remove(name)
		return 0, err
	}
	renamed := name + ".ok"
	if err := r.Rename(name, renamed); err != nil {
		_ = r.Remove(name)
		return 0, err
	}
	got, err := r.ReadFile(renamed)
	if err == nil && !bytes.Equal(got, data) {
		err = errors.New("read back different data")
	}
	if rmErr := r.Remove(renamed); err == nil {
		err = rmErr
	}
	if err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

// noSymlinks refuses symbolic links in the components of p below base.
func noSymlinks(base, p string) error {
	rel, err := filepath.Rel(base, p)
	if err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("%s is not below %s", p, base)
	}
	cur := base
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return nil // missing components are reported by the caller
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link", cur)
		}
	}
	return nil
}

func (m *Manager) missingHint(t Target) string {
	switch {
	case t.ID == LocalTargetID:
		return "Create PICACHE_CACHE_DIR (" + t.Path + ") and make it writable for PiCache."
	case t.Mode == ModeHostApply:
		return "Apply the mount (Apply button, or run: sudo picache storage apply " + t.ID + ")."
	case t.Kind == KindLocal:
		return "Create the directory or mount the disk there."
	}
	return "Mount the share at this path on the host (see the configuration snippets)."
}

func (m *Manager) notMountedHint(t Target) string {
	h := "PiCache refuses to write to the disk underneath the mount point and serves downloads uncached until it is mounted. "
	switch {
	case t.Mode == ModeHostApply:
		return h + "Apply the mount (Apply button, or run: sudo picache storage apply " + t.ID + ")."
	case t.Kind == KindLocal:
		return h + "Mount the disk there, or turn off \"require mount point\"."
	}
	return h + "Mount the share on the host (see the configuration snippets); in Docker bind the mount root with propagation rslave."
}

// appliedNotMountedHint replaces the hint when the root helper reported a
// successful mount that PiCache cannot see.
const appliedNotMountedHint = "The root helper reported this share as mounted, but PiCache does not see a mount here. " +
	"If the mount failed later (for example the NAS was offline at boot), apply it again. In a container whose / is not " +
	"a shared mount, PiCache only sees mounts made before it started: restart PiCache, and make / shared (docs/DEPLOYMENT.md, host-apply)."

func (m *Manager) accessHint() string {
	return fmt.Sprintf("PiCache runs as uid %d / gid %d; make the directory accessible for that user.", m.caps.UID, m.caps.GID)
}

// writeHint explains common write test failures.
func (m *Manager) writeHint(err error) string {
	switch {
	case errors.Is(err, syscall.EROFS):
		return "The file system is read-only for PiCache. Under systemd PiCache may only write to its own directories and below " +
			m.cfg.MountRoot + " (PICACHE_MOUNT_ROOT); in Docker, do not bind the directory read-only."
	case errors.Is(err, fs.ErrPermission):
		return fmt.Sprintf("PiCache runs as uid %d / gid %d and may not write here. For SMB mount with uid=%d,gid=%d; "+
			"for NFS give this user write access on the NAS (owner of the export, or all_squash with anonuid/anongid).",
			m.caps.UID, m.caps.GID, m.caps.UID, m.caps.GID)
	case errors.Is(err, syscall.ENOSPC):
		return "The file system is full."
	}
	return ""
}

// errText is the message of err without the wrapping path when it is a
// *fs.PathError (the path is already part of the surrounding message).
func errText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Op + ": " + pe.Err.Error()
	}
	return err.Error()
}

// formatBytes renders a size for step logs.
func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit && exp < 5; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// sanitizeDevice keeps a device name printable and short.
func sanitizeDevice(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if b.Len() >= 64 {
			break
		}
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == ':' {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// mountEntry is one line of /proc/self/mountinfo.
type mountEntry struct {
	majMin     string // st_dev of the mounted file system, "major:minor"
	mountPoint string
	fsType     string
	source     string
}

// maxMountinfo bounds how much of /proc/self/mountinfo is parsed.
const maxMountinfo = 16 << 20

// parseMountinfo parses proc(5) mountinfo lines:
// "36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue".
func parseMountinfo(r io.Reader) ([]mountEntry, error) {
	sc := bufio.NewScanner(io.LimitReader(r, maxMountinfo))
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	var out []mountEntry
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		sep := -1
		for i := 6; i < len(f); i++ {
			if f[i] == "-" {
				sep = i
				break
			}
		}
		if len(f) < 7 || sep < 0 || sep+2 >= len(f) {
			continue
		}
		out = append(out, mountEntry{
			majMin:     f[2],
			mountPoint: unescapeMountinfo(f[4]),
			fsType:     f[sep+1],
			source:     unescapeMountinfo(f[sep+2]),
		})
	}
	return out, sc.Err()
}

// unescapeMountinfo decodes the kernel's octal escapes (\040 space, \011 tab,
// \012 newline, \134 backslash).
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] >= '0' && s[i+1] <= '3' && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// fileExists reports whether a file (or directory) exists.
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
