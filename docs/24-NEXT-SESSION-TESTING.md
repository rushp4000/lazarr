# Lazarr — Next Session: Expanded Canary Testing + Cleanup

> **For Claude (next session):** Read this doc top-to-bottom before doing anything.  
> Read memory file `/root/.claude/projects/-root/memory/project_build_own_catbox.md` for full project history.

---

## Current State (as of 2026-06-10)

- **Branch `main` = `11a8a16`, tagged `v1.0.0`, pushed to origin.**
- **GHCR release pipeline running** — `ghcr.io/rushp4000/lazarr:1.0.0` + `:latest` will be built by GitHub Actions (multi-arch amd64+arm64, stamped version).
- **`lazarr_canary` container running** on host `192.168.7.133`:
  - Image: `lazarr:webui` (locally built from `webui` branch = v1.0.0 code)
  - Ports: `8088` (qBit arr-facing), `8082` (Web UI), `9091` (metrics/health)
  - Config: `/config/lazarr_canary/config.yaml`
  - Canary arr: **radarr_hin** (port 7880) — Lazarr_Canary client enabled, Decypharr disabled
  - Categories: `radarr_hin` only
  - idle_ttl: 168h, max_hold: 720h, puid/pgid: 1003
- **Web UI:** http://192.168.7.133:8082 (no auth)
- **Health:** http://192.168.7.133:9091/health

### What is NOT yet done (Phase 4 — this session's goal)
- Add `sonarr_hin` to the canary (test TV shows, multi-episode, multi-file torrents)
- Add 2–3 more movies to radarr_hin to stress-test multi-slot behaviour
- Verify repair scanner works on a real evicted hash
- Do the canary cleanup once testing passes
- Decide on Phase 4 cutover order for main arrs

---

## PART 1: Expand Canary to sonarr_hin

### 1a. Check sonarr_hin exists and is reachable

```bash
curl -s "http://192.168.7.133:8989/api/v3/system/status" \
  -H "X-Api-Key: $(grep -A2 'sonarr_hin' /config/decypharr/config.json | grep api_key | cut -d'"' -f4)" \
  | python3 -m json.tool | head -5
```

If sonarr_hin doesn't exist on port 8989, check what sonarr instances are running:
```bash
docker ps | grep sonarr
```

### 1b. Add Lazarr_Canary as a download client in sonarr_hin

- Open Sonarr_hin in browser (find its port from `docker ps`)
- Settings → Download Clients → Add → qBittorrent
  - Name: `Lazarr_Canary`
  - Host: `192.168.7.133`
  - Port: `8088`
  - Username: `lazarr`
  - Password: `lazarr`
  - Category: `sonarr_hin`  ← **exact string, must match config**
  - Test → Save
