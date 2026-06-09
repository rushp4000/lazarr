# 18 — Standalone test/verify prompt (run on **Sonnet**)

Paste the block below into a fresh **Sonnet** Claude Code session on host `192.168.7.133`
(where the repo lives at `/root/Github/Lazarr`). It is **read-only / hermetic** — it builds,
runs the whole test suite + race + Docker + a dead-address boot smoke test. It makes **NO live
TorBox calls** and does **not** touch the decypharr stack, the arrs, or Plex. Safe to run any
time.

---

```
You are verifying the Lazarr build (a Go service) on this host. Model: Sonnet. Work read-only:
do NOT modify source, do NOT call the live TorBox API, do NOT touch the decypharr/arr/Plex
stack. Repo: /root/Github/Lazarr (branch main). Go 1.26.4 at /usr/local/go.

Run each step, capture output, and STOP + report if any step fails (don't "fix" — just report).

STEP 1 — Toolchain + clean tree
  export PATH=$PATH:/usr/local/go/bin
  cd /root/Github/Lazarr
  go version            # expect go1.26.4
  git status -sb        # expect a clean tree on main (report if dirty)
  git log --oneline -3

STEP 2 — Build + vet (pure Go, proves CGO-free)
  CGO_ENABLED=0 go build ./...
  go vet ./...

STEP 3 — Full unit suite (CGO off)
  CGO_ENABLED=0 go test ./... 2>&1 | tee /tmp/lazarr_test.txt
  # Expect every package ok; note the totals for materialize/vfs/qbit/catalog/symlink/torbox.

STEP 4 — Race detector on the concurrent packages (needs cgo/gcc; gcc is installed)
  CGO_ENABLED=1 go test -race -count=2 ./internal/materialize/... ./internal/vfs/...
  # Expect ok, no DATA RACE. goleak runs in materialize's TestMain (no goroutine leaks).

STEP 5 — Targeted Phase-2 regression checks (verbose, so you can see them pass)
  CGO_ENABLED=1 go test -race -v -run \
    'Status200|PartialPrefix|RefreshOn4xx|RepeatedExpiry|HostPin|Singleflight|LRUEviction|NeverEvictActiveReader|Reaper|AuditTOS|ProbeCache|SlotBudget' \
    ./internal/materialize/...
  CGO_ENABLED=0 go test -v -run 'Infohash|ParseMagnet|TorrentFileUpload|ArrLifecycle' \
    ./internal/qbit/...
  # These cover: 200-on-range guard, probe short-read guard, link refresh-on-4xx (exactly one
  # re-request + retry, no loop), SSRF host-pin rejection, singleflight dedupe, LRU + never-
  # evict-active-reader, idle/max-hold reapers, ToS-audit leak/scope, slot budget, infohash
  # (hex/base32/traversal) validation.

STEP 6 — Docker image (pure-Go, fuse3)
  docker build -t lazarr:test .
  # Expect: build stage runs `CGO_ENABLED=0 go build`, final stage `apk add fuse3`, image written.

STEP 7 — Hermetic boot + FUSE-mount smoke test (NO live TorBox: api_base points at a dead
         local address so user/me fails fast and the non-fatal boot path is exercised; no file
         read happens, so NOTHING is ever added to any TorBox account)
  SM=/tmp/lazarr-verify; rm -rf $SM; mkdir -p $SM/{symlinks,fuse,probe}
  cat > $SM/config.yaml <<'YAML'
  torbox: { api_key: "dummy", api_base: "http://127.0.0.1:1/v1/api" }
  qbit: { listen: ":8080" }
  paths: { download_dir: "/data/symlinks", fuse_mount: "/data/fuse", db_path: "/data/lazarr.sqlite", probe_cache_dir: "/data/probe" }
  categories: ["radarr_hin"]
  policy: { allow_uncached: false, idle_ttl: "15m", max_hold: "24h", active_slots: 3, probe_cache: true }
  YAML
  docker rm -f lazarr_verify >/dev/null 2>&1 || true
  docker run -d --name lazarr_verify \
    --cap-add SYS_ADMIN --device /dev/fuse --security-opt apparmor:unconfined \
    -p 18099:8080 -v $SM:/data lazarr:test -config /data/config.yaml
  sleep 3
  docker logs lazarr_verify 2>&1
  # ASSERT in the logs: "qbit listening", "vfs mounted path=/data/fuse",
  #   "torbox user/me check failed (continuing)" (non-fatal, and the URL is redacted/no token).
  curl -s http://127.0.0.1:18099/api/v2/app/version            # expect v4.6.0
  curl -s "http://127.0.0.1:18099/api/v2/torrents/info?category=radarr_hin"   # expect []
  docker exec lazarr_verify sh -c 'mount | grep fuse.lazarr'   # expect the FUSE mount line

STEP 8 — Graceful shutdown
  docker stop -t 12 lazarr_verify
  docker logs lazarr_verify 2>&1 | tail -4
  # ASSERT: "shutting down" then "vfs unmounted"; exit code 0:
  docker inspect -f '{{.State.ExitCode}}' lazarr_verify

STEP 9 — Clean up
  docker rm -f lazarr_verify >/dev/null 2>&1; rm -rf /tmp/lazarr-verify
  docker rmi lazarr:test >/dev/null 2>&1 || true

REPORT: a short table — step, pass/fail, and the key numbers (test totals, race result, image
built y/n, boot asserts y/n, exit code). If anything failed, paste the exact failing output.
Do NOT attempt fixes; this is a verification run only.
```
