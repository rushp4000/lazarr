// Command lazarr is a self-hosted, ToS-compliant TorBox lazy-materialize shim that
// presents as a qBittorrent client to Sonarr/Radarr. See /root/Github/Lazarr/docs.
//
// Phase 1 wires catalog -> torbox -> symlink -> qbit and serves the qBittorrent
// WebUI emulation. At grab time it symlinks with NO TorBox add (the ToS-compliant
// core). Phase 2 (vfs/materialize) adds playback materialization + idle release.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/config"
	"github.com/rushp4000/lazarr/internal/materialize"
	"github.com/rushp4000/lazarr/internal/qbit"
	"github.com/rushp4000/lazarr/internal/symlink"
	"github.com/rushp4000/lazarr/internal/torbox"
	"github.com/rushp4000/lazarr/internal/vfs"
)

// auditInterval is how often the ToS-audit loop diffs mylist vs the materialized
// set (docs/12 guardrail 2). Cheap (one mylist pull); 5m keeps the proof current.
const auditInterval = 5 * time.Minute

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	flag.Parse()

	level := slog.LevelInfo
	if v := os.Getenv("LAZARR_LOG_LEVEL"); strings.EqualFold(v, "debug") {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("load config", "path", *cfgPath, "err", err)
		os.Exit(1)
	}

	slog.Info("lazarr starting",
		"qbit", cfg.QBit.Listen, "fuse", cfg.Paths.FuseMount,
		"download_dir", cfg.Paths.DownloadDir, "db", cfg.Paths.DBPath,
		"slots", cfg.Policy.ActiveSlots, "uncached", cfg.Policy.AllowUncached,
		"categories", cfg.Categories)

	// catalog -> torbox -> symlink -> qbit (Phase-1 wiring order).
	store, err := catalog.OpenSQLite(cfg.Paths.DBPath)
	if err != nil {
		slog.Error("open catalog", "db", cfg.Paths.DBPath, "err", err)
		os.Exit(1)
	}
	defer func() { _ = store.Close() }()

	tb := torbox.New(cfg.TorBox)

	// Best-effort connectivity/slot check; non-fatal so lazarr still boots if
	// TorBox is briefly unreachable. Never logs the API key.
	if acct, err := tb.UserMe(); err != nil {
		slog.Warn("torbox user/me check failed (continuing)", "err", err)
	} else {
		slog.Info("torbox account", "plan", acct.Plan, "active_slots", acct.ActiveSlots,
			"long_term_storage", acct.LongTermStore, "cooldown_until", acct.CooldownUntil)
	}

	sym := symlink.New(cfg.Paths, cfg.Ownership)

	qsrv := qbit.New(qbit.Deps{Config: cfg, Store: store, TorBox: tb, Symlink: sym})

	srv := &http.Server{
		Addr:              cfg.QBit.Listen,
		Handler:           qsrv,
		ReadHeaderTimeout: 15 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("qbit listening", "addr", cfg.QBit.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server", "err", err)
			stop()
		}
	}()

	// Phase 2: the lazy playback path — materialize engine, FUSE mount, idle +
	// max-hold reapers, and the ToS-audit loop (diff mylist vs the materialized set).
	eng, err := materialize.New(materialize.Deps{
		Store:         store,
		TorBox:        tb,
		Policy:        cfg.Policy,
		ProbeCacheDir: cfg.Paths.ProbeCacheDir,
		// Readahead 0 => constants.DefaultReadahead (8 MiB).
	})
	if err != nil {
		slog.Error("materialize engine", "err", err)
		os.Exit(1)
	}
	eng.Start(ctx) // idle + max-hold reapers; stop on ctx cancel and at eng.Close.

	fsys := vfs.New(cfg.Paths.FuseMount, store, eng)
	if err := fsys.Mount(); err != nil {
		// FUSE is the core of Phase 2 — without it nothing can be read/played.
		slog.Error("vfs mount failed (need --cap-add SYS_ADMIN --device /dev/fuse)",
			"mount", cfg.Paths.FuseMount, "err", err)
		_ = eng.Close()
		os.Exit(1)
	}

	// Broken-mount guard (CRITICAL): the reapers call Release -> ControlDelete. If the
	// FUSE mount goes unhealthy on a transient blip the reapers must NOT mass-delete from
	// the TorBox account. Hand the engine a cheap mount-health probe; the reapers skip a
	// sweep (logging a Warn) whenever it reports unhealthy.
	eng.SetMountHealthy(fsys.Healthy)

	// ToS-audit loop: periodic proof that the account holds nothing we believe is
	// released (scoped to Lazarr-added ids while it coexists with decypharr).
	go func() {
		t := time.NewTicker(auditInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := eng.AuditTOS(); err != nil {
					slog.Warn("tos audit failed", "err", err)
				}
			}
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown", "err", err)
	}

	// Stop new reads (unmount FUSE), then release everything still on TorBox so the
	// account is left clean (ToS). eng.Close stops the reapers and waits for them.
	if err := fsys.Unmount(); err != nil {
		slog.Error("vfs unmount", "err", err)
	}
	if err := eng.Close(); err != nil {
		slog.Error("engine close (release-all)", "err", err)
	}
}
