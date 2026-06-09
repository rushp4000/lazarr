# 02 — TorBox API surface

Base URL: **`https://api.torbox.app/v1/api/...`** (the `/v1` prefix is required — VERIFIED
live in P0, see `08-p0-verification-results.md`; decypharr's `/api/...` strings are
relative to its own base). Auth: **Bearer token** (`Authorization: Bearer <API_KEY>`).
Our key lives in decypharr config `debrids[torbox].api_key` / `download_api_keys[0]`.

> **P0 STATUS (verified live 2026-06-08):** `user/me` ✅ (403 was the missing `/v1`),
> `checkcached?list_files=true` ✅ returns names+sizes with **no add** (mylist 392→392)
> and works for non-account hashes, `requestdl`+range GET ✅ HTTP 206 real bytes.
> `createtorrent`/`controltorrent` not yet exercised (gated on user go-ahead).

Verified against:
- decypharr source `pkg/debrid/providers/torbox/torbox.go` (the authoritative reference
  — exact paths/params below are taken from it)
- TorBox SDK docs (`TorBox-App/torbox-sdk-py`, `eliasbenb/TorBox.py`)
- TorBox Help Center (account restrictions, WebDAV, requestdl window)

> Base pinned in P0: **`/v1/api/...`** (resolved — see header note above).

## The endpoints Lazarr needs

### `GET /api/torrents/checkcached` — cache check, **NO add** ✅ core primitive
- Params: `hash` (comma-separated, **batch ≤100 hashes**), `format=object|list`,
  `list_files=true`.
- Returns: per-hash cache data; **with `list_files=true`, the file list with names +
  sizes**. decypharr treats `Size > 0` as "cached/available".
- **This is how we get name+size at grab time without touching the account.**

### `GET /api/torrents/torrentinfo` — BT-network scrape, **NO add** (fallback)
- Params: `hash`, `timeout` (default 10s).
- Returns: file list + sizes scraped from the BitTorrent network. Slower, can fail for
  dead/low-seed torrents. Use only when `checkcached` misses but we still want size for
  an uncached release.

### `POST /api/torrents/createtorrent` — add (used only at materialize time)
- Form params: `magnet` (or `file`), `name`, `seed`, `allow_zip`, `as_queued`, and
  **`add_only_if_cached=true`** (decypharr sets this to filter uncached). Returns the
  new **torrent id + hash**. Begins downloading immediately if an active slot is free.

### `GET /api/torrents/requestdl` — presigned stream URL
- Params: `token=<API_KEY>`, `torrent_id`, `file_id`, `redirect=true` (or `zip_link`).
- Returns: a CDN URL that serves real bytes (HTTP 206 on range requests).
- **Window:** "opens the link for **3 hours** for downloads; once a download is
  started, nearly unlimited time." In practice decypharr saw tokens go stale much
  sooner (issue #179) → **always be ready to re-request on HTTP 400 / 4xx.**

### `GET /api/torrents/mylist` (and `/mylist/?id=<id>`) — list / detail
- `/mylist?offset=N` paginates the account's torrents (what's currently **added**).
- `/mylist/?id=<id>` returns one torrent's detail; decypharr builds per-file links as
  `torbox://{id}/{fileId}` once `download_finished`.
- We use this to (a) confirm materialize succeeded, (b) audit that releases really got
  deleted (account stays empty when idle = ToS proof).

### `POST /api/torrents/controltorrent` — release ✅ (CORRECTED, verified live)
- JSON body: **`{ "torrent_id": <id>, "operation": "delete" }`** → `success:true,
  detail:"Torrent deleted successfully."` Valid `operation` (lowercase): `reannounce`,
  `delete`. **Do NOT use `DELETE …/{id}` (→ "Method Not Allowed") or `operation:"Delete"`
  (→ "Invalid operation").** decypharr's docs implied DELETE/{id}+action — that's stale.
- **This is the release step.**

### `GET /api/user/me?settings=true` — plan / slots
- Returns plan type (decypharr maps `1=essential, 2=pro, 3=standard`) → slot counts.
- NOTE: earlier this returned **403 err 1010** from our host. decypharr calls it
  successfully, so the fix is param/scope (it passes `settings=true`) or a transient
  IP/rate issue — **replay during P0** to read our real slot count.

## TorBox WebDAV — researched, **rejected as the lazy core**
- Auth = TorBox **email + password** (creds, not presigned) → *would* sidestep the
  #179 presigned-token bug.
- **BUT** it only exposes torrents you have **already added** to the account (not the
  whole cache), it **hides downloads >1000 files**, and it **refreshes every ~15 min**.
- ⇒ WebDAV is **not lazy** (only-added = still hoarding) and is too stale for instant
  playback. Rejected as the primary mechanism. *Possible* later use: once an item is
  materialized (added), stream it via WebDAV (creds auth) instead of `requestdl` to
  dodge #179 — but the 15-min appearance lag makes `requestdl` better for play-now.
  Keep WebDAV as a fallback transport idea only.

## Plan / rate-limit facts (TorBox Help Center)
- The binding limit is **"Allowed Active Slots"** — concurrent downloading **or**
  seeding items. Essential is the smallest; exact count to be read from `/user/me`.
  Implication: Lazarr must **cap concurrent materializations to the slot count** and
  release aggressively (our idle-release already does this).
- Retention: items left idle are purged after the cache window (~30 days). Lazarr's
  release step means we never approach this.
- WebDAV refresh 15 min; checkcached batches ≤100. Treat the API as low-QPS; cache
  cache-check results in SQLite.

## Endpoint → lifecycle mapping

| Lifecycle step | TorBox call | Adds to account? |
|---|---|---|
| Grab / size+name | `checkcached?list_files=true` (→ `torrentinfo` fallback) | **No** |
| Materialize (on read) | `createtorrent` → `requestdl` | Yes (temporarily) |
| Stream | range GET on the `requestdl` CDN URL | — |
| Refresh on 4xx | `requestdl` again | — |
| Release (idle) | `controltorrent/{id}` Delete | removes |
| Audit empty | `mylist` | — |
