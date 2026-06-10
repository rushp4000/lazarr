# Lazarr architecture

How Lazarr works internally, which assumptions are **TorBox-specific**, and exactly what
would have to change to support another debrid provider (Real-Debrid etc.). Lazarr is
deliberately **TorBox-only today** — this file is the map for anyone (including future
maintainers) who wants to change that.

## Component map

```
cmd/lazarr ──── wiring, lifecycle, signals, settings save/restart
internal/
  qbit         qBittorrent WebUI emulation (the arr-facing API) + grab pipeline
               + on_cache_miss=wait download watcher
  catalog      SQLite store: releases (by infohash), files, presigned-link cache
  symlink      import-symlink tree management (download_dir -> fuse_mount targets)
  vfs          read-only FUSE filesystem  /<infohash>/<file path>
  materialize  the playback engine: slots, LRU, add/release lifecycle, range proxy,
               link refresh, readahead, reapers, ToS audit, repair scan, probe cache
  torbox       the ONLY provider client (HTTP, auth, decode, error taxonomy)
  webui        human dashboard + editable settings
  logging      slog tee -> ring buffer, runtime level
  metrics      Prometheus counters/gauges + /health
```

## The lifecycle, precisely

1. **Grab** (`qbit.handleAdd`): arr POSTs a magnet/.torrent. Lazarr extracts the
   infohash, calls **`checkcached`** (read-only — the central trick: file names + sizes
   with NO account mutation), stores a `Release{state: virtual}` + `File` rows, creates
   symlinks `download_dir/<category>/<name>/<file> -> fuse_mount/<hash>/<file>`, and
   reports the torrent "complete" instantly. Cache miss → `policy.on_cache_miss`:
   `error` | `reject` (arr falls back to next release) | `wait` (uncached TorBox add,
   real progress reported to the arr, budgeted by ETA; on completion the account copy is
   deleted — the content is now in TorBox's global cache — and the grab becomes a normal
   virtual import).
2. **Read** (`vfs` → `materialize.ReadAt`): first read of any file under
   `fuse_mount/<hash>/` triggers *materialization*: slot admission (semaphore sized to
   your plan's concurrent slots, LRU-evicting idle items under pressure, singleflight per
   hash), **`createtorrent`** with `add_only_if_cached`, **`requestdl`** for a presigned
   CDN URL, then SSRF-safe HTTP range-proxying of exactly the requested windows. A
   bounded parallel **readahead** (policy.readahead_windows) feeds sequential playback;
   a small on-disk **probe cache** absorbs Plex/arr header scans.
3. **Release**: idle reaper (`idle_ttl` since last read), max-hold reaper (`max_hold`
   since materialization), LRU eviction, qbit delete, shutdown, and boot reconciliation
   all converge on `controltorrent {operation: delete}` — the account returns to empty.
   A 5-minute **ToS audit** diffs the account against what Lazarr believes it holds and
   alarms on leaks; a daily **repair scan** re-runs `checkcached` over the whole catalog
   to find content TorBox evicted.

## TorBox-specific constraints (the provider contract)

Everything below is baked into `internal/torbox` + `internal/constants` and is **verified
against the live API**, not just docs. A second provider must replace or re-derive each:

| # | Assumption | Where it lives |
|---|---|---|
| 1 | **`checkcached` exists, is free, batches ≤100 hashes, returns per-file names+sizes without adding** — the entire grab path depends on this | `torbox.CheckCached`, `qbit.handleAdd` |
| 2 | `createtorrent` supports `add_only_if_cached=true` and is rate-limited **~60/hour** (error body text, no rate-limit headers) | `torbox.CreateTorrent`, `ErrRateLimited` |
| 3 | Presigned CDN links come from `requestdl`, live ~3h, pin one CDN node, and die with **4xx** (refresh) or **transport errors** (node down → refresh) or "not found" (**purged content** → mark errored, arr re-search) | `materialize/proxy.go`, `torbox.ErrLinkExpired/ErrNotFound` |
| 4 | CDN hosts are pinned to `*.tb-cdn.io` (SSRF guard) | `materialize/proxy.go cdnHostSuffix` |
| 5 | Concurrent-download **slots** per plan (Essential=3), readable from `user/me` (`active_slots`) | `materialize` slot semaphore, `torbox.UserMe` |
| 6 | Delete = `POST controltorrent {torrent_id, operation:"delete"}` (NOT a REST DELETE) | `torbox.ControlDelete` |
| 7 | `mylist` pages at ≤1000, needs `bypass_cache=true`, and **lags fresh adds ~1 min** (audit drift grace) | `torbox.MyList`, `materialize/audit.go` |
| 8 | ToS: no transfer hoarding; ~30-day retention; rolling-15-day bandwidth budget — drives `idle_ttl`/`max_hold` defaults and "release after wait-download completes" | policy defaults, `qbit/waitpool.go` |
| 9 | Uncached adds expose `download_state/progress/eta/download_speed` via `mylist` (drives `on_cache_miss=wait`) | `torbox.TorrentDetail`, `qbit/waitpool.go` |
| 10 | API quirk: Cloudflare 403s some user agents (e.g. python-urllib); Go's default client is fine | ops knowledge, docs |

## What adding Real-Debrid (or another provider) would take

1. **Extract a provider interface** from `torbox.Client` (it is already an interface —
   the work is removing TorBox-isms from its *semantics*):
   - RD has **no free checkcached** since 2024 (instant-availability endpoint removed!)
     — the grab path's "size without adding" primitive must be rethought per provider
     (RD path: add → select files → immediately delete? That mutates the account at grab
     time and burns API quota; or BT-scrape via `torrentinfo`-equivalent).
   - RD links come from `unrestrict/link` per file and expire differently; no
     `add_only_if_cached`; different host pin (`*.real-debrid.com` download hosts).
   - RD's concurrency model is different (no slot concept like TorBox's; instead device
     and traffic limits).
2. **Parameterize the policy engine**: slots, rate budgets, retention windows, and the
   ToS-audit semantics are all per-provider numbers/behaviors.
3. **Catalog**: add a `provider` column to `release` (today implicit = torbox) and keep
   per-provider torrent ids.
4. **Config**: `torbox:` becomes `providers: [{type: torbox, ...}, {type: realdebrid,
   ...}]` with per-category provider routing (an arr category maps to one provider).
5. **Keep `qbit`, `vfs`, `symlink`, `webui` unchanged** — they are provider-agnostic by
   design; only `materialize`'s provider calls and the grab path's cache-check move
   behind the interface.

Until then: for Real-Debrid, run decypharr/Zurg side-by-side — Lazarr replaces only the
TorBox leg.

## Privacy / telemetry

Lazarr makes outbound connections to exactly two places: `api.torbox.app` (the API) and
`*.tb-cdn.io` (the content CDN). There is **no telemetry, no phone-home, no update
check, no analytics** — nothing is collected or sent anywhere else, ever. The Web UI
serves everything from the embedded binary (no CDN fonts/scripts).
