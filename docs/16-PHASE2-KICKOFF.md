# 16 — Phase 2 Kickoff Prompt (playback: materialize → stream → release)

Hand this to the next **Opus** driver session. Phase 1 (grab → import, no TorBox add) is
done, canary-proven, and published (`github.com/rushp4000/lazarr`, private, HEAD `9412ba9`).
Phase 2 makes playback work: on file read, **materialize** (add → presigned URL →
range-proxy), then **release on idle** so the account returns to empty. This is the other
half of the ToS-compliant lazy lifecycle.

Read `15-PHASE1-SUMMARY.md` first (status + open items + the detailed security notes).

---

## STEP 0 — ORIENT
Read, in order:
1. Memory: `project-build-own-catbox.md`, `feedback-skill-install-approval.md`,
   `todo-gh-plaintext-creds.md`.
2. `docs/15-PHASE1-SUMMARY.md`, then `docs/05-spec.md` §3 (vfs) §4 (materialize) §6
   (probe-header strategy), `docs/11-constraints-and-constants.md` (verified limits + Go
   constants), `docs/12-torbox-tos-compliance.md` (ToS boundary).
3. The **frozen interface contracts** (do not change a signature without flagging it — see
   the interface-change note below):
   - `internal/vfs/vfs.go` — the `Materializer` interface vfs consumes.
   - `internal/materialize/materialize.go` — `Deps`, the `Engine` interface (ReadAt,
     Release, AuditTOS, Close).
4. The Phase-1 implementations you depend on: `internal/catalog` (`Store`), `internal/torbox`
   (`Client`, `ErrLinkExpired`, `ErrRateLimited`), `internal/config`, and the wiring comments
   in `cmd/lazarr/main.go`.
5. Project skills: **`torbox-api`** (Agent M), **`lazarr-canary`** (the Phase-2 check), and
   the installed `golang-*` skills.

## STANDING RULES (unchanged)
- **Never install a skill/plugin without explicit user approval.**
- **Subagent CODE makes NO live TorBox calls** — fixtures + `httptest` only.
- **Anything that mutates the live TorBox account or the real arr/Plex stack requires the
  user's go-ahead first.** Phase-2 canary performs **real adds/deletes** → hard gate.
- Stay strictly ToS-compliant: never add at grab; release on idle; ship the ToS-audit loop.
- Work Phase 2 on a **branch** (`phase2`) so `/security-review` and `/code-review` diff
  cleanly against `origin/main`; merge after the review gate passes.

---

## ★ PRE-PHASE-2 INTERFACE DECISION (flag to the user before spawning agents)
`materialize.Deps` is currently `{Store, TorBox, Slots int}`. The engine also needs policy
knobs: `allow_uncached`, `idle_ttl`, `max_hold`, `readahead`, `probe_cache`,
`link_refresh_statuses`, and the FUSE mount/probe-cache dir. **Recommended (additive) change:**
extend `materialize.Deps` to carry `Policy config.Policy` (and a probe-cache dir + readahead),
rather than threading many scalars. This is an additive change to a frozen contract → **get
the user's OK first**, then update `materialize.go` + the `main.go` wiring comment before
spawning Agent M. (vfs's `New` is not frozen; Agent V defines it.)

---

## MODELS — which Anthropic model per agent
- **Driver: Opus** (you). Orchestrate, own integration + the live canary.
- **Agent M — `internal/materialize` → OPUS.** This is the most correctness-critical package
  in the whole project: a slot **semaphore**, **singleflight** dedupe of concurrent first-reads
  per release, **LRU eviction** under slot pressure, **refresh-on-4xx**, two **reapers**, an
  **HTTP range-proxy**, and the **ToS-audit** loop — all concurrent, all touching the live
  account's lifecycle. Use Opus.
- **Agent V — `internal/vfs` → SONNET.** A read-only FUSE filesystem whose hard parts are
  well-bounded (Getattr/Lookup/Readdir from the catalog; Open/Read delegate to the
  Materializer). Sonnet is sufficient. **Escalate to Opus** only if FUSE concurrency/locking
  proves fiddly (go-fuse callbacks are concurrent — see golang-concurrency).
- **Review: built-in skills** (`code-review` then `security-review`) — run by you, the driver.

> Rationale recap from the project: "Opus driver + Sonnet workers, keep qbit + materialize on
> Opus." vfs is the one Phase-2 package safe for Sonnet.

---

## STEP 1 — FIXTURES (driver, ~10 min)
Existing `testdata/torbox/*.json` already cover `createtorrent_cached`,
`createtorrent_ratelimited`, `requestdl`, `controltorrent_delete_ok`, `mylist`, `user_me`.
Add for Phase 2:
- A **fake CDN** is built with `httptest` in Agent M's tests (serve HTTP 206 partial content
  with a correct `Content-Range`, and a 400/403/410 path to exercise `ErrLinkExpired`
  refresh). No new JSON needed, but add a short `testdata/cdn/README.md` documenting the
  expected 206 behavior so the agent's fake matches the real CDN shape from `docs/08`.

