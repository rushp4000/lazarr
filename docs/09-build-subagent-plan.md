# 09 — Build plan & subagent prompts

How we'll build Lazarr with subagents, **without spawning anything until you approve**.

## Strategy
- **Serial foundation first (I do this, not a subagent):** scaffold the repo, pin the
  Go module, define the shared **interfaces + types + config** every package depends on,
  add the `Dockerfile`/compose, and commit. Shared contracts must exist before parallel
  work or agents will collide on types.
- **Then parallel package agents, each in an isolated git worktree** (`isolation:
  worktree`), scoped to one `internal/` package, with: the relevant doc, the exact
  interface to satisfy, test requirements, and a hard rule: **no live TorBox calls —
  use recorded fixtures.** Background-run the independent ones.
- **Review gate:** after each package lands, run built-in `code-review` then
  `security-review` (key-handling, SSRF on proxied URLs) before merge.
- **Integration + canary: I do this serially** on the host (wiring radarr_hin, FUSE
  mount, the live add→release cycle), since it touches the real stack + account.

## Dependency order
```
[FOUNDATION: scaffold + interfaces + config + go.mod + Dockerfile]   ← me, serial
        │
   ┌────┼───────────────┬──────────────┬───────────────┐
   ▼    ▼               ▼              ▼                ▼
 torbox  qbit         catalog       symlink        (config done)     ← parallel agents (P1)
   └────┴──────┬───────┴──────────────┘
              ▼
   [INTEGRATE P1: arr grab → import, mylist stays flat]  ← me, on host (canary half 1)
              │
        ┌─────┴───────┐
        ▼             ▼
       vfs        materialize                                        ← parallel agents (P2)
        └──────┬──────┘
              ▼
   [INTEGRATE P2: play → materialize → idle release]    ← me, on host (canary half 2)
```

## Agent roster (general-purpose, worktree-isolated)
Each prompt assumes the agent has read `/root/Github/Lazarr/docs/`. **Spawn only after
foundation is committed + you approve.**

### Agent T — `internal/torbox`
```
Build the internal/torbox Go package for Lazarr. Read docs/02-torbox-api.md and
docs/08-p0-verification-results.md first. Implement a TorBox client against base
https://api.torbox.app/v1/api with Bearer auth, methods exactly matching the Client
interface in internal/torbox/iface.go: CheckCached(hashes,listFiles), TorrentInfo(hash),
CreateTorrent(magnet,addOnlyIfCached), RequestDL(torrentID,fileID), ControlDelete(id),
MyList(offset)/MyListByID(id), UserMe(). Parse the response shapes documented in doc 08
(data keyed by infohash for checkcached; files[].size). Implement retry/backoff and
treat HTTP 4xx on RequestDL as 'refresh needed' (return a sentinel error). DO NOT make
live network calls in tests — use httptest with fixtures captured from doc 08's verified
responses (I'll provide /root/Github/Lazarr/testdata/torbox/*.json). Add table-driven
unit tests. Run `go test ./internal/torbox/...` and `go vet`. Do not touch other
packages. Report the test output.
```

### Agent Q — `internal/qbit`
```
Build the internal/qbit Go package: a qBittorrent WebUI API emulation server for the
*arr suite. Read docs/03-arr-qbit-integration.md. Implement every endpoint in that doc's
'MUST implement' table on a net/http mux, satisfying the Handler interface in
internal/qbit/iface.go, backed by the Store interface (from internal/catalog) and the
TorBox Client interface (from internal/torbox) — both are dependencies you call, not
implement; use the provided interfaces + fakes. Key behaviour: on torrents/add, create a
virtual release (call Store + TorBox.CheckCached for sizes, NO add) and immediately
report it complete in torrents/info (progress=1.0, state=pausedUP, content_path). Match
the exact torrents/info field set in doc 03. Write tests that replay real Sonarr/Radarr
request shapes (auth/login → add → info → delete) against the handler with fake Store +
fake TorBox. `go test ./internal/qbit/...` + `go vet`. Don't touch other packages.
```

### Agent C — `internal/catalog`
```
Build internal/catalog: the SQLite store (modernc.org/sqlite, cgo-free) implementing the
Store interface in internal/catalog/iface.go. Schema = release/file/dl_link tables from
docs/05-spec.md §Data model. Provide migrations, CRUD, and queries: upsert release+files,
get-by-hash, list-by-category, set materialize state/torbox_id, touch last_access,
idle/over-max candidates for the reaper, dl_link cache get/set. Table-driven tests
against a temp file DB. `go test ./internal/catalog/...`. Don't touch other packages.
```

### Agent S — `internal/symlink`
```
Build internal/symlink: manage the category symlink tree per docs/03 (Path model) and
docs/05 §5. Implement the Manager interface in internal/symlink/iface.go: Create(release)
makes <download_dir>/<category>/<name>/<rel_path> symlinks pointing at the vfs path
<fuse_mount>/<hash>/<rel_path>; Remove(hash) cleans them. Handle nested dirs, idempotency,
and safe removal (never follow into and delete real files). Tests use a temp dir.
`go test ./internal/symlink/...`. Don't touch other packages.
```

### Agent V — `internal/vfs` (Phase 2)
```
Build internal/vfs: a FUSE filesystem (github.com/hanwen/go-fuse/v2) mounted at the
configured fuse_mount, layout /<hash>/<rel_path>. Read docs/05 §3. Getattr/Lookup/Readdir
serve names+sizes FROM the catalog Store (no TorBox calls) so stat/ls work without
materializing. Open/Read(offset,len) call the Materializer interface (internal/materialize)
to fetch that byte range, and update last_access. Implement the FS against fakes for Store
+ Materializer. Add an integration test that mounts to a temp dir (sk's if /dev/fuse
absent) and asserts stat returns catalog size while read delegates to the Materializer.
Document the required Docker caps. Don't touch other packages.
```

### Agent M — `internal/materialize` (Phase 2)
```
Build internal/materialize per docs/05 §4: the lazy engine satisfying the Materializer
interface. On first Read for a release: slot-check (semaphore sized from
TorBox.UserMe().activeSlots; LRU-release idle releases when full), CreateTorrent if not
materialized, ensure a fresh dl_link (RequestDL; refresh on the 4xx sentinel from
internal/torbox), then range-proxy bytes from the CDN URL. Background idle reaper
(now-last_access > idle_ttl → ControlDelete, state→virtual) and hard max_hold reaper.
Expose a ToS-audit hook (diff MyList vs materialized set). All deps via interfaces +
fakes; NO live TorBox calls in tests. Cover: slot exhaustion/eviction, 4xx-refresh,
idle-release, range proxying. `go test ./internal/materialize/...`. Don't touch others.
```

### Agent R — review (per package, built-in skills, no worktree)
Run `code-review` then `security-review` on each merged package. Focus: API-key never
logged/leaked, no SSRF via attacker-influenced URLs, range/seek correctness, goroutine/
mutex safety in materialize, clean FUSE unmount.

## What I keep off subagents (do myself, serially)
- Foundation/interfaces (shared contracts).
- Anything hitting the **live TorBox account** or the **real arr/Plex stack** (P0 cycle,
  P1/P2 integration, canary) — these need host access + your authorization, not parallel
  agents.
- Final integration, Docker build, GHCR/repo decisions.

## Parallelism note
T, Q, C, S are independent once interfaces exist → run as background worktree agents
together. V and M depend on the integrated P1 core → run them as the second wave. Keep
each agent's scope to one package to avoid merge conflicts; I integrate.
