# testdata/torbox — recorded TorBox API fixtures

These JSON files reproduce the **shapes** of real TorBox `/v1/api/...` responses, captured
from the live verification runs documented in `docs/08-p0-verification-results.md` and
`docs/11-constraints-and-constants.md`. They exist so the `internal/torbox` (and any
dependent) tests can serve responses via `httptest` with **zero live network calls**.

All responses follow the TorBox envelope: `{ "success": bool|null, "detail": string,
"data": ... }`.

| File | Endpoint | Notes |
|---|---|---|
| `checkcached_cached.json` | `GET /torrents/checkcached?format=object&list_files=true` | `data` is an **object keyed by lowercase infohash**; each value has `name`, `size`, `files[{id,name,size}]`. NO add. |
| `checkcached_miss.json` | same | cache miss → `data` is an **empty object** (hash key simply absent). Clients should also tolerate `data: null`. |
| `torrentinfo.json` | `GET /torrents/torrentinfo` | BT-scrape fallback; `data` is a **single object** (not keyed), `name`/`hash`/`size`/`files`. NO add. |
| `createtorrent_cached.json` | `POST /torrents/createtorrent` (cached) | `success:true`, `detail:"Found Cached Torrent. Using Cached Torrent."`, `data:{hash,torrent_id}`. |
| `createtorrent_ratelimited.json` | same (rate limited) | `success:null`, `detail:"60 per 1 hour"`. The ~60/hr createtorrent cap → map to `torbox.ErrRateLimited`. |
| `requestdl.json` | `GET /torrents/requestdl?redirect=false` | `data` is the presigned CDN URL **string** (`*.tb-cdn.io`). Token in URL is fake. |
| `controltorrent_delete_ok.json` | `POST /torrents/controltorrent` `{torrent_id,operation:"delete"}` | release OK. |
| `mylist.json` | `GET /torrents/mylist` | `data` is an **array** of account torrents (only ADDED items): `id`, `hash`, `name`, `size`, `download_finished`, `files[]`. |
| `user_me.json` | `GET /user/me?settings=true` | `plan:1` (Essential), `additional_concurrent_slots:0` → 3 base slots, `long_term_storage:false`, `cooldown_until`. |

Consistent test data across files:
- **Big Buck Bunny** `dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c` — cached, total `691087437`,
  3 files (mp4 id 0 / srt id 1 / nfo id 2). torrent_id `7654321` once added (createtorrent + mylist).
- **Sintel** `08ada5a7a6183aae1e09d831df6748d566095a10` — used for torrentinfo + a second mylist row.

> These are **fixtures, not live captures**: API keys/tokens are fabricated, byte sizes are
> plausible (not byte-exact). They encode the verified response *structure* only. Never put a
> real TorBox key in this directory.
