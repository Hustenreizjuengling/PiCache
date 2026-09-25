package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unicode"

	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/update"
	"github.com/hustenreizjuengling/picache/internal/version"
)

const updateUsage = `usage: picache update --check [--prerelease]
       sudo picache update [--version vX.Y.Z] [--prerelease] [--allow-downgrade] [--yes]
       sudo picache update --from DIR [--version vX.Y.Z] [--allow-downgrade] [--yes]
       picache update apply-pending   (the root helper started by picache-update.path)`

// Exit codes of `picache update --check` (1 = error, 2 = usage).
const (
	exitUpToDate        = 0
	exitUpdateAvailable = 10
)

// maxNoteLines of the release notes are shown before asking.
const maxNoteLines = 20

// errDeclined: the admin answered no.
var errDeclined = errors.New("aborted: nothing was changed")

// newReleaseClient returns the GitHub client (tests point it elsewhere).
var newReleaseClient = func() *update.Client { return &update.Client{} }

func updateCmd(args []string) int {
	if len(args) > 0 && args[0] == "apply-pending" {
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, updateUsage)
			return 2
		}
		return updateApplyPending()
	}
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	check := fs.Bool("check", false, "")
	ver := fs.String("version", "", "")
	pre := fs.Bool("prerelease", false, "")
	downgrade := fs.Bool("allow-downgrade", false, "")
	yes := fs.Bool("yes", false, "")
	from := fs.String("from", "", "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Println(updateUsage)
			return 0
		}
		fmt.Fprintf(os.Stderr, "picache update: %v\n%s\n", err, updateUsage)
		return 2
	}
	switch {
	case fs.NArg() > 0,
		*check && (*ver != "" || *downgrade || *yes || *from != ""),
		*from != "" && *pre:
		fmt.Fprintln(os.Stderr, updateUsage)
		return 2
	}
	if *ver != "" {
		if _, err := update.ParseVersion(*ver); err != nil {
			fmt.Fprintln(os.Stderr, "picache update:", err)
			return 2
		}
	}
	if *check {
		return updateCheck(*pre)
	}
	return updateInstall(*ver, *pre, *downgrade, *yes, *from)
}

// updateCheck prints the running and the latest version (no root needed).
func updateCheck(pre bool) int {
	ctx, cancel := context.WithTimeout(context.Background(), update.CheckTimeout)
	defer cancel()
	rel, err := newReleaseClient().Latest(ctx, pre)
	if err != nil {
		fmt.Fprintln(os.Stderr, "picache update:", err)
		return 1
	}
	fmt.Printf("running: %s\n", version.Version)
	if rel == nil {
		fmt.Printf("latest:  none (no release with a binary for linux/%s)\n", runtime.GOARCH)
		return exitUpToDate
	}
	fmt.Printf("latest:  %s%s\n", rel.Version, published(rel))
	fmt.Println("release: " + rel.URL)
	v, err := update.ParseVersion(rel.Version)
	if err == nil && update.ParseRunning(version.Version).Accepts(v) {
		fmt.Println("An update is available: sudo picache update --version " + rel.Version)
		return exitUpdateAvailable
	}
	fmt.Println("PiCache is up to date.")
	return exitUpToDate
}

func published(rel *update.Release) string {
	if rel.PublishedAt.IsZero() {
		return ""
	}
	return " (published " + rel.PublishedAt.Format("2006-01-02") + ")"
}

