# Lazarr

**A self-hosted, ToS-compliant streaming bridge between the *arr suite and [TorBox](https://torbox.app).**

Lazarr looks like a qBittorrent download client to Sonarr/Radarr — but nothing is ever
downloaded. When an arr grabs a release, Lazarr verifies it is in TorBox's cache and
instantly places a file link in your library. When you press **play** (Plex, Jellyfin,
Emby…), Lazarr adds the torrent to your TorBox account on the fly and streams the bytes
straight to your player. After the item sits unwatched for an idle window, Lazarr
**removes it from TorBox again** — automatically, every time.

The result: an effectively infinite, instant library, while your TorBox account stays
clean. No hoarding, no 30-day purges silently breaking your files, no dead 0-byte
"downloads".

> **⚠️ Status: early.** Lazarr is young, single-tenant, and TorBox-only. It is running
> in production on the author's stack, but expect rough edges. Back up your arr configs
> before pointing them at anything new.
>
> **Open source, no support.** This is published as-is under MIT. There is no support,
> no SLA, and no roadmap commitments — issues and PRs are welcome but may not get a
> response. Fork freely.
>
> **🤖 This project is 100% AI-developed** (designed, written, reviewed, and tested by
> Claude, driven by a human operator). Read the code accordingly.

---

## How it works

```
  Sonarr/Radarr            Lazarr                       TorBox
       │   grab               │                            │
       ├────────────────────► │  checkcached (no add!)     │
       │                      ├──────────────────────────► │
       │   "complete"         │  symlink → virtual file    │
       │ ◄────────────────────┤                            │
       │   import (instant)   │                            │
       │                      │                            │
  Plex │   play ▶             │                            │
       ├────────────────────► │  add torrent + request CDN │
       │                      ├──────────────────────────► │
       │   bytes ◄────────────┤ ◄────────── stream ────────┤
       │                      │                            │
       │   (idle 7 days)      │  delete torrent            │
       │                      ├──────────────────────────► │
       │   play again ▶       │  ...repeats transparently  │
```

1. **Grab — instant, nothing added.** Lazarr checks the release against TorBox's cache
   (`checkcached` — a read-only call), records the file list and sizes, and creates
   symlinks into a virtual FUSE drive. The arr imports immediately. Your TorBox account
   is untouched.
2. **Play — materialize on demand.** The first read of the file (a player, or the arr's
   import probe) makes Lazarr add the torrent to TorBox, fetch a presigned CDN link, and
   range-proxy the bytes through the FUSE mount. First play costs a few extra seconds.
3. **Clean up — automatic.** A reaper releases items after `idle_ttl` unwatched (default
   7 days) and enforces a hard `max_hold` ceiling (default 30 days). When all streaming
   slots are full, the least-recently-used idle item is evicted to admit a new play.
   Your library files keep working — the next play just re-materializes.

### Why this is ToS-compliant

TorBox prohibits tools that artificially keep transfers active ("excessive transfer
retention") and purges unaccessed transfers after ~30 days. Eager-add tools collide with
this head-on: they push your whole library into the account, TorBox purges it, and your
files die. Lazarr never adds at grab time, releases after use, and continuously audits
its own behavior: a built-in **ToS audit** diffs your TorBox account against what Lazarr
believes it added and alarms on any leak. There is also a daily **availability scanner**
that detects content TorBox has evicted from cache so your arr can re-search it.

---

## Features

- **qBittorrent WebUI API emulation** — Sonarr/Radarr connect natively; one category per
  arr instance.
- **Cached-only by default** — only accepts releases TorBox already has (instant
  playback). Configurable cache-miss handling: surface an error, **reject so the arr
  instantly tries another release**, or **wait** — let TorBox download it with a real
  progress bar in the arr, bounded by a configurable ETA budget.
- **Configurable readahead** — parallel window prefetch sized for your bitrate, from
  light 1080p up to 4K remux.
- **FUSE virtual drive** — files exist at full size without using disk; reads are
  SSRF-safe range-proxies to TorBox's CDN with automatic link refresh on expiry *and* on
  dead CDN nodes.
- **Slot-aware** — respects your plan's concurrent-download slots (Essential = 3) with
  LRU eviction under pressure; auto-detects from your account if unset.
- **Web UI** — dashboard, library, active streams, content-health/repair, live logs, and
  a **fully editable settings page** (every config value, including the API key, log
  level applied live; restart button for the rest).
- **Self-auditing** — ToS audit every 5 minutes, daily availability scan, Prometheus
  `/metrics` + `/health`, structured logs with runtime-switchable levels.
- **Crash-safe** — boot reconciliation releases anything left on TorBox by an unclean
  exit; stale FUSE mounts are detected and recovered automatically.
- Single static Go binary, pure-Go SQLite, multi-arch Docker images (amd64/arm64).

---

## Installation

### Requirements

- Docker (or a Linux host with FUSE3 for bare-metal).
- A TorBox account + API key ([torbox.app/settings](https://torbox.app/settings)).
- Host `/etc/fuse.conf` must contain `user_allow_other` (one-time:
  `echo user_allow_other | sudo tee -a /etc/fuse.conf`).
- Sonarr/Radarr and your media server must mount the **same host data path at the same
  container path** as Lazarr (details below).

### 1. Compose

```yaml
services:
  lazarr:
    image: ghcr.io/rushp4000/lazarr:latest
    container_name: lazarr
    restart: unless-stopped          # required for the Web UI's Restart button
    ports:
      - "8780:8080"                  # qBittorrent API (arrs connect here)
      - "8781:8081"                  # Web UI
      - "8782:9090"                  # metrics /health (optional)
    volumes:
      - ./config:/config             # config.yaml + database
      - type: bind                   # shared data root (symlinks + FUSE mount)
        source: /srv/lazarr/data
        target: /data
        bind:
          propagation: rshared       # REQUIRED: propagates the FUSE mount
    cap_add:
      - SYS_ADMIN                    # REQUIRED to mount FUSE
    devices:
      - /dev/fuse
    security_opt:
      - apparmor:unconfined          # REQUIRED for FUSE in Docker
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:9090/health"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 20s
```

Lazarr runs as **root inside the container** (FUSE mounting in Docker requires effective
`CAP_SYS_ADMIN`, which a non-root `user:` does not get). Set `ownership.puid/pgid` in the
config to your arr's uid:gid instead — Lazarr chowns every symlink it creates so the arrs
can move them.

Your **arr and media-server containers** must bind the same host dir at the same path so
the symlinks resolve, e.g. add to each:

```yaml
    volumes:
      - type: bind
        source: /srv/lazarr/data
        target: /data
        bind:
          propagation: rslave
```

### 2. Config

Copy [`config.example.yaml`](config.example.yaml) to `./config/config.yaml`, set
`torbox.api_key`, and adjust paths if needed. Start the container. From then on,
**everything is editable in the Web UI** (`http://<host>:8781` → Settings) — it rewrites
config.yaml for you and offers a one-click restart.

### 3. Connect your arrs

In each Sonarr/Radarr: **Settings → Download Clients → ➕ → qBittorrent**

| Field    | Value                                          |
|----------|------------------------------------------------|
| Host     | your Docker host's IP                          |
| Port     | `8780` (or your mapping)                       |
| Username | `lazarr` (config `qbit.username`)              |
| Password | `lazarr` (config `qbit.password`)              |
| Category | one unique name per arr, e.g. `radarr`, `sonarr_4k` — must match a `categories:` entry in Lazarr |

The Web UI's Settings page shows this exact table with your live values, and the
category editor adds new arrs in two clicks.

#### Tested with

| Application | Version | Status |
|---|---|---|
| Radarr | v6.1.x | ✅ grab → import → playback, tested in production |
| Sonarr | v4.0.x | ✅ grab pipeline tested; season packs work via multi-file torrents |
| Plex | current | ✅ direct play through the FUSE mount |
| Lidarr / Readarr / Whisparr | — | ❓ untested; the qBittorrent emulation is generic |
| Jellyfin / Emby | — | ❓ untested; any player that reads files should work |

### Portainer / GitOps

The compose above works as a Portainer **Stack** (paste or point at a Git repo). Two
notes:

- While this GitHub repository/package is **private**, pulling from GHCR needs a
  registry credential in Portainer: *Registries → Add → Custom* →
  `ghcr.io` + your GitHub username + a PAT with `read:packages`.
- The `restart: unless-stopped` policy is what makes the Web UI's *Restart Lazarr*
  button work — keep it.

---

## Configuration reference

All values live in `config.yaml` and are editable from the Web UI (the API key is
write-only there: it can be replaced but never displayed).

| Key | Default | What it does |
|---|---|---|
| `log_level` | `info` | `debug` / `info` / `warn` / `error` — applied live, no restart |
| `torbox.api_key` | — | your TorBox API key (**required**) |
| `torbox.api_base` | `https://api.torbox.app/v1/api` | API base URL |
| `qbit.listen` | `:8080` | arr-facing qBittorrent API |
| `qbit.username` / `qbit.password` | `lazarr`/`lazarr` | credentials the arrs use |
| `categories` | `[]` | one per arr instance |
| `paths.download_dir` | — | where import symlinks are created (arr "save path") |
| `paths.fuse_mount` | — | the virtual drive mountpoint |
| `paths.db_path` | `/data/lazarr.sqlite` | catalog database |
| `paths.probe_cache_dir` | `/data/probe` | small on-disk header cache |
| `policy.allow_uncached` | `false` | accept releases TorBox hasn't cached yet |
| `policy.idle_ttl` | `168h` (7d) | remove from TorBox after this long unwatched |
| `policy.max_hold` | `720h` (30d) | absolute per-item ceiling on TorBox |
| `policy.active_slots` | `3` | concurrent items on TorBox (match your plan; `0` = auto) |
| `policy.probe_cache` | `true` | cache file headers so library scans don't cost adds |
| `policy.on_cache_miss` | `error` | `error` / `reject` (arr retries another release) / `wait` (TorBox downloads it) |
| `policy.cache_wait_budget` | `15m` | `wait` mode: bail if TorBox's ETA exceeds this |
| `policy.max_wait_downloads` | `1` | `wait` mode: concurrent TorBox downloads cap |
| `policy.readahead_windows` | `4` | parallel 1 MiB prefetch depth; 0 = off, 4–8 for 4K |
| `ownership.puid` / `pgid` | `0` | chown created symlinks to your arr's uid:gid |
| `metrics.listen` | `:9090` | Prometheus `/metrics` + `/health`; empty = off |
| `webui.listen` | `:8081` | Web UI; empty = off |
| `webui.username` / `password` | — | enable HTTP Basic Auth (recommended) |

---

## Security & reverse proxy

- **Set a Web UI login** (Settings → Web UI login, or `webui.username/password`). The
  dashboard can change every setting including the TorBox key, so on anything but a
  fully trusted LAN, auth on.
- The arr-facing qBittorrent port uses the configured username/password; the metrics
  port is unauthenticated — keep both LAN-only (don't port-forward any of them).
- **Reverse proxy (optional):** if you want the Web UI reachable from outside, put it
  behind your proxy with TLS, e.g. Caddy:

  ```
  lazarr.example.com {
      reverse_proxy 192.168.1.10:8781
  }
  ```

  or nginx:

  ```nginx
  location / {
      proxy_pass http://192.168.1.10:8781;
      proxy_set_header Host $host;
  }
  ```

  Only ever expose the Web UI port (8781) — never the qbit (8780) or metrics (8782)
  ports. Plain HTTP Basic Auth over the internet requires TLS at the proxy.

## Privacy

Lazarr talks to exactly two endpoints: `api.torbox.app` and TorBox's `*.tb-cdn.io`
content CDN. **No telemetry, no phone-home, no update checks, no analytics.** The Web UI
is fully embedded in the binary (no external fonts/scripts). See
[ARCHITECTURE.md](ARCHITECTURE.md) for the full internals and the TorBox-only provider
constraints.

---

## Observability

- `GET :8782/health` — JSON: mount health, slots in use, last audit.
- `GET :8782/metrics` — Prometheus counters (grabs, materializations, releases, link
  refreshes, rate limits, **`lazarr_tos_audit_leaks`** — alert if ever non-zero).
- Web UI → Logs — recent records with level filter; full stream in `docker logs`.

## FAQ

**Does playback ever touch my disk?** No — bytes are range-proxied from TorBox's CDN
through the FUSE mount to your player. The only local state is the SQLite catalog,
symlinks, and a tiny header cache.

**What happens if Lazarr crashes mid-stream?** On restart it clears the stale FUSE
mount, reconciles with TorBox, and releases anything it left behind. Your library
links are untouched.

**What if TorBox evicts content I imported?** The daily availability scan marks it in
the Web UI's Health tab; one click deletes the dead link so your arr re-searches a
fresh copy.

**Lidarr/Readarr/other clients?** The qBittorrent emulation is generic; only
Sonarr/Radarr are tested so far.

## License

[MIT](LICENSE)
