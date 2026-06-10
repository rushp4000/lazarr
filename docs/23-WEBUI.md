# Lazarr Web UI — Design & Operator Guide

> **Branch:** `webui`  
> **Config key:** `webui.listen` (empty = disabled)  
> **Port in example:** `:8081` (separate from the arr-facing qbit port :8080)

## 1. Overview

The Web UI is an **opt-in human dashboard** for Lazarr. It is served on its own port,
completely isolated from the qBittorrent-emulation port the arrs use. When `webui.listen`
is empty (the default) no extra listener is started.

The UI provides:

| Screen | Purpose |
|---|---|
| **Dashboard** | TorBox account info, slot gauge, FUSE health, last ToS audit, build version, metric counters |
| **Releases** | Searchable/filterable table of every grab Lazarr has seen; force-release action for materialized items |
| **Materialized** | Live view of what's currently held on TorBox; per-item and bulk release |
| **Audit** | Drift from last ToS audit; "Run audit now" button |
| **Config** | Effective config read-only view — `api_key` and passwords are **always redacted** |

## 2. Enabling

```yaml
# config.yaml
webui:
  listen: ":8081"      # expose dashboard on port 8081
  username: ""         # leave empty for trusted-LAN unauthenticated access
  password: ""         # must be set together with username (both or neither)
```

Set `username` + `password` together to enable HTTP Basic Auth. This is **recommended**:
the UI exposes mutating actions (force-release, run-audit). Neither value is reflected back
in the `/api/config` response.

## 3. Trust Model

The Web UI is **unauthenticated by default** — same trusted-LAN model as the qbit and
metrics ports. Bind it to the LAN interface or behind a reverse proxy with auth if you need
access control beyond Basic Auth.

The mutating endpoints (`POST /api/releases/{hash}/release`, `POST /api/audit/run`) are
wrapped by the same auth middleware as the rest of the UI — no separate token is needed.

## 4. JSON API

All endpoints return `Content-Type: application/json`.

### Read-only

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/status` | Version, uptime, FUSE health, slot counters, last-audit timestamp, TorBox account info |
| `GET` | `/api/releases` | Paginated release table; query params: `q` (substring), `state`, `category`, `limit` (default 50), `offset` |
| `GET` | `/api/materialized` | Live materialized set with per-item hash, TorBox ID, ref count, last-used timestamp |
| `GET` | `/api/metrics-summary` | Counter/gauge values from the embedded Prometheus registry |
| `GET` | `/api/config` | Effective config — `api_key` and auth passwords **always omitted** |

### Mutating (require auth when Basic Auth is enabled)

| Method | Path | Description |
|---|---|---|
| `POST` | `/api/releases/{hash}/release` | Force-release a materialized item; calls `eng.Release(hash)` |
| `POST` | `/api/audit/run` | Trigger `eng.AuditTOS()` synchronously; returns 200 on success |

### `/api/status` response shape

```json
{
  "version": "dev",
  "uptime_seconds": 42,
  "mounted": true,
  "slots_in_use": 1,
  "slots_total": 3,
  "last_audit_unix": 1749470000,
  "account": {
    "plan": 2,
    "active_slots": 3,
    "cooldown_until": "",
    "long_term_store": false
  }
}
```

`account` is `null` if the boot-time `/user/me` call to TorBox failed (non-fatal; Lazarr
still starts).

### `/api/releases` response shape

```json
{
  "releases": [ { "hash": "...", "name": "...", "category": "radarr_hin", "state": "done",
                  "size_bytes": 1234567890, "materialized_at": 0, "added_on": 1749470000 } ],
  "total": 17,
  "limit": 50,
  "offset": 0
}
```

### `/api/config` response shape (api_key always absent)

```json
{
  "torbox_api_base": "https://api.torbox.app/v1/api",
  "qbit_listen": ":8080",
  "admin_listen": ":9090",
  "webui_listen": ":8081",
  "download_dir": "/data/symlinks",
  "fuse_mount": "/data/torbox",
  "db_path": "/data/lazarr.sqlite",
  "categories": ["radarr_hin"],
  "allow_uncached": false,
  "idle_ttl": "15m0s",
  "max_hold": "24h0m0s",
  "active_slots": 3,
  "probe_cache": true,
  "ownership_puid": 0,
  "ownership_pgid": 0,
  "auth_enabled": false
}
```

## 5. Implementation Notes

- **Embedded assets:** `//go:embed assets/templates/index.html` — the binary is
  self-contained; no external files are needed at runtime.
- **No build step:** Pure `html/template` + vanilla JavaScript (fetch API, setInterval).
  No Node, no npm, no htmx CDN dependency.
- **Auto-refresh:** The dashboard polls `/api/status` and `/api/materialized` every 10 s.
  The releases table has debounced search (300 ms) and pagination controls.
- **CGO_ENABLED=0 clean:** No cgo dependencies; the binary cross-compiles without a C
  toolchain.
- **api_key never rendered:** `webuiProvider.SafeConfig()` in `cmd/lazarr/main.go`
  explicitly omits `TorBox.APIKey` and `QBit.Password` before populating `SafeConfig`.
  There is no code path in the UI package that can access those fields.

## 6. New Exported Accessors

This feature added the following public symbols that downstream code or tests may use:

| Package | Symbol | Purpose |
|---|---|---|
| `catalog` | `ReleaseFilter` struct | Filter params for ListReleases |
| `catalog.Store` | `ListReleases(ReleaseFilter) ([]*Release, int, error)` | Paginated release query |
| `materialize` | `MaterializedEntry` struct | Snapshot entry (hash, id, refs, last-used) |
| `materialize.*materializer` | `MaterializedSnapshot() []MaterializedEntry` | Live held-set read |
| `metrics` | `Summary` struct | Prometheus counter/gauge snapshot |
| `metrics` | `GatherSummary() (*Summary, error)` | Collect snapshot from registry |
| `config` | `WebUI` struct (Listen, Username, Password) | webui config block |

## 7. Docker Compose

```yaml
ports:
  - "8081:8081"   # optional Web UI
```

The example `docker-compose.example.yml` already includes this port. It is commented out
of the `OPTIONAL` section only when `webui.listen` is empty.
