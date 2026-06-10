package materialize

import (
	"context"
	"time"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/constants"
	"github.com/rushp4000/lazarr/internal/metrics"
)

// runReapers runs the idle + max-hold sweeps on a single ticker until ctx is cancelled.
// One goroutine drives both sweeps so Close awaits exactly one worker (leak-free).
func (m *materializer) runReapers(ctx context.Context) {
	interval := constants.DefaultReaperEvery
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Broken-mount guard (CRITICAL): the reapers delete from the TorBox account.
			// If the FUSE mount is unhealthy (a transient blip, a stale connection), skip
			// the whole sweep so we never mass-delete the account on a mount hiccup. The
			// items stay materialized and are reaped on a later cycle once the mount is back.
			if !m.mountIsHealthy() {
				metrics.IncReaperSkipped()
				m.log.Warn("reaper: skipping sweep — FUSE mount unhealthy (guarding against mass account-delete)")
				continue
			}
			m.reapIdle(ctx)
			m.reapOverMaxHold(ctx)
		}
	}
}

// reapIdle releases materialized releases whose last access is older than IdleTTL. It skips
// any release currently being read (Release itself is refs-aware). This is the core of ToS
// compliance: the account drains back to ~0 shortly after playback stops.
func (m *materializer) reapIdle(ctx context.Context) {
	ttl := m.policy.IdleTTL.D()
	if ttl <= 0 {
		ttl = constants.DefaultIdleTTL
	}
	before := m.now().Add(-ttl).Unix()

	cands, err := m.store.IdleCandidates(before)
	if err != nil {
		m.log.Warn("reaper: idle candidates query failed", "err", err)
		return
	}
	m.releaseCandidates(ctx, cands, "idle")
}

// reapOverMaxHold force-releases anything materialized longer than MaxHold regardless of
// access (belt-and-suspenders ceiling, well under TorBox's 30-day window).
func (m *materializer) reapOverMaxHold(ctx context.Context) {
	hold := m.policy.MaxHold.D()
	if hold <= 0 {
		hold = constants.DefaultMaxHold
	}
	before := m.now().Add(-hold).Unix()

	cands, err := m.store.OverMaxHold(before)
	if err != nil {
		m.log.Warn("reaper: max-hold candidates query failed", "err", err)
		return
	}
	m.releaseCandidates(ctx, cands, "max-hold")
}

// releaseCandidates releases each candidate, honoring ctx cancellation between items.
// Release is a no-op for releases that are pinned by an active reader or already gone.
func (m *materializer) releaseCandidates(ctx context.Context, cands []*catalog.Release, reason string) {
	for _, rel := range cands {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if rel == nil {
			continue
		}
		if err := m.Release(rel.Hash); err != nil {
			m.log.Warn("reaper: release failed", "reason", reason, "hash", short(rel.Hash), "err", err)
		}
	}
}
