# 22 — PR #1 fix plan (Sonnet driver prompt)

> **This document IS the prompt.** Hand it verbatim to a Sonnet driver session. It directs
> fixing every blocker and should-fix from the PR #1 review (`docs/21-PR1-REVIEW-FINDINGS.md`)
> on branch `phase3`, including how to use subagents safely.

---

## Your role

You are the implementation driver for **Lazarr** (`/root/Github/Lazarr`), a ToS-compliant
TorBox lazy-materialize shim that fronts as a qBittorrent client for Sonarr/Radarr. A
principal-engineer review of PR #1 (`phase3` → `main`) found 3 blockers (B1–B3), 8
should-fixes (S1–S8) and 7 nice-to-haves (N1–N7) — full detail with file:line in
`docs/21-PR1-REVIEW-FINDINGS.md`. **Read docs/21 first, in full.** Your job: fix B1–B3 and
S1–S8 on `phase3`, with tests, leaving the suite green.

## Environment & hard constraints

- Repo: `/root/Github/Lazarr`, branch `phase3` (check out if needed). Go 1.26.4:
  `export PATH=$PATH:/usr/local/go/bin`.
- **NO live TorBox calls** — tests use the existing fixture/fake-server patterns
  (see `internal/torbox/*_test.go`, `internal/materialize/*_test.go`).
