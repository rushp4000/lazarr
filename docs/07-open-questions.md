# 07 — Open questions, risks, decisions needed

## Decisions — LOCKED 2026-06-08 (session 5)
1. **Architecture: C — greenfield Lazarr in Go.** ✅ Confirmed. (Not a decypharr fork;
   use decypharr/rdt-client as reference only.)
2. **Name: Lazarr.** ✅ Confirmed.
3. **GitHub repo: decide later.** ✅ Build locally in `/root/Github/Lazarr` first; pick
   public/private + org + LICENSE at Phase 3 packaging.
4. **Uncached policy: cached-only by default, with a config toggle to allow uncached
   downloads.** ✅ Matches `policy.allow_uncached` in `05-spec.md` §7 (default `false`).

## Technical risks / unknowns (resolve in P0/P2)
- **TorBox slot count on Essential** is the hard concurrency ceiling. If it's tiny
  (1–2), simultaneous streams across the family will queue. May justify a TorBox plan
  upgrade later — but Lazarr's release step is what makes even a small plan viable.
- **`/user/me` 403 err 1010** from our host earlier — must be resolved in P0 (param,
  scope, or IP). decypharr calls it fine, so it's solvable.
- **Presigned link reality vs docs.** Docs say `requestdl` opens a 3-hour window;
  decypharr empirically saw stale tokens far sooner (#179). Mitigation is built in
  (refresh-on-4xx) — but verify the real expiry in P2 to tune proactive refresh.
- **Metadata probe without crowdsourced data.** Default = brief materialize-on-read +
  idle release (cheap given Plex deep-analysis is off). If Plex/agents do more reading
  than expected, build the Phase-2 probe-header cache. Watch this during soak.
- **Dead-cache (cached→0-byte).** TorBox can report a hash cached but serve 0 bytes
  (same attrition that broke the RD library). Lazarr should: detect 0-byte/short reads
  post-materialize, mark the release `error`, and let the arr re-grab a different
  release (or expose a "repair" hook). Decide how aggressive in P2.
- **FUSE in Docker** needs `SYS_ADMIN` + `/dev/fuse` + apparmor unconfined; verify on
  the host's kernel (6.12) early. Unmount cleanly on container stop to avoid stale
  mounts (decypharr's `Transport endpoint is not connected` class of issue).
- **Whole-file vs ranged proxy.** Must serve ranges (Plex seeks); never buffer the
  whole file. Tune readahead so playback is smooth without over-fetching (over-fetch =
  wasted TorBox bandwidth + longer holds).
- **Concurrent arr polling.** `torrents/info` may be polled often by several arrs after
  cutover; keep it served from the catalog (no TorBox call per poll).

## Explicitly out of scope (for now)
- RealDebrid / AllDebrid / DebridLink / Usenet — Lazarr is **TorBox-only**; decypharr
  keeps the RD leg.
- Multi-tenant / crowdsourced probe sharing (that's CatBox's hosted advantage).
- A fancy web UI — Phase 3+, optional.

## Compliance guardrails (non-negotiable)
- Never add at grab time.
- Always release on idle (`idle_ttl`) and never exceed `max_hold` (≪ 30 days).
- The ToS audit log (mylist vs materialized diff) must stay clean; alarm if the account
  ever holds something we believe is released.
