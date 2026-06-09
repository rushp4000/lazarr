# 19 — Phase 3 continuation + Phase 4 (hand to the next driver)

Phase 3 is **almost done**. The live canary HARD GATE passed and four of the five Phase-3
agents are committed on branch `phase3`. This doc is the ready-to-use prompt for finishing
Phase 3 (docs + review gate + merge) and the Phase-4 plan. Pair it with `docs/17` (original
kickoff) and the memory note `lazarr-phase3-canary-findings`.

## State at handoff (branch `phase3`, all commits build+vet+test+-race GREEN)

| Item | Commit | Status |
|---|---|---|
| Live canary (Plex Direct Play → materialize → idle release → re-materialize) | — | ✅ PROVEN live |
| vfs nested rel_path (#3) + inode fix | d662c45 | ✅ |
| FUSE AllowOther / DirectMountStrict / MaxWrite / fuse.conf | c6ec0c0, 8dbd5b5 | ✅ live-proven |
| Robustness: config validation, PUID/PGID chown, dead-cache ErrPurged, broken-mount guard, EBUSY unmount | 49351b3, 603c83a | ✅ |
| Observability: /metrics + /health on opt-in admin port, counters wired | 2d579b4 | ✅ |
| CI/CD: Actions, golangci, govulncheck, Dependabot, GoReleaser, GHCR-on-tag | 41a9084 | ✅ |
| Readahead drop (#4) | 3dd4716 | ✅ |
| MIT LICENSE | (this batch) | ✅ |

Canary container is running as **root** on host `:8088` (`lazarr:phase2`), Decypharr disabled
on radarr_hin. TorBox `mylist` queried with `bypass_cache=true` returns the true count.

## STEP A — finish Agent D (docs) — the only remaining feature work
Write on `phase3`:
- `README.md`: what Lazarr is; the **ToS-compliance statement** (symlink-at-grab, materialize-on-
  play, drain-at-idle — pull from `docs/12`); quickstart; **full config reference** — every key now
  in `internal/config/config.go`: `torbox.{api_key,api_base}`, `qbit.{listen,username,password}`,
  `paths.{download_dir,fuse_mount,db_path,probe_cache_dir}`, `categories`,
  `policy.{allow_uncached,idle_ttl,max_hold,active_slots,probe_cache}`, `ownership.{puid,pgid}`,
  `metrics.listen`; the **trusted-LAN trust model** (`docs/15` §4.D — no auth on the qbit or admin
  surface, bind to LAN); Docker/compose run with the **FUSE caps + run-as-root + PUID/PGID** model
  (`--cap-add SYS_ADMIN --device /dev/fuse --security-opt apparmor:unconfined`, NOT `--user`; set
  `ownership.puid/pgid` to the arr uid; `/decypharr_symlinks:rshared` propagation); the
  observability section (`metrics.listen`, `/metrics`, `/health`); a clear "use at your own risk /
  respect TorBox ToS" note.
- `CONTRIBUTING.md`: build (`CGO_ENABLED=0 go test ./...`), lint (`golangci-lint run`), the
  fixtures-not-live-TorBox rule, conventional-commit prefixes.
- LICENSE is already MIT (done).
Skill: `golang-documentation`. No code changes; commit `phase3(docs): …`.

## STEP B — REVIEW GATE (run by the driver)
On `phase3`, run `/code-review` then `/security-review` vs `origin/main`. Focus per docs/17:
- `/metrics` + `/health` leak no secrets / API key; admin port stays off the arr surface.
- broken-mount guard actually prevents mass-delete; config validation rejects unsafe combos.
- CI workflows have least-privilege `permissions:` and no secret echo (they do today).
- the new chown path can't be tricked into chowning outside the download tree.
Fix findings before merge.

## STEP C — lint to green (flip CI lint from advisory → blocking)
The CI lint job is `continue-on-error: true` because the tree carries pre-existing findings.
Run `golangci-lint run`; resolve the residue (≈ errcheck 8, gosec 11 [several defensible — add
justified `//nolint:gosec // reason` or config excludes], ineffassign 1, unused 2 in
`internal/torbox` option helpers, bodyclose 1), then drop `continue-on-error` in
`.github/workflows/ci.yml`. This is hygiene, not correctness.

## STEP D — INTEGRATE → merge `phase3` → `main` (★ PUSH GATE — user OK first)
Fast-forward/merge `phase3` into `main`, push. CHECKPOINT with the user before pushing.

## v1 EXIT CRITERIA → then PUBLIC (★ user-only hard gates)
- All tests green; `/security-review` clean; CI green; lint blocking-green.
- Canary stable across materialize/release cycles **for days**, `mylist` (bypass_cache) idling
  near baseline; no FUSE/reaper leaks (goleak).
- **User** flips the repo public, **tags `v1.0.0`** (triggers GoReleaser + GHCR push — confirm the
  GHCR image builds the first time; the workflow uses `GITHUB_TOKEN`), finalizes release notes.

## PHASE 4 — soak & cutover (after v1)
1. **Soak**: leave the canary (radarr_hin) on Lazarr for days; watch `/metrics`
   (`lazarr_tos_audit_leaks` must stay 0, `materialized_count` idle near 0) and the rolling-15-day
   TorBox bandwidth. Add a Prometheus scrape + a tiny alert on `lazarr_tos_audit_leaks > 0`.
2. **Gradual cutover**: point more arrs at Lazarr for the **TorBox leg only** (decypharr keeps the
   RealDebrid leg). For each arr: add the Lazarr qbit client, set `ownership.puid/pgid`, move one
   category at a time, watch the audit + bandwidth between steps.
3. **Resume the shelved debrid recovery THROUGH Lazarr** (the 740 broken-file re-grab from
   `project-cleanuparr-decypharr`): now that materialize-on-play is ToS-clean, re-grab via Lazarr
   instead of decypharr's eager-add. Watch slot pressure + createtorrent rate limit
   (`lazarr_createtorrent_ratelimited_total`).
4. **Cleanups**: canary teardown (`docs/15` §3.1 rollback) once fully cut over; the
   `gh`-plaintext-credentials fix (`todo-gh-plaintext-creds`).

## Gotchas the next driver must know
- **Agent-tool worktree isolation branches from the repo DEFAULT (main), not the current branch.**
  For core-code agents, run WITHOUT isolation on `phase3`, or rebase their branch onto `phase3`.
- **Editing `/config/lazarr_canary/config.yaml` with a root-running tool resets it to root:600** →
  `chown 1003:1003 && chmod 640` after, or the canary container (running as root reads it fine, but
  uid-1003 deployments won't).
- **Manual TorBox `mylist` checks MUST pass `bypass_cache=true`** or you get a stale count.
- Monthly **spend limit** halted subagents mid-Phase-3; the remaining work above was sized to be
  done by the driver directly.
