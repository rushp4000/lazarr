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

type Config struct {
	TorBox     TorBox    `yaml:"torbox"`
	QBit       QBit      `yaml:"qbit"`
	Paths      Paths     `yaml:"paths"`
	Categories []string  `yaml:"categories"`
	Policy     Policy    `yaml:"policy"`
	Ownership  Ownership `yaml:"ownership"`
	Metrics    Metrics   `yaml:"metrics"`
}

// Default returns a Config pre-populated with sane defaults; YAML overlays it.
func Default() *Config {
	return &Config{
		TorBox: TorBox{APIBase: constants.TorBoxAPIBase},
		QBit:   QBit{Listen: ":8080", Username: "lazarr", Password: "lazarr"},
		Policy: Policy{
			AllowUncached: false,
			IdleTTL:       Duration(constants.DefaultIdleTTL),
			MaxHold:       Duration(constants.DefaultMaxHold),
			ActiveSlots:   constants.DefaultActiveSlots,
			ProbeCache:    true,
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

	return nil
}
