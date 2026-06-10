# 20 — Deploying Lazarr with Docker

Lazarr presents to Sonarr/Radarr as a **qBittorrent download client**. At grab time it
symlinks (NO TorBox add); on playback it lazily materializes via a FUSE mount, range-proxies
the bytes, and releases the item when idle. This guide gets it running with Docker.

> **Is there a web UI?** No human dashboard (yet). Lazarr's only HTTP surfaces are the
> **qBittorrent API** the *arr suite talks to (port 8080) and an optional **`/metrics`
> (Prometheus) + `/health` (JSON)** admin endpoint (see §6). You configure it via
> `config.yaml`, operate the catalog through the arrs, and observe it via Grafana/curl. A
> human UI is a possible future addition; today it is a headless daemon.

---

## 1. The image
Published to **GitHub Container Registry (GHCR)**: `ghcr.io/rushp4000/lazarr`. Tags:
`vX.Y.Z`, `vX.Y`, and `latest`, built multi-arch (`linux/amd64`, `linux/arm64`) by the
release workflow when a `v*` tag is pushed. Pull:

```bash
docker pull ghcr.io/rushp4000/lazarr:latest
```

(Or build locally: `docker build -t lazarr .` — pure-Go, `CGO_ENABLED=0`, alpine + fuse3.)

## 2. Host prerequisites
- `/dev/fuse` present on the host (load the `fuse` module if needed).
- The container needs `--cap-add SYS_ADMIN --device /dev/fuse --security-opt apparmor:unconfined`.
- A **shared data directory** on the host (e.g. `/srv/lazarr/data`) that Lazarr **and** your
  arrs **and** Plex all bind-mount at the **same path** — this is how they follow Lazarr's
  symlinks into the FUSE tree. See §4.

## 3. The privilege model (run as root + PUID/PGID) — read this
Mounting FUSE inside Docker needs an **effective** `CAP_SYS_ADMIN`. Docker only grants added
caps effectively to **uid 0**, so Lazarr must run as **root** (do **not** set `user:`). But
your arrs run as their own uid and must be able to move the symlinks Lazarr creates on import.
So set in `config.yaml`:

```yaml
ownership:
  puid: 1000   # the uid your Sonarr/Radarr runs as
  pgid: 1000   # its gid
```

Lazarr (as root) then `chown`s every symlink + directory it creates to `puid:pgid`, so the
arrs can move them. `puid/pgid: 0` disables chowning (links stay root-owned).

## 4. Path sharing + mount propagation (the #1 gotcha)
Lazarr creates the symlink tree at `paths.download_dir` and a **FUSE mount** at
`paths.fuse_mount`. For the arrs to import and Plex to play, those paths must be visible inside
**their** containers, and the FUSE mount must **propagate** out of Lazarr's container.

Put both under one shared host dir and bind it `rshared` into Lazarr:

```yaml
# in the lazarr service
- type: bind
  source: /srv/lazarr/data        # host
  target: /data                   # config: download_dir=/data/symlinks, fuse_mount=/data/torbox
  bind: { propagation: rshared }
```

Bind the **same host dir at the same target** into each arr and Plex (propagation `rslave` is
fine for consumers):

```yaml
- type: bind
  source: /srv/lazarr/data
  target: /data
  bind: { propagation: rslave }
```

If the host dir isn't already a shared mount, Docker may error; make it shared once with
`mount --make-rshared /` (or the specific mountpoint).

## 5. Wire the arrs
In each Sonarr/Radarr: **Settings → Download Clients → add qBittorrent** → host = the Lazarr
container/host, port `8080`, category = the arr's category (must be listed in `categories:` in
`config.yaml`). Login accepts anything (trusted-LAN model — see §7). Set the arr's *Remote Path
Mapping* / save path so the client's `download_dir` resolves to the shared `/data/symlinks`.

## 6. Observability (optional)
Set `metrics.listen: ":9090"` to expose:
- `GET /metrics` — Prometheus (`lazarr_materializes_total`, `lazarr_releases_total`,
  `lazarr_slots_in_use`, `lazarr_tos_audit_leaks`, …). Scrape with Prometheus; graph in Grafana.
- `GET /health` — JSON `{mounted, slots_in_use, slots_total, last_audit_unix, version}`; usable
  as a Docker `healthcheck`.

Recommended alerts:
- **`lazarr_tos_audit_leaks > 0`** — the account is holding something Lazarr believes it
  released (a ToS-compliance regression).
- **`increase(lazarr_reaper_skipped_total[15m]) > 0`** (sustained) — the idle/max-hold
  reapers are being skipped because the FUSE mount reports unhealthy, so **reaping is paused**
  and materialized items are NOT being released. A brief blip is normal (a transient mount
  hiccup); a *rising* counter over many minutes means the mount is wedged and items will be
  held past `max_hold` (toward TorBox's 30-day purge). Investigate the mount alongside
  `/health`'s `mounted:false`.

## 7. Security / trust model
No authentication on the qbit port or the metrics port (matches decypharr/rdt-client — it's a
trusted-LAN download-client shim). **Bind it to your LAN; do not expose these ports to the
internet.** Keep `torbox.api_key` only in `config.yaml` (never logged).

## 8. Quickstart
```bash
mkdir -p /srv/lazarr/config /srv/lazarr/data
cp config.example.yaml /srv/lazarr/config/config.yaml   # edit: torbox.api_key, categories, ownership.puid/pgid
cp docker-compose.example.yml docker-compose.yml         # edit LAZARR_DATA / paths
docker compose up -d
docker compose logs -f lazarr        # expect "vfs mounted" + "qbit listening"
```
Then add the qBittorrent client in your arr (§5) and grab something cached on TorBox.

## 9. Troubleshooting
- **`vfs mount failed … operation not permitted`** → missing FUSE caps, or you set `user:`
  (must run as root). Keep the caps; drop `user:`; use `ownership.puid/pgid`.
- **Arr can't move/import the file** → `ownership.puid/pgid` not set to the arr's uid:gid.
- **Plex sees the file but playback fails / reads zeros** → the FUSE mount isn't propagating;
  check the `rshared`/`rslave` bind propagation and that Plex mounts the same host path.
- **`fusermount3: could not determine username`** → only happens if not running as root; the
  image uses the raw `mount(2)` path (`DirectMountStrict`) so as root this won't occur.
- **Verify TorBox state**: query `mylist` with `bypass_cache=true` (the default view is cached).
