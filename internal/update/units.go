package update

// Unit files (docs/ARCHITECTURE.md 14.4): an update also installs the
// systemd units of the release, where the helper is allowed to write them.
// They come only from the signed release archive (picache-deploy.tar.gz,
// checked against the verified SHA256SUMS and parsed in memory), only the
// allow-listed names, only over existing regular files in the unit
// directory, never drop-ins. Replacing them is never fatal: on any error the
// units replaced in this run are put back and the update goes on with the
// binary alone (every binary starts with the previous release's units).

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
)

// DeployArchive is the release asset with deploy/ (install.sh and the units).
const DeployArchive = "picache-deploy.tar.gz"

// DefaultUnitDir is where install.sh installs the unit files.
const DefaultUnitDir = "/usr/local/lib/systemd/system"

// Bounds of the deploy archive.
const (
	maxDeployArchive  = 16 << 20 // compressed, held in memory
	maxDeployUnpacked = 64 << 20 // everything tar reads, headers included
	maxDeployEntries  = 4096
	maxUnitFile       = 64 << 10
	maxInstalledUnit  = 1 << 20 // an installed unit read for the comparison and its .prev
)

// unitNames are the units an update may replace (deploy/systemd/<name>).
var unitNames = []string{"picache.service", "picache-update.service", "picache-update.path", "picache-storage.service",
	"picache-storage.path", "picache-shared-mounts.service"}

// errNoUnits: the unit step does not run in this update.
var errNoUnits = errors.New("no unit files in this update")

// deployUnits downloads the deploy archive into memory, checks it against
// SHA256SUMS and returns the allow-listed unit files it holds. It returns
// errNoUnits when SHA256SUMS does not list the archive, or when a local
// directory (--from) does not hold it; any other failure aborts the update.
func (a *applier) deployUnits(ctx context.Context, sums map[string][32]byte) (map[string][]byte, error) {
	want, ok := sums[DeployArchive]
	if !ok {
		a.log.Info("the release lists no " + DeployArchive + ": the unit files are not updated")
		return nil, errNoUnits
	}
	a.progress(StepDownload, "downloading "+DeployArchive+" (the unit files)")
	b, err := readLimited(ctx, a.o.Files, DeployArchive, maxDeployArchive)
	if errors.Is(err, fs.ErrNotExist) {
		a.log.Info(DeployArchive + " is not in " + a.o.Files.Describe() + ": the unit files are not updated")
		return nil, errNoUnits
	}
	if err != nil {
		return nil, err
	}
	a.progress(StepVerify, "checking the SHA-256 of "+DeployArchive)
	if sha256.Sum256(b) != want {
		return nil, fmt.Errorf("%s does not match %s (the download is corrupt or was tampered with)", DeployArchive, SumsFile)
	}
	units, err := parseDeployArchive(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", DeployArchive, err)
	}
	return units, nil
}

// countReader counts what is read through it and refuses more than max.
type countReader struct {
	r   io.Reader
	n   int64
	max int64
}

var errBomb = errors.New("unpacks to more than 64 MiB")

func (c *countReader) Read(p []byte) (int, error) {
	if c.n >= c.max {
		return 0, errBomb
	}
	if int64(len(p)) > c.max-c.n {
		p = p[:c.max-c.n]
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// parseDeployArchive reads the gzip-compressed tar archive b (at most 64
// MiB unpacked and 4096 entries in total) and returns the files
// deploy/systemd/<name> of the allow-list: regular files (typeflag '0' or
// NUL) of at most 64 KiB, each at most once; any other form of an
// allow-listed name is an error, other entries are skipped.
func parseDeployArchive(b []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(&countReader{r: gz, max: maxDeployUnpacked})
	units := map[string][]byte{}
	for entries := 0; ; entries++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if entries >= maxDeployEntries {
			return nil, fmt.Errorf("holds more than %d entries", maxDeployEntries)
		}
		dir, name := path.Split(path.Clean(hdr.Name))
		if dir != "deploy/systemd/" || !slices.Contains(unitNames, name) {
			continue
		}
		switch {
		case hdr.Typeflag != tar.TypeReg && hdr.Typeflag != '\x00':
			return nil, fmt.Errorf("deploy/systemd/%s is not a regular file", name)
		case units[name] != nil:
			return nil, fmt.Errorf("holds deploy/systemd/%s twice", name)
		case hdr.Size > maxUnitFile:
			return nil, fmt.Errorf("deploy/systemd/%s is larger than %d KiB", name, maxUnitFile>>10)
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxUnitFile+1))
		if err != nil {
			return nil, err
		}
		if len(data) > maxUnitFile {
			return nil, fmt.Errorf("deploy/systemd/%s is larger than %d KiB", name, maxUnitFile>>10)
		}
		units[name] = data
	}
	return units, nil
}

