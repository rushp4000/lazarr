# 06 — Roadmap & the radarr_hin canary

**Principle: run entirely in parallel to the live decypharr stack. Touch nothing the
stack uses until cutover.** Lazarr gets its own container, its own FUSE mount, its own
symlink dir, and is wired only to **radarr_hin** (port 7880, intentionally empty).

## Phase 0 — Validate assumptions (no Lazarr code; ~½ day)
Replay the TorBox API from the host with our key to pin the contract:
1. `checkcached?hash=<known-cached>&list_files=true` → confirm names+sizes, **no add**.
2. `mylist` before/after → confirm checkcached did **not** add anything.
3. `createtorrent` (cached magnet) → `requestdl` → range GET (expect 206 real bytes).
4. `controltorrent` Delete → `mylist` shows it gone (release works).
5. `user/me?settings=true` → read plan + **active slot count** (resolve the old 403
   err 1010 — confirm param/scope/IP). Record the number; it sizes our semaphore.
6. Pin the exact API base (`/api/...` vs `/v1/api/...`).
**Exit criteria:** every endpoint in `02` verified from our host; slot count known.

## Phase 1 — qBit shim + symlink, NO materialize (the "import" half; ~2–3 days)
Build `qbit/`, `torbox.CheckCached/TorrentInfo`, `catalog/`, `symlink/`, plus a **stub
`vfs`** that serves correct sizes (read returns zeros for now).
- Point **radarr_hin**'s download client at Lazarr (`:8080`, category `radarr_hin`).
- Test: search+grab one well-seeded cached movie in radarr_hin.
- **Verify:** Lazarr logs a checkcached hit with size; symlink appears; radarr_hin
  imports it into the library; **TorBox `mylist` stays empty** (nothing added). ✅ This
  proves the ToS-compliant grab→import path end to end.

## Phase 2 — FUSE materialize + proxy + release (the "playback" half; ~3–5 days)
Build real `vfs` read hook + `materialize/` (add→requestdl→proxy→idle-release) +
link-refresh-on-4xx + **configurable slot semaphore (default 3)** + reapers + the
**probe-header cache** (confirmed deliverable — so a new import's Plex scan doesn't cost
a TorBox add; see `05-spec.md` §6).
- Add the radarr_hin path to Plex (its own library/section, or scan the symlink dir).
- **Verify the full lifecycle on the canary title:**
  - Before play: `mylist` empty (virtual).
  - Press play in Plex → Lazarr materializes (add appears in `mylist`), stream plays.
  - Stop, wait `idle_ttl` → idle reaper releases (`mylist` empty again), symlink + Plex
    entry remain.
  - Force a stale link (or wait past expiry) → confirm refresh-on-400 keeps playback alive.
- **Verify slot safety:** start more concurrent streams than slots → confirm queue/
  LRU-release behaves and never errors the account.

## Phase 3 — Hardening & packaging (parallel, before any cutover)
- `Dockerfile` + `docker-compose.example.yml` (FUSE caps documented) + `config.example`.
- `/health`, `/metrics`, ToS audit log (mylist vs materialized diff).
- GitHub repo: README, LICENSE, CI build + GHCR image publish, semver tags.
- Optional Torznab endpoint for Prowlarr.
- Soak the canary for days; watch for: dead-cache (cached→0-byte) handling, slot
  exhaustion, FUSE I/O errors under Plex scans.

## Phase 4 — Gradual cutover (only after canary is solid; user-gated)
Per arr, one at a time, lowest-risk first:
1. Add the arr's category to Lazarr config; point that arr's qBit client at Lazarr.
2. **Leave decypharr running** for RealDebrid (Lazarr replaces only the **TorBox leg**).
   The stack keeps RD via decypharr; TorBox grabs now flow through Lazarr.
3. Order suggestion: radarr_hin (canary) → radarr_4k → radarr_rd → sonarr_4k →
   sonarr_rd (largest/most-broken last).
4. Recovery work (sonarr_rd 749 missing, sonarr_4k tail) resumes **through Lazarr** once
   it's the TorBox path — re-grabs become lazy + self-releasing instead of hoarding.

## Canary wiring cheat-sheet (radarr_hin)
- Lazarr container: own `/data/symlinks` + `/data/torbox` (FUSE), `--cap-add SYS_ADMIN
  --device /dev/fuse --security-opt apparmor:unconfined`, **not** sharing decypharr's
  volumes.
- radarr_hin (7880) → Settings → Download Clients → qBittorrent → host `lazarr`,
  port `8080`, category `radarr_hin`. API key for radarr_hin is its `token` in the
  decypharr config.
- Plex: add the radarr_hin symlink dir as a (test) library path; heavy analysis already
  globally disabled — leave it.
- **Roll back instantly** by repointing radarr_hin's download client back to decypharr
  (`:8282`). No stack change is irreversible at any point.

## Rough effort
P0 ≈ 0.5d · P1 ≈ 2–3d · P2 ≈ 3–5d · P3 ≈ 2–3d (+ soak) · P4 = incremental.
First end-to-end lazy playback on the canary ≈ **end of Phase 2**.
