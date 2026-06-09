// Package catalog is the SQLite-backed store of releases, files, and link cache.
// Schema mirrors docs/05-spec.md §Data model. The Store interface is the foundation
// contract; the SQLite implementation is built by Agent C (see docs/09).
package catalog

// State is a release's materialization state.
type State string

const (
	StateVirtual      State = "virtual"      // symlinked, NOT on TorBox
	StateMaterialized State = "materialized" // added to TorBox, streamable
	StateError        State = "error"        // checkcached/torrentinfo failed, or dead-cache
)

// Release is one grabbed item (one torrent), keyed by infohash.
type Release struct {
	Hash       string // infohash
	Name       string
	Category   string // = arr name
	Magnet     string // original magnet/URL (to add at materialize time)
	TotalSize  int64
	State      State
	Cached     bool  // checkcached hit at grab time?
	TorBoxID   int64 // set only while materialized
	AddedOn    int64 // unix; when added to the catalog (grab time)
	LastAccess int64 // unix; drives the idle reaper
	// MaterializedAt is the unix time the release last entered StateMaterialized
	// (0 when not materialized). The max-hold reaper measures the hold window from
	// THIS, not AddedOn — a release grabbed long before its first playback must not
	// be an instant max-hold candidate the moment it materializes (add/delete churn).
	MaterializedAt int64
	CreatedAt      int64
}

// File is one file within a release.
type File struct {
	Hash    string // FK -> Release.Hash
	FileID  int    // TorBox file id within the torrent
	RelPath string // path within the virtual folder
	Size    int64
}

// DLLink caches a presigned CDN URL, refreshed on 4xx.
type DLLink struct {
	Hash      string
	FileID    int
	URL       string
	FetchedAt int64
	ExpiresAt int64
}

// Store is the catalog contract. All timestamps are unix seconds.
type Store interface {
	UpsertRelease(r *Release, files []File) error
	GetRelease(hash string) (*Release, []File, error)
	ListByCategory(category string) ([]*Release, error)
	SetState(hash string, st State, torboxID int64) error
	TouchAccess(hash string, ts int64) error
	// IdleCandidates returns materialized releases whose LastAccess is before ts.
	IdleCandidates(before int64) ([]*Release, error)
	// OverMaxHold returns materialized releases whose MaterializedAt is before ts
	// (hard ceiling, measured from materialize time — not grab time).
	OverMaxHold(before int64) ([]*Release, error)
	// MaterializedIDs returns the TorBox ids Lazarr believes are added (ToS audit).
	MaterializedIDs() ([]int64, error)
	// MaterializedReleases returns all releases currently in StateMaterialized. Drives the
	// boot-time reconciliation sweep that releases crash/restart leftovers (B2).
	MaterializedReleases() ([]*Release, error)
	GetLink(hash string, fileID int) (*DLLink, error)
	SetLink(l *DLLink) error
	DeleteRelease(hash string) error
	Close() error
}
