# 05 — Lazarr component spec (what to code)

Target of Option C (greenfield Go). Each component below maps to an `internal/` package.

## Data model (SQLite catalog)

```
release(
  hash TEXT PRIMARY KEY,        -- infohash from the magnet
  name TEXT,                    -- release/torrent name
  category TEXT,                -- = arr name
  magnet TEXT,                  -- original magnet/URL (to add later)
  total_size INTEGER,
  state TEXT,                   -- virtual | materialized | error
  cached INTEGER,               -- checkcached hit at grab? (0/1)
  torbox_id INTEGER,            -- set only while materialized
  added_on INTEGER,
  last_access INTEGER,          -- drives idle release
  created_at INTEGER
)
file(
  hash TEXT,                    -- FK release.hash
  file_id INTEGER,             -- TorBox file id within the torrent
  rel_path TEXT,               -- path within the virtual folder
  size INTEGER,
  PRIMARY KEY(hash, file_id)
)
dl_link(                        -- cache of presigned URLs, refreshed on 4xx
  hash TEXT, file_id INTEGER,
  url TEXT, fetched_at INTEGER, expires_at INTEGER,
  PRIMARY KEY(hash, file_id)
)
```

## 1. `qbit/` — qBittorrent WebUI emulation
Implements the endpoints in `03-arr-qbit-integration.md`. Behaviour on **add**:
1. Parse magnet (extract infohash + name) or .torrent upload (parse infohash + files).
2. Insert `release` (state=`virtual`) under the posted category.
3. Call `torbox.CheckCached([hash], list_files=true)`:
   - **cached** → store `file` rows (names+sizes), `total_size`, `cached=1`.
   - **not cached** → fall back to `torbox.TorrentInfo(hash)` for names+sizes; mark
     `cached=0`. If that also fails → mark `error` and report the torrent as errored
     (arr will grab a different release). *(Default policy: only accept cached; uncached
     acceptance is a config toggle — see config.)*
4. Create the **symlink tree**: `symlink/<category>/<name>/<rel_path>` → `vfs` path.
5. Thereafter `torrents/info` reports the release as **complete**
   (`progress=1.0, state=pausedUP, content_path=<symlink>`).

**No TorBox add happens here.** This is the ToS-compliant core.

## 2. `torbox/` — TorBox client
Thin wrapper (reference: decypharr `torbox.go`). Methods:
- `CheckCached(hashes []string, listFiles bool) → map[hash]{Size, Files[]}`
- `TorrentInfo(hash, timeout) → Files[]` (BT-scrape fallback)
- `CreateTorrent(magnet, addOnlyIfCached bool) → {id, hash}`
- `RequestDL(torrentID, fileID) → url` (`token,…,redirect=true`)
- `ControlDelete(torrentID)` (release)
- `MyList(offset) / MyListByID(id)` (audit + confirm materialize)
- `UserMe() → {plan, activeSlots}` (read slot budget at boot)
All calls: `Authorization: Bearer <key>`; retry/backoff; treat 4xx on `requestdl` as
"refresh needed". Batch `checkcached` ≤100 hashes.

## 3. `vfs/` — FUSE virtual tree
Mount at e.g. `/data/torbox`. Layout `/<hash>/<rel_path>` (one dir per release).
- `Getattr`/`Lookup`/`Readdir`: serve sizes + names **from the catalog** (no TorBox
  call) → `stat()`/`ls` work instantly, so the arr import and Plex's size checks pass
  **without materializing**.
- `Open`/`Read(offset, len)`: the **materialize trigger** → delegate to `materialize`
  for that `(hash, file_id)` and return the requested byte range.
- Update `release.last_access = now` on every read (drives the idle reaper).

## 4. `materialize/` — the lazy engine
On first `Read` for a release:
1. **Slot check:** if active materialized count ≥ `activeSlots`, run the reaper /
   LRU-release the least-recently-used idle release first.
2. If `torbox_id` unset → `CreateTorrent(magnet, addOnlyIfCached=!allowUncached)` →
   store `torbox_id`. (If add returns "not cached" and uncached disabled → error the
   read.)
