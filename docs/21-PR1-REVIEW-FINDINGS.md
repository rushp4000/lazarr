# 21 — PR #1 (phase3 → main) pre-production review findings

Reviewer pass: 2026-06-09, full diff `origin/main..phase3` (d662c45..d09581d, 42 files).
**Verification caveat:** `go build/vet/test/-race` + `govulncheck` could NOT be executed this
session (harness permission outage blocked all non-read-only shell commands). Memory/handoff
records the suite green at `9cc91e7`, and `d09581d` is docs-only — but **re-run the full suite +
govulncheck before merge**. Findings below are from code audit.

---

## ✅ FIX STATUS (2026-06-09, per docs/22 fix plan)

All blockers and should-fixes are FIXED, with tests, on the `webui` branch (the fix work was
consolidated there alongside the Web UI dashboard; `phase3` is now stale). Full suite re-run
**green**: `go build/vet/test/-race` across the module + `govulncheck` (0 vulnerabilities
affecting the code).

| ID | Status | Commit |
|----|--------|--------|
| B1 max-hold from materialize time     | ✅ FIXED | `dd52da9` |
| B2 untracked-release leak + reconcile | ✅ FIXED | `dd52da9` |
| B3 force-release pinned on Close      | ✅ FIXED | `dd52da9` |
| S1 SetMountHealthy-before-Start       | ✅ FIXED | `2987a44` |
| S2 release on qbit delete             | ✅ FIXED | `2987a44` |
| S3 reaper-skip metric + Healthy timeout | ✅ FIXED | `c8f2a39` |
| S4 chown no-follow TOCTOU + ancestor bound | ✅ FIXED | `c25df6f` |
| S5 puid/pgid both-or-neither validation | ✅ FIXED | `906328b` (bundled w/ webui) |
| S6 RequestDL dead-cache ordering      | ✅ FIXED | `c25df6f` |
| S7 version-stamped GHCR image         | ✅ FIXED | `08807c9` |
| S8 example metrics/healthcheck consistency | ✅ FIXED | `08807c9` |
| N6 pin govulncheck                    | ✅ FIXED | `08807c9` |

Nice-to-haves N1–N5, N7 were NOT attempted in this pass (droppable; see list below).

**Still pending before merge (USER-gated, must not touch the live stack):** the two canary
re-validations in the Verdict section.

---

## BLOCKERS (must fix before merge / Phase-4 cutover)

### B1. `max_hold` is measured from GRAB time, not materialize time → add/delete churn
`catalog/sqlite.go:271` `OverMaxHold` filters `state='materialized' AND added_on < ?`, but
`added_on` is set at **grab** (`qbit/server.go:282`). Any release grabbed more than `max_hold`
(default 24h) before first playback becomes an instant max-hold candidate the moment it
materializes. The reaper ticks every 30s; between FUSE read windows `refs==0`, so `Release` is
NOT blocked by pinning → **ControlDelete mid-playback → next read re-adds → delete/add loop**,
burning the ~60/hr createtorrent budget until `ErrRateLimited` kills the stream. This is the
decypharr-churn failure mode the project exists to avoid; the live canary missed it only because
grab and playback happened the same day. Phase-4 (re-grab 740 old items, play later) hits it
immediately.
**Fix:** add a `materialized_at` column set in `SetState(..., StateMaterialized, ...)`; use it in
`OverMaxHold`. (Do not reuse `added_on` — qbit metadata depends on it.)

### B2. Restart leaves TorBox items that can never be reaped — and the audit is blind to them
`materialize/engine.go:530` `Release()` returns nil no-op when the hash is not in the in-memory
`track` map. After a crash (or any restart — see B3), catalog rows persist in
`StateMaterialized` with a `torbox_id`; the reapers fetch them as candidates
(`reaper.go:47,64`) and call `Release` → **no-op forever; ControlDelete is never issued.**
Worse, `AuditTOS` (`audit.go:23-30`) puts `MaterializedIDs()` into `believedSet`, so the
lingering item is "believed held" and **never flagged as a leak** → the item sits on the account
until TorBox's 30-day purge = exactly the "excessive transfer retention" the ToS prohibits, with
the compliance alarm silent.
**Fix:** in `Release` (or the reaper candidate path), when the hash is untracked but the store
says `StateMaterialized` with `TorBoxID != 0`, issue `ControlDelete` + `SetState(virtual)`
(guard against a concurrent adopt-materialize via singleflight/track check). Alternative or
additional: a boot-time reconciliation sweep.

