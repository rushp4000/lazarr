// Package webui is Lazarr's opt-in human dashboard. It serves a browser UI on a
// separate listener (webui.listen in config.yaml), kept off the arr-facing qbit port
// and the Prometheus admin port. The UI is unauthenticated by default (trusted-LAN
// model) but supports HTTP Basic Auth when webui.username + webui.password are set.
//
// The UI communicates with the browser via a JSON API at /api/* and renders HTML
// using Go html/template. No external build step is required; all assets are embedded
// in the binary via go:embed.
//
// Security: torbox.api_key is never rendered or logged by this package — the settings
// endpoint accepts a new key (write-only) but only ever reports whether one is set.
// All mutating endpoints (force-release, audit, settings, restart) accept POST only
// and respect the auth middleware.
package webui

// MaterializedItem is the web UI view of one live in-memory materialized release
// ("active stream" in UI language), joined with its catalog identity so humans see
// titles, not infohashes.
type MaterializedItem struct {
	Hash       string `json:"hash"`
	Name       string `json:"name"`     // catalog release name ("" if unknown)
	Category   string `json:"category"` // arr name
	TotalSize  int64  `json:"total_size"`
	TorBoxID   int64  `json:"torbox_id"`
	Refs       int    `json:"refs"`
	LastUsedNs int64  `json:"last_used_ns"`
}

// AccountInfo is a cached snapshot of the TorBox /user/me response.
// May be nil if the account is not yet fetched or the fetch failed.
type AccountInfo struct {
	Plan          int    `json:"plan"`
	ActiveSlots   int    `json:"active_slots"`
	CooldownUntil string `json:"cooldown_until"`
	LongTermStore bool   `json:"long_term_store"`
}

// StatusSnapshot is the /api/status response body.
type StatusSnapshot struct {
	Version       string       `json:"version"`
	UptimeSeconds int64        `json:"uptime_seconds"`
	Mounted       bool         `json:"mounted"`
	SlotsInUse    int          `json:"slots_in_use"`
	SlotsTotal    int          `json:"slots_total"`
	LastAuditUnix int64        `json:"last_audit_unix"`
	Account       *AccountInfo `json:"account,omitempty"`
}

// SafeConfig is the effective configuration with secrets redacted (api_key, passwords).
// Kept for the read-only view; the editable form uses Settings.
type SafeConfig struct {
	TorBoxAPIBase string   `json:"torbox_api_base"`
	QBitListen    string   `json:"qbit_listen"`
	AdminListen   string   `json:"admin_listen"`
	WebUIListen   string   `json:"webui_listen"`
	DownloadDir   string   `json:"download_dir"`
	FuseMount     string   `json:"fuse_mount"`
	DBPath        string   `json:"db_path"`
	Categories    []string `json:"categories"`
	AllowUncached bool     `json:"allow_uncached"`
	IdleTTL       string   `json:"idle_ttl"`
	MaxHold       string   `json:"max_hold"`
	ActiveSlots   int      `json:"active_slots"`
	ProbeCache    bool     `json:"probe_cache"`
	OwnershipPUID int      `json:"ownership_puid"`
	OwnershipPGID int      `json:"ownership_pgid"`
	AuthEnabled   bool     `json:"auth_enabled"`
}

// Settings is the editable configuration exchanged with the settings page.
//
// GET /api/settings returns it with TorBoxAPIKey and WebUIPassword ALWAYS empty;
// the *Set flags tell the form whether a value exists. POST /api/settings accepts
// the same shape: an empty TorBoxAPIKey / WebUIPassword means "keep the current
// value" (so the form can save without re-entering secrets).
type Settings struct {
	LogLevel string `json:"log_level"` // debug|info|warn|error; applied live on save

	TorBoxAPIKey    string `json:"torbox_api_key,omitempty"` // write-only
	TorBoxAPIKeySet bool   `json:"torbox_api_key_set"`
	TorBoxAPIBase   string `json:"torbox_api_base"`

	QBitListen   string `json:"qbit_listen"`
	QBitUsername string `json:"qbit_username"`
	QBitPassword string `json:"qbit_password"` // shown: the arr needs it to connect

	Categories []string `json:"categories"` // one per arr instance

	DownloadDir   string `json:"download_dir"`
	FuseMount     string `json:"fuse_mount"`
	DBPath        string `json:"db_path"`
	ProbeCacheDir string `json:"probe_cache_dir"`

	AllowUncached bool   `json:"allow_uncached"`
	IdleTTL       string `json:"idle_ttl"` // Go duration string, e.g. "168h"
	MaxHold       string `json:"max_hold"` // Go duration string, e.g. "720h"
	ActiveSlots   int    `json:"active_slots"`
	ProbeCache    bool   `json:"probe_cache"`

	PUID int `json:"puid"`
	PGID int `json:"pgid"`

	MetricsListen string `json:"metrics_listen"`

	WebUIListen      string `json:"webui_listen"`
	WebUIUsername    string `json:"webui_username"`
	WebUIPassword    string `json:"webui_password,omitempty"` // write-only
	WebUIPasswordSet bool   `json:"webui_password_set"`
}

// SaveResult is the POST /api/settings response.
type SaveResult struct {
	Saved           bool `json:"saved"`
	RestartRequired bool `json:"restart_required"`
}
