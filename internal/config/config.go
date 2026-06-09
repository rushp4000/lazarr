// Package config loads Lazarr's config.yaml. See docs/05-spec.md §7 and docs/11.
package config

import (
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

type Config struct {
	TorBox     TorBox   `yaml:"torbox"`
	QBit       QBit     `yaml:"qbit"`
	Paths      Paths    `yaml:"paths"`
	Categories []string `yaml:"categories"`
	Policy     Policy   `yaml:"policy"`
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
	return c, nil
}