### B3. Graceful shutdown can also leak (feeds B2)
`engine.go` `Close()` → `Release(h)` **skips pinned entries** (`refs > 0`). `main.go` unmounts
first, but the lazy-detach fallback (vfs/fs.go Unmount) lets an in-flight `ReadAt` (proxy GET
timeout up to 5m) survive past `Close` → its entry is skipped → item left on the account, and
after restart B2 makes the leak permanent.
**Fix:** in `Close`, after the reaper wait, force-release regardless of refs (the mount is gone;
no reader can be served) or wait-with-deadline for refs to drain, then release.

---

## SHOULD-FIX (before v1.0.0 tag)

### S1. Data race: `SetMountHealthy` is called AFTER `Start` — `cmd/lazarr/main.go:112,127`
`eng.Start(ctx)` (line 112) launches the reaper goroutine that reads `m.mountHealthy`;
`eng.SetMountHealthy(fsys.Healthy)` (line 127) then writes the field unsynchronized. The
engine's own contract (`engine.go:63-68`) says "set once before Start". Window is ~30s ticker
but it is a real data race. **Fix:** reorder — create engine → mount vfs → `SetMountHealthy` →
`Start`. (Or make the field an `atomic.Value`.)

### S2. qbit `torrents/delete` never releases a materialized item — `qbit/server.go:578-602`
Delete removes symlinks + the catalog row only. If the release is currently materialized:
TorBox item + slot stay held in-memory; the store row is gone so the idle/max-hold reapers
can't see it; it lingers until LRU pressure or shutdown. With 3 slots, a few delete-while-
materialized events (upgrade-replace during playback; Phase-4 import→delete churn) pin the slot
budget. **Fix:** wire the engine into qbit Deps and call `Release(hash)` on delete, or have the
reaper also sweep tracked entries whose store row is missing.

### S3. Broken-mount guard has no escape hatch; `Healthy()` can hang
`reaper.go:27-30`: a permanently unhealthy mount disables ALL reaping indefinitely (Warn log
only) → items held past 30d (ToS) while the daemon runs. Add a `lazarr_reaper_skipped_total`
metric + alert guidance in docs/20, and consider still enforcing max-hold after sustained
unhealthiness (e.g. >1h). Separately, `vfs.Healthy()`'s `os.Stat` on a wedged FUSE mount can
block forever → the single reaper goroutine and `/health` hang with it. Stat under a timeout
(goroutine + select).

### S4. chown TOCTOU + ancestor escape — `symlink/manager.go:174-206`
(a) `mkdirAllOwned` chowns created dirs with `os.Chown`, which **follows symlinks**; an
arr-uid process with write access to the shared tree can swap a freshly created dir for a
symlink between `MkdirAll` and `chown` → root chowns an arbitrary path to the arr uid. Use
`Lchown` for dirs as well, or re-`Lstat` each created path and require `IsDir()` before chown.
(b) When `download_dir` itself does not exist, the `created` chain walks ABOVE it (e.g. chowns
`/data`). Bound the chown loop to paths under `downloadDir` (`mustBeUnder`).

### S5. Config: puid/pgid half-set silently disables chown — `config.go validate` + `manager.go:66`
`chownEnabled()` requires BOTH > 0; `puid: 1000, pgid: 0` silently no-ops and produces exactly
the "arr can't move/import" failure docs/20 §9 troubleshoots. Validation should reject one-set-
without-the-other.

### S6. Dead-cache 400 masked by link-expired ordering — `torbox/client.go:399-411`
In `RequestDL`, statuses in `LinkRefreshStatuses` {400,403,410} short-circuit to
`ErrLinkExpired` BEFORE the not-found detail check; a gone torrent reported as HTTP 400
"not found" is treated as a stale link → refresh → fail → generic error → EIO retry loop
instead of `ErrPurged`/StateError. Check `detailNotFound` on the error body before (or inside)
the refresh-status branch.

### S7. GHCR image is not version-stamped — `.github/workflows/release.yml` + `Dockerfile`
GoReleaser stamps `internal/version.Version` for the tar.gz binaries only; the Docker build has
no `-X` ldflag → the published image's `/health` reports `"dev"`. Pass a `VERSION` build-arg in
the image job and use it in the Dockerfile's `go build`.

