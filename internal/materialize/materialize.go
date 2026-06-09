// Package materialize is the lazy engine: add->requestdl->proxy->idle-release, with a
// configurable slot semaphore (default 3), link-refresh-on-4xx, idle + max-hold reapers,
// the probe-header cache, and the ToS-audit loop. Phase 2; built by Agent M (docs/09).
// Implements vfs.Materializer. See docs/05 §4 + docs/11 + docs/12. Use torbox-api skill.
package materialize

import (
	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/torbox"
)

// Deps are the engine's dependencies (wired in main).
type Deps struct {
	Store  catalog.Store
	TorBox torbox.Client
	// Slots caps concurrent materializations; 0 = auto from TorBox.UserMe().
	Slots int
}

// Engine is the concrete materializer (built by Agent M). It must satisfy
// vfs.Materializer: ReadAt(hash, fileID, p, off) and Release(hash).
type Engine interface {
	ReadAt(hash string, fileID int, p []byte, off int64) (int, error)
	Release(hash string) error
	// AuditTOS diffs TorBox mylist against our materialized set; logs/alarms leaks.
	// Scoped to Lazarr-added ids while the account is shared with decypharr (docs/12).
	AuditTOS() error
	Close() error
}
