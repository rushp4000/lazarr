package materialize

import (
	"net/url"
	"time"

	"github.com/rushp4000/lazarr/internal/metrics"
)

// This file exposes a minimal, test-only surface so the white-box tests can:
//   - allow the httptest server's 127.0.0.1 host through the host-pin WITHOUT weakening the
//     production default (the seam described in testdata/cdn/README.md & docs/15 §4.F),
//   - drive a deterministic clock for expiry/reaper tests,
//   - inspect slot-budget and in-memory tracking invariants.
//
// None of these are part of the frozen Engine interface or reachable from production code.

// AllowTestHost adds a host (or host:port) to the proxy's test-only allowlist and enables
// loopback dialing. Production New() never calls this; the allowlist is empty by default so
// only *.tb-cdn.io passes in production.
func (m *materializer) AllowTestHost(host string) { m.prox.allowHost(host) }

// SetNow overrides the engine clock for deterministic tests.
func (m *materializer) SetNow(f func() time.Time) { m.now = f }

// SetDrainTimeout overrides the Close ref-drain window (B3) so shutdown tests don't wait
// the full production grace period.
func (m *materializer) SetDrainTimeout(d time.Duration) { m.drainTimeout = d }

// Reconcile runs the boot-time reconciliation sweep synchronously for tests (B2).
func (m *materializer) Reconcile() { m.reconcile(testCtx()) }

// SlotCap exposes the resolved active-slot budget.
func (m *materializer) SlotCap() int { return m.slotCap() }

// TrackedCount returns how many releases are currently materialized in memory.
func (m *materializer) TrackedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.track)
}

// IsTracked reports whether a hash is currently materialized in memory.
func (m *materializer) IsTracked(hash string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.track[hash]
	return ok
}

// Refs returns the active-reader count for a hash (0 if untracked).
func (m *materializer) Refs(hash string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.track[hash]; ok {
		return e.refs
	}
	return 0
}

// ValidateURLForTest exposes the SSRF gate so the negative host-pin test can assert a URL is
// rejected BEFORE any GET is attempted.
func (m *materializer) ValidateURLForTest(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	return m.prox.validateURL(u)
}

// reapOnce runs both reaper sweeps synchronously (no ticker) for deterministic tests.
func (m *materializer) reapOnce() {
	m.reapIdle(testCtx())
	m.reapOverMaxHold(testCtx())
}

// runReapOnceGuarded runs one reaper cycle through the broken-mount guard exactly as
// runReapers does per tick: skip both sweeps (and count the skip) when the mount is
// unhealthy, else reap.
func (m *materializer) runReapOnceGuarded() {
	if !m.mountIsHealthy() {
		metrics.IncReaperSkipped()
		return
	}
	m.reapOnce()
}
