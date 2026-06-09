# 10 — Skills proposal (NOTHING installed without your approval)

> Standing rule (saved to memory `feedback-skill-install-approval`): I will **not
> install any skill/plugin without your explicit prior approval.** Everything below is
> a *proposal* for you to vet.

## A. Built-in skills already in your environment — no install needed
We'll lean on these as-is during the Lazarr build:
- **`code-review`** — review each diff for correctness bugs before merge.
- **`security-review`** — important: Lazarr handles a TorBox API key + proxies CDN URLs.
- **`simplify`** — tidy reuse/altitude after a module lands.
- **`verify` / `run`** — drive the canary and confirm behaviour in the real stack.
- **`claude-api`** — only if we add any Claude-powered tooling (unlikely here).

## B. Third-party skills worth considering (require install → your approval)
| Skill | Source | Why it helps Lazarr | Caveat |
|---|---|---|---|
| **go-development-skill** | `netresearch/go-development-skill` (agentskills.io) | Enterprise Go patterns: resilient services, ret/backoff, table-driven tests, optimized Docker client patterns — directly on-point for our Go service. | **Third-party code/instructions** → supply-chain trust. Vet the repo before install. |
| **Official Go/Docker plugins** | `claude-plugins-official` (Anthropic, built-in marketplace, ~101 plugins as of 03/2026) | First-party, lower trust risk; check for a Go and a Docker/Compose plugin. | Need to confirm exact plugin names in your marketplace before installing. |

> My honest take: third-party skill bundles are the main thing you'd want to vet — they
> ship instructions (and sometimes hooks/MCP) that run in your env. Prefer the
> **official marketplace** equivalents where they exist.

## C. Custom *project* skills I'd author for Lazarr (safest — I write them, you review, no external code)
These encode our already-verified contracts so every build agent stays correct. They
live in the repo (`.claude/skills/`) and need no third-party trust:
1. **`torbox-api`** — the verified TorBox surface: base `https://api.torbox.app/v1/api`,
   Bearer auth, `checkcached`/`createtorrent`/`requestdl`/`controltorrent`/`mylist`/
   `user/me`, the cached-without-add rule, refresh-on-4xx. (From docs 02 + 08.)
2. **`qbit-emu`** — the qBittorrent WebUI contract the arrs require + the
   `torrents/info` field set + the "report complete from checkcached size" trick.
   (From doc 03.)
3. **`lazarr-canary`** — the procedure to wire `radarr_hin → Lazarr`, grab a known
   cached title, and assert `mylist` stays flat (grab) then materialize/release on
   play. (From doc 06.)

## Recommendation
- **Approve me to author the three custom project skills (C)** — highest value, zero
  external trust, keeps build agents accurate.
- **Optionally approve `go-development-skill` (B)** if you want the Go-patterns boost,
  after you glance at the repo — or I check the official marketplace for a first-party
  Go plugin instead and bring you the exact name.
- All built-in skills (A) are already available; nothing to install.

**Awaiting your yes/no on each before any install or skill file is created.**