// unitStepReady reports why the unit files are not replaced ("" when they
// are): not a systemd host, picache.service loaded from another file (for
// example a copy in /etc/systemd/system), or, in a root run, a unit
// directory someone other than root could change.
func (a *applier) unitStepReady(ctx context.Context) string {
	dir := a.h.UnitDir
	if dir == "" {
		return "not a systemd host"
	}
	if a.h.UnitFragment != nil {
		frag, err := a.h.UnitFragment(ctx)
		if err != nil {
			return "the unit file of " + Service + " is not known: " + err.Error()
		}
		if frag != filepath.Join(dir, Service) {
			return Service + " is loaded from " + clip(frag, 120) + ", not from " + dir
		}
	}
	if a.h.Strict {
		if err := checkRootOwnedChain(dir); err != nil {
			return err.Error()
		}
	}
	return ""
}

// writeUnit writes one file of the unit directory atomically (a variable
// for tests).
var writeUnit = func(r *os.Root, name string, data []byte, strict bool) error {
	uid, gid := -1, -1
	if strict {
		uid, gid = 0, 0
	}
	return writeFileAtomic(r, name, data, 0o644, uid, gid)
}

// installUnits replaces the unit files of the archive that differ from the
// installed ones (existing regular files only; the current one is kept as
// <name>.prev), then runs daemon-reload. On any failure the units replaced
// so far are put back and the reason is kept for the final message.
func (a *applier) installUnits(ctx context.Context) {
	if len(a.units) == 0 {
		return
	}
	dir := a.h.UnitDir
	a.progress(StepInstall, "installing the unit files of "+a.version+" in "+dir)
	r, err := os.OpenRoot(dir)
	if err != nil {
		a.unitsFailed(ctx, nil, err)
		return
	}
	defer r.Close()
	for _, name := range unitNames {
		data, ok := a.units[name]
		if !ok {
			continue
		}
		fi, err := r.Lstat(name)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue // not installed (for example no host-apply helper)
		case err != nil:
			a.unitsFailed(ctx, r, err)
			return
		case !fi.Mode().IsRegular():
			a.log.Warn("not replacing a unit that is not a regular file", slog.String("unit", filepath.Join(dir, name)))
			continue
		}
		cur, err := readSmallFile(r, name, maxInstalledUnit)
		if err != nil {
			a.unitsFailed(ctx, r, err)
			return
		}
		if bytes.Equal(cur, data) {
			continue
		}
		if err := writeUnit(r, name+".prev", cur, a.h.Strict); err != nil {
			a.unitsFailed(ctx, r, err)
			return
		}
		a.replaced = append(a.replaced, name)
		if err := writeUnit(r, name, data, a.h.Strict); err != nil {
			a.unitsFailed(ctx, r, err)
			return
		}
	}
	if len(a.replaced) == 0 {
		return
	}
	syncDir(dir)
	if err := a.h.Systemctl(ctx, "daemon-reload"); err != nil {
		a.unitsFailed(ctx, r, err)
		return
	}
	a.log.Info("unit files updated", slog.Any("units", a.replaced))
}

// unitsFailed puts back the units replaced in this run and remembers why
// the unit files were not updated.
func (a *applier) unitsFailed(ctx context.Context, r *os.Root, cause error) {
	a.unitNote = sanitizeMessage(cause.Error())
	a.log.Error("the unit files were not updated; continuing with the binary only", slog.Any("err", cause))
	if r != nil {
		if err := a.restoreUnits(context.WithoutCancel(ctx), r); err != nil {
			a.log.Error("putting back the unit files failed", slog.Any("err", err))
		}
	}
}

// restoreUnits puts back exactly the units replaced in this run from their
// .prev copies and runs daemon-reload (best effort).
func (a *applier) restoreUnits(ctx context.Context, r *os.Root) error {
	if len(a.replaced) == 0 {
		return nil
	}
	var errs []error
	for _, name := range a.replaced {
		prev, err := readSmallFile(r, name+".prev", maxInstalledUnit)
		if err == nil {
			err = writeUnit(r, name, prev, a.h.Strict)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	a.replaced = nil
	syncDir(a.h.UnitDir)
	if err := a.h.Systemctl(ctx, "daemon-reload"); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// rollbackUnits puts back this run's units before the binary is rolled
// back (the health-failure and the SIGTERM rollback).
func (a *applier) rollbackUnits(ctx context.Context) error {
	if len(a.replaced) == 0 {
		return nil
	}
	r, err := os.OpenRoot(a.h.UnitDir)
	if err != nil {
		return err
	}
	defer r.Close()
	return a.restoreUnits(ctx, r)
}
