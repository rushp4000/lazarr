# Session 11 (2026-06-10): UI v2, live fixes, expanded canary testing

Branch `ui-v2`. Context: docs/24 testing plan + user feedback that the v1.0.0 Web UI was
unusable (no titles, read-only config, "materialize" jargon).

## Fixed this session

1. **Dashboard showed hashes/blank names (the docs/24 complaint).** Root cause:
   `catalog.Release` had no JSON tags → `/api/releases` emitted PascalCase keys the JS
   never read. Tags added (snake_case), `Magnet` excluded from the wire. Same fix for
   `materialize.RepairEntry`. Regression tests assert the wire format.
2. **Dead CDN node stalled playback (live outage 2026-06-10 ~04:28).** nexus-136 died
   mid-stream; the presigned URL never 4xxed so refresh-on-4xx never fired and reads
   retried the dead URL for minutes. Fix: transport-level failures (dial refused/timeout,
   header timeout, mid-body reset) now classify as `errCDNUnreachable` → one RequestDL
   refresh + one retry, exactly like 4xx. Context cancellation is excluded (a stopped
   player must not burn refreshes). **Verified live**: the same dead-node link refreshed
   and the read succeeded.
3. **Audit drift false-positive on fresh adds.** mylist lags createtorrent by ~1 min even
   with bypass_cache=true. Drift WARN now grace-skipped for items materialized <10 min ago.
4. **Unknown `/api/*` paths returned the HTML shell (200)** — now JSON 404.
5. **Web UI v2** (the big one):
   - Tabs: Dashboard (with a 3-step "How Lazarr works" explainer) · Library · Streams ·
     Health · Logs · Settings.
   - Plain language: "materialized" → **On TorBox**, "virtual" → **On demand**, release →
     **Remove from TorBox**, repair → **availability / Re-search**.
   - Titles everywhere; infohash demoted to a small sub-label.
   - **Settings page is fully editable** (Decypharr-style): TorBox key (write-only),
     categories chip editor (one per arr) + a live "connect your arr" box showing exact
     qBittorrent client values, streaming policy in hours/days selectors, log level,
     network ports, auth, advanced paths + PUID/PGID. Save → validate → atomic
     config.yaml rewrite (0600) → `restart_required` flag → Restart button (graceful
     shutdown; Docker `restart: unless-stopped` brings it back; UI polls and reloads).
   - **Log levels**: `log_level` config (debug|info|warn|error), hot-applied via
     slog.LevelVar on save — no restart. New `internal/logging` ring buffer (1000
     records) feeds the Logs tab (level filter, follow mode). `LAZARR_LOG_LEVEL` env
     still overrides for compat.

## Live canary results (expanded per docs/24)

- **sonarr_hin wired** (port 8991): Lazarr_Canary client added (category `sonarr_hin`),
  Decypharr client disabled. Category added through the new settings UI flow end-to-end
  (save → restart → categories live).
- **Sintel (tmdb 45745) auto-imported cleanly** — grab → checkcached (no add) → symlink →
  radarr import. Multi-file + **nested rel_path proven live**: the torrent has
  `<dir>/Screens/*.png` two levels deep, all served by FUSE.
- **Slot pressure / LRU**: with `active_slots: 1`, reading Sintel while BBB held the slot
  evicted idle BBB and admitted Sintel; at most one Lazarr item on the account at all
  times. (Used slots=1 because only 2 test torrents are TorBox-cached; logic is identical
  at 3.)
- **Repair scanner on real evicted hashes**: Tears of Steel + 2 old VODO Pioneer One
  grabs aren't TorBox-cached → `cache_status=evicted` after a live scan; **Forget**
  removed Tears of Steel from catalog+symlinks (arr re-search path).
- **Cache-miss path**: uncached grabs land as `state=error` with `allow_uncached: false`
  — correct, but see "open items" for the queue UX.
- **Throughput**: warm sequential read ≈ 7 MB/s per stream (serial 1 MB FUSE windows,
  each one HTTP round-trip); seeks ~0.2–0.3 s. Fine for 1080p; borderline for 80 Mbps 4K
  REMUX.
- Crash recovery re-verified incidentally: `docker rm -f` (SIGKILL) → stale-mount
  auto-clear + boot reconcile released the leftover add. Account stayed clean all session
  (audits green; mylist delta = only the intentionally-materialized items, all released).

## Corrections to docs/24

- Cosmos Laundromat TMDB is **358332**, NOT 333371 (333371 = 10 Cloverfield Lane — that
  movie got auto-added/grabbed by mistake and was removed from radarr + forgotten from
  Lazarr before any TorBox add; it never left `virtual`).
- Blender shorts and CC web series are mostly **not TorBox-cached** — cached-only canary
  tests must use popular content or temporarily set `allow_uncached: true`.

## Open items / future work

1. **Cached-only grab rejection (design decision needed).** Today a cache-miss grab is
   stored as `state=error`; the arr queue shows it as "warning / downloading" forever
   (Cleanuparr or manual removal cleans it). Alternative: reject the `/torrents/add` so
   the arr instantly falls back to the next release. Trade-off: repeated add-failures can
   trip the arr's download-client backoff (blocks ALL grabs for minutes). Proposal:
   `policy.on_cache_miss: error|reject` (default `error`), decide with user.
2. **Arr client backoff after settings restarts.** A grab pushed during the ~2 s restart
   window marks the client unavailable and sonarr's backoff is sticky (re-saving the
   client resets it). Document; consider draining/delaying restart while a push is in
   flight.
3. **Import costs one TorBox add** (arr ffprobe reads the file at import). Expected; with
   7d idle TTL these sit on the account up to slot count; LRU keeps it bounded. Future
   option: short post-import release.
4. **4K throughput**: parallel window prefetch (readahead ring) if real 4K REMUX playback
   stutters during Phase-4 cutover.
5. **Dependabot PRs #3–#7 open** (actions bumps, all CI-green) — user to merge.
6. Remote branches `origin/phase3`, `origin/webui` are merged & deletable (left for user).
7. Local image is `lazarr:ui-v2`; canary container runs it with
   VERSION=v1.1.0-dev.<sha>. GHCR v1.0.0 release pipeline completed successfully.

## Rollback reference (canary, updated)

Same as docs/24 §4d plus: sonarr_hin has Lazarr_Canary client id=2 (delete) and Decypharr
client id=1 (re-enable), series "Pioneer One" id=1 (delete w/ files), and radarr_hin now
has movie ids 2 (Sintel, imported), 3 (Elephants Dream, no file), 5 (Cosmos Laundromat,
no file), 6 (Tears of Steel, no file) in addition to Big Buck Bunny id=1.
