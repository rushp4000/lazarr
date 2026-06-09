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
	"github.com/rushp4000/lazarr/internal/qbit"
	"github.com/rushp4000/lazarr/internal/symlink"
	"github.com/rushp4000/lazarr/internal/torbox"
)

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

	sym := symlink.New(cfg.Paths)

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

	// Phase 2 will start here: materialize engine, FUSE mount, idle/max-hold
	// reapers, and the ToS-audit loop (diff mylist vs materialized set).

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown", "err", err)
	}
}
