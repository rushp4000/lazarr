---
name: qbit-emu
description: "qBittorrent WebUI API emulation contract for Lazarr. Use when implementing or reviewing the internal/qbit package that presents Lazarr as a qBittorrent download client to Sonarr/Radarr/Lidarr/Readarr — the endpoints to implement, the torrents/info field set, and the 'report complete immediately from the checkcached size' trick."
user-invocable: true
license: MIT
metadata:
  author: lazarr
  version: "1.0.0"
---

# qBittorrent emulation (what the *arr suite needs)

The arrs talk to the qBittorrent WebUI API under `/api/v2/`. Implement enough that the
arr authenticates, adds, sees the item complete with a real path+size, categorizes, and
deletes.

## Endpoints to implement
- `POST /api/v2/auth/login` → body `Ok.` (200). `POST /api/v2/auth/logout` → 200.
- `GET /api/v2/app/version` (e.g. `v4.6.0`), `app/webapiVersion` (e.g. `2.9.3`),
  `app/buildInfo`, `app/preferences` (incl. `save_path`), `app/defaultSavePath`.
- `POST /api/v2/torrents/add` — params `urls` (magnet/http) and/or torrent file,
  `category`. On add: parse infohash+name, create a **virtual** release, call
  `checkcached` for files+sizes (**NO TorBox add**), build the symlink tree, return 200.
- `GET /api/v2/torrents/info` — array of torrent objects (fields below), filter by
  `category`/`hashes`.
- `GET /api/v2/torrents/properties`, `GET /api/v2/torrents/files` — per-hash detail.
- `POST /api/v2/torrents/delete` — `hashes`, `deleteFiles`; remove catalog+symlinks; if
  materialized, release from TorBox.
- `GET /api/v2/torrents/categories`, `POST createCategory`, `POST setCategory`,
  `POST removeCategories`.
- `POST /api/v2/torrents/pause|resume|topPrio` → no-op 200 (we're always "done").
- `GET /api/v2/sync/maindata`, `GET /api/v2/transfer/info`.

## `torrents/info` object — required fields
`hash` (arr matches the grab by this), `name`, `size` (from checkcached), `progress`
(**report 1.0**), `state` (**`pausedUP`** = complete → arr imports), `category`,
`save_path`, **`content_path`** (full path to the file the arr imports), `completed`,
`amount_left` (0), `completion_on`, `added_on`, plus zeros for `dlspeed/upspeed/eta/
ratio` and `seq_dl:false, f_l_piece_prio:false`.

## The trick
Because `checkcached` already gives file list + sizes, mark the torrent **complete the
instant it's added** (`progress=1.0, state=pausedUP, content_path=<symlink>`). No
download is simulated; no TorBox add happens. Size MUST be real (>0) — the arr rejects
0-byte files ("is this a sample?"). The arr hardlinks (fails on symlink → falls back to
move/rename of the symlink itself = no data read).

## Compatibility
Return `Ok.`/200 on auth (don't rely on qBit 5.2's 204). Categories = arr names. Serve
`torrents/info` from the catalog — never call TorBox per poll.

See `/root/Github/Lazarr/docs/03-arr-qbit-integration.md`.
