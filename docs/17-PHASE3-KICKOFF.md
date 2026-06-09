# 17 — Phase 3 Kickoff Prompt (productionization → public v1)

Hand this to the next **Opus** driver session. Phase 1 (grab→import, no add) and Phase 2
(playback: materialize→stream→release, idle/max-hold reapers, ToS-audit, probe cache) are
**built, reviewed, fixed, integrated, smoke-tested, and on `main`** (`github.com/rushp4000/
lazarr`, private, HEAD `fca0d02`). Phase 3 turns the working shim into a **bug-free, public
v1**: observability, robustness, docs, CI/CD, and the two deferred Phase-2 findings.

> ⛓️ **Order:** the **live canary (Phase-2 STEP 5)** should run *before or alongside* Phase 3
> — it's the only thing that proves the materialize→release lifecycle works against the real
> TorBox account + Plex. It is a **hard gate** (real adds/deletes). See §0 and the
> `lazarr-canary` skill. Phase 3 work that doesn't need the live stack can proceed in parallel.

---

## STEP 0 — ORIENT
Read, in order:
1. Memory: `project-build-own-catbox.md` (★ Phase-2 status), `feedback-skill-install-approval.md`,
   `todo-gh-plaintext-creds.md`, the latest session recap.
2. `docs/15-PHASE1-SUMMARY.md`, `docs/16-PHASE2-KICKOFF.md` (esp. the "REMAINING ROADMAP"
   section — Phase 3 / 3.x / 4 / v1 exit criteria), `docs/11`, `docs/12`.
3. The shipped code you'll extend: `internal/materialize/{engine,proxy,reaper,audit,probecache}.go`,
   `internal/vfs/fs.go`, `internal/qbit/server.go`, `cmd/lazarr/main.go`, `internal/config`,
   `internal/constants`.
4. Skills: `golang-observability`, `golang-continuous-integration`, `golang-lint`,
   `golang-documentation`, `golang-security`, plus `torbox-api` / `lazarr-canary`.

## STANDING RULES (unchanged)
- **Never install a skill/plugin without explicit user approval.**
- **Subagent CODE makes NO live TorBox calls** — fixtures + `httptest` only.
- **Anything that mutates the live TorBox account or the arr/Plex stack needs the user's
  go-ahead first.** The canary (STEP 5) and any GHCR/secret push are gates.
- Stay ToS-compliant: never add at grab; release on idle; keep the ToS-audit loop.
- Work Phase 3 on a **branch** (`phase3`) so `/code-review` + `/security-review` diff cleanly
  vs `origin/main`; merge after the review gate.

---

## ★ THE LIVE CANARY (STEP 5 from Phase 2) — what it is, run it first
The canary is a **parallel, isolated, instantly-reversible** live test of Lazarr against ONE
real arr (`radarr_hin`, port 7880, 0 movies) on host `192.168.7.133`, running **alongside the
live decypharr stack without touching it**. Phase 1 already proved grab→import live (mylist
stayed flat = no add). Phase-2's canary proves the **playback half**:
1. Redeploy the `lazarr_canary` container with the **real `vfs` FUSE mount** (replacing the
   Phase-1 sparse-placeholder harness), FUSE caps (`--cap-add SYS_ADMIN --device /dev/fuse
   --security-opt apparmor:unconfined`), `--user 1003:1003`. Add the radarr_hin symlink dir as
   a **Plex** test library.
2. **Press play in Plex.** ASSERT: the item appears in `mylist` (materialized), the stream
   plays (HTTP 206 ranged proxy), bytes are real.
3. Stop; wait `idle_ttl` (15m). ASSERT: the **idle reaper released it** (`mylist` back to
   baseline) while the symlink + Plex entry remain (re-materializes on next play).
4. Force a stale link / wait past expiry. ASSERT: **refresh-on-4xx** keeps playback alive.
5. Start more concurrent streams than `active_slots` (3). ASSERT: queue/LRU behaves, the
   account never errors. Keep a mylist-vs-materialized audit running throughout.
