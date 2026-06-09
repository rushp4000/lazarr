# 03 — How Sonarr/Radarr drive a qBittorrent client (the API Lazarr must emulate)

Sonarr/Radarr/Lidarr/Readarr share the same .NET download-client code. The
qBittorrent client talks to the **qBittorrent WebUI API** under `/api/v2/`. Lazarr
must implement enough of it that the arr (a) authenticates, (b) accepts an add, (c)
sees the item complete with a real path+size, (d) can categorize and delete.

Verified against rdt-client's qBit emulation (DeepWiki) and decypharr's qbit package —
both are working emulations the arrs already accept.

## The arr's lifecycle against the client

1. **Test/connect:** `POST /api/v2/auth/login` → then `GET /api/v2/app/webapiVersion`
   (and `app/version`). The connection test in Settings → Download Clients.
2. **Grab:** arr decides on a release, then `POST /api/v2/torrents/add` with the
   magnet/URL + `category=<arr name>`.
3. **Poll:** arr periodically `GET /api/v2/torrents/info?category=<arr>` (or
   `sync/maindata`) and **matches the release to a torrent by `hash`**.
4. **Detect completion:** arr watches `state` / `progress`. `progress==1.0` and a
   completed state (`pausedUP`) with a `content_path` ⇒ ready to import.
5. **Import:** arr reads `content_path`, imports the file(s) into the library
   (hardlink → falls back to move/rename for symlinks), then
6. **Cleanup:** `POST /api/v2/torrents/delete?hashes=<h>&deleteFiles=...` (governed by
   the arr's "Remove Completed" setting).

## Endpoints Lazarr MUST implement

| Endpoint | Method | Lazarr returns / does |
|---|---|---|
| `/api/v2/auth/login` | POST | `Ok.` (accept any/no creds; or check configured ones). qBit 5.2 returns 204 — return `Ok.`/200 like rdt-client to satisfy current arrs. |
| `/api/v2/auth/logout` | POST | 200 |
| `/api/v2/app/version` | GET | e.g. `v4.6.0` |
| `/api/v2/app/webapiVersion` | GET | e.g. `2.9.3` |
| `/api/v2/app/buildInfo` | GET | static lib versions (optional but nice) |
| `/api/v2/app/preferences` | GET | JSON incl. `save_path`, `temp_path`, queueing/categories off |
| `/api/v2/torrents/add` | POST | parse magnet/torrent + `category`; create catalog entry; **checkcached for files+sizes; NO TorBox add**; create symlink tree; return 200 |
| `/api/v2/torrents/info` | GET | array of torrent objects (fields below), filterable by `category`/`hashes` |
| `/api/v2/torrents/properties` | GET | per-hash detail (sizes, save_path) |
| `/api/v2/torrents/files` | GET | per-hash file list (name, size, progress, `index`) |
| `/api/v2/torrents/delete` | POST | remove catalog entry + symlinks; if materialized, release from TorBox |
| `/api/v2/torrents/categories` | GET | map of categories → save paths |
| `/api/v2/torrents/createCategory` | POST | register category (= arr name) |
| `/api/v2/torrents/setCategory` | POST | set category on hashes |
| `/api/v2/torrents/pause` `/resume` `/topPrio` | POST | no-op 200 (we're always "done") |
| `/api/v2/sync/maindata` | GET | snapshot {torrents keyed by hash, categories, server_state} |
| `/api/v2/transfer/info` | GET | aggregate speeds (can be zeros) |

### The `torrents/info` object — fields that matter for import

```jsonc
{
  "hash": "<infohash>",          // arr matches the grab by this
  "name": "<release name>",
  "size": 12345678901,            // total bytes — from checkcached
  "progress": 1.0,                // we report 1.0 immediately (it's "cached")
  "state": "pausedUP",            // completed → arr imports
  "category": "radarr_hin",
  "save_path": "/data/symlinks/radarr_hin",
  "content_path": "/data/symlinks/radarr_hin/<release>/<file>",  // arr imports from here
  "completed": 12345678901,
  "amount_left": 0,
  "completion_on": 1733690000,
  "added_on": 1733689000,
  "dlspeed": 0, "upspeed": 0, "eta": 0, "ratio": 0,
  "seq_dl": false, "f_l_piece_prio": false
}
```

**Key trick (same as decypharr/CatBox):** because TorBox `checkcached` already gives
us the file list + sizes, Lazarr can mark the torrent **complete the moment it's
added** — `progress=1.0`, `state=pausedUP`, `content_path` pointing at the symlink
tree — so the arr imports almost instantly. No download is simulated; no TorBox add
happens. The bytes are only ever fetched later, on read.

## Why the size must be real
The arr will **reject a 0-byte file** ("Unable to determine if file is a sample"). It
hardlinks/copies; on a symlink the hardlink fails (EPERM) and it falls back to
move/rename, which moves the **symlink file itself** without reading data — exactly
what we want (no materialize at import). This matches the live decypharr behaviour
(`copyUsingHardlinks: true` on all the stack's debrid arrs).

## Path model (how the symlink reaches the bytes)
```
arr root folder (library)        Lazarr download dir (category)        FUSE virtual tree
/movies/<Movie>/<file>  ──move── /data/symlinks/<cat>/<rel>/<file> ──►  /data/torbox/<id-or-hash>/<file>
   (symlink, after import)              (symlink created at grab)        (FUSE: stat=size, read=materialize)
```
- The **symlink** is what the arr imports (moves into the library).
- It points into the **FUSE tree**, where `stat()` returns the correct size (from the
  catalog) but `open()/read()` triggers materialize-on-demand (see `05-spec.md`).
- Plex already follows these symlinks (the stack runs Plex with `allow_other`-style
  access to `/decypharr_symlinks` + `/decypharr_mount`); Lazarr mounts the same way.

## Live stack endpoints (for the canary)
Arr instances (host 192.168.7.133), qBit client config points at Lazarr:
- radarr_hin **7880** ← **canary** (empty library, safe).
- radarr_rd 7881, radarr_4k 7879, sonarr_rd 8992, sonarr_4k 8990 (later cutover).
API keys per arr are in the decypharr config (`token` field) — same as recovery work.
