# 15 — Phase 1 Summary, Open Items & Security Notes

**Status as of 2026-06-09.** Repo: **private** `github.com/rushp4000/lazarr`, branch `main`,
HEAD `9412ba9`. Phase 1 (grab → import, ToS-compliant, no TorBox add) is **built,
reviewed, canary-proven on the live stack, and published.** Phase 2 (playback
materialize + release) is **not started** (gated on review).

---

## 1. What Phase 1 delivers

A self-hosted, ToS-compliant TorBox lazy-materialize shim that presents to the *arr suite
as a qBittorrent download client. At grab time it symlinks with **no TorBox add**; the
account stays empty until playback (Phase 2).

| Package | Responsibility | Tests |
|---|---|---|
| `internal/torbox` | TorBox HTTP client: `checkcached` (batch ≤100), `createtorrent`, `requestdl`, `controltorrent` delete, `mylist`, `user/me`. `ErrLinkExpired` on 4xx, `ErrRateLimited` on the ~60/hr cap. Base/HTTP-client overridable for tests. | 14 + redaction |
| `internal/catalog` | `modernc.org/sqlite` (cgo-free) `Store` + idempotent migrations. release/file/dl_link tables, reaper queries (`IdleCandidates`/`OverMaxHold`), `MaterializedIDs` (ToS audit), dl_link cache, cascade deletes. Single-conn + WAL + busy_timeout. | 22 (CGO off) |
| `internal/symlink` | Idempotent category symlink tree `<download_dir>/<cat>/<name>/<relpath>` → `<fuse_mount>/<hash>/<relpath>`. Safe removal (only unlinks `ModeSymlink` entries whose target is under `<fuse>/<hash>/`). Path-traversal guards. | 17 |
| `internal/qbit` | qBittorrent WebUI emulation. On add → `checkcached` for names+sizes (**no add**) → `UpsertRelease` → `Symlink.Create`; reports complete immediately (`progress=1, pausedUP, content_path`). `releasesForQuery` resolves info by hash / category / all. | 23 + regression |
| `cmd/lazarr` | Wires catalog → torbox → symlink → qbit; graceful shutdown; best-effort `UserMe` at boot; `LAZARR_LOG_LEVEL` + request logging. vfs/materialize remain Phase-2 stubs. | — |

**76 tests green**, `go build`/`go vet` clean (CGO disabled — proves pure-Go). Docker image
`lazarr:phase1` (multi-stage, `CGO_ENABLED=0`, alpine + fuse3) builds and smoke-tests.

### Build method (recap)
Foundation + frozen interfaces were authored serially; four packages were then built by
parallel **Sonnet** general-purpose agents in isolated git worktrees, each scoped to one
package against its **frozen interface contract**, using only fixtures (no live TorBox).
Integrated on `main`; the frozen interface files were verified unchanged.

---

## 2. Canary result (live, radarr_hin, 2026-06-09)

Real **Radarr v6.1.1** → grabbed a real **Torrentio** release of a verified TorBox-cached
Big Buck Bunny → Lazarr `checkcached` (real size, **no add**) → symlink tree → reported
complete → Radarr enqueued it → **imported into `/movies` as a symlink into the FUSE tree**
(moved the link, not the bytes). **TorBox `mylist` held at 516 across two grabs (zero
adds)** — the ToS-compliant core is proven end-to-end.