## STEP 2 — SPAWN PHASE-2 AGENTS (worktree-isolated)

### Agent V — `internal/vfs` (Sonnet, worktree)
```
Build internal/vfs: a read-only FUSE filesystem (github.com/hanwen/go-fuse/v2) mounted at the
configured fuse_mount, layout /<hash>/<rel_path>. Read docs/05 §3 and 15-PHASE1-SUMMARY.md.
Implement ONLY this package against the frozen vfs.Materializer interface (do not change it).
Provide New(fuseMount string, store catalog.Store, mat Materializer) returning a type with
Mount() error and Close()/Unmount() error (wiring: fs := vfs.New(cfg.Paths.FuseMount, store,
eng) in main.go). Getattr/Lookup/Readdir serve names+sizes FROM the catalog Store (no TorBox
calls) so stat/ls/import work without materializing. Open/Read(offset,len) call
Materializer.ReadAt(hash,fileID,p,off) for that byte range and rely on it to update
last_access. Map the /<hash>/<rel_path> path back to (hash, fileID) via the Store (GetRelease
-> files; match rel_path -> file_id). Handle missing entries with ENOENT, not panics. Ensure
clean unmount on Close (Phase-1 graceful shutdown calls it). Tests: mount to a t.TempDir()
(t.Skip if /dev/fuse absent or unprivileged), with a FAKE Store + FAKE Materializer; assert
(a) stat returns the catalog size WITHOUT calling the materializer, (b) read delegates to the
materializer for the right (hash,fileID,offset,len), (c) readdir lists the right names, (d)
ENOENT for unknown paths. Document required Docker caps (--cap-add SYS_ADMIN --device
/dev/fuse --security-opt apparmor:unconfined) in a package comment. NO live TorBox. go test
./internal/vfs/... + go vet. Don't touch other packages. Report test output and the New/Mount
signatures you chose. Use skills: golang-concurrency, golang-safety, golang-context,
golang-error-handling, golang-testing.
```