- Disable the existing Decypharr client (don't delete — just uncheck "Enable")

### 1c. Add `sonarr_hin` to Lazarr canary config

Edit `/config/lazarr_canary/config.yaml`:
```yaml
categories: ["radarr_hin", "sonarr_hin"]
```

Then restart the canary:
```bash
docker restart lazarr_canary
sleep 3 && docker logs lazarr_canary --tail 10
```

Verify the new category appears in the qBit API:
```bash
curl -s http://192.168.7.133:8088/api/v2/torrents/categories | python3 -m json.tool
```
Should show both `radarr_hin` and `sonarr_hin`.

---

## PART 2: Add Test Content

### 2a. Movies for radarr_hin (2–3 more to test multi-slot)

Use **public-domain films verified TorBox-cached** (same approach as Big Buck Bunny):

| Film | TMDB ID | Notes |
|---|---|---|
| Sintel (2010) | 45745 | Blender short, public domain, hash `08ada5a7a6183aae1e09d831df6748d566095a10` |
| Elephants Dream (2006) | 9761 | Blender short |
| Cosmos Laundromat (2015) | 333371 | Blender short |

Steps in radarr_hin (http://192.168.7.133:7880):
1. Movies → Add New → search by name → Add (Any quality, any path)
2. Radarr will search Torrentio and grab via Lazarr_Canary
3. Watch the Lazarr log: `docker logs lazarr_canary -f`
   - Look for `checkcached` log entries (no TorBox add at grab time)
   - Look for a symlink created in `/decypharr_symlinks/lazarr_canary/`

**Verify TorBox mylist stays flat during grabs:**
```bash
# Before adds:
curl -s "https://api.torbox.app/v1/api/torrents/mylist?bypass_cache=true" \
  -H "Authorization: Bearer 34c6a383-6449-43f7-8f74-3938182d1c35" | python3 -c "import sys,json; d=json.load(sys.stdin); print('mylist count:', len(d.get('data',{}).get('torrents',[])))"
# Do the grabs, then re-run — count must not increase from Lazarr grabs
```

### 2b. TV shows for sonarr_hin

Use **public-domain or freely licensed series**. Good options:
- **Cosmos: A Spacetime Odyssey** — check if TorBox-cached
- Any anime series in the public domain

Actually the safest approach: search Sonarr for something you already have elsewhere and know is on TorBox. The key thing to test is **multi-file torrent import** (a TV season pack with multiple episode files) since that exercises the FUSE nested rel_path support.

In sonarr_hin:
1. Series → Add New → search for a series → Add
2. Season Pass → select a season → Automatic Search
3. Watch logs:
   - Multiple files per torrent → multiple symlinks in the tree
   - Each `<hash>/<episode_filename>` should appear under `/decypharr_symlinks/lazarr_canary/sonarr_hin/`
   - Sonarr should import all episodes successfully

---

## PART 3: Playback / Materialization Tests

### 3a. Slot pressure test (Essential plan = 3 slots)

1. Play movie 1 in Plex → check Web UI Materialized tab: 1 item, 1 slot in use
2. Without stopping, play movie 2 → 2 items, 2 slots
3. Play movie 3 → 3 items, 3 slots (all filled)
4. Play movie 4 (should LRU-evict the oldest idle item):
   - Check logs for `lru evict` or `release` of the least-recently-used item
   - Check mylist: should never exceed 3 items at once

### 3b. TV episode multi-file

Play an episode from the sonarr_hin import. Watch logs for the materialize sequence:
- `materialize: adding torrent` (one add per torrent, not per episode)
- `proxy: range GET` for that specific episode file
- Other episodes in the same torrent should be readable without a second add

### 3c. Idle release (can simulate with a short TTL override)

To test the reaper without waiting 7 days, temporarily set `idle_ttl: "2m"` in the canary config, restart, play something, wait 2 minutes, check mylist goes back to 0.

**Reset to `idle_ttl: "168h"` when done.**

### 3d. Repair scanner

To test with a real evicted hash:
1. Grab something, get its hash from the Web UI releases table
2. Manually remove it from TorBox cache (not possible to force-evict, but you can wait for natural expiry, OR use a known-old hash that's no longer cached)
3. Hit "Scan Now" in the Web UI Repair tab
4. The item should appear as evicted within ~30s
5. Click "Forget" → symlink deleted → radarr_hin flags the file missing → health check

---

## PART 4: Cleanup Tasks

### 4a. Remove Big Buck Bunny placeholder artifacts

The Phase-1 canary used **sparse placeholder files** in `/decypharr_symlinks/lazarr_fuse/` to stand in for FUSE before Phase 2 was built. These are NOT Lazarr code — they were a manual harness. Remove them:

```bash
# Check what's there
ls -la /decypharr_symlinks/lazarr_fuse/

# If you see hash-named directories with sparse files, these are the placeholders
# The FUSE mount now handles this properly — the dirs should be empty or gone
# ONLY delete if you see the old placeholder dirs, not the live FUSE mount files
```

### 4b. Check for stale worktree branches

```bash
cd /root/Github/Lazarr
git branch -a | grep worktree
# Delete any: git branch -d worktree-agent-*
# Also clean remote refs if needed:
git remote prune origin
```

### 4c. Old Docker images

```bash
docker images lazarr
# Safe to remove phase1, phase2 once you're happy with webui/v1.0.0:
# docker rmi lazarr:phase1 lazarr:phase2 lazarr:phase3
```

### 4d. Canary rollback prep (NOT doing this yet — only after Phase 4 cutover)

When you're ready to move the main arrs to Lazarr and retire the canary:
```bash
# Re-enable Decypharr client in radarr_hin
curl -s -X PUT "http://192.168.7.133:7880/api/v3/downloadclient/1" \
  -H "X-Api-Key: <radarr_hin_token>" \
  -H "Content-Type: application/json" \
  -d '{"enable": true}'

# Delete Lazarr_Canary client (id 2)
curl -s -X DELETE "http://192.168.7.133:7880/api/v3/downloadclient/2" \
  -H "X-Api-Key: <radarr_hin_token>"

# Delete Big Buck Bunny movie from radarr_hin (id 1, deleteFiles=true)
curl -s -X DELETE "http://192.168.7.133:7880/api/v3/movie/1?deleteFiles=true" \
  -H "X-Api-Key: <radarr_hin_token>"

docker rm -f lazarr_canary
rm -rf /config/lazarr_canary
rm -rf /decypharr_symlinks/lazarr_canary /decypharr_symlinks/lazarr_fuse
```

### 4e. radarr_hin API token location

```bash
grep -A2 radarr_hin /config/decypharr/config.json | grep api_key
# OR
cat /tmp/.rhin_token 2>/dev/null
# OR check Radarr Settings → General → API Key
```

---

## PART 5: Phase 4 Cutover Plan (after testing passes)

The goal is to replace decypharr's TorBox leg with Lazarr, one arr at a time. Order (least-risky first):

1. **radarr_rd** (port 7881) — smallest Radarr instance
2. **radarr_4k** (port 7879) — 4K movies
3. **sonarr_rd** (port 8992) — largest, 749 missing episodes (recovery works through Lazarr once wired)
4. **sonarr_4k** (port 8990)

For each arr:
1. Add a new Lazarr download client pointing to a new Lazarr instance (or the canary if adding its category to config)
2. Disable the Decypharr client for that arr (keep as fallback for 24h)
3. Watch for grabs — verify mylist stays flat
4. Watch for playback — verify materialize + release cycle
5. After 24h clean, delete the Decypharr client for that arr
6. Once all arrs migrated, shut down decypharr container

**Note:** decypharr's RealDebrid leg stays running for the RD arrs (radarr_rd, sonarr_rd). Lazarr only replaces the TorBox leg. The arr is pointed at EITHER decypharr (RD) OR Lazarr (TorBox), not both simultaneously for the same category.

---

## Quick Reference

| Item | Value |
|---|---|
| Canary Web UI | http://192.168.7.133:8082 |
| Canary health | http://192.168.7.133:9091/health |
| Canary qBit (for arrs) | 192.168.7.133:8088 |
| radarr_hin | http://192.168.7.133:7880 |
| sonarr_hin | check `docker ps` for port |
| TorBox API key | `34c6a383-6449-43f7-8f74-3938182d1c35` |
| Canary config | `/config/lazarr_canary/config.yaml` |
| Canary symlink tree | `/decypharr_symlinks/lazarr_canary/` |
| Canary FUSE mount | `/decypharr_symlinks/lazarr_fuse/` |
| Canary DB | `/config/lazarr_canary/lazarr.sqlite` |
| Repo | `/root/Github/Lazarr` (branch: main, tag: v1.0.0) |
| GHCR image | `ghcr.io/rushp4000/lazarr:1.0.0` (building now) |