### S8. compose example healthcheck vs. metrics-off default
`docker-compose.example.yml` healthchecks `:9090/health`, but `config.example.yaml` ships
`metrics.listen: ""` → the container is permanently `unhealthy` out of the box. Comment the
healthcheck out by default (or default metrics on in the example pair).

---

## NICE-TO-HAVE

- **N1.** `ErrSlotsExhausted` is dead code; `admit()` blocks forever on `context.Background()`
  (ReadAt has no deadline) — a FUSE read hangs indefinitely when all slots are pinned. Bounded
  admit timeout → EIO; longer term, plumb the FUSE ctx through `Materializer.ReadAt`.
- **N2.** Probe cache index is in-memory only: after restart, on-disk entries are orphaned
  (never served, never counted against the 512 MiB budget) → slow unbounded growth across
  restarts. Scan the dir at startup (or wipe it).
- **N3.** Eviction race in `ensureMaterialized` (`engine.go:336-343`): a just-materialized entry
  can be LRU-evicted before its first pin → wasted add+delete against the 60/hr budget. Register
  with refs pre-pinned for the singleflight winner.
- **N4.** `ipAllowed` doesn't block CGNAT `100.64.0.0/10`; consider also pinning port 443.
- **N5.** TorBox-supplied `rel_path`s enter the catalog (and vfs dirent names) unvalidated;
  the symlink layer re-validates, but reject `..`/absolute at `UpsertRelease` too (single
  chokepoint).
- **N6.** Pin `govulncheck` to a version/sha in ci.yml (everything else is pinned).
- **N7.** Known/tracked: lint advisory→blocking (~22 findings, docs/19 STEP C); Agent D
  README/CONTRIBUTING still missing.

---

## What's GOOD (verified in the diff)

- **ToS at grab:** no path in qbit/symlink/catalog touches `CreateTorrent`; add = checkcached +
  symlink only. `MyList` sets `bypass_cache=true` (client.go:468) so the audit reads truth.
- **SSRF proxy:** validate-before-egress + dial-time IP re-validation (DNS-rebinding closed),
  no env proxy, redirect re-validation, suffix pin with label boundary, token-redacting errors.
  Test seam (`allowHost`) never reachable from production config.
- **FUSE:** nested rel_path resolution is correct (component-wise, files-win, deterministic
  Readdir); `fileIno`/`dirIno` namespacing fixes the inode collision; `DirectMountStrict` +
  `AllowOther` + `MaxWrite` reasoning matches the live-canary findings; EROFS on write opens;
  EBUSY unmount retry + MNT_DETACH fallback is sound.
- **Engine concurrency:** slot semaphore + singleflight + refs/pinning + coalesced idle signal
  are correct under audit (incl. markPurged's exactly-once slot free on both create and stream
  paths); mu never held across network I/O.
- **HTTP 200-vs-206 and all-or-nothing probe reads** (the two Phase-2 review bugs) remain fixed
  with regression tests; bencode/magnet hardening + infohash chokepoint present.
- **CI/release:** least-privilege `permissions:` blocks, GHCR via `GITHUB_TOKEN`, release only
  on `v*` tag.
- **docs/20** is accurate on the privilege model (root + effective CAP_SYS_ADMIN), rshared/
  rslave propagation, and the trusted-LAN trust model (cross-fix S5/S8).

## Verdict
**Original verdict: do not merge yet.** B1+B2(+B3) are ToS-compliance regressions at the heart
of the product's reason to exist — the audit loop cannot see B2, which is the worst kind of
leak. All are small, local fixes (one column + one untracked-release path + one Close tweak +
a main.go reorder).

**Updated 2026-06-09:** all blockers + should-fixes are FIXED with tests (see the FIX STATUS
table above); `go build/vet/test/-race` + `govulncheck` re-run **green**. The code blockers
are cleared.

**Still required before merge — USER-gated, NOT done by the fix pass (must not touch the live
stack):**
1. **Boot-reconciliation canary (B2/B3):** restart the canary with `kill -9` (not SIGTERM)
   *mid-materialize*, then confirm boot reconciliation releases the leftover on next start —
   verify via mylist with `bypass_cache=true` (the account no longer holds the orphaned id).
2. **Max-hold-from-materialize canary (B1):** play a release whose grab is >24h old and
   confirm there is NO mid-playback delete/add churn (the stream is not torn down).

After both canary checks pass, proceed to STEP C/D of docs/19. Branch note: the fixes live on
`webui`; reconcile/merge the branch strategy (`phase3` vs `webui` vs PR #1) before tagging.
