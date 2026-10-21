package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hustenreizjuengling/picache/internal/prefill"
	"github.com/hustenreizjuengling/picache/internal/prefill/prefillapi"
	"github.com/hustenreizjuengling/picache/internal/version"
)

const prefillUsage = "usage: picache prefill [serve|status] | sudo picache prefill enable | sudo picache prefill disable [--forget]"

// prefillCmd runs the prefill service (picache prefill [serve]), prints
// its status (picache prefill status), or installs, enables and starts it
// (enable), or stops it (disable). The service is a process of its own
// under its own user (deploy/systemd/picache-prefill.service), never part
// of picache serve: the store sign-ins it holds stay out of PiCache.
func prefillCmd(args []string) int {
	sub := "serve"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "enable":
		return prefillEnable(args)
	case "disable":
		return prefillDisable(args)
	}
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, prefillUsage)
		return 2
	}
	switch sub {
	case "serve":
		return prefillServe()
	case "status":
		return prefillStatus()
	default:
		fmt.Fprintln(os.Stderr, prefillUsage)
		return 2
	}
}

func prefillServe() int {
	if os.Geteuid() == 0 && os.Getenv("PICACHE_PREFILL_ALLOW_ROOT") != "1" {
		fmt.Fprintln(os.Stderr, "picache prefill: refusing to run as root: run it as an unprivileged user "+
			"(the systemd unit picache-prefill.service uses DynamicUser=yes)")
		return 2
	}
	level := slog.LevelInfo
	if os.Getenv("PICACHE_PREFILL_DEBUG") == "1" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)
	// A soft limit below the unit's MemoryMax: the GC works harder before
	// the kernel ends the service for a large manifest.
	setMemoryLimit(log)
	cfg, err := prefill.ConfigFromEnv(version.Get().Version, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "picache prefill: configuration error:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := prefill.Run(ctx, cfg); err != nil {
		if errors.Is(err, prefill.ErrRestart) {
			return prefill.ExitRestart
		}
		log.Error("picache prefill exited with error", "err", err)
		return 1
	}
	return 0
}

func prefillStatus() int {
	socket := os.Getenv("PICACHE_PREFILL_SOCKET")
	if socket == "" {
		socket = prefillapi.DefaultSocket
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := prefillapi.NewClient(socket).Status(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "picache prefill:", err)
		return 1
	}
	b, err := json.Marshal(st, json.Deterministic(true))
	if err != nil {
		fmt.Fprintln(os.Stderr, "picache prefill:", err)
		return 1
	}
	fmt.Println(escapeControls(string(b)))
	return 0
}
