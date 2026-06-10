// Package config loads Lazarr's config.yaml. See docs/05-spec.md §7 and docs/11.
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/rushp4000/lazarr/internal/constants"
	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from YAML strings like "15m".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	pd, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(pd)
	return nil
}

// MarshalYAML renders the duration back as a human string ("168h0m0s"), so a
// Config round-trips through Save/Load (the Web UI settings editor rewrites the
// whole file).
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

type TorBox struct {
	APIKey  string `yaml:"api_key"`
	APIBase string `yaml:"api_base"`
}

type QBit struct {
	Listen   string `yaml:"listen"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type Paths struct {
	DownloadDir   string `yaml:"download_dir"`    // arr's qBit save path (per-category subdirs)
	FuseMount     string `yaml:"fuse_mount"`      // virtual tree root
	DBPath        string `yaml:"db_path"`         // SQLite catalog file (default /data/lazarr.sqlite)
	ProbeCacheDir string `yaml:"probe_cache_dir"` // bounded header cache (default /data/probe)
}

type Policy struct {
	AllowUncached bool     `yaml:"allow_uncached"`
	IdleTTL       Duration `yaml:"idle_ttl"`
	MaxHold       Duration `yaml:"max_hold"`
	ActiveSlots   int      `yaml:"active_slots"` // 0 = auto-detect from /user/me; else your plan's slots
	ProbeCache    bool     `yaml:"probe_cache"`

	// OnCacheMiss decides what happens when an arr grabs a release TorBox has NOT
	// cached (only consulted when AllowUncached is false — with AllowUncached true
	// every grab is accepted):
	//   "error"  (default) — accept the grab and surface it as a qBit error state;
	//             the arr shows a warning until something (Cleanuparr, a human, or
	//             the repair tab) removes it.
	//   "reject" — refuse the add outright; the arr immediately falls back to the
	//             next release in its decision list. Cleanest queues, but repeated
	//             rejections can trip the arr's download-client backoff.
	//   "wait"   — start the TorBox download and watch it: if TorBox's reported ETA
	//             stays within CacheWaitBudget the grab is held in "downloading"
	//             state (real progress reported to the arr) until cached, then the
	//             torrent is RELEASED from the account (the content is now in
	//             TorBox's cache) and the grab completes as a normal lazy import.
	//             If the ETA exceeds the budget or the download stalls, it is
	//             deleted and the grab goes to error state.
	OnCacheMiss string `yaml:"on_cache_miss"`
	// CacheWaitBudget is the max time a "wait" download may need (TorBox ETA) before
	// Lazarr bails on it. Only used when OnCacheMiss == "wait".
	CacheWaitBudget Duration `yaml:"cache_wait_budget"`
	// MaxWaitDownloads caps concurrent "wait" downloads (each holds a TorBox slot
	// while downloading). Overflow misses fall back to error state.
	MaxWaitDownloads int `yaml:"max_wait_downloads"`

	// ReadaheadWindows is the number of 1 MiB windows prefetched in parallel ahead
	// of a sequential read (per open stream). 0 disables prefetch (each read is one
	// serial CDN round-trip ≈ 5-8 MB/s). 4-8 is the 4K-streaming range; memory cost
	// is ReadaheadWindows MiB per active stream and discarded prefetches count
	// against TorBox bandwidth.
	ReadaheadWindows int `yaml:"readahead_windows"`
}

// Ownership controls the privilege model for the symlink tree (docs/05 §5).
//
// Lazarr's daemon must run as root inside the container to mount FUSE: in Docker
// the CAP_SYS_ADMIN capability needed for mount(2) is only effective for uid 0.
// But the *arr suite (Sonarr/Radarr) runs as its own uid (e.g. 1003) and must be
// able to move/delete the symlinks Lazarr creates during import — which requires
// those symlinks and their parent dirs to be owned by the arr's uid:gid.
//
// The chosen model is "run as root + chown to PUID/PGID": after creating each
// directory and each symlink, the symlink manager chowns it to PUID:PGID. PUID/
// PGID == 0 disables chown (leave ownership as-is — i.e. root, the daemon's uid).
type Ownership struct {
	PUID int `yaml:"puid"` // 0 = disabled (leave as-is); else chown created symlinks/dirs to this uid
	PGID int `yaml:"pgid"` // 0 = disabled (leave as-is); else chown created symlinks/dirs to this gid
}

// Metrics configures the OPT-IN observability admin server. It is disabled by
// default (empty Listen). When set it serves /metrics (Prometheus) and /health
// (JSON) on a SEPARATE listener, kept off the arr-facing qbit port; like the
// qbit listener it is unauthenticated, so bind it to the trusted LAN.
type Metrics struct {
	Listen string `yaml:"listen"` // e.g. ":9090"; empty = admin server disabled
}

// WebUI configures the OPT-IN human dashboard. It is disabled by default (empty
// Listen). When set it serves the Lazarr Web UI on a SEPARATE listener, kept off
// the arr-facing qbit port. It is unauthenticated by default (trusted-LAN model,
// like the qbit and metrics ports). Set Username + Password to enable HTTP Basic
// Auth, which is recommended because the UI exposes more context and includes
// mutating actions (force-release, run-audit).
type WebUI struct {
	Listen   string `yaml:"listen"`   // e.g. ":8081"; empty = web UI disabled
	Username string `yaml:"username"` // optional basic-auth username
	Password string `yaml:"password"` // optional basic-auth password
}

type Config struct {
	// LogLevel is one of debug|info|warn|error (default info). The Web UI settings
	// page applies a change live via slog.LevelVar — no restart needed.
	LogLevel   string    `yaml:"log_level"`
	TorBox     TorBox    `yaml:"torbox"`
	QBit       QBit      `yaml:"qbit"`
	Paths      Paths     `yaml:"paths"`
	Categories []string  `yaml:"categories"`
	Policy     Policy    `yaml:"policy"`
	Ownership  Ownership `yaml:"ownership"`
	Metrics    Metrics   `yaml:"metrics"`
	WebUI      WebUI     `yaml:"webui"`
}

// Default returns a Config pre-populated with sane defaults; YAML overlays it.
func Default() *Config {
	return &Config{
		TorBox: TorBox{APIBase: constants.TorBoxAPIBase},
		QBit:   QBit{Listen: ":8080", Username: "lazarr", Password: "lazarr"},
		Policy: Policy{
			AllowUncached:    false,
			IdleTTL:          Duration(constants.DefaultIdleTTL),
			MaxHold:          Duration(constants.DefaultMaxHold),
			ActiveSlots:      constants.DefaultActiveSlots,
			ProbeCache:       true,
			OnCacheMiss:      "error",
			CacheWaitBudget:  Duration(15 * time.Minute),
			MaxWaitDownloads: 1,
			ReadaheadWindows: 4,
		},
	}
}

// Load reads config.yaml over the defaults.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := Default()
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if c.TorBox.APIBase == "" {
		c.TorBox.APIBase = constants.TorBoxAPIBase
	}
	if c.Paths.DBPath == "" {
		c.Paths.DBPath = "/data/lazarr.sqlite"
	}
	if c.Paths.ProbeCacheDir == "" {
		c.Paths.ProbeCacheDir = "/data/probe"
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return c, nil
}

// validate rejects nonsensical configurations after defaults are applied, so the
// daemon fails fast at startup instead of misbehaving at runtime. It is called by
// Load; tests call it directly via table-driven cases.
func (c *Config) validate() error {
	// (a) If we are serving any categories we need both the download dir (where the
	// import symlinks live) and the FUSE mount (their targets). Empty either way is a
	// misconfiguration that would silently break import.
	if len(c.Categories) > 0 {
		if c.Paths.DownloadDir == "" {
			return fmt.Errorf("download_dir is required when categories are set")
		}
		if c.Paths.FuseMount == "" {
			return fmt.Errorf("fuse_mount is required when categories are set")
		}
	}

	// (b) idle_ttl must be strictly less than max_hold: max_hold is the hard ceiling
	// the idle reaper sits under, so idle >= max_hold means the idle sweep never wins.
	if c.Policy.IdleTTL.D() >= c.Policy.MaxHold.D() {
		return fmt.Errorf("idle_ttl (%s) must be strictly less than max_hold (%s)",
			c.Policy.IdleTTL.D(), c.Policy.MaxHold.D())
	}

	// (c) active_slots may be 0 (auto-detect from /user/me) but never negative.
	if c.Policy.ActiveSlots < 0 {
		return fmt.Errorf("active_slots (%d) must be >= 0 (0 = auto-detect)", c.Policy.ActiveSlots)
	}

	// (d) puid/pgid are uids/gids; 0 disables chown, negative is meaningless.
	if c.Ownership.PUID < 0 {
		return fmt.Errorf("puid (%d) must be >= 0 (0 = disabled)", c.Ownership.PUID)
	}
	if c.Ownership.PGID < 0 {
		return fmt.Errorf("pgid (%d) must be >= 0 (0 = disabled)", c.Ownership.PGID)
	}
	// (d2/S5) chown needs BOTH puid and pgid: chownEnabled() requires both > 0, so setting
	// exactly one silently disables chown and produces the "arr can't move/import" failure
	// docs/20 §9 troubleshoots. Reject the half-set config with a clear, named error.
	if (c.Ownership.PUID > 0) != (c.Ownership.PGID > 0) {
		return fmt.Errorf("ownership.puid (%d) and ownership.pgid (%d) must both be set (>0) or both be 0 (chown disabled)",
			c.Ownership.PUID, c.Ownership.PGID)
	}

	// (e) webui basic auth: both username AND password must be set, or neither.
	if (c.WebUI.Username == "") != (c.WebUI.Password == "") {
		return fmt.Errorf("webui.username and webui.password must both be set or both be empty")
	}

	// (f2) on_cache_miss must be a recognized mode (empty = error).
	switch c.Policy.OnCacheMiss {
	case "", "error", "reject", "wait":
	default:
		return fmt.Errorf("on_cache_miss %q must be one of error|reject|wait", c.Policy.OnCacheMiss)
	}
	if c.Policy.OnCacheMiss == "wait" && c.Policy.CacheWaitBudget.D() <= 0 {
		return fmt.Errorf("cache_wait_budget must be > 0 when on_cache_miss is \"wait\"")
	}
	if c.Policy.MaxWaitDownloads < 0 {
		return fmt.Errorf("max_wait_downloads (%d) must be >= 0", c.Policy.MaxWaitDownloads)
	}
	if c.Policy.ReadaheadWindows < 0 || c.Policy.ReadaheadWindows > 32 {
		return fmt.Errorf("readahead_windows (%d) must be 0..32", c.Policy.ReadaheadWindows)
	}

	// (f) log_level must be a recognized name (empty = info).
	switch c.LogLevel {
	case "", "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level %q must be one of debug|info|warn|error", c.LogLevel)
	}

	return nil
}

// Validate exposes the post-load validation for callers that construct or mutate a
// Config in memory (the Web UI settings editor) before saving it.
func (c *Config) Validate() error { return c.validate() }

// Save atomically writes c as YAML to path (tmp file + rename, mode 0600 — the file
// holds the TorBox API key). Callers should Validate() first.
func Save(path string, c *Config) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("config: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config: rename: %w", err)
	}
	return nil
}
