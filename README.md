# Lazarr

**A self-hosted, ToS-compliant TorBox lazy-materialize shim for Sonarr / Radarr / Lidarr / Readarr.**

Lazarr presents itself to the *arr suite as a qBittorrent download client. At grab
time it creates a **symlink into a virtual tree with nothing added to TorBox**. The
item only becomes real — added to TorBox, link fetched, bytes streamed — **on actual
playback**, and is **released (deleted from TorBox) after a short idle window**. So
your TorBox account never hoards a library beyond active use, which is exactly what
TorBox's 30-day retention policy requires.

It is an open, self-hostable clone of ElfHosted's (hosted-only) **CatBox**, built for
the [Cleanuparr + Decypharr stack](#) on host `192.168.7.133`.

---

## Why the name "Lazarr"

- **lazy** + **-arr** → the whole thesis is *lazy materialization*.
- Evokes **Lazarus** — files are "raised from the dead" (materialized) on demand and
  laid back to rest (released) when idle.
- Easy to spell, four-letters-plus-arr, fits the Servarr naming convention.
- **Confirmed non-conflicting** (June 2026): absent from `awesome-arr`, `Discovarr`,
  and `locatarr` catalogs and unclaimed as a GitHub repo in this ecosystem. It does
  not collide with Sonarr, Radarr, Lidarr, Readarr, Whisparr, Prowlarr, Bazarr,
  Tdarr, Recyclarr, Maintainerr, Janitorr, Cleanuparr, **Decypharr**, Huntarr,
  Quasarr, Pulsarr, Reiverr, Prunerr, or any other catalogued *arr.

---

## The one-paragraph pitch

decypharr (what the stack runs today) **eager-adds** every grabbed torrent to TorBox
and keeps it there. TorBox prohibits automated tools from artificially holding items
past the ~30-day cache window → TorBox purges them → you get "dead-cache" 0-byte
files (and decypharr 2.x has a real stale-presigned-token bug, issue #179). CatBox
fixes this by never adding at grab time and only materializing on play — but CatBox
is **ElfHosted-hosted-only**. Lazarr is our own, self-hosted, single-tenant
implementation of the same lazy-materialize lifecycle, TorBox-only, in one small Go
service you can build and host from GitHub.

---

## Docs in this folder

| File | What it is |
|---|---|
| `docs/01-catbox-reference.md` | Reverse-engineered CatBox lifecycle (the spec we replicate) |
| `docs/02-torbox-api.md` | The TorBox API surface, verified against decypharr's source |
| `docs/03-arr-qbit-integration.md` | How Sonarr/Radarr actually drive a qBittorrent client + the exact API we must emulate |
| `docs/04-architecture-decision.md` | A vs B vs C, with the recommendation and tradeoffs |
| `docs/05-spec.md` | The Lazarr component/build spec — what to code |
| `docs/06-roadmap-and-canary.md` | Phased plan + the `radarr_hin` canary test, parallel to the live stack |
| `docs/07-open-questions.md` | Risks, unknowns, and decisions still needed from the user |

> Status: **DESIGN / RESEARCH complete. No code yet.** Architecture recommendation is
> in `04`; awaiting user sign-off before Phase 1 (see `06`).