**Topology learned (host 192.168.7.133):** `/decypharr_symlinks` (symlink tree, mounted
into the arrs at the same path) and `/decypharr_mount` (decypharr's rclone FUSE). radarr_hin
`/movies` = `/decypharr_symlinks/Movies/radarr_hin`. The arrs reach download clients by
**host IP** (not docker DNS). uid **1003 = `dbots`** owns the shared tree, so Lazarr must run
`--user 1003:1003` for the arrs to move its symlinks.

**Canary-discovered bug (fixed `9412ba9`):** Radarr polls `torrents/info` **by hash with no
category**; the handler returned `[]` unless a category was supplied, so Radarr never saw its
grabbed download and import never started. Fixed via `releasesForQuery` (hash → `GetRelease`;
category → `ListByCategory`; neither → all configured categories), plus a grab Info log and a
regression test.

---

## 3. Open items / things to finish later

### 3.1 Operational / environment
- **★ Canary teardown — DEFERRED by user until the project is stable, then ROLL BACK.**
  Live artifacts on the stack right now:
  - radarr_hin: movie *Big Buck Bunny* (id 1, `hasFile` via a placeholder symlink),
    download client **`Lazarr_Canary` (id 2) ENABLED**, **`Decypharr` (id 1) DISABLED**.
  - `lazarr_canary` Docker container **running** (uid 1003, host `:8088`).
  - Dirs: `/config/lazarr_canary`, `/decypharr_symlinks/lazarr_canary`,
    `/decypharr_symlinks/lazarr_fuse` (sparse placeholders).
  - **Rollback:** re-enable Decypharr (`PUT downloadclient/1 enable=true`), delete
    `Lazarr_Canary` (`DELETE downloadclient/2`) + the movie (`DELETE movie/1
    deleteFiles=true`), `docker rm -f lazarr_canary`, `rm -rf` the canary dirs.
  - radarr_hin API key cached at `/tmp/.rhin_token` (ephemeral).
- **★ GitHub credentials stored in PLAIN TEXT.** `gh auth login` (account `rushp4000`) on the
  host saved the token in plaintext (`~/.config/gh/hosts.yml`, git_protocol=https). Move to a
  credential helper / OS keyring, scope a fine-grained PAT, restrict file perms (600), and
  keep it out of backups. Low priority, do before the host is shared/backed up.
- **Phase-1 FUSE is a stub.** The canary used **sized sparse placeholder files** at the
  symlink targets to stand in for the real FUSE layer (so Radarr could `stat` sizes). This is
  a test harness only — Phase 2 replaces it with the real `vfs` FUSE mount.
- **Radarr title-parser caveat (not a Lazarr bug).** Radarr could not auto-parse the
  public-domain BBB release names ("Unable to parse download/file"); import was completed via
  Radarr **manual import**. Real releases with standard naming auto-import; decypharr hits the
  same parser. No action needed in Lazarr.

### 3.2 Functional gaps (Phase 2 / later)
- `vfs` + `materialize` are interface-only stubs — the entire playback path (read →
  add → presigned URL → range-proxy → idle release) is Phase 2.
- **Probe-header cache** (Phase 2): cache the first N MiB so each new import's Plex header
  scan doesn't cost a TorBox add against the ~55/hr budget.
- **`active_slots: 0` auto-detect** from `UserMe()` is wired through config but only consumed
  once `materialize` exists.

---

## 4. Security notes (DETAILED — for the hardening pass)

> The two highest-value issues were **already fixed in Phase 1** (items A, B). Items C–H are
> open hardening tasks, ordered roughly by priority. `/security-review` (run 2026-06-09 over
> the full scaffold→HEAD diff) found **no HIGH/MEDIUM exploitable vulnerabilities**; the items
> below are defense-in-depth except where noted.

### A. [FIXED] API-key disclosure via wrapped transport errors — `internal/torbox/client.go`
`RequestDL` sends the API key as the `token` query param. A transport failure returns a
`*url.Error` whose `Error()` includes the full URL (with the token). The old code wrapped it
with `%w`, so any log of that error leaked the key.
**Fix shipped:** `redactURLError(err)` extracts `*url.Error` via `errors.As`, blanks
`RawQuery`, and re-wraps preserving the inner error (so `errors.Is(ctx.DeadlineExceeded)` etc.
still work). Applied to both `NewRequestWithContext` and `hc.Do` error paths. Verified live
(the `UserMe` failure log showed the query stripped). **Regression test:**
`client_redact_test.go::TestRequestDL_TransportError_DoesNotLeakKey`.
**Stay-fixed rule:** never wrap a raw `*url.Error`/`http` error from a request whose URL can
carry `token=`/credentials without redaction. Keep the API key out of struct `String()`/`%v`.

### B. [FIXED] Bencode parser hardening — `internal/qbit/torrent.go`
The `.torrent` upload body is attacker-influenceable (indexer content). Three issues, all
fixed (these are DoS-class, hence excluded from the formal review, but real robustness):
1. **Overflow → slice-bounds panic:** a string length near `MaxInt64` made `colon+1+length`
   wrap negative, bypassing the `> len(buf)` guard → `buf[start:start+length]` panics. Fixed
   with overflow-safe bounds: `length < 0 || length > len(buf)-start`.
2. **Unbounded recursion → fatal stack overflow** on deeply nested lists/dicts. Fixed with
   `maxBencodeDepth = 128` threaded through `bencodeSkipDepth`.
3. **Unbounded read** of the upload. Fixed with `io.LimitReader(r, maxTorrentBytes+1)` (10 MiB)
   in `parseTorrentFile`, plus `http.MaxBytesReader(33<<20)` on the add handler.
**Regression tests:** `torrent_hardening_test.go` (hostile length, deep nesting, oversize,
valid round-trip).

### C. [OPEN — recommended Phase 2] Infohash not validated → symlink-target traversal — `internal/qbit/server.go`
`parseMagnet` takes the bytes after `btih:` verbatim and lowercases them; this `hash` becomes
the `<hash>` path segment of the symlink **target** `filepath.Join(fuse_mount, hash, relpath)`.
`symlink.Manager` validates the link **location** (category/name/relpath stay under
`download_dir`) but **not** the hash segment of the target. A crafted magnet
(`xt=urn:btih:../../../etc`) could produce a symlink in the download dir pointing **outside**
`fuse_mount`. Radarr would then move that symlink into the library; following it reads an
arbitrary path at the media user's privilege.
- **Confidence/severity:** low-to-medium. Source is the semi-trusted arr/indexer; real
  `.torrent` infohashes are always 40-hex (SHA-1) and magnets are normally hex/base32; impact
  is bounded by the media user's read access.
- **Fix:** in `qbit` add, after extracting `hash`, **validate it is 40 lowercase hex chars or
  32 base32 chars** and reject otherwise (mark the release errored). Defense-in-depth: also
  validate the hash segment inside `symlink.Manager` (reject any component containing a
  separator or `..`, which `safeComponent` already does for category/name — extend to the
  target's hash).

### D. [OPEN] `qbit` login accepts ANY credentials — `internal/qbit/server.go`
`handleLogin` always returns `Ok.` regardless of the configured `qbit.username/password`. This
is **intentional** for a trusted-LAN download-client shim and matches decypharr/rdt-client. It
is **not** an auth boundary today.
- **Risk only if** Lazarr's port is exposed beyond the LAN, since there is no auth on any
  endpoint (`torrents/add`, `delete`, `info` are all open). The shim holds only release
  metadata + can create/delete symlinks and (Phase 2) trigger TorBox adds/deletes.
- **Recommendation:** keep bound to the trusted network (don't publish the port). Optional
  hardening: enforce the configured credentials when set (return 403 on mismatch), and/or a
  static API token check. Document the trust model in the README.

### E. [OPEN] Magnet parser is hex-`btih` only; hand-rolled percent-decode — `internal/qbit/server.go`
`parseMagnet` only handles hex infohashes (not 32-char base32) and `urlDecode` is a hand-rolled
percent/plus decoder. **Not a security issue** (a base32 magnet just becomes a `checkcached`
miss → errored release → arr tries another), but: replace `urlDecode` with `net/url`
(`QueryUnescape`) and add base32→hex conversion for robustness. Pairs naturally with the
item-C infohash validation.

### F. [OPEN — Phase 2, IMPORTANT] SSRF / proxy safety in `materialize` (not yet written)
Phase 2's `materialize` will fetch the presigned CDN URL returned by `requestdl` and
range-proxy bytes. That URL comes from TorBox (trusted) but the engine must still:
- **Pin the expected host suffix** (`*.tb-cdn.io` observed) or at least require `https` and a
  non-private IP before issuing the GET, so a compromised/spoofed `requestdl` response can't
  turn Lazarr into an SSRF pivot to internal services. Per the review rule, path-only SSRF is
  out of scope, but **host/scheme must be constrained.**
- Use a dedicated `http.Client` with sane timeouts; do **not** follow redirects to private
  ranges; cap the proxied range size to the requested window + bounded readahead.
- Continue to keep the API key out of any error/log (the `token` is in the `requestdl`
  request URL, not the CDN URL — but apply the same redaction discipline).

### G. [OPEN] SQLite robustness (not a vuln) — `internal/catalog/sqlite.go`
`PRAGMA foreign_keys`/WAL rely on the single persistent connection (`SetMaxOpenConns(1)`).
This is fine in practice but brittle if the pool is ever resized. **Recommendation:** set
pragmas via DSN/connector params (e.g. `?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)`)
so they apply per-connection regardless of pool config. No security impact.

### H. [OPEN] Request logging — `internal/qbit/server.go`
Per-request Debug logging records method/path/**query**. The qBit layer's query params are
`category`/`hashes` (non-secret) and the TorBox token never reaches this layer, so this is
safe today. **Keep it that way:** if any future qBit endpoint ever carries a secret in the
query, redact before logging (mirror `torbox.redactURLError`).

---

## 5. Verification commands
```bash
export PATH=$PATH:/usr/local/go/bin
cd /root/Github/Lazarr
go build ./... && go vet ./... && CGO_ENABLED=0 go test ./...   # 76 tests, all green
docker build -t lazarr:phase1 .                                  # pure-Go image
```

See also: `05-spec.md` (component spec), `11-constraints-and-constants.md` (verified TorBox
limits), `12-torbox-tos-compliance.md` (ToS boundary), `16-PHASE2-KICKOFF.md` (next phase).
