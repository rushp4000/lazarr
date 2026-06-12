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
	// DefaultIdleTTL: release a materialized item after 7 days of no playback.
	// This matches the "watch-then-forget" model — items stay hot for a week so
	// re-watching doesn't cost a TorBox re-add, while still well under TorBox's
	// 30-day natural expiry. LRU eviction handles slot pressure when all 3 slots
	// are occupied and a new item needs materializing.
	DefaultIdleTTL     = 7 * 24 * time.Hour  // 7 days
	DefaultMaxHold     = 30 * 24 * time.Hour // 30 days — matches TorBox's own purge window
	DefaultReaperEvery = 30 * time.Second
	DefaultActiveSlots = EssentialActiveSlots
	// DefaultCloseDrain is how long Close waits for in-flight readers to release their refs
	// before force-releasing pinned entries on shutdown (B3 — never leak a TorBox item).
	DefaultCloseDrain      = 5 * time.Second
	LinkRefreshStatuses    = []int{400, 403, 410} // re-request presigned URL then retry
	DefaultRepairScanEvery = 24 * time.Hour       // how often the repair scan runs
	// QueuedDeferral is how long the engine waits before retrying createtorrent for a
	// hash TorBox has parked in its server-side queue ("Download already queued.",
	// account cooldown / slots full). Reads inside the window fail fast with NO TorBox
	// call: hot retries burn the 60/hr createtorrent budget without changing the
	// outcome, and arr import loops retry every ~60s otherwise.
	QueuedDeferral = 10 * time.Minute
	// RateLimitBackoff is the GLOBAL (account-wide) createtorrent pause after TorBox
	// answers "60 per 1 hour". The limit is a sliding hourly window, so a single stuck
	// item read every ~60s by an arr import loop otherwise issues ~120 createtorrent
	// calls/hour against a 60/hour budget — every one a guaranteed 429. While the
	// backoff is active EVERY materialize fast-fails with NO TorBox call, capping the
	// account to ~6 probe calls/hour until the window clears. Shorter than the full
	// hour so a transient spike recovers quickly once real headroom returns.
	RateLimitBackoff = 10 * time.Minute
)