**Rollback anytime:** repoint radarr_hin's download client to decypharr (`:8282`). Full
teardown steps in `15-PHASE1-SUMMARY.md` §3.1. This is a HARD GATE — get the user's go-ahead
before the redeploy (it performs real adds/deletes). Use the `lazarr-canary` skill.

---

## MODELS — which Anthropic model per agent
- **Driver: Opus** (you). Integration, the live canary, GHCR/release, secret handling.
- **Agent O — observability → SONNET.** `/health`, `/metrics`, counters. Bounded; Sonnet ok.
- **Agent R — robustness → OPUS.** Touches the concurrent engine + FUSE lifecycle (dead-cache
  state machine, broken-mount guard, reaper/unmount edge cases). Correctness-sensitive → Opus.
- **Agent D — docs → SONNET.** README/CONTRIBUTING/LICENSE; no code logic.
- **Agent C — CI/CD → SONNET.** GitHub Actions, Dependabot/Renovate, GoReleaser, GHCR.
- **Agent P — Phase-2 deferred fixes → SONNET.** vfs nested-path + readahead (see §Deferred).
- **Review:** built-in `code-review` then `security-review`, run by you.

> Use **worktree isolation** for each agent. Because they touch overlapping files
> (`main.go`, `config.go`, `materialize/*`), prefer running them **in dependency order or
> serially per file** rather than all-parallel, and reconcile `go.mod`/`main.go` at integration
> (Phase-2 lesson: parallel worktrees diverge on `go.mod` + `main.go` — copy files + `go mod
> tidy` + re-wire `main.go` by hand at the end).

---

## STEP 1 — Phase 3 work items (each → its agent)

### Agent O — Observability (`internal/metrics` + handlers in qbit or a new admin mux)
- `GET /health` (200 + JSON: mounted?, slots in use/total, last-audit time, build version).
- `GET /metrics` Prometheus: `lazarr_materialized_count`, `lazarr_slots_in_use`,
  `lazarr_grabs_total`, `lazarr_materializes_total`, `lazarr_releases_total`,
  `lazarr_link_refresh_total` (4xx refreshes), `lazarr_tos_audit_leaks`,
  `lazarr_probe_cache_hits_total` / `_misses_total`, `lazarr_createtorrent_ratelimited_total`.
- Wire counters at the call sites in `materialize` (atomic counters or a small `metrics`
  package the engine holds) + `qbit` grab. Decide: expose on the qbit listener under a
  non-colliding path, or a **separate admin `listen_admin` port** (recommended — keeps it off
  the arr-facing surface). Add `qbit.listen_admin` / `metrics.enabled` config.
- Skills: `golang-observability`. Tests: handler returns valid Prometheus text; counters move.

### Agent R — Robustness (Opus)
- **Dead-cache handling:** when `checkcached` AND `torrentinfo` both miss at grab → release is
  already `error` (Phase 1). At materialize time, surface a TorBox "not found / purged" as a
  clear error state to the arr (so it blacklists + re-grabs) instead of a silent EIO. Add a
  state/transition + test.
- **Broken-mount guard (CRITICAL):** the reapers/Release call `ControlDelete`. If the FUSE
  mount is unhealthy/stale, never mass-delete from the account on a transient mount blip — add
  a guard (e.g. skip reaping if the mount is not healthy; see `lazarr-canary` "Guards"). Test.
