// Package webui is Lazarr's opt-in human dashboard. It serves a browser UI on a
// separate listener (webui.listen in config.yaml), kept off the arr-facing qbit port
// and the Prometheus admin port. The UI is unauthenticated by default (trusted-LAN
// model) but supports HTTP Basic Auth when webui.username + webui.password are set.
//
// The UI communicates with the browser via a JSON API at /api/* and renders HTML
// using Go html/template. No external build step is required; all assets are embedded
// in the binary via go:embed.
//
// Security: torbox.api_key is never rendered or logged by this package. All mutating
// endpoints (force-release, audit) accept POST only and respect the auth middleware.
package webui

// MaterializedItem is the web UI view of one live in-memory materialized release.
type MaterializedItem struct {
	Hash       string `json:"hash"`
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
