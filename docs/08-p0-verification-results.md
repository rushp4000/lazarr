# 08 — Phase 0 verification results (run live, 2026-06-08)

All run from host `192.168.7.133` with the real TorBox key (decypharr config,
plan 1 Essential, account `sushi14921776@gmail.com`). **Read-only calls only** — no
add, no delete performed.

## ✅ API base pinned
`https://api.torbox.app/**v1/api**/...` (the `/v1/` prefix is required; decypharr's
source shows `/api/...` relative to its own configured base). Auth =
`Authorization: Bearer <key>`. **The old `user/me` 403 err1010 was just the missing
`/v1` base — resolved.**

## ✅ `user/me?settings=true`
`success:true`. `plan: 1` (Essential), `is_subscribed: true`,
`additional_concurrent_slots: 0`, **`cooldown_until: 2026-06-10T01:08:06Z`** (Essential
download cooldown currently active), `total_downloaded: 25`.
→ Essential = base slots only (no extras). Lazarr's slot semaphore must respect this
and the cooldown window; the release step is what makes a small plan workable.

## ✅ `checkcached?hash=<h>&format=object&list_files=true` — THE grab-time primitive
- Returned `success:true` with `name`, **`size: 2826926687`**, and the full **file list
  with per-file sizes** (`2826905698`, `20860`, `129` bytes — names included).
- **`mylist` count was 392 BEFORE and 392 AFTER** the call ⇒ **checkcached does NOT
  add to the account.** ✅ (This is the ToS-compliant core.)
- Also returned a hit for **Big Buck Bunny** (a hash NOT on our account) ⇒ we can size
  releases we've never added. ✅
- Implementation note: response `data` is an object keyed by infohash; batch hashes
  comma-separated (≤100).

## ✅ `requestdl` + ranged streaming — THE playback primitive
- `requestdl?token=<key>&torrent_id=<id>&file_id=<fid>&redirect=false` →
  presigned CDN URL on `https://nexus-138.snam.tb-cdn.io/...` (len ~116).
- Range GET `Range: bytes=0-1048575` → **HTTP 206 Partial Content**,
  `content-range: bytes 0-1048575/2826905698` (total **matches checkcached size
  exactly**), **1048576 real bytes** received. ✅
- ⇒ Lazarr's FUSE `Read(offset,len)` maps cleanly to a ranged GET on this URL. Seek =
  new range. Refresh-on-4xx covers #179.

## ✅ Account reality (quantifies the problem we're solving)
`mylist` = **392 torrents currently held on the TorBox account** — decypharr's
eager-add hoard. This is precisely the >30-day retention that TorBox purges into
dead-cache. Lazarr's lazy+release model means this number should sit near **0** at
idle.

## ⏸ NOT yet tested (mutating — needs user go-ahead)
The only two unverified calls, both simple, both needed for a full materialize cycle:
- `POST /v1/api/torrents/createtorrent` (`magnet`, `add_only_if_cached=true`) → adds.
- `DELETE /v1/api/torrents/controltorrent/{id}` (`{torrent_id, action:"Delete"}`) →
  releases.
Plan: test them as one **add→requestdl→release** cycle on a single throwaway cached
magnet (held to seconds) — the exact lazy lifecycle, ToS-compliant — once approved.

## Bottom line
Every assumption the architecture depends on is **confirmed against the live account**:
size-without-add ✅, presigned ranged stream ✅, auth/base ✅, slot/cooldown reality ✅.
Greenfield Lazarr is safe to build. Remaining unknown (add/release) is low-risk and
gated on approval.