- **Config validation on load:** `config.Load` should reject nonsense (empty download_dir/
  fuse_mount when categories set, idle_ttl ≥ max_hold, active_slots < 0) with clear errors;
  finish the `active_slots: 0` auto-detect path (it's wired, exercise it).
- **Graceful-shutdown hardening:** unmount may return EBUSY with in-flight reads — bounded
  retry/force. (Reaper + audit-loop cancellation already wired in `main.go`.)
- Skills: `golang-safety`, `golang-concurrency`, `golang-context`, `golang-testing`.

### Agent P — Phase-2 deferred fixes (Sonnet)
- **#3 vfs nested `rel_path`** (`internal/vfs/fs.go`): `hashDirNode.Lookup` currently exact-
  matches flat names only → a file whose rel_path contains `/` (subdir, e.g. season packs)
  returns ENOENT → broken symlink → import fails. Implement synthetic intermediate dir nodes
  (split rel_path on `/`, expose dir nodes, resolve the leaf to a `fileNode`). Add a mount test
  with a nested rel_path. **Coordinate with `internal/symlink`** which builds the target path.
- **#4 readahead** (`internal/materialize/proxy.go`): currently inert — the extra requested
  range is discarded (no prefetch buffer). Either (a) implement a small per-(hash,fileID)
  readahead buffer reused by the next sequential ReadAt, or (b) drop the readahead request
  widening entirely and document that each ReadAt fetches exactly its window. Benchmark with
  `golang-benchmark`/`golang-performance` before adding complexity; (b) may be the right call.

### Agent D — Docs (Sonnet)
- `README.md`: what Lazarr is, the ToS-compliance statement (docs/12), quickstart, the
  **trusted-LAN trust model** (`15-PHASE1-SUMMARY.md` §4.D — no auth on the qbit surface; bind
  to the LAN), full config reference (every `config.yaml` key), Docker/compose with the FUSE
  caps, a clear "use at your own risk / respect TorBox ToS" note.
- `CONTRIBUTING.md`, `LICENSE` (pick one with the user — MIT/Apache-2.0).
- Skills: `golang-documentation`.

### Agent C — CI/CD & supply chain (Sonnet) — `.github/`
- GitHub Actions: `go build` + `go vet` + `CGO_ENABLED=0 go test ./...` (matrix incl. a
  `-race` job with cgo) + **golangci-lint** (`golang-lint`) + **govulncheck**.
- Optional: the Claude `code-review` / `security-review` automation (needs the user's API key
  in repo secrets → **gate on user**).
- Dependabot **or** Renovate for `go.mod` + Actions; **pin the toolchain** (`go.mod` `go
  1.26.x` + a `toolchain` directive).
- **GoReleaser + GHCR** multi-arch image (`linux/amd64,arm64`) build & push **on tag** →
  needs `GHCR` auth (**gate on user**; don't push images without go-ahead). Fold the existing
  `Dockerfile` (CGO=0 + fuse3) in.

## STEP 2 — REVIEW GATE
On `phase3` run `code-review` then `security-review` vs `origin/main`. Focus: metrics endpoint
doesn't leak secrets or the API key; admin/metrics port not exposed to the arr surface
unintentionally; broken-mount guard actually prevents mass-delete; config-validation rejects
unsafe combos; CI workflow has least-privilege `permissions:` and no secret echo. Fix before
merge.

## STEP 3 — INTEGRATE → merge `phase3` → main, push (push gate = user OK).

---

## WHAT'S LEFT AFTER PHASE 3 → v1 (exit criteria, then PUBLIC)
- All tests green; `/security-review` clean; **canary stable across materialize/release cycles
  for days** with `mylist` idling near baseline; no FUSE/reaper leaks (goleak); CI green;
  GHCR image published.
- **Flip the repo public** (★ HARD GATE — user only), **tag `v1.0.0`**, release notes,
  finalize README with the ToS statement.
- **Phase 4 soak & cutover** (docs/16): gradually point more arrs at Lazarr for the **TorBox
  leg only** (decypharr keeps RealDebrid), watch the ToS-audit + rolling-15-day bandwidth. Then
  resume the shelved debrid recovery THROUGH Lazarr.
- **Then the cleanups:** canary teardown (`15-PHASE1-SUMMARY.md` §3.1) and the
  `gh`-plaintext-credentials fix (`todo-gh-plaintext-creds`).

## Checkpoints (stop for the user)
The live canary redeploy (real adds/deletes); enabling CI secrets / pushing a GHCR image;
flipping the repo public + tagging v1.