### Agent M — `internal/materialize` (Opus, worktree)
```
Build internal/materialize per docs/05 §4 + docs/11 + docs/12 + 15-PHASE1-SUMMARY.md §4.F.
Implement ONLY this package against the frozen Engine interface (ReadAt, Release, AuditTOS,
Close) + the (updated) Deps. Use the torbox-api skill. NO live TorBox calls — interfaces +
fakes + httptest only.

Behaviour on ReadAt(hash, fileID, p, off):
1. Slot control: a semaphore sized from Deps.Slots (0 => auto from TorBox.UserMe().ActiveSlots;
   cap to the verified Essential=3). When full, LRU-release the least-recently-used idle
   materialized release before admitting a new one; queue if still full.
2. Dedupe concurrent first-reads of the same hash with singleflight (golang.org/x/sync or
   samber) so one materialize happens per release.
3. If the release isn't materialized: TorBox.CreateTorrent(magnet, addOnlyIfCached=
   !Policy.AllowUncached) -> Store.SetState(hash, materialized, torboxID). Handle
   ErrRateLimited (surface a clear error; do not spin) and "not cached + uncached disabled".
4. Ensure a fresh dl_link: Store.GetLink; if missing/near-expiry -> TorBox.RequestDL ->
   Store.SetLink. Range-proxy the requested window: HTTP Range GET to the CDN URL, copy bytes
   into p. On ErrLinkExpired (4xx in LinkRefreshStatuses) invalidate the link, RequestDL once
   more, retry once.
5. Store.TouchAccess(hash, now) on every read.

SECURITY (see 15-PHASE1-SUMMARY §4.F): before issuing the range GET, require https and pin the
CDN host (allow only the expected *.tb-cdn.io suffix or a configurable allowlist); refuse
private/loopback IPs; use a dedicated http.Client with timeouts that does not follow redirects
to private ranges. Never log the API key or the requestdl token (mirror torbox.redactURLError).

Reapers (background goroutines, interval ~30s, ctx-cancellable):
- Idle: Store.IdleCandidates(now-idle_ttl) -> TorBox.ControlDelete -> Store.SetState(virtual,0).
- Max-hold: Store.OverMaxHold(now-max_hold) -> same release path.
Release(hash): force-release a materialized item (used by reapers/shutdown/LRU).
AuditTOS(): diff TorBox.MyList vs Store.MaterializedIDs; log/alarm anything the account holds
that we believe is released. Scope to Lazarr-added ids while the account is shared with
decypharr (docs/12).
Probe-header cache (docs/05 §6): on first materialize, cache the first N MiB per (hash,fileID)
to a bounded on-disk dir; serve subsequent header-region reads from cache so Plex scans don't
re-add (protects the ~55/hr createtorrent budget).

Tests (table-driven, fakes + httptest CDN, NO network): slot exhaustion + LRU eviction; per-
hash singleflight; createtorrent rate-limit + not-cached paths; dl_link cache + refresh-on-4xx
(assert exactly one re-RequestDL + retry); range proxying correctness (offset/len, partial
reads); idle reaper releases + SetState(virtual); max-hold reaper; AuditTOS leak detection;
probe-cache hit avoids a second add; CDN host-pin rejects a non-tb-cdn / private-IP URL.
go test ./internal/materialize/... + go vet. Don't touch other packages. Report test output.
Use skills: golang-concurrency, golang-context, golang-error-handling, golang-security,
golang-safety, golang-testing, golang-performance, torbox-api.
```

## STEP 3 — REVIEW GATE (per package, before merge)
On the `phase2` branch run **`code-review`** then **`security-review`** (now diffs cleanly vs
`origin/main`). Focus: API key/token never logged; **SSRF host/scheme pinning on the proxied
CDN URL**; range/seek correctness; goroutine/mutex/semaphore safety + no leaks (consider
goleak); singleflight correctness; clean FUSE unmount; reaper cancellation on shutdown. Fix
findings before merge. Also land the **Phase-1 hardening items C & E** (infohash validation,
base32/url-decode) and **G** (SQLite DSN pragmas) while you're in the code.

## STEP 4 — INTEGRATE (driver, on `phase2` → merge to main)
Uncomment the Phase-2 wiring in `cmd/lazarr/main.go`:
`eng := materialize.New(Deps{...})` → `fs := vfs.New(cfg.Paths.FuseMount, store, eng)` →
`fs.Mount()` → `go eng.AuditTOS` loop + `go` reapers (or have New start them) → unmount + Close
on graceful shutdown. `go build ./... && go vet ./... && CGO_ENABLED=0 go test ./...` green.
Build the Docker image (FUSE caps already in Dockerfile/compose). Merge `phase2` → `main`, push.

