# testdata/cdn — fake presigned-CDN contract (Phase 2, Agent M)

`internal/materialize` proxies bytes from the presigned URL that `requestdl` returns
(`data` field in `testdata/torbox/requestdl.json`, e.g.
`https://nexus-138.snam.tb-cdn.io/dl/<hash>/<file_id>/<name>?token=…&expires=…`).

There is **no JSON fixture for the CDN itself** — the byte source is an `httptest.Server`
that Agent M stands up in-test. This file documents the shape that fake must reproduce so
the tests exercise the real CDN behavior verified live in `docs/08`.

## What the real CDN does (verified live, docs/08 + docs/11)

- **Host:** `*.tb-cdn.io` (observed `nexus-138.snam.tb-cdn.io`), always **HTTPS**.
- **Ranged GET** with `Range: bytes=<a>-<b>` →
  - **`206 Partial Content`**
  - `Content-Range: bytes <a>-<b>/<total>` where **`<total>` equals the `checkcached`
    size exactly** (e.g. `bytes 0-1048575/2826905698`).
  - `Content-Length` = number of bytes in the returned window.
  - body = exactly those bytes (verified `1048576` bytes for `bytes=0-1048575`).
- A request with **no `Range`** header returns `200` + the full body (Agent M always sends
  a Range, but the fake may support both).
- **Seek** = a new ranged GET at the new offset. There is no server-side cursor.

## Expiry / refresh paths the fake must offer (the #179 fix)

The presigned URL carries `token=` + `expires=`. When it is stale the CDN returns a **4xx**;
Lazarr must invalidate the cached `dl_link`, call `requestdl` once more, and retry the range
**once**. The statuses that trigger refresh are `constants.LinkRefreshStatuses` =
**`{400, 403, 410}`** (surfaced by the torbox client as `torbox.ErrLinkExpired`).

Agent M's fake CDN should be able to switch a given URL between:
- **fresh** → `206` + correct `Content-Range`, real bytes;
- **expired** → one of `400 / 403 / 410` (assert exactly one re-`RequestDL` + one retry, and
  that a *second* expiry does **not** loop infinitely).

## Security shape to assert (SSRF host-pin, docs/15 §4.F)

Before issuing the GET the engine must require `https` and pin the host to the
`*.tb-cdn.io` suffix (or a configurable allowlist) and refuse private/loopback IPs. Because
the `httptest.Server` listens on `127.0.0.1`, tests that exercise the **happy path** must
inject the fake via a host-pin override/allowlist hook (e.g. allow the test server host), and
a dedicated **negative** test must assert a non-`tb-cdn.io` / private-IP URL is **rejected**
before any GET is made. Do not weaken the production default to make tests pass — gate the
allowlist behind a test-only seam.

## Never log secrets

The `token=` lives in the `requestdl` **request** URL (TorBox side), not the CDN response
URL Lazarr GETs — but apply the same redaction discipline as `torbox.redactURLError`: never
log a URL bearing `token=`/credentials, and keep the API key out of any error string.