- **Do NOT touch the live stack** (no docker commands against running containers).
- **Do NOT merge PR #1, do NOT tag** — those are user-gated. Committing to `phase3` and
  pushing `origin/phase3` (which updates PR #1) IS allowed.
- Verification command, run after EVERY stage (it was blocked during the review session, so
  you are also re-establishing the baseline — run it FIRST, before any edits):
  ```bash
  export PATH=$PATH:/usr/local/go/bin
  go build ./... && go vet ./... && CGO_ENABLED=0 go test ./... && go test -race ./...
  ```
  Run `govulncheck ./...` once at the end.
- Commit per stage with prefix `phase3(fix): ...`, referencing the finding IDs.

## ★ Subagent rules (read carefully)

- **Worktree gotcha:** the Agent tool's worktree isolation branches from the repo DEFAULT
  branch (`main`), NOT `phase3`. **Never use `isolation: "worktree"` for these fixes.**
  Subagents must work directly in `/root/Github/Lazarr` on `phase3`.
- Because subagents share one working tree, **never run two code-editing subagents in
  parallel**. Run stages serially. Stages 3 and 5 touch disjoint files but still run them
  one at a time — the saved wall-clock isn't worth a corrupted tree.
- Use a subagent (general-purpose) per stage ONLY if the stage is large enough to be worth
  the context handoff (Stages 1 and 3 qualify); do Stages 2, 4, 5, 6 yourself. Each
  subagent prompt must include: the repo path, the PATH export, the relevant docs/21
  excerpt, the exact files to touch, "no live TorBox calls", and "run the verification
  command before reporting done".
- After each subagent returns, YOU re-run the verification command and review its diff
  (`git diff`) before committing. Verify, don't trust.

---

## Stage 0 — baseline (driver)

`git -C /root/Github/Lazarr status && git log --oneline -3` (expect HEAD = d09581d or later
on `phase3`, clean tree). Run the verification suite. It is expected green (last known green
was 9cc91e7; everything since is docs). If it is NOT green, stop and report — do not fix
unrelated breakage silently.

## Stage 1 — BLOCKERS B1 + B2 + B3 (one subagent OK; catalog + materialize + tests)

These three interlock (all are "engine forgets it holds something on TorBox" paths) — fix
them together in one commit.

**B1 — `max_hold` from grab time → mid-playback delete churn**
- `internal/catalog/sqlite.go`: add column `materialized_at INTEGER NOT NULL DEFAULT 0`.
  Migration must be **idempotent** (the canary DB already exists): attempt
  `ALTER TABLE releases ADD COLUMN ...` and ignore the duplicate-column error, or check
  `PRAGMA table_info`. In the same migration, backfill
  `UPDATE releases SET materialized_at = <now> WHERE state='materialized' AND materialized_at=0`
  so pre-existing materialized rows are NOT instantly reap-eligible.
- `SetState(...)`: when the new state is `StateMaterialized`, set `materialized_at = now`;
  when leaving materialized (→ virtual/error), zero it.
- `OverMaxHold` (currently `sqlite.go:271`): filter on
  `state='materialized' AND materialized_at > 0 AND materialized_at < ?`. Do NOT touch
  `added_on` (qbit metadata depends on it).
- Test: grab-then-materialize where `added_on` is older than max_hold but `materialized_at`
  is fresh → NOT a candidate; and the inverse → candidate.

**B2 — untracked `Release()` no-op → permanent post-restart leak, invisible to the audit**
- `internal/materialize/engine.go` (`Release`, ~line 530): when the hash is NOT in `track`,
  do not return nil. Consult the store: if the release exists with `State==StateMaterialized`
  and `TorBoxID != 0`, issue `ControlDelete(torboxID)` and `SetState(hash, StateVirtual, 0)`
  (match existing state-transition helpers). Guard against racing a concurrent in-flight
  materialize: re-check `track`/singleflight under `mu` before deleting (if a materialize for
  that hash is in flight or tracked, defer to the tracked path instead).
- Add a **boot-time reconciliation** in `Start` (or a small `reconcile()` called from it):
  enumerate store rows in `StateMaterialized`; for each not in `track` (at boot, none are),
  log + release via the same path. This both repairs crash leftovers immediately and keeps
  `AuditTOS`'s believed-set honest.
- Test: fake store with a materialized row + fake TorBox; engine with empty track; call
  `Release(hash)` (and separately `Start`-reconcile) → expect exactly one ControlDelete +
  state flips to virtual. Also: in-flight materialize for the same hash is NOT deleted.

**B3 — graceful shutdown skips pinned entries → leak feeding B2**
- `internal/materialize/engine.go` (`Close`): after stopping/waiting for reapers, wait with
  a short deadline (~5s) for `refs` to drain, then **force-release every tracked entry
  regardless of refs** — by the time `Close` runs, `main.go` has unmounted (possibly lazy-
  detach), so no reader can meaningfully continue; an EIO to a zombie read beats a ToS leak.
  Log any force-release at Warn with the hash.
- Test: tracked entry with refs>0 → Close still issues ControlDelete.

Commit: `phase3(fix): B1 materialized_at max-hold, B2 untracked release + boot reconcile, B3 force-release on Close (docs/21)`

## Stage 2 — S1 + S2 (driver; main.go + qbit wiring — depends on Stage 1 semantics)

**S1 — data race, `cmd/lazarr/main.go:112,127`**: reorder to: create engine → `fsys.Mount()`
→ `eng.SetMountHealthy(fsys.Healthy)` → `eng.Start(ctx)`. Keep the mount-failure path
working (`eng.Close()` must be safe before `Start`; check, don't assume).

**S2 — qbit delete never releases** (`internal/qbit/server.go:578-602`): add an optional
engine dep to `qbit.Deps` as a small interface (e.g.
`type Releaser interface { Release(hash string) error }`), nil-safe so existing qbit tests
don't need an engine. In `handleTorrentsDelete`, call `Release(hash)` BEFORE
`Store.DeleteRelease` (with Stage 1's B2 fix, Release now handles both tracked and
store-only cases; deleting the row first would orphan it). Log, don't fail the request, on
release error. Wire `eng` into the Deps in `main.go` — note main.go currently constructs
qbit BEFORE the engine; reorder construction (engine first) as part of the S1 reorder.
Test: delete of a materialized release calls Release exactly once.

Commit: `phase3(fix): S1 SetMountHealthy-before-Start reorder, S2 release on qbit delete (docs/21)`

## Stage 3 — S4 + S5 + S6 (one subagent OK; three disjoint packages)

**S4 — chown TOCTOU + ancestor escape** (`internal/symlink/manager.go:174-206`):
(a) for each created dir: `Lstat` → require `IsDir()` (reject symlink) → then chown; prefer
opening with `O_NOFOLLOW|O_DIRECTORY` and `fchown` if straightforward, else Lstat+`os.Lchown`.
(b) bound the created-ancestor chain with `mustBeUnder(downloadDir, ...)` so a missing
`download_dir` never causes chowns above it; better, require `download_dir` to exist (create
it once at `symlink.New`) and only chown strictly-below paths.
Tests: symlink swapped in place of a created dir → no chown + error; ancestor above
download_dir never chowned.

**S5 — puid/pgid half-set** (`internal/config/config.go` validate): reject exactly-one-set
(`puid>0 XOR pgid>0`) with a clear error naming both keys. Test both directions.

**S6 — dead-cache 400 masked as ErrLinkExpired** (`internal/torbox/client.go:399-411`):
in `RequestDL`, check `detailNotFound` on the response body BEFORE (or inside) the
`LinkRefreshStatuses` branch; a 400/403/410 whose detail says not-found returns
`ErrNotFound`, not `ErrLinkExpired`. Table test: 400+"not found" → ErrNotFound;
403+other detail → ErrLinkExpired (existing behavior preserved).

Commit: `phase3(fix): S4 chown hardening, S5 puid/pgid validation, S6 RequestDL not-found ordering (docs/21)`

## Stage 4 — S3 (driver; reaper guard observability + Healthy timeout)

- `internal/metrics/metrics.go`: add counter `lazarr_reaper_skipped_total`; increment in
  `internal/materialize/reaper.go` where the broken-mount guard skips a sweep.
- `internal/vfs/fs.go` `Healthy()`: run the `os.Stat` in a goroutine with a ~2s
  `select`-timeout; timeout ⇒ unhealthy (a wedged FUSE mount must not hang the reaper or
  `/health`). The leaked goroutine on a truly-wedged stat is acceptable; comment it.
- `docs/20-DEPLOYMENT.md` §6: add `lazarr_reaper_skipped_total` rising ⇒ mount unhealthy,
  reaping paused — alert on it alongside `lazarr_tos_audit_leaks`.
- (Optional, only if clean): enforce max-hold even when unhealthy after >1h sustained
  unhealthiness. If it complicates the guard, skip — the metric+alert is the requirement.

Commit: `phase3(fix): S3 reaper-skip metric + Healthy() timeout (docs/21)`

## Stage 5 — S7 + S8 (driver; CI/deploy files only, no Go)

**S7** — `.github/workflows/release.yml` + `Dockerfile`: add `ARG VERSION=dev` and use
`-ldflags "-X github.com/rushp4000/lazarr/internal/version.Version=${VERSION}"` in the
image build; pass `VERSION=${{ github.ref_name }}` (or the GoReleaser-provided tag) as a
build-arg in the workflow so the GHCR image's `/health` reports the real version.
**S8** — make the example pair consistent: set `metrics.listen: ":9090"` in
`config.example.yaml` (it's the documented observability default in docs/20) so the
`docker-compose.example.yml` healthcheck works out of the box; note in a comment that
clearing it disables the admin port AND requires removing the healthcheck.
Also (cheap, from N6): pin `govulncheck` in `ci.yml` to a version instead of `@latest`.

Commit: `phase3(fix): S7 version-stamped image, S8 example healthcheck/metrics consistency, pin govulncheck (docs/21)`

## Stage 6 — verify, document, push (driver)

1. Full suite + `go test -race ./...` + `govulncheck ./...` — all green.
2. Update `docs/21-PR1-REVIEW-FINDINGS.md`: mark each fixed finding `✅ FIXED <commit>` and
   update the Verdict section (blockers cleared; canary re-validation still pending).
3. Nice-to-haves N1–N5: do NOT start them in this pass unless everything above is done and
   green; if attempted, each is its own commit and is droppable.
4. `git push origin phase3` (updates PR #1). **Do NOT merge. Do NOT tag.**
5. Report back: per-stage commit hashes, test summary, govulncheck output, and the two
   canary re-validation steps the USER must run before merge (Sonnet must not touch the
   live stack):
   - restart canary with `kill -9` (not SIGTERM) mid-materialize → confirm boot
     reconciliation releases the leftover (mylist via `bypass_cache=true`).
   - a grab >24h old played fresh → confirm no mid-playback delete (B1).

## Acceptance criteria

- B1–B3, S1–S8 each fixed with a test (where testable) and named in a commit message.
- `go build/vet/test/-race` green across the module; `govulncheck` clean.
- No live TorBox call anywhere (tests use fakes/fixtures); live stack untouched.
- PR #1 updated on `origin/phase3`; NOT merged, NOT tagged.
