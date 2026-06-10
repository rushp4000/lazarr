// Package materialize is the lazy engine: add->requestdl->proxy->idle-release, with a
// configurable slot semaphore (default 3), link-refresh-on-4xx, idle + max-hold reapers,
// the probe-header cache, and the ToS-audit loop. Phase 2; built by Agent M (docs/09).
// Implements vfs.Materializer. See docs/05 §4 + docs/11 + docs/12. Use torbox-api skill.
package materialize

import (
	"context"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/config"
	"github.com/rushp4000/lazarr/internal/torbox"
)

// Deps are the engine's dependencies (wired in main).
type Deps struct {
	Store  catalog.Store
	TorBox torbox.Client
	// Policy carries the materialization knobs: AllowUncached, IdleTTL, MaxHold,
	// ActiveSlots (0 = auto-detect the slot count from TorBox.UserMe()), and ProbeCache.
	Policy config.Policy
	// ProbeCacheDir is a bounded on-disk dir for cached file-header regions, so Plex
	// header scans of freshly imported items don't trigger a fresh TorBox add. Required
	// when Policy.ProbeCache is true.
	//
	// Reaper interval, link-refresh statuses, and the createtorrent budget are read from
	// internal/constants (not tunable here). Each read fetches exactly its window — there
	// is no readahead widening (it only wasted CDN bandwidth; see proxy.getRange).
	ProbeCacheDir string
}

// RepairEntry describes a single release whose content is no longer available on TorBox's
// CDN. JSON tags are the Web UI wire format (/api/repair/scan), snake_case like the rest.
type RepairEntry struct {
	Hash     string `json:"hash"`
	Name     string `json:"name"`
	Category string `json:"category"`
}

// Engine is the concrete materializer (built by Agent M). It must satisfy
// vfs.Materializer: ReadAt(hash, fileID, p, off) and Release(hash).
type Engine interface {
	ReadAt(hash string, fileID int, p []byte, off int64) (int, error)
	Release(hash string) error
	// AuditTOS diffs TorBox mylist against our materialized set; logs/alarms leaks.
	// Scoped to Lazarr-added ids while the account is shared with decypharr (docs/12).
	AuditTOS() error
	// RepairScan batch-checks every catalogued hash against TorBox's checkcached endpoint
	// (no TorBox adds). Marks each release's CacheStatus in the catalog, then returns
	// the evicted set. Runs daily via the repair ticker in main.
	RepairScan(ctx context.Context) ([]RepairEntry, error)
	Close() error
}
