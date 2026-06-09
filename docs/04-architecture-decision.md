# 04 — Architecture decision

## The three candidates

### A. Fork decypharr (or rdt-client-torbox)
Both already emulate the qBit API, have a TorBox provider, and decypharr has a FUSE
mount + repair. Change behaviour to: lazy add, add a release step, refresh-on-400.
- **+** Fastest to a *working* canary; the hard plumbing exists.
- **−** Inherits decypharr's whole surface (RealDebrid, AllDebrid, DebridLink, Usenet,
  rclone, repair, WebDAV) and its bugs (#179, queue-not-importing, playback perf).
- **−** Not "our own clean repo with a fresh name" — it's a fork the user must track
  upstream and carry a behavioural diff against. The user explicitly wants to move
  **off** decypharr's model and own the code.
- **−** Licence/attribution entanglement to manage on a public GitHub repo.

### B. rclone + TorBox WebDAV mount + thin qBit shim
- **−** **Rejected.** TorBox WebDAV only lists **already-added** torrents (= still
  hoarding, not lazy), **hides >1000-file** items, and **refreshes every ~15 min** (too
  stale for play-now). It cannot be the lazy core. (Detail in `02-torbox-api.md`.)
  WebDAV survives only as a *possible* alt-transport for already-materialized items.

### C. Greenfield "Lazarr" — purpose-built, TorBox-only, lazy-first  ✅ RECOMMENDED
A new small Go service that implements exactly the slice we need, using decypharr's
`torbox.go` and rdt-client's qBit emulation as **reference** (we already extracted the
exact endpoints/params — see `02` and `03`), not as a fork.
- **+** Clean repo the user owns, fresh name, hostable on GitHub, minimal surface.
- **+** TorBox-only + lazy-first → an order of magnitude smaller than decypharr; no
  RD/Usenet/rclone/repair baggage.
- **+** Purpose-built for the ToS-compliant lifecycle (release step + slot-aware
  concurrency are first-class, not bolted on).
- **+** Go: mature FUSE (`hanwen/go-fuse`), single static binary, trivial Docker, and
  it matches the reference code we can read line-for-line.
- **−** More upfront code than a fork (FUSE + proxy + materializer). Mitigated: the
  reference implementations show exactly how, and the canary scope is tiny.

## Recommendation: **C**, with A's code as a reference cheat-sheet.

Rationale: the user's stated goal is *their own* ToS-compliant CatBox on GitHub, off
decypharr's eager-add model. A fork (A) is fastest to first-light but permanently ties
us to decypharr's architecture and bugs — the very thing we're leaving. B is ruled out
by WebDAV's only-added/stale/1000-file limits. C gives a clean, minimal, owned codebase
and we've already de-risked it by pulling the precise TorBox + qBit contracts out of
the existing implementations.

> **De-risk option (offered):** if first-light speed matters more than clean ownership,
> we can do a **2–3 day spike on A** (fork decypharr, flip add→lazy, prove the canary),
> then port the proven lifecycle into the greenfield Lazarr. This is a fallback, not
> the recommendation.

## Language & key libraries (for C)
- **Go 1.22+**.
- HTTP: stdlib `net/http` (qBit shim + stream proxy).
- FUSE: `github.com/hanwen/go-fuse/v2` (same lib decypharr uses).
- DB: `modernc.org/sqlite` (cgo-free) for the catalog.
- Config: YAML (`gopkg.in/yaml.v3`) — single `config.yaml`.
- Container: distroless/alpine; needs `--cap-add SYS_ADMIN --device /dev/fuse` +
  `security_opt: apparmor:unconfined` for FUSE in Docker.

## Repo shape (proposed)
```
lazarr/
  cmd/lazarr/main.go
  internal/
    qbit/        # qBittorrent WebUI API emulation (03)
    torbox/      # TorBox client (02) — checkcached/createtorrent/requestdl/control
    catalog/     # SQLite store: releases, files, materialize state
    vfs/         # FUSE virtual tree: stat from catalog, read → materialize
    materialize/ # add→requestdl→proxy, idle reaper, slot limiter, link refresh
    symlink/     # category symlink tree management
    config/      # yaml config
  Dockerfile
  docker-compose.example.yml
  config.example.yaml
  README.md
```
