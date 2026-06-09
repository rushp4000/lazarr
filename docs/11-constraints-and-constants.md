# 11 — Constraints & constants (all verified live 2026-06-08/09)

Single source of truth for the limits Lazarr must encode. Numbers marked ✅ were
confirmed against the real TorBox account / live arrs; others are documented defaults.

## TorBox — account & API (Essential plan, acct id 169538)
| Constant | Value | Source |
|---|---|---|
| API base | `https://api.torbox.app/v1/api` | ✅ verified (the `/v1` fixes the old 403) |
| Auth | `Authorization: Bearer <key>` | ✅ |
| Plan | `1` = Essential | ✅ `user/me` |
| **Concurrent active slots** | **3** (`additional_concurrent_slots: 0`) | ✅ pricing + user/me |
| `long_term_storage` | **false** | ✅ user/me (Essential purges) |
| `long_term_seeding` | **false** | ✅ user/me |
| Download cooldown | `cooldown_until` timestamp; **cached adds STILL succeed during cooldown** | ✅ (added BBB during active cooldown) |
| `createtorrent` rate limit | **~60 per 1 hour** (HTTP returns `detail:"60 per 1 hour", success:None`) | ✅ hit + recovered |
| `checkcached` batch | **≤100 hashes** per call (comma-sep); returns names+per-file sizes; **no add** | ✅ (batch of 2 ok; mylist unchanged) |
| `torrentinfo` | BT-scrape fallback; returns files+sizes for a hash; **no add**; slower | ✅ (BBB) |
| `requestdl` window | docs ~3h; treat as **refresh-on-4xx** (decypharr #179) | ✅ 206 stream |
| `mylist` | returns all account torrents (only ADDED items); `offset`/`limit`; **default cap ~1000** | ✅ (438 at off0, 0 at off1000) |
| Rate-limit headers | **none exposed** (only `cf-ray`) → must catch error bodies, not headers | ✅ |
| WebDAV | only-added, hides >1000 files, refreshes 15m → NOT the lazy core | research |

### Corrected call contracts (decypharr's docs were partly stale)
- **Release = `POST /torrents/controltorrent`** body `{torrent_id:<int>, operation:"delete"}`.
  Valid `operation` values: **`reannounce`, `delete`** (lowercase). `DELETE …/{id}` →
  *Method Not Allowed*; `operation:"Delete"` → *Invalid operation*. ✅ verified.
- **Add = `POST /torrents/createtorrent`** multipart `magnet`, `add_only_if_cached=true`.
  On a cached item returns `success:true, detail:"Found Cached Torrent. Using Cached
  Torrent.", data:{hash, torrent_id}`. ✅
- **Stream = `GET /torrents/requestdl?token=&torrent_id=&file_id=&redirect=false`** →
  CDN URL (`*.tb-cdn.io`); ranged GET → 206, `content-range` total == checkcached size. ✅

## Abuse / fair-use thresholds (see `12-torbox-tos-compliance.md`)
- Files stored **at least 30 days**; transfers **not accessed in >30 days are auto-removed**.
- Bandwidth abuse: **rolling 15-day usage** checked daily → warning → **3 days** to drop
  below threshold or ban. Don't waste bandwidth (bounded readahead).

## Lazarr policy defaults (tunable in config.yaml)
| Constant | Default | Rationale |
|---|---|---|
| `idle_ttl` | `15m` | release shortly after playback stops (≪30d) |
| `max_hold` | `24h` | hard ceiling regardless of access |
| `active_slots` | **`3`** (configurable; see guidance below) | concurrency cap for materializations |
| `allow_uncached` | `false` | cached-only default + toggle (your decision) |
| `link_refresh_on_status` | `[400,403,410]` | re-request presigned URL then retry |
| `checkcached_batch` | `100` | API max |
| `createtorrent_max_per_hour` | `55` | stay under the ~60/hr limit |
| `readahead` | `8 MiB` | smooth playback without over-fetching bandwidth |
| `reaper_interval` | `30s` | idle/max-hold sweep cadence |

## Arrs (live, verified)
- Client type Lazarr emulates: **`QBittorrent`** (Radarr/Sonarr download-client impl).
- Radarr qBit config fields: host, port, useSsl, urlBase, username, password,
  **`movieCategory`**, movieImportedCategory, …; Sonarr uses **`tvCategory`**. ✅ schema
- decypharr is wired as: host `192.168.7.133`, port `8282`, useSsl false, category =
  arr name, username `admin`. Lazarr mirrors this shape on its own port. ✅
- **Canary radarr_hin (7880): 0 movies, rootfolder `/movies` accessible, Radarr
  v6.1.1.10360, one QBittorrent client (Decypharr) currently.** Ready. ✅

### Choosing `active_slots` (user-configurable; default 3)
`active_slots` is a **config value, default `3`** (the TorBox Essential ceiling). Lazarr
never lets concurrent materializations exceed it; beyond it, it LRU-releases idle items
and queues. Guidance to put in the config comment + README:
- **Set it to your TorBox plan's concurrent-slot count.** Essential = **3** (default).
  Pro/Standard plans allow more — raise it to match, or you're leaving capacity unused.
- **Don't set it higher than your plan** — TorBox will reject the extra adds and playback
  will error instead of queueing.
- One slot is consumed per *active* materialization: each concurrent playing stream, plus
  briefly each Plex header-scan of a freshly imported item (mitigated by the Phase-2
  probe-header cache). If you regularly run more simultaneous streams than slots, either
  raise the plan or accept short queue waits.
- `0` = auto-detect from `user/me` (uses the plan's reported slots).

## Go constants block (drop into `internal/constants/constants.go`)
```go
package constants

import "time"

const (
	TorBoxAPIBase = "https://api.torbox.app/v1/api"

	// Verified TorBox limits (Essential plan).
	EssentialActiveSlots   = 3
	CheckCachedBatchMax    = 100
	CreateTorrentPerHour   = 60 // observed "60 per 1 hour"; budget to 55
	CreateTorrentBudget    = 55
	MyListPageMax          = 1000

	// Paths/methods (corrected contracts).
	EpCheckCached   = "/torrents/checkcached"   // GET  hash,format=object,list_files=true
	EpTorrentInfo   = "/torrents/torrentinfo"   // GET  hash,timeout
	EpCreateTorrent = "/torrents/createtorrent" // POST magnet,add_only_if_cached
	EpRequestDL     = "/torrents/requestdl"     // GET  token,torrent_id,file_id,redirect=false
	EpControl       = "/torrents/controltorrent"// POST {torrent_id,operation:"delete"|"reannounce"}
	EpMyList        = "/torrents/mylist"        // GET  offset,limit,bypass_cache
	EpUserMe        = "/user/me"                // GET  settings=true
)

var (
	DefaultIdleTTL      = 15 * time.Minute
	DefaultMaxHold      = 24 * time.Hour
	DefaultReaperEvery  = 30 * time.Second
	DefaultReadahead    = 8 << 20 // 8 MiB
	LinkRefreshStatuses = []int{400, 403, 410}
)
```
