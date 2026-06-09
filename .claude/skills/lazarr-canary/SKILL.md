---
name: lazarr-canary
description: "Procedure to canary-test Lazarr against radarr_hin on host 192.168.7.133 in parallel with the live decypharr stack. Use when wiring the test arr, running the grab→import→materialize→release end-to-end check, or rolling back. Includes the ToS-audit assertion (mylist stays flat on grab)."
user-invocable: true
license: MIT
metadata:
  author: lazarr
  version: "1.0.0"
---

# Lazarr canary (radarr_hin, parallel to decypharr)

**Principle:** never touch what the live stack uses. Lazarr gets its own container, FUSE
mount, and symlink dir, wired ONLY to radarr_hin (port 7880, empty). Instant rollback =
repoint radarr_hin's download client back to decypharr (`:8282`).

## Wiring
- Lazarr container: own `/data/symlinks` + `/data/torbox` (FUSE), Docker flags
  `--cap-add SYS_ADMIN --device /dev/fuse --security-opt apparmor:unconfined`. Do NOT
  share decypharr's volumes.
- radarr_hin (7880) → Settings → Download Clients → qBittorrent → host `lazarr`,
  port `8080`, category `radarr_hin`. (radarr_hin API key = its `token` in
  `/config/decypharr/config.json`.)
- Plex: add the radarr_hin symlink dir as a test library path; deep analysis already
  globally disabled — leave it.

## Canary movie (chosen)
**Big Buck Bunny (2008), tmdbId 10378, imdbId tt1254207** — public-domain, verified
TorBox-cached (hash `dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c`). Backup: **Sintel
(tmdbId 45745)**. radarr_hin profile **Any (id 1)**, root **/movies**.

## Phase 1 check (grab → import, no materialize)
1. In radarr_hin, add Big Buck Bunny (profile Any, root /movies) and search+grab.
2. Assert: Lazarr logs a `checkcached` hit with a real size; symlink appears; radarr_hin
   imports into the library.
3. **ToS assertion:** TorBox `mylist` count is **unchanged** before vs after the grab
   (nothing added). This is the core compliance proof.

## Phase 2 check (play → materialize → release)
1. Press play in Plex. Assert: the item now appears in `mylist` (materialized), stream
   plays (HTTP 206 ranged proxy).
2. Stop; wait `idle_ttl`. Assert: idle reaper released it (`mylist` back to baseline),
   while the symlink + Plex entry remain.
3. Force a stale link (or wait past expiry) → assert refresh-on-4xx keeps playback alive.
4. Slot safety: start more concurrent streams than `active_slots` → assert queue/LRU
   behaves and the account never errors.

## Guards
- Confirm a broken/empty FUSE mount is NOT mistaken for broken symlinks — never delete
  on an unstable mount; confirm counts stable across two reads.
- Respect TorBox `createtorrent` rate limit (~60/hour) and `cooldown_until`.
- Keep a `mylist`-vs-materialized audit running; alarm if the account holds anything we
  believe is released.

See `/root/Github/Lazarr/docs/06-roadmap-and-canary.md`.