3. Ensure a fresh `dl_link` for the file: if missing/near-expiry → `RequestDL` → cache.
4. **Proxy** the requested range: HTTP `Range: bytes=a-b` GET to the CDN URL, stream
   bytes back to FUSE. On **HTTP 400/403/410** → invalidate `dl_link`, `RequestDL`
   again once, retry (this is the #179 fix).
5. **Idle reaper** (background, every ~30–60s): for any `materialized` release with
   `now - last_access > idle_ttl` → `ControlDelete(torbox_id)`, clear `torbox_id`,
   state→`virtual`. Symlinks/catalog untouched → item still in library, re-materializes
   on next play.
6. **Hard-max reaper** (belt-and-suspenders): force-release anything materialized
   longer than `max_hold` regardless of access (default well under 30 days, e.g. 24h).

### Concurrency / slots
- `activeSlots` read from `UserMe()` (Essential is small). Materializer holds a
  semaphore of that size. Sequential playback of one title across episodes reuses the
  same release; distinct concurrent streams compete for slots → queue/evict.
- A small **readahead** window keeps playback smooth without fetching the whole file.

## 5. `symlink/` — category tree
- Owns `<download_dir>/<category>/<name>/<rel_path>` symlinks → `vfs` paths.
- Create on add; remove on `torrents/delete`.
- `<download_dir>` is the path the arr has configured as the qBit "save path" for that
  category (must be visible to the arr container at the same path).

## 6. Metadata-probe strategy (the single-tenant gap vs CatBox)
CatBox returns crowdsourced synthetic probe data; we can't. Lazarr's approach, in
order of preference:
- **Default — materialize-on-read with idle release.** Plex's header scan (the stack
  already disabled deep analysis: `ButlerTaskDeepMediaAnalysis=0`, chapter/intro/ad/
  music = never) reads only a few MB → a brief materialize → released by the idle
  reaper minutes later. Cheap and correct. **Start here.**
- **Phase 2 deliverable (confirmed) — probe-header cache.** On the first materialize,
  capture the file's header region (the first N MiB ffprobe/Plex reads) and cache it
  locally; serve subsequent metadata scans from that cache so a freshly imported item's
  Plex scan does **not** trigger a TorBox add. Critical because each new import otherwise
  costs one add against the ~60/hr limit — bulk imports/recovery would flood it. Keyed by
  (hash,file_id); small bounded on-disk cache.
- Avoid faking headers from release names — fragile, breaks players.

## 7. Config (`config.yaml`)
```yaml
torbox:
  api_key: "<YOUR-TORBOX-KEY>"     # from decypharr config debrids[torbox].api_key
  api_base: "https://api.torbox.app"
qbit:
  listen: ":8080"
  username: "lazarr"               # what the arr will send (optional check)
  password: "lazarr"
paths:
  download_dir: "/data/symlinks"   # arr's qBit save path (per-category subdirs)
  fuse_mount:  "/data/torbox"
categories: ["radarr_hin"]         # start: canary only
policy:
  allow_uncached: false            # default cached-only (ToS + reliability)
  idle_ttl: "15m"                  # release after this much no-access
  max_hold: "24h"                  # hard ceiling, well under 30d
  active_slots: 3                  # concurrent materializations; SET TO YOUR PLAN'S SLOTS.
                                   # Essential=3 (default). Higher plan -> raise it.
                                   # Never exceed your plan. 0 = auto-detect from /user/me.
  probe_cache: true                # Phase 2: cache file headers so Plex scans don't re-add
torznab:
  enabled: false                   # Phase 3, optional Prowlarr endpoint
```

## 8. Observability
- `/health` + `/metrics` (materialized count, slot usage, releases/min, 4xx-refreshes).
- A `/ui` later (Phase 3). For canary, structured logs are enough:
  log every grab (cached? size), materialize (add id), refresh, release.
- **ToS audit log:** periodically diff `MyList()` against our `materialized` set —
  assert the account holds nothing we think is released. This is the compliance proof.
