# 14 — Detailed Phase 1 kickoff prompt

Driver model: **Opus** (set by user). Subagents: **Sonnet** (pass `model: "sonnet"` to
every Agent call). Paste the block below to start the build session.

---

```
You are the DRIVER for building Lazarr — a self-hosted, ToS-compliant TorBox
lazy-materialize shim ("our own CatBox") that presents as a qBittorrent download client
to the *arr suite. At grab time it symlinks with NO TorBox add; on playback it
materializes (add → presigned URL → proxy stream); after idle it releases (deletes from
TorBox). Host 192.168.7.133, Docker access works, configs local. You are on Opus; spawn
all subagents on Sonnet.

═══ STEP 0 — ORIENT (read before doing anything) ═══
Read, in order:
1. Memory: project-build-own-catbox.md, feedback-skill-install-approval.md, and
   project-cleanuparr-decypharr.md (Open Item #0 for stack context).
2. /root/Github/Lazarr/docs/: 05-spec (component spec), 09-build-subagent-plan (the agent
   prompts), 11-constraints-and-constants (verified limits + Go constants), 12-tos-
   compliance, 08-p0-verification (the verified live API responses → your fixtures),
   02-torbox-api, 03-arr-qbit-integration.
3. The scaffold at /root/Github/Lazarr (git repo, branch main, commit 0e3a9c4). Module
   github.com/rushp4000/lazarr. Go 1.26.4 at /usr/local/go (PATH already in /root/.bashrc;
   if not, `export PATH=$PATH:/usr/local/go/bin`). `go build ./...` and `go vet ./...`
   already pass. internal/{constants,config,catalog,torbox,symlink,qbit,vfs,materialize}
   are INTERFACE-ONLY and those interfaces are FROZEN CONTRACTS — implement against them;
   do not change a signature without flagging it to me first.

═══ STANDING RULES (do not break) ═══
- NEVER install a skill/plugin without my explicit approval.
- Anything that MUTATES the live TorBox account or touches the real arr/Plex stack
  (the canary) requires my go-ahead first. Subagent CODE must make NO live TorBox calls —
  use fixtures only.
- Creating the GitHub repo / pushing requires my go-ahead.
- Stay strictly ToS-compliant: never add at grab; release on idle; ship the ToS-audit
  loop. See docs/12.
- Use the project skills torbox-api / qbit-emu / lazarr-canary and the installed golang-*
  skills (golang-testing, golang-concurrency, golang-error-handling, golang-security,
  golang-database, golang-project-layout, golang-context, stretchr-testify, etc.).

═══ STEP 1 — FIXTURES (you do this; ~15 min) ═══
Create /root/Github/Lazarr/testdata/torbox/ with JSON fixtures captured from the VERIFIED
live responses in docs/08 (and the shapes in docs/02/11):
  checkcached_cached.json (object keyed by infohash; name,size,files[{id,name,size}]),
  checkcached_miss.json, torrentinfo.json, createtorrent_cached.json
  (success:true, detail:"Found Cached Torrent…", data:{hash,torrent_id}),
  createtorrent_ratelimited.json (detail:"60 per 1 hour", success:null),
  requestdl.json (data:"https://…tb-cdn.io/…"), controltorrent_delete_ok.json
  (success:true, detail:"Torrent deleted successfully."), mylist.json, user_me.json
  (data.plan=1, additional_concurrent_slots=0, long_term_storage=false, cooldown_until).
Agents will serve these via httptest — no network.

═══ STEP 2 — SPAWN PHASE 1 SUBAGENTS (Sonnet, worktree-isolated, parallel) ═══
Spawn FOUR general-purpose subagents with model:"sonnet", isolation:"worktree". They are
independent (interfaces already exist) so run them in parallel/background. Give each the
matching prompt from docs/09-build-subagent-plan.md (Agents T, Q, C, S). Enforce in every
prompt: implement ONLY your one package against its frozen interface; NO live TorBox calls
(use testdata fixtures + httptest); add table-driven tests; run `go test ./... -run <pkg>`
+ `go vet`; do not touch other packages; report test output.
  - Agent T → internal/torbox  (HTTP client; ErrLinkExpired on 4xx; release = POST
    controltorrent {torrent_id,operation:"delete"}; checkcached batch ≤100).
  - Agent C → internal/catalog (modernc.org/sqlite, cgo-free; Store impl + migrations).
  - Agent S → internal/symlink (category tree; idempotent; safe removal).
  - Agent Q → internal/qbit    (qBittorrent WebUI emulation; on add → checkcached for
    sizes, NO add; report complete immediately: progress=1.0,state=pausedUP,content_path;
    serve torrents/info from catalog). Q depends on the catalog.Store + torbox.Client
    interfaces only (use fakes in tests).

═══ STEP 3 — REVIEW GATE (per package, before merge) ═══
For each finished package run the built-in `code-review` then `security-review`. Focus:
API key never logged/leaked; no SSRF via attacker-influenced URLs; range/seek correctness;
goroutine/mutex safety; clean error handling. Fix findings before merging the worktree.

═══ STEP 4 — INTEGRATE (you, on main) ═══
Merge the four worktrees. Wire cmd/lazarr/main.go in the documented order:
catalog.OpenSQLite → torbox.New(cfg) → symlink.New(cfg.Paths) →
qbit.New(qbit.Deps{cfg,store,tb,sym}) → http.ListenAndServe(cfg.QBit.Listen). (vfs +
materialize are Phase 2 — leave stubbed.) `go build ./...`, `go vet ./...`, all tests
green. Commit. Then build the Docker image locally.

═══ STEP 5 — CANARY (radarr_hin) — STOP, get my go-ahead first (touches live stack) ═══
Movie: **Big Buck Bunny (2008), tmdbId 10378, imdbId tt1254207** — public-domain,
verified TorBox-cached (hash dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c). Backup: Sintel
(tmdbId 45745). radarr_hin = port 7880, API key = its `token` in /config/decypharr/
config.json, quality profile "Any" (id 1), root /movies (accessible).
Procedure (see lazarr-canary skill + docs/06):
  1. Run Lazarr (own volumes; FUSE caps --cap-add SYS_ADMIN --device /dev/fuse
     --security-opt apparmor:unconfined). Phase-1 vfs is a stub (read→zeros) — that's fine;
     we are only proving grab→import here.
  2. Record TorBox mylist count (baseline).
  3. In radarr_hin, add Big Buck Bunny (profile Any, root /movies), point its qBittorrent
     download client at Lazarr (host lazarr, port 8080, category radarr_hin), search+grab.
  4. ASSERT: Lazarr logs a checkcached hit with real size; the category symlink appears;
     radarr_hin imports the file into /movies; **TorBox mylist count is UNCHANGED** (no add)
     = the ToS-compliant core proven.
  5. Roll back anytime by repointing radarr_hin's download client to decypharr (:8282).
Report results. Do NOT start Phase 2 (vfs/materialize playback) until I review.

═══ STEP 6 — PUBLISH (only after Phase 1 is green AND I approve) ═══
Create private GitHub repo rushp4000/lazarr, add it as origin, push main. Keep config.yaml
and *.sqlite out (already gitignored). Then propose the Phase 2 plan (vfs + materialize +
probe cache + reapers + ToS-audit) per docs/05/06/09.

Confirm you've read the docs and the frozen interfaces, then start at STEP 1. Checkpoint
with me before STEP 5 (canary) and STEP 6 (publish).
```

---

### Quick reference for the driver
- Canary movie: **Big Buck Bunny 2008 / tmdb 10378 / hash `dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c`** (cached-verified). Backup **Sintel / tmdb 45745**.
- radarr_hin: **7880**, profile **Any=1**, root **/movies**, token = `arrs[radarr_hin].token` in `/config/decypharr/config.json`.
- TorBox key: `debrids[torbox].api_key` in the same file. Base `https://api.torbox.app/v1/api`.
- Release contract (verified): **POST** `/torrents/controltorrent` `{torrent_id, operation:"delete"}`.
- Subagents: **Sonnet**, `isolation:"worktree"`. Driver: **Opus**.
