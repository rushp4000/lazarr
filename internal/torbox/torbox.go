// Package torbox is the TorBox API client contract. Endpoint paths, params and the
// verified response shapes are in docs/02-torbox-api.md + docs/08 + docs/11.
// The HTTP implementation is built by Agent T (see docs/09). Use the torbox-api skill.
package torbox

import "errors"

// ErrLinkExpired signals a presigned URL returned 4xx and must be re-requested.
var ErrLinkExpired = errors.New("torbox: presigned link expired (refresh)")

// ErrRateLimited signals the ~60/hour createtorrent limit ("60 per 1 hour").
var ErrRateLimited = errors.New("torbox: createtorrent rate limited")

// ErrNotFound signals the torrent/cache is gone at materialize time: createtorrent
// (cached-only) reports the hash is not cached / not found, or requestdl reports the
// torrent does not exist. This is the dead-cache case (TorBox purged a stale item) —
// distinct from a transient presigned-link 4xx (ErrLinkExpired), which is recoverable
// by re-requesting the link. The engine surfaces this as a permanent errored state so
// the arr blacklists and re-grabs rather than retrying forever.
var ErrNotFound = errors.New("torbox: torrent not found / not cached (purged)")

// CachedFile is a file within a cached/added torrent.
type CachedFile struct {
	ID   int
	Name string
	Size int64
}

// CachedItem is the result of checkcached/torrentinfo for one hash (NO add).
type CachedItem struct {
	Hash  string
	Name  string
	Size  int64
	Files []CachedFile
}

// TorrentDetail is an account torrent (from mylist), present only while ADDED.
type TorrentDetail struct {
	ID               int64
	Hash             string
	Name             string
	Files            []CachedFile
	DownloadFinished bool
	// Download-progress fields (populated while TorBox is fetching an uncached
	// add; they drive the on_cache_miss=wait mode). Progress is 0..1, ETA in
	// seconds, DownloadSpeed in bytes/s.
	DownloadState string
	Progress      float64
	ETA           int64
	DownloadSpeed int64
}

// Account is the relevant slice of /user/me.
type Account struct {
	Plan          int    // 1=essential
	ActiveSlots   int    // base concurrent slots (Essential=3) + additional
	CooldownUntil string // RFC3339; cached adds still succeed during cooldown
	LongTermStore bool   // Essential=false (purges)
}

// Client is the TorBox contract. Implementations must:
//   - batch CheckCached hashes <=100,
//   - return ErrLinkExpired on 4xx from a presigned URL,
//   - release via POST controltorrent {torrent_id, operation:"delete"}.
type Client interface {
	// CheckCached returns cache hits keyed by lowercase infohash, with file
	// names+sizes, WITHOUT adding to the account.
	CheckCached(hashes []string) (map[string]CachedItem, error)
	// TorrentInfo scrapes the BT network for files+sizes (no add); slower fallback.
	TorrentInfo(hash string) (*CachedItem, error)
	// CreateTorrent adds a torrent. With addOnlyIfCached, only cached items are added.
	CreateTorrent(magnet string, addOnlyIfCached bool) (id int64, hash string, err error)
	// RequestDL returns a presigned CDN URL for one file.
	RequestDL(torrentID int64, fileID int) (url string, err error)
	// ControlDelete releases (removes) a torrent from the account.
	ControlDelete(torrentID int64) error
	MyList(offset int) ([]TorrentDetail, error)
	MyListByID(id int64) (*TorrentDetail, error)
	UserMe() (*Account, error)
}