## STEP 5 — CANARY HALF 2 (radarr_hin + Plex) — STOP, get user go-ahead (REAL account mutation)
This performs **real TorBox adds/deletes** and needs the real FUSE mount (replaces the Phase-1
sparse-placeholder harness). Per the `lazarr-canary` skill "Phase 2 check":
1. Redeploy `lazarr_canary` with the real FUSE mount (its own /decypharr_symlinks/lazarr_fuse
   served by vfs, FUSE caps). Add the radarr_hin symlink dir as a Plex test library.
2. Press play in Plex. ASSERT: the item appears in `mylist` (materialized), stream plays
   (HTTP 206 ranged proxy), bytes are real.
3. Stop; wait `idle_ttl`. ASSERT: the idle reaper released it (`mylist` back to baseline) while
   the symlink + Plex entry remain.
4. Force a stale link (or wait past expiry) -> ASSERT refresh-on-4xx keeps playback alive.
5. Slot safety: start more concurrent streams than `active_slots` -> ASSERT queue/LRU behaves
   and the account never errors. Keep a mylist-vs-materialized audit running throughout.
Roll back anytime by repointing radarr_hin's client to decypharr (:8282). Report results.

---

## REMAINING ROADMAP TO A BUG-FREE v1 (public on GitHub)

**Phase 2 — playback** (above): vfs + materialize + canary half 2. *Exit:* play→materialize→
release proven live; mylist returns to baseline after idle; refresh + slot safety verified.

**Phase 2.x — security/hardening pass** (fold into the Phase-2 review gate where possible):
- C: infohash hex/base32 validation in `qbit` add (symlink-target traversal).
- E: `net/url` percent-decode + base32 magnet support.
- F: CDN host/scheme pinning in `materialize` (done as part of Agent M).
- G: SQLite DSN-level pragmas.
- Re-run `/security-review`; aim for zero open findings.

**Phase 3 — productionization:**
- Observability: `/health`, Prometheus `/metrics` (materialized count, slot usage, grabs/min,
  4xx-refreshes, releases/min, ToS-audit deltas); structured slog already in place
  (skill: golang-observability).
- Robustness: graceful FUSE unmount + reaper shutdown on SIGINT/TERM; dead-cache handling
  (checkcached/torrentinfo miss -> error state surfaced to the arr); broken-mount guard (never
  delete on an unstable FUSE mount — see lazarr-canary "Guards").
- Docs: README (quickstart, the trusted-LAN trust model from §4.D, ToS-compliance statement,
  config reference, Docker/compose with FUSE caps), CONTRIBUTING, LICENSE
  (skill: golang-documentation).
- Config polish: validate config on load; sane errors; `active_slots:0` auto-detect path.

**Phase 3.x — CI/CD & supply chain** (skill: golang-continuous-integration):
- GitHub Actions: build + `go vet` + `CGO_ENABLED=0 go test` (matrix) + **golangci-lint**
  (golang-lint) + **govulncheck** + the Claude code-review/security-review automation.
- Dependabot/Renovate for go.mod; pin the toolchain.
- GoReleaser + **GHCR** multi-arch image (`linux/amd64,arm64`) build & push on tag.

**Phase 4 — soak & cutover:**
- Gradually point more arrs at Lazarr for the **TorBox leg only** (decypharr keeps RD), watch
  the ToS-audit + bandwidth (rolling 15-day, docs/12). Fix bugs found under real load.
- Resume the shelved debrid recovery THROUGH Lazarr once it is the TorBox path.

**v1 release (exit criteria → public):**
- All tests green; `/security-review` clean; canary stable across materialize/release cycles
  for days with mylist idling near baseline; no FUSE/reaper leaks (goleak); CI + GHCR image
  published.
- **Flip the repo public**, tag **v1.0.0**, write release notes, finalize README with the
  ToS-compliance statement and a clear "use at your own risk / respect TorBox ToS" note.
- Then perform the **canary teardown** and the **gh-plaintext-credentials** cleanup (see
  `15-PHASE1-SUMMARY.md` §3.1 and `todo-gh-plaintext-creds`).

## Checkpoints
Stop for the user before: the materialize.Deps interface change (pre-spawn), STEP 5 (live
canary — real account mutation), and flipping the repo public.
