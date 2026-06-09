// Package constants holds verified TorBox limits and Lazarr defaults.
// Values were confirmed live against the real TorBox account on 2026-06-08/09.
// See docs/11-constraints-and-constants.md.
package constants

import "time"

const (
	TorBoxAPIBase = "https://api.torbox.app/v1/api"

	// Verified TorBox limits (Essential plan).
	EssentialActiveSlots = 3
	CheckCachedBatchMax  = 100
	CreateTorrentPerHour = 60 // observed "60 per 1 hour"
	CreateTorrentBudget  = 55 // stay under the limit
	MyListPageMax        = 1000

	// Endpoint paths (relative to TorBoxAPIBase). Contracts verified live.
	EpCheckCached   = "/torrents/checkcached"    // GET  hash,format=object,list_files=true
	EpTorrentInfo   = "/torrents/torrentinfo"    // GET  hash,timeout
	EpCreateTorrent = "/torrents/createtorrent"  // POST magnet,add_only_if_cached
	EpRequestDL     = "/torrents/requestdl"      // GET  token,torrent_id,file_id,redirect=false
	EpControl       = "/torrents/controltorrent" // POST {torrent_id,operation:"delete"|"reannounce"}
	EpMyList        = "/torrents/mylist"         // GET  offset,limit,bypass_cache
	EpUserMe        = "/user/me"                 // GET  settings=true
)

// Lazarr policy defaults (overridable via config.yaml).
var (
	DefaultIdleTTL      = 15 * time.Minute
	DefaultMaxHold      = 24 * time.Hour
	DefaultReaperEvery  = 30 * time.Second
	DefaultReadahead    = int64(8 << 20) // 8 MiB
	DefaultActiveSlots  = EssentialActiveSlots
	LinkRefreshStatuses = []int{400, 403, 410} // re-request presigned URL then retry
)
