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
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/config"
	"github.com/rushp4000/lazarr/internal/constants"
	"github.com/rushp4000/lazarr/internal/logging"
	"github.com/rushp4000/lazarr/internal/materialize"
	"github.com/rushp4000/lazarr/internal/metrics"
	"github.com/rushp4000/lazarr/internal/qbit"
	"github.com/rushp4000/lazarr/internal/symlink"
	"github.com/rushp4000/lazarr/internal/torbox"
	"github.com/rushp4000/lazarr/internal/version"
	"github.com/rushp4000/lazarr/internal/vfs"
	"github.com/rushp4000/lazarr/internal/webui"
)

// auditInterval is how often the ToS-audit loop diffs mylist vs the materialized
// set (docs/12 guardrail 2). Cheap (one mylist pull); 5m keeps the proof current.
const auditInterval = 5 * time.Minute

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	flag.Parse()

	// Logger: text to stdout + a bounded in-memory ring for the Web UI Logs tab.
	// The level is a LevelVar so the settings page can change verbosity live.
	levelVar := new(slog.LevelVar)
	levelVar.Set(slog.LevelInfo)
	logRing := logging.NewRing(logging.RingCapacity)
	base := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: levelVar})
	slog.SetDefault(slog.New(logging.NewHandler(base, logRing)))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("load config", "path", *cfgPath, "err", err)
		os.Exit(1)
	}

	// config log_level first, then the LAZARR_LOG_LEVEL env var as an override
	// (kept for backwards compatibility with existing deployments).
	if lv, lerr := logging.ParseLevel(cfg.LogLevel); lerr == nil {
		levelVar.Set(lv)
	}
	if v := os.Getenv("LAZARR_LOG_LEVEL"); v != "" {
		if lv, lerr := logging.ParseLevel(v); lerr == nil {
			levelVar.Set(lv)
		} else {
			slog.Warn("ignoring invalid LAZARR_LOG_LEVEL", "value", v)
		}
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

	startTime := time.Now()
	tb := torbox.New(cfg.TorBox)

	// Best-effort connectivity/slot check; non-fatal so lazarr still boots if
	// TorBox is briefly unreachable. Never logs the API key.
	var cachedAccount *torbox.Account
	if acct, err := tb.UserMe(); err != nil {
		slog.Warn("torbox user/me check failed (continuing)", "err", err)
	} else {
		cachedAccount = acct
		slog.Info("torbox account", "plan", acct.Plan, "active_slots", acct.ActiveSlots,
			"long_term_storage", acct.LongTermStore, "cooldown_until", acct.CooldownUntil)
	}

	sym := symlink.New(cfg.Paths, cfg.Ownership)

	// Phase 2: the lazy playback path — materialize engine, FUSE mount, idle + max-hold
	// reapers, and the ToS-audit loop. The engine is constructed BEFORE the qbit server and
	// the mount so it can be wired into both (qbit releases on delete; the mount drives
	// reads), and so SetMountHealthy lands BEFORE Start (S1: no data race on mountHealthy).
	eng, err := materialize.New(materialize.Deps{
		Store:         store,
		TorBox:        tb,
		Policy:        cfg.Policy,
		ProbeCacheDir: cfg.Paths.ProbeCacheDir,
	})
	if err != nil {
		slog.Error("materialize engine", "err", err)
		os.Exit(1)
	}

	// qbit gets the engine so torrents/delete releases an in-flight materialization (S2).
	qsrv := qbit.New(qbit.Deps{Config: cfg, Store: store, TorBox: tb, Symlink: sym, Engine: eng})

	srv := &http.Server{
		Addr:              cfg.QBit.Listen,
		Handler:           qsrv,
		ReadHeaderTimeout: 15 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// on_cache_miss=wait watcher (no-op when nothing is downloading). Also resumes
	// any StateDownloading rows left by a restart.
	qsrv.StartWaitPoller(ctx)

	go func() {
		slog.Info("qbit listening", "addr", cfg.QBit.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server", "err", err)
			stop()
		}
	}()

	fsys := vfs.New(cfg.Paths.FuseMount, store, eng)
	if err := fsys.Mount(); err != nil {
		// FUSE is the core of Phase 2 — without it nothing can be read/played. Close is safe
		// here even though Start has not run yet (no reapers to stop, empty track).
		slog.Error("vfs mount failed (need --cap-add SYS_ADMIN --device /dev/fuse)",
			"mount", cfg.Paths.FuseMount, "err", err)
		_ = eng.Close()
		os.Exit(1)
	}

	// Broken-mount guard (CRITICAL): the reapers call Release -> ControlDelete. If the
	// FUSE mount goes unhealthy on a transient blip the reapers must NOT mass-delete from
	// the TorBox account. Hand the engine a cheap mount-health probe; the reapers skip a
	// sweep (logging a Warn) whenever it reports unhealthy. MUST be set before Start so the
	// reaper goroutine never reads mountHealthy while this writes it (S1).
	eng.SetMountHealthy(fsys.Healthy)
	eng.Start(ctx) // idle + max-hold reapers; stop on ctx cancel and at eng.Close.

	// Observability admin server (opt-in, separate port): /metrics (Prometheus) + /health
	// (JSON). Disabled when metrics.listen is empty. Kept off the arr-facing qbit port; like
	// the qbit listener it is unauthenticated, so bind it to the trusted LAN.
	var adminSrv *http.Server
	if cfg.Metrics.Listen != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics.MetricsHandler())
		mux.Handle("/health", metrics.HealthHandler(healthProvider{eng: eng, fsys: fsys}))
		adminSrv = &http.Server{
			Addr:              cfg.Metrics.Listen,
			Handler:           mux,
			ReadHeaderTimeout: 15 * time.Second,
		}
		go func() {
			slog.Info("admin listening", "addr", cfg.Metrics.Listen, "endpoints", "/metrics /health")
			if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("admin server", "err", err)
				stop()
			}
		}()
	}

	// Web UI server (opt-in, separate port). Disabled when webui.listen is empty.
	// Kept off the arr-facing qbit port. Optionally protected by Basic Auth (set
	// webui.username + webui.password); otherwise trusted-LAN unauthenticated.
	var webuiSrv *http.Server
	if cfg.WebUI.Listen != "" {
		wp := newWebuiProvider(eng, fsys, store, sym, cfg, cachedAccount, startTime)
		wp.cfgPath = *cfgPath
		wp.levelVar = levelVar
		wp.logRing = logRing
		wp.stop = stop
		wh, err := webui.New(wp, cfg.WebUI.Username, cfg.WebUI.Password)
		if err != nil {
			slog.Error("webui setup", "err", err)
			os.Exit(1)
		}
		webuiSrv = &http.Server{
			Addr:              cfg.WebUI.Listen,
			Handler:           wh,
			ReadHeaderTimeout: 15 * time.Second,
		}
		go func() {
			slog.Info("webui listening", "addr", cfg.WebUI.Listen,
				"auth", cfg.WebUI.Username != "")
			if err := webuiSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("webui server", "err", err)
				stop()
			}
		}()
	}

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

	// Repair-scan loop: daily checkcached sweep over the whole catalog to find content
	// that TorBox has evicted from its CDN. No TorBox adds; purely read-only. Results
	// are written to the catalog and visible in the Web UI repair tab.
	go func() {
		t := time.NewTicker(constants.DefaultRepairScanEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if evicted, err := eng.RepairScan(ctx); err != nil {
					slog.Warn("repair scan failed", "err", err)
				} else if len(evicted) > 0 {
					slog.Warn("repair scan: evicted content detected", "count", len(evicted))
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
	if adminSrv != nil {
		if err := adminSrv.Shutdown(shutdownCtx); err != nil {
			slog.Error("admin shutdown", "err", err)
		}
	}
	if webuiSrv != nil {
		if err := webuiSrv.Shutdown(shutdownCtx); err != nil {
			slog.Error("webui shutdown", "err", err)
		}
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

// engStats is the slice of the materialize engine the /health endpoint needs. The concrete
// engine type is unexported, so we bridge to it through this interface (it satisfies it via
// its exported SlotsInUse/SlotsTotal/LastAuditUnix methods).
type engStats interface {
	SlotsInUse() int
	SlotsTotal() int
	LastAuditUnix() int64
}

// healthProvider adapts the engine + FUSE mount to metrics.HealthProvider for /health.
type healthProvider struct {
	eng  engStats
	fsys *vfs.FS
}

func (h healthProvider) Mounted() bool        { return h.fsys.Healthy() }
func (h healthProvider) SlotsInUse() int      { return h.eng.SlotsInUse() }
func (h healthProvider) SlotsTotal() int      { return h.eng.SlotsTotal() }
func (h healthProvider) LastAuditUnix() int64 { return h.eng.LastAuditUnix() }
func (h healthProvider) Version() string      { return version.Version }

// ─── Web UI provider ──────────────────────────────────────────────────────────

// webuiEngine is the slice of *materialize.materializer the webuiProvider needs.
// It deliberately does not overlap with engStats so both can coexist without a
// wide interface, and uses the concrete *materialize.materializer type in practice.
type webuiEngine interface {
	SlotsInUse() int
	SlotsTotal() int
	LastAuditUnix() int64
	Release(hash string) error
	AuditTOS() error
	RepairScan(ctx context.Context) ([]materialize.RepairEntry, error)
	MaterializedSnapshot() []materialize.MaterializedEntry
}

// webuiProvider adapts Lazarr's concrete internals to webui.Provider.
type webuiProvider struct {
	eng     webuiEngine
	fsys    *vfs.FS
	store   catalog.Store
	sym     symlink.Manager
	cfg     *config.Config
	account *webui.AccountInfo // cached from boot-time UserMe (may be nil)
	start   time.Time

	// Settings/observability plumbing (wired by main after construction).
	cfgPath  string         // path of the loaded config.yaml (settings editor target)
	levelVar *slog.LevelVar // live log-level control
	logRing  *logging.Ring  // recent-records buffer for /api/logs
	stop     func()         // cancels the root signal context (graceful restart)
}

func newWebuiProvider(
	eng webuiEngine,
	fsys *vfs.FS,
	store catalog.Store,
	sym symlink.Manager,
	cfg *config.Config,
	acct *torbox.Account,
	start time.Time,
) *webuiProvider {
	wp := &webuiProvider{eng: eng, fsys: fsys, store: store, sym: sym, cfg: cfg, start: start}
	if acct != nil {
		wp.account = &webui.AccountInfo{
			Plan:          acct.Plan,
			ActiveSlots:   acct.ActiveSlots,
			CooldownUntil: acct.CooldownUntil,
			LongTermStore: acct.LongTermStore,
		}
	}
	return wp
}

func (p *webuiProvider) Status() webui.StatusSnapshot {
	return webui.StatusSnapshot{
		Version:       version.Version,
		UptimeSeconds: int64(time.Since(p.start).Seconds()),
		Mounted:       p.fsys.Healthy(),
		SlotsInUse:    p.eng.SlotsInUse(),
		SlotsTotal:    p.eng.SlotsTotal(),
		LastAuditUnix: p.eng.LastAuditUnix(),
		Account:       p.account,
	}
}

func (p *webuiProvider) ListReleases(f catalog.ReleaseFilter) ([]*catalog.Release, int, error) {
	return p.store.ListReleases(f)
}

func (p *webuiProvider) MaterializedSet() []webui.MaterializedItem {
	snap := p.eng.MaterializedSnapshot()
	out := make([]webui.MaterializedItem, len(snap))
	for i, e := range snap {
		it := webui.MaterializedItem{
			Hash:       e.Hash,
			TorBoxID:   e.TorBoxID,
			Refs:       e.Refs,
			LastUsedNs: e.LastUsedNs,
		}
		// Join with the catalog so the UI shows the release title, not an infohash.
		if rel, _, err := p.store.GetRelease(e.Hash); err == nil && rel != nil {
			it.Name = rel.Name
			it.Category = rel.Category
			it.TotalSize = rel.TotalSize
		}
		out[i] = it
	}
	return out
}

func (p *webuiProvider) MetricsSummary() (*metrics.Summary, error) {
	return metrics.GatherSummary()
}

func (p *webuiProvider) ForceRelease(hash string) error {
	return p.eng.Release(hash)
}

func (p *webuiProvider) TriggerAudit() error {
	return p.eng.AuditTOS()
}

func (p *webuiProvider) TriggerRepairScan(ctx context.Context) ([]materialize.RepairEntry, error) {
	return p.eng.RepairScan(ctx)
}

func (p *webuiProvider) ListEvicted() ([]*catalog.Release, error) {
	return p.store.ListEvicted()
}

// ForgetRelease removes a release from Lazarr entirely — engine release (if materialized),
// symlinks deleted, catalog entry removed — so the arr's next health-check flags the file
// missing and triggers a re-search on TorBox / Torrentio.
func (p *webuiProvider) ForgetRelease(hash string) error {
	// Best-effort engine release first (no-op if not materialized, returns ErrNotFound).
	_ = p.eng.Release(hash)
	if err := p.sym.Remove(hash); err != nil {
		slog.Warn("webui: forget: remove symlinks", "hash", hash, "err", err)
	}
	return p.store.DeleteRelease(hash)
}

// GetSettings maps the running config to the editable settings form. Secrets are
// blanked; *Set flags report presence so the form can say "configured".
func (p *webuiProvider) GetSettings() webui.Settings {
	c := p.cfg
	return webui.Settings{
		LogLevel:         effectiveLogLevel(c.LogLevel),
		TorBoxAPIKeySet:  c.TorBox.APIKey != "",
		TorBoxAPIBase:    c.TorBox.APIBase,
		QBitListen:       c.QBit.Listen,
		QBitUsername:     c.QBit.Username,
		QBitPassword:     c.QBit.Password,
		Categories:       append([]string(nil), c.Categories...),
		DownloadDir:      c.Paths.DownloadDir,
		FuseMount:        c.Paths.FuseMount,
		DBPath:           c.Paths.DBPath,
		ProbeCacheDir:    c.Paths.ProbeCacheDir,
		AllowUncached:    c.Policy.AllowUncached,
		IdleTTL:          c.Policy.IdleTTL.D().String(),
		MaxHold:          c.Policy.MaxHold.D().String(),
		ActiveSlots:      c.Policy.ActiveSlots,
		ProbeCache:       c.Policy.ProbeCache,
		OnCacheMiss:      effectiveCacheMiss(c.Policy.OnCacheMiss),
		CacheWaitBudget:  c.Policy.CacheWaitBudget.D().String(),
		MaxWaitDownloads: c.Policy.MaxWaitDownloads,
		ReadaheadWindows: c.Policy.ReadaheadWindows,
		PUID:             c.Ownership.PUID,
		PGID:             c.Ownership.PGID,
		MetricsListen:    c.Metrics.Listen,
		WebUIListen:      c.WebUI.Listen,
		WebUIUsername:    c.WebUI.Username,
		WebUIPasswordSet: c.WebUI.Password != "",
	}
}

// effectiveLogLevel normalizes "" to "info" for display.
func effectiveLogLevel(s string) string {
	if s == "" {
		return "info"
	}
	return s
}

// effectiveCacheMiss normalizes "" to "error" for display.
func effectiveCacheMiss(s string) string {
	if s == "" {
		return "error"
	}
	return s
}

// SaveSettings overlays s onto the on-disk config, validates, writes the file
// atomically, hot-applies the log level, and reports whether anything else changed
// (= restart required). Empty secret fields keep the current values.
func (p *webuiProvider) SaveSettings(s webui.Settings) (bool, error) {
	if p.cfgPath == "" {
		return false, fmt.Errorf("settings editing unavailable: config path unknown")
	}
	// Start from the file on disk (source of truth), not the running config — a
	// previous save that still awaits restart must not be silently reverted.
	nc, err := config.Load(p.cfgPath)
	if err != nil {
		return false, fmt.Errorf("reload current config: %w", err)
	}

	nc.LogLevel = s.LogLevel
	if s.TorBoxAPIKey != "" {
		nc.TorBox.APIKey = s.TorBoxAPIKey
	}
	nc.TorBox.APIBase = s.TorBoxAPIBase
	nc.QBit.Listen = s.QBitListen
	nc.QBit.Username = s.QBitUsername
	nc.QBit.Password = s.QBitPassword
	nc.Categories = normalizeCategories(s.Categories)
	nc.Paths.DownloadDir = s.DownloadDir
	nc.Paths.FuseMount = s.FuseMount
	nc.Paths.DBPath = s.DBPath
	nc.Paths.ProbeCacheDir = s.ProbeCacheDir
	nc.Policy.AllowUncached = s.AllowUncached
	idle, err := time.ParseDuration(s.IdleTTL)
	if err != nil {
		return false, fmt.Errorf("keep on TorBox for (idle_ttl) %q: %w", s.IdleTTL, err)
	}
	hold, err := time.ParseDuration(s.MaxHold)
	if err != nil {
		return false, fmt.Errorf("absolute limit (max_hold) %q: %w", s.MaxHold, err)
	}
	nc.Policy.IdleTTL = config.Duration(idle)
	nc.Policy.MaxHold = config.Duration(hold)
	nc.Policy.ActiveSlots = s.ActiveSlots
	nc.Policy.ProbeCache = s.ProbeCache
	nc.Policy.OnCacheMiss = s.OnCacheMiss
	if s.CacheWaitBudget != "" {
		budget, berr := time.ParseDuration(s.CacheWaitBudget)
		if berr != nil {
			return false, fmt.Errorf("cache wait budget %q: %w", s.CacheWaitBudget, berr)
		}
		nc.Policy.CacheWaitBudget = config.Duration(budget)
	}
	nc.Policy.MaxWaitDownloads = s.MaxWaitDownloads
	nc.Policy.ReadaheadWindows = s.ReadaheadWindows
	nc.Ownership.PUID = s.PUID
	nc.Ownership.PGID = s.PGID
	nc.Metrics.Listen = s.MetricsListen
	nc.WebUI.Listen = s.WebUIListen
	nc.WebUI.Username = s.WebUIUsername
	if s.WebUIPassword != "" {
		nc.WebUI.Password = s.WebUIPassword
	}
	if s.WebUIUsername == "" {
		nc.WebUI.Password = "" // auth off: clear the orphan password so validate passes
	}

	if err := nc.Validate(); err != nil {
		return false, err
	}
	if err := config.Save(p.cfgPath, nc); err != nil {
		return false, err
	}

	// Hot-apply the log level to the running process.
	if lv, lerr := logging.ParseLevel(nc.LogLevel); lerr == nil && p.levelVar != nil {
		p.levelVar.Set(lv)
	}
	p.cfg.LogLevel = nc.LogLevel // keep the running view consistent for the next diff

	// Restart needed iff anything besides the log level differs from what is running.
	runningCmp := *p.cfg
	newCmp := *nc
	runningCmp.LogLevel, newCmp.LogLevel = "", ""
	restart := !reflect.DeepEqual(runningCmp, newCmp)
	return restart, nil
}

// normalizeCategories trims whitespace and drops empties/duplicates, preserving order.
func normalizeCategories(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}

func (p *webuiProvider) Logs(level string, limit int) []logging.Entry {
	if p.logRing == nil {
		return nil
	}
	lv, err := logging.ParseLevel(level)
	if err != nil {
		lv = slog.LevelDebug // unknown filter -> show everything captured
	}
	return p.logRing.Snapshot(lv, limit)
}

// Restart schedules a graceful shutdown just after the HTTP response flushes. The
// container's restart policy (restart: unless-stopped) brings Lazarr back up.
func (p *webuiProvider) Restart() error {
	if p.stop == nil {
		return fmt.Errorf("restart unavailable")
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		slog.Info("webui: restarting (graceful shutdown; supervisor restarts the container)")
		p.stop()
	}()
	return nil
}

func (p *webuiProvider) SafeConfig() webui.SafeConfig {
	return webui.SafeConfig{
		TorBoxAPIBase: p.cfg.TorBox.APIBase,
		// api_key intentionally omitted
		QBitListen:    p.cfg.QBit.Listen,
		AdminListen:   p.cfg.Metrics.Listen,
		WebUIListen:   p.cfg.WebUI.Listen,
		DownloadDir:   p.cfg.Paths.DownloadDir,
		FuseMount:     p.cfg.Paths.FuseMount,
		DBPath:        p.cfg.Paths.DBPath,
		Categories:    p.cfg.Categories,
		AllowUncached: p.cfg.Policy.AllowUncached,
		IdleTTL:       p.cfg.Policy.IdleTTL.D().String(),
		MaxHold:       p.cfg.Policy.MaxHold.D().String(),
		ActiveSlots:   p.cfg.Policy.ActiveSlots,
		ProbeCache:    p.cfg.Policy.ProbeCache,
		OwnershipPUID: p.cfg.Ownership.PUID,
		OwnershipPGID: p.cfg.Ownership.PGID,
		AuthEnabled:   p.cfg.WebUI.Username != "",
	}
}