// updateInstall runs the install procedure (root): from GitHub after
// showing the release notes, or from a directory of release files.
func updateInstall(ver string, pre, downgrade, yes bool, from string) int {
	cfg, err := config.Load(nil, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	host := update.SystemHost(localHealth)
	if err := host.CheckPrivileges(); err != nil {
		fmt.Fprintln(os.Stderr, "picache update:", err)
		return 1
	}
	bin, err := installedBinary()
	if err != nil {
		fmt.Fprintln(os.Stderr, "picache update:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	o := update.Options{Version: ver, AllowDowngrade: downgrade, BinPath: bin, DataDir: cfg.DataDir,
		Current: version.Version, Host: host, Progress: func(step, msg string) { fmt.Printf("[%s] %s\n", step, msg) }}
	if from != "" {
		o.Files = update.DirFiles(from)
		if !yes {
			o.Confirm = func(v string) error {
				return confirm(fmt.Sprintf("Install PiCache %s from %s (running: %s) and restart it?", v, from, version.Version))
			}
		}
	} else {
		c := newReleaseClient()
		var rel *update.Release
		if ver != "" {
			rel, err = c.Release(ctx, ver)
		} else {
			rel, err = c.Latest(ctx, pre)
		}
		switch {
		case err != nil:
			fmt.Fprintln(os.Stderr, "picache update:", err)
			return 1
		case rel == nil:
			fmt.Fprintf(os.Stderr, "picache update: no release with a binary for linux/%s found\n", runtime.GOARCH)
			return 1
		}
		if v, err := update.ParseVersion(rel.Version); err == nil && !downgrade && !update.ParseRunning(version.Version).Accepts(v) {
			switch {
			case ver == "":
				fmt.Printf("PiCache %s is up to date (latest release: %s).\n", version.Version, rel.Version)
				return 0
			case ver == version.Version:
				fmt.Printf("PiCache %s is already installed.\n", ver)
				return 0
			}
			fmt.Fprintf(os.Stderr, "picache update: %s is not newer than the running %s; add --allow-downgrade to install it anyway\n",
				rel.Version, version.Version)
			return 1
		}
		printRelease(os.Stdout, rel)
		if !yes {
			if err := confirm("Install PiCache " + rel.Version + " and restart it?"); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		o.Version, o.Files = rel.Version, c.Files(rel.Version)
	}
	res, err := update.Apply(ctx, o)
	switch {
	case errors.Is(err, errDeclined):
		fmt.Fprintln(os.Stderr, err)
		return 1
	case err != nil:
		fmt.Fprintf(os.Stderr, "picache update: %s: %s\n", res.State, res.Message)
		return 1
	}
	fmt.Println(res.Message)
	return 0
}

// updateApplyPending is the root helper started by picache-update.path.
func updateApplyPending() int {
	cfg, err := config.Load(nil, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	bin, err := installedBinary()
	if err != nil {
		fmt.Fprintln(os.Stderr, "update apply-pending:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	o := update.Options{BinPath: bin, DataDir: cfg.DataDir, Current: version.Version,
		Host: update.SystemHost(localHealth), Log: newLogger(cfg)}
	if err := update.ApplyPending(ctx, newReleaseClient(), o); err != nil {
		fmt.Fprintln(os.Stderr, "update apply-pending:", err)
		return 1
	}
	return 0
}

// installedBinary is this executable (symbolic links resolved): the binary
// an update replaces.
func installedBinary() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// printRelease shows the version change and the start of the release notes.
// The notes come from GitHub: control characters are replaced, so they
// cannot drive the terminal.
func printRelease(w io.Writer, rel *update.Release) {
	fmt.Fprintf(w, "Update PiCache %s → %s%s\n\n", version.Version, rel.Version, published(rel))
	lines := strings.Split(strings.TrimSpace(rel.Notes), "\n")
	for i, l := range lines {
		if i == maxNoteLines {
			fmt.Fprintln(w, "  …")
			break
		}
		fmt.Fprintln(w, "  "+strings.Map(func(r rune) rune {
			if r != '\t' && unicode.IsControl(r) {
				return '?'
			}
			return r
		}, strings.TrimRight(l, "\r")))
	}
	fmt.Fprintf(w, "\nRelease notes: %s\n", rel.URL)
}

// confirm asks a yes/no question on the terminal.
func confirm(question string) error {
	fmt.Printf("%s [y/N] ", question)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	if !strings.HasSuffix(line, "\n") { // no answer (end of input)
		fmt.Println()
	}
	return errDeclined
}
