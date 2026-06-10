package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validConfig returns a Config that passes validate(); cases mutate one field.
func validConfig() *Config {
	c := Default()
	c.Categories = []string{"radarr_hin"}
	c.Paths.DownloadDir = "/downloads"
	c.Paths.FuseMount = "/mnt/lazarr"
	c.Policy.IdleTTL = Duration(15 * time.Minute)
	c.Policy.MaxHold = Duration(24 * time.Hour)
	c.Policy.ActiveSlots = 3
	return c
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string // substring; "" means expect no error
	}{
		{
			name:   "valid",
			mutate: func(*Config) {},
		},
		{
			name:    "categories without download_dir",
			mutate:  func(c *Config) { c.Paths.DownloadDir = "" },
			wantErr: "download_dir is required",
		},
		{
			name:    "categories without fuse_mount",
			mutate:  func(c *Config) { c.Paths.FuseMount = "" },
			wantErr: "fuse_mount is required",
		},
		{
			name: "no categories tolerates empty paths",
			mutate: func(c *Config) {
				c.Categories = nil
				c.Paths.DownloadDir = ""
				c.Paths.FuseMount = ""
			},
		},
		{
			name:    "idle_ttl equal to max_hold",
			mutate:  func(c *Config) { c.Policy.IdleTTL = c.Policy.MaxHold },
			wantErr: "must be strictly less than max_hold",
		},
		{
			name: "idle_ttl greater than max_hold",
			mutate: func(c *Config) {
				c.Policy.IdleTTL = Duration(48 * time.Hour)
			},
			wantErr: "must be strictly less than max_hold",
		},
		{
			name:    "negative active_slots",
			mutate:  func(c *Config) { c.Policy.ActiveSlots = -1 },
			wantErr: "active_slots",
		},
		{
			name:   "zero active_slots is valid (auto-detect)",
			mutate: func(c *Config) { c.Policy.ActiveSlots = 0 },
		},
		{
			name:    "negative puid",
			mutate:  func(c *Config) { c.Ownership.PUID = -5 },
			wantErr: "puid",
		},
		{
			name:    "negative pgid",
			mutate:  func(c *Config) { c.Ownership.PGID = -5 },
			wantErr: "pgid",
		},
		{
			name: "valid puid/pgid set",
			mutate: func(c *Config) {
				c.Ownership.PUID = 1003
				c.Ownership.PGID = 1003
			},
		},
		{
			name:    "puid set without pgid (S5)",
			mutate:  func(c *Config) { c.Ownership.PUID = 1003; c.Ownership.PGID = 0 },
			wantErr: "must both be set",
		},
		{
			name:    "pgid set without puid (S5)",
			mutate:  func(c *Config) { c.Ownership.PUID = 0; c.Ownership.PGID = 1003 },
			wantErr: "must both be set",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)
			err := c.validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("validate() = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("validate() = nil, want error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("validate() = %q, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestLoad_RejectsInvalid proves Load wires validate() in (and wraps the error).
func TestLoad_RejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// categories set but no paths -> validate must reject.
	yaml := "categories: [radarr_hin]\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() = nil error, want validation failure")
	}
	if !strings.Contains(err.Error(), "config:") {
		t.Fatalf("Load() error %q missing wrap prefix %q", err, "config:")
	}
}

// TestLoad_Valid parses a complete, valid config including ownership.
func TestLoad_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := strings.Join([]string{
		"categories: [radarr_hin]",
		"paths:",
		"  download_dir: /downloads",
		"  fuse_mount: /mnt/lazarr",
		"policy:",
		"  idle_ttl: 15m",
		"  max_hold: 24h",
		"  active_slots: 0",
		"ownership:",
		"  puid: 1003",
		"  pgid: 1003",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if c.Ownership.PUID != 1003 || c.Ownership.PGID != 1003 {
		t.Fatalf("ownership = %+v, want 1003/1003", c.Ownership)
	}
	if c.Policy.ActiveSlots != 0 {
		t.Fatalf("active_slots = %d, want 0 (auto-detect)", c.Policy.ActiveSlots)
	}
}

// TestSaveLoadRoundTrip guards the Web UI settings editor: a Config written by Save
// must Load back identical (Duration marshal, log_level, secrets intact, mode 0600).
func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	c := Default()
	c.LogLevel = "debug"
	c.TorBox.APIKey = "k-123"
	c.Paths.DownloadDir = "/dl"
	c.Paths.FuseMount = "/fuse"
	c.Paths.DBPath = "/data/x.sqlite"
	c.Paths.ProbeCacheDir = "/data/probe"
	c.Categories = []string{"radarr_hin", "sonarr_hin"}
	c.Ownership = Ownership{PUID: 1003, PGID: 1003}
	c.WebUI = WebUI{Listen: ":8082"}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if err := Save(path, c); err != nil {
		t.Fatalf("save: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("config file mode = %v, want 0600 (holds api key)", fi.Mode().Perm())
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.LogLevel != "debug" || got.TorBox.APIKey != "k-123" ||
		got.Policy.IdleTTL.D() != c.Policy.IdleTTL.D() ||
		got.Policy.MaxHold.D() != c.Policy.MaxHold.D() ||
		len(got.Categories) != 2 || got.Categories[1] != "sonarr_hin" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestValidateLogLevel(t *testing.T) {
	c := Default()
	c.LogLevel = "loud"
	if err := c.Validate(); err == nil {
		t.Fatalf("expected log_level validation error")
	}
}
