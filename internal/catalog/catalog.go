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
	// StateDownloading: on_cache_miss=wait — TorBox is fetching the uncached torrent;
	// the qbit wait-poller watches progress/ETA and flips it to virtual (released,
	// now cached) or error (over budget / stalled). The item IS on the account.
	StateDownloading State = "downloading"
)

// CacheStatus reports whether the content is still available on TorBox's CDN.
// Set by the repair scanner (engine.RepairScan). "evicted" means checkcached returned
// false — the content is gone and playback would fail with ErrPurged.
type CacheStatus string

const (
	CacheStatusUnknown CacheStatus = ""        // not yet checked
	CacheStatusCached  CacheStatus = "cached"  // confirmed available
	CacheStatusEvicted CacheStatus = "evicted" // no longer on TorBox CDN
)

// Release is one grabbed item (one torrent), keyed by infohash.
//
// JSON tags are the Web UI wire format (/api/releases, /api/repair) — snake_case,
// matched by the dashboard JS. Magnet is excluded: it is internal plumbing and can
// be very large (full trackers list) for zero UI value.
type Release struct {
	Hash       string `json:"hash"` // infohash
	Name       string `json:"name"`
	Category   string `json:"category"` // = arr name
	Magnet     string `json:"-"`        // original magnet/URL (to add at materialize time)
	TotalSize  int64  `json:"total_size"`
	State      State  `json:"state"`
	Cached     bool   `json:"cached"`      // checkcached hit at grab time?
	TorBoxID   int64  `json:"torbox_id"`   // set only while materialized
	AddedOn    int64  `json:"added_on"`    // unix; when added to the catalog (grab time)
	LastAccess int64  `json:"last_access"` // unix; drives the idle reaper
	// MaterializedAt is the unix time the release last entered StateMaterialized
	// (0 when not materialized). The max-hold reaper measures the hold window from
	// THIS, not AddedOn — a release grabbed long before its first playback must not
	// be an instant max-hold candidate the moment it materializes (add/delete churn).
	MaterializedAt int64       `json:"materialized_at"`
	CreatedAt      int64       `json:"created_at"`
	CacheStatus    CacheStatus `json:"cache_status"`     // set by repair scanner; "" = not yet checked
	LastCacheCheck int64       `json:"last_cache_check"` // unix; when CacheStatus was last set
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

// ReleaseFilter is the query for ListReleases (Web UI table).
type ReleaseFilter struct {
	Q        string // substring match on name or hash (case-insensitive); empty = all
	State    State  // empty = all states
	Category string // empty = all categories
	Limit    int    // 0 → default (50)
	Offset   int
}

// Store is the catalog contract. All timestamps are unix seconds.
type Store interface {
	UpsertRelease(r *Release, files []File) error
	GetRelease(hash string) (*Release, []File, error)
	ListByCategory(category string) ([]*Release, error)
	// ListReleases returns releases matching f with a total count (for pagination).
	ListReleases(f ReleaseFilter) ([]*Release, int, error)
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
	// DownloadingReleases returns releases in StateDownloading (on_cache_miss=wait).
	// The qbit wait-poller resumes these after a restart; the ToS audit counts them
	// as legitimately held.
	DownloadingReleases() ([]*Release, error)
	// ListAllHashes returns every hash in the catalog. Used by the repair scanner to
	// batch-check availability with TorBox's checkcached endpoint (no TorBox adds).
	ListAllHashes() ([]string, error)
	// SetCacheStatus updates the cache_status and last_cache_check for a release.
	// Called by engine.RepairScan after each checkcached batch.
	SetCacheStatus(hash string, status CacheStatus, checkedAt int64) error
	// ListEvicted returns releases whose CacheStatus is CacheStatusEvicted, newest first.
	// These are items that are no longer available on TorBox's CDN.
	ListEvicted() ([]*Release, error)
	GetLink(hash string, fileID int) (*DLLink, error)
	SetLink(l *DLLink) error
	DeleteRelease(hash string) error
	Close() error
}
