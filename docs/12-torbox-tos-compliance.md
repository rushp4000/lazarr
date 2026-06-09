# 12 — TorBox ToS & how Lazarr stays compliant

Sources (authoritative): TorBox official ToS repo
`github.com/TorBox-App/hosted-terms_of_service` (README.md) + Help Center "The TorBox
Abuse System" and "How Long Are TorBox Files Stored For?".

## What the ToS / policy actually says

**Fair Usage and Abuse (ToS, verbatim):**
> "TorBox operates as a shared service with any number of concurrent users accessing
> resources at a given time. It is your responsibility to ensure that you are not
> negatively impacting other TorBox users. You must ensure your resource consumption
> (such as bandwidth, storage, or API requests) are within acceptable limits."

> "TorBox reserves the right to cease any access or transfer that is negatively
> affecting the service. TorBox also reserves the right to remove any files or content
> from our servers at any time without notification. Your account will be warned and
> banned if you do not abide by these terms."

**Retention (Help Center):**
- Files are stored **at least 30 days**.
- Transfers **not accessed in over 30 days are automatically removed** to prevent stale
  buildup.

**Abuse system — explicitly prohibited automated behaviours (Help Center):**
- Preemptively transferring files for other users (**cache building**).
- **Excessive transfer retention** — "using tools to artificially keep transfers
  active." ← *this is exactly what decypharr's eager-add does, and what gets purged.*
- Using **misconfigured** automated tools.
- Enforcement: daily check of **rolling 15-day usage** → warning → **3 days** to reduce
  below threshold, else ban.

## Why decypharr (today) is non-compliant
It **adds every grabbed torrent and keeps it indefinitely** → "excessive transfer
retention." TorBox purges past the window → the "dead-cache" 0-byte files we fought in
recovery. A 392–445-item standing library on the account (what we measured) is precisely
the artificial-retention pattern the policy targets.

## How Lazarr is compliant by design
| Policy concern | Lazarr behaviour |
|---|---|
| Excessive/artificial retention | **Never adds at grab.** Adds only on real playback; **releases (`operation:delete`) after `idle_ttl` (mins)** and a hard `max_hold` (≪30d). Account holds ~0 at rest. |
| Cache-building / pre-transfer | Never pre-adds or pre-fetches for others. Only materializes content a user actually plays. |
| Misconfiguration | Purpose-built: slot-aware (≤3), rate-aware (≤55 `createtorrent`/hr), serves `torrents/info` from local catalog (no API spam). |
| Bandwidth (rolling 15-day) | Streams **only the bytes actually played** via bounded ranged reads (`readahead` 8 MiB); no whole-file prefetch; concurrency capped at 3 slots. |
| Storage / API requests "within limits" | `checkcached` cached in SQLite; batched ≤100; presigned links cached + refreshed on 4xx, not polled. |

## Built-in compliance guardrails (must ship)
1. **Release-on-idle + max-hold reapers** — non-negotiable; the core of compliance.
2. **ToS audit loop** — periodically diff `mylist` against Lazarr's `materialized` set;
   **alarm if the account holds anything we believe is released** (catches leaks).
   **During canary/coexistence the account is SHARED with decypharr (which still hoards
   ~440 items), so the audit must scope to Lazarr-added torrent_ids only** — not the whole
   account — until decypharr's TorBox leg is removed at full cutover.
3. **Slot + rate limiter** — never exceed 3 active or ~55 adds/hr; back off on
   `60 per 1 hour` and respect `cooldown_until`.
4. **Bandwidth restraint** — bounded readahead; no speculative full-file fetches.

> Net: Lazarr uses TorBox the way the ToS intends — as a transient cache during active
> use — instead of as durable storage. This is the entire reason for the project.
