---
name: torbox-api
description: "TorBox API contract for the Lazarr build. Use when writing or reviewing any code that calls TorBox (checkcached, createtorrent, requestdl, controltorrent, mylist, user/me) or when reasoning about the lazy-materialize lifecycle, presigned-link refresh, slots, or ToS compliance. Encodes the base URL, auth, exact params, and the rules verified live against the real account on 2026-06-08."
user-invocable: true
license: MIT
metadata:
  author: lazarr
  version: "1.0.0"
---

# TorBox API (verified contract)

Base: **`https://api.torbox.app/v1/api`** (the `/v1` prefix is required — its absence
caused the earlier 403 err1010). Auth: header `Authorization: Bearer <API_KEY>`.
Responses wrap data as `{ success, error, detail, data }`; `success:true` = OK.

## Endpoints (all verified live unless noted)

| Call | Method / path | Key params | Returns | Adds to account? |
|---|---|---|---|---|
| Cache check | `GET /torrents/checkcached` | `hash` (comma-sep, **batch ≤100**), `format=object`, `list_files=true` | per-hash `{name,size,files:[{id,name,size}]}` | **No** ✅ |
| BT scrape | `GET /torrents/torrentinfo` | `hash`, `timeout=10` | files+sizes from the swarm (slow, may fail) | **No** |
| Add | `POST /torrents/createtorrent` (multipart) | `magnet`, `add_only_if_cached=true` | `{torrent_id, hash}` + `detail:"Found Cached Torrent…"` | **Yes** |
| Stream URL | `GET /torrents/requestdl` | `token=<key>`, `torrent_id`, `file_id`, `redirect=false` | presigned CDN URL (`*.tb-cdn.io`) | no |
| **Release** | **`POST /torrents/controltorrent`** | JSON **`{torrent_id, operation:"delete"}`** (ops: `reannounce`,`delete`, lowercase) | success | removes |
| List | `GET /torrents/mylist` (`?id=` for one) | `offset`, `limit`, `bypass_cache=true` | account torrents (only ADDED items; cap ~1000) | no |
| Account | `GET /user/me` | `settings=true` | `plan(1=essential)`, `additional_concurrent_slots(=0)`, `cooldown_until`, `long_term_storage(false)` | no |

> ⚠️ Release is **POST** with `operation:"delete"` — NOT `DELETE …/{id}` (→ Method Not
> Allowed) and NOT `action:"Delete"` (→ Invalid operation). Verified live.
> **Essential = 3 concurrent slots**, `createtorrent` ~60/hr, cached adds work even
> during `cooldown_until`. See `docs/11-constraints-and-constants.md`.

## Hard rules (do not violate)

1. **Grab time = `checkcached` only.** Never `createtorrent` at grab. The arr's file
   name+size come from `checkcached` (or `torrentinfo` fallback). Verified: checkcached
   does NOT change `mylist` count and works for hashes not on the account.
2. **`createtorrent` is rate-limited (~`60 per 1 hour`).** Dedupe adds (one per
   release), queue, and back off on the `60 per 1 hour` / cooldown responses. Respect
   `cooldown_until` and the Essential active-slot budget (read from `user/me`).
3. **Presigned links:** `requestdl` opens a window (docs say ~3h; empirically stale
   sooner — decypharr #179). On HTTP **400/403/410** from the CDN, **re-request the
   link once and retry** the range. Cache links in the catalog with an expiry.
4. **Stream via ranged GET:** `Range: bytes=a-b` on the CDN URL → expect **206** with
   `content-range` total == the `checkcached` size. Map FUSE `Read(off,len)` to this.
5. **Release on idle** (`controltorrent` Delete) so `mylist` trends to ~0 at rest =
   ToS-compliant. Never hold an item beyond active use; enforce a hard `max_hold` ≪30d.
6. **Never log the API key**; never proxy a URL not derived from `requestdl`.

## Response shape notes
- `checkcached` `data` is an **object keyed by infohash**; each value has `name`,
  `size`, and (with `list_files=true`) `files[]` with per-file `id`/`size`.
- `mylist/?id=<id>` detail has `files:[{id,size,name,...}]` and `download_finished`.

See `/root/Github/Lazarr/docs/02-torbox-api.md` and `08-p0-verification-results.md`.
