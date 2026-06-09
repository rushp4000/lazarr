# 13 — Pick-up prompt for a new session

Paste the block below to resume. Recommended model: **Opus** (see notes under it).

---

```
Resume the Lazarr build (self-hosted ToS-compliant TorBox lazy-materialize shim, our own
CatBox, presents as a qBittorrent client to the *arr suite). Host 192.168.7.133, Docker
access works, configs local.

First read, in order:
1. Memory: project-build-own-catbox.md, feedback-skill-install-approval.md.
2. /root/Github/Lazarr/docs/ — especially 05-spec, 09-build-subagent-plan, 11-constraints-
   and-constants, 12-torbox-tos-compliance, and 08-p0-verification-results.
3. The scaffold under /root/Github/Lazarr/ (go.mod module github.com/rushp4000/lazarr;
   internal/{constants,config,catalog,torbox,symlink,qbit,vfs,materialize}; cmd/lazarr).
   It already builds + vets clean. The interfaces in those packages are the FROZEN
   contracts — implement against them, don't change signatures without flagging.

Standing rules: (a) NEVER install a skill/plugin without my explicit approval. (b) Anything
that mutates the live TorBox account or touches the real arr/Plex stack needs my go-ahead.
(c) Use the project skills torbox-api / qbit-emu / lazarr-canary and the installed golang-*
skills. (d) Stay strictly ToS-compliant: never add at grab; release on idle; ToS-audit loop.

Then BUILD PHASE 1 per docs/09:
- You (driver) own the integration + anything touching the live account/stack.
- git init + initial commit first (worktree-isolated agents need it).
- Spawn the parallel worktree subagents T (internal/torbox), Q (internal/qbit),
  C (internal/catalog), S (internal/symlink) using the prompts in docs/09. NO live TorBox
  calls in agent code — use fixtures from the verified responses in docs/08.
- Gate each merged package with code-review then security-review.
- Integrate, then do the Phase-1 canary on radarr_hin (7880, empty): wire its qBittorrent
  download client to Lazarr, grab one TorBox-cached movie, and assert the symlink imports
  while TorBox `mylist` stays flat (no add). Report before Phase 2.

Confirm the module path github.com/rushp4000/lazarr (rename if I want a different repo) and
which model the subagents should use, then proceed.
```

---

## Model recommendation
- **Driver/orchestrator session: Opus.** Best for the protocol-correctness, concurrency,
  FUSE, and ToS-safety reasoning (Phase 2 vfs/materialize especially). Recommended.
- **Subagents: Sonnet is fine** for the mechanical packages (torbox client, catalog CRUD,
  symlink); keep **Q (qbit) and M (materialize) on Opus** for correctness. (Model is set
  per-agent, so you can mix.)
- **Sonnet 1M context:** worth it only if you want one long marathon session that holds all
  docs + generated code without summarization. The docs are compact and the code is
  modular, so it isn't required — pick it for throughput/cost over a long build, pick Opus
  for max correctness on the hard parts. **Default suggestion: Opus driver + Sonnet workers.**
