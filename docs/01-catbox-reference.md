# 01 — CatBox reference (the spec we replicate)

Reverse-engineered from ElfHosted's CatBox product + docs pages (June 2026). CatBox is
**hosted-only**; this is the behaviour Lazarr clones.

Sources:
- https://store.elfhosted.com/product/catbox/
- https://docs.elfhosted.com/app/catbox/ (301-redirects to the store page)
- https://docs.elfhosted.com/guides/media/plex-torbox-aars/

## What CatBox is

A **TorBox-specific library shim** that sits between the *arr suite and TorBox and
makes a TorBox library safe to keep "forever" without violating TorBox's 30-day
retention policy. It is explicitly distinguished from Decypharr (which handles
Real-Debrid, where there is no retention restriction). **TorBox only.**

## How it presents

- Presents as a **qBittorrent client** at `http://catbox:8080`. "Sonarr / Radarr /
  Lidarr / Readarr point at CatBox as a qBit client and don't know the difference."
- Optional **Torznab** endpoint at `http://catbox:8080/torznab` for Prowlarr.
- **Categories must match arr names** (`radarr`, `sonarr`, `radarr4k`, `sonarr4k`,
  `sonarranime`, …).
- Auth: a TorBox API key pasted into the CatBox UI (`/ui/settings`).
- State: a per-tenant **SQLite** catalog of releases + settings.

## The lifecycle — "lazy materialize"

### 1. Submission (Virtual)
Arr finds a release, submits the magnet/.torrent over the qBit API. CatBox records it
in its catalog and creates a **symlink** under
`/storage/symlinks/downloads/<category>/...` pointing into a virtual tree under
`/storage/torbox/...`. **Nothing is added to your TorBox account.** Default behaviour
uses TorBox's `add_only_if_cached` semantics (an uncached toggle exists in the UI).

> The arr needs a **file name + size** to import (it rejects 0-byte / "is this a
> sample?"). CatBox gets these from TorBox's cache check **without adding** (see
> `02-torbox-api.md`, `checkcached` with `list_files=true`).

### 2. Library import
Arr "sees" the file via the symlink, imports it into its root folder (moves the
symlink), and notifies the media server. The library entry now exists permanently.

### 3. Metadata phase (Protected)
When the media server (Plex/Jellyfin/Emby) probes the file for codec/resolution/
runtime/subtitle tracks, CatBox **intercepts the probe** and answers with **"synthetic
or crowdsourced probe data"** rather than fetching from TorBox. This avoids
materializing the item just for a metadata scan.
> ⚠️ This is the one piece that **relies on ElfHosted's multi-tenant scale**
> (crowdsourced probe data). A single-tenant Lazarr can't crowdsource. See the
> Lazarr alternative in `05-spec.md` (materialize-on-read with idle release; optional
> probe-result cache).

### 4. Playback (Materialize)
**Only when playback begins** does CatBox: add the item to TorBox → fetch a presigned
download URL → **proxy the stream** to the player.

### 5. Release (Cleaned)
After playback, CatBox **removes the item from your TorBox account**. The symlink and
library entry remain (item still appears in the library), but TorBox no longer holds
it against your account. → never holds anything beyond active use = ToS-compliant.

### 6. Refresh / retry
Presigned links expire; on expiry/4xx CatBox fetches a fresh link. (This is the
correct fix for decypharr issue #179's stale token.)

## The five invariants Lazarr must preserve

1. Looks exactly like qBittorrent to the arrs (categories = arr names).
2. Grab time = symlink only, **zero TorBox add**, but real file name + size known.
3. Materialize **only on real access**, proxy the bytes.
4. **Release** well under 30 days (we'll use minutes-of-idle, not days).
5. Refresh the link on expiry / HTTP 4xx.
