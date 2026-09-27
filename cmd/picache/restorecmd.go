package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/app"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

const restoreUsage = "usage: picache restore <file> [--sections a,b] [--force]"

// restoreCmd is `picache restore`: the checks of an upload (the backup
// against the live database, the schema versions, the section rules and
// the dry-run merge), then the file is staged as picache.db.restore for
// the next start (restore.by = "cli", audited then). It never swaps
// databases (the service may hold them open), needs no password (host
// access) and ignores PICACHE_DESTRUCTIVE_API. As root it switches to the
// owner of the data directory first.
func restoreCmd(args []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sections := fs.String("sections", "", "comma-separated sections (default: everything)")
	force := fs.Bool("force", false, "replace a staged restore")
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			fmt.Fprintf(os.Stderr, "picache: %v\n%s\n", err, restoreUsage)
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, restoreUsage)
		return 2
	}
	var secs []string
	if set := isFlagSet(fs, "sections"); set {
		names := strings.Split(*sections, ",")
		if strings.TrimSpace(*sections) == "" {
			names = nil
		}
		var err error
		if secs, err = settings.CheckSections("sections", names, settings.RestoreSections, "restore"); err != nil {
			fmt.Fprintf(os.Stderr, "picache: %v\n%s\n", err, restoreUsage)
			return 2
		}
	}
	cfg, err := config.LoadWithoutSecrets(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	wasRoot := os.Geteuid() == 0
	if err := becomeOwnerOf(cfg.DataDir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := app.StageRestoreFile(context.Background(), cfg.DataDir, pos[0], secs, *force); err != nil {
		if errors.Is(err, os.ErrPermission) {
			fmt.Fprintln(os.Stderr, restorePermissionHint(err, wasRoot))
			return 1
		}
		return fail(err)
	}
	fmt.Println("restore staged: restart PiCache to apply it (systemctl restart picache)")
	return 0
}

// restorePermissionHint explains a refused file access of restore (the
// backup or the data directory): restore runs as the owner of the data
// directory (as root it switches to it first), so the backup must be
// readable by that user.
func restorePermissionHint(err error, wasRoot bool) string {
	msg := "picache: " + escapeControls(err.Error())
	if wasRoot {
		return msg + "\n(as root, restore runs as the owner of the data directory: the backup file must be readable by that user, e.g. copied to /tmp)"
	}
	return msg + "\n(run `sudo picache restore …`; the backup file must be readable by the owner of the data directory)"
}

// isFlagSet reports whether a flag was given.
func isFlagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}
