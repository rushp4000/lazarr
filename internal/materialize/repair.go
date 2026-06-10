package materialize

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/constants"
)

// RepairScan checks every catalogued hash against TorBox's checkcached endpoint
// (no adds, no deletes) and marks each release's CacheStatus in the catalog.
// Returns the set of evicted hashes — those no longer available on TorBox's CDN.
//
// The scan is purely read-only from TorBox's perspective: checkcached works even
// for hashes not on the account, so this never contributes to the 60/hr createtorrent
// rate limit. Hashes currently being materialized (inflight) are skipped to avoid
// a race with concurrent first-reads.
func (m *materializer) RepairScan(ctx context.Context) ([]RepairEntry, error) {
	hashes, err := m.store.ListAllHashes()
	if err != nil {
		return nil, fmt.Errorf("repair: list hashes: %w", err)
	}

	// Snapshot inflight set so we can skip hashes mid-materialize.
	m.mu.Lock()
	inflight := make(map[string]struct{}, len(m.inflight))
	for h := range m.inflight {
		inflight[h] = struct{}{}
	}
	m.mu.Unlock()

	var evicted []RepairEntry
	now := time.Now().Unix()

	// Process in batches of CheckCachedBatchMax (100).
	for len(hashes) > 0 {
		if ctx.Err() != nil {
			return evicted, ctx.Err()
		}

		batch := hashes
		if len(batch) > constants.CheckCachedBatchMax {
			batch = hashes[:constants.CheckCachedBatchMax]
		}
		hashes = hashes[len(batch):]

		// Filter out inflight hashes from this batch.
		filtered := batch[:0]
		for _, h := range batch {
			if _, ok := inflight[h]; !ok {
				filtered = append(filtered, h)
			}
		}
		if len(filtered) == 0 {
			continue
		}

		cached, err := m.tb.CheckCached(filtered)
		if err != nil {
			slog.Warn("repair: checkcached batch error, skipping batch", "err", err, "count", len(filtered))
			continue
		}

		for _, h := range filtered {
			_, available := cached[h] // present in map = TorBox has it cached
			status := catalog.CacheStatusCached
			if !available {
				status = catalog.CacheStatusEvicted
			}
			if setErr := m.store.SetCacheStatus(h, status, now); setErr != nil {
				slog.Warn("repair: set cache status", "hash", h, "err", setErr)
				continue
			}
			if !available {
				// Look up name/category for the caller's display.
				r, _, getErr := m.store.GetRelease(h)
				if getErr != nil {
					evicted = append(evicted, RepairEntry{Hash: h})
					continue
				}
				evicted = append(evicted, RepairEntry{
					Hash:     h,
					Name:     r.Name,
					Category: r.Category,
				})
				slog.Warn("repair: content evicted from TorBox CDN — arr will need to re-grab",
					"hash", h, "name", r.Name, "category", r.Category)
			}
		}
	}

	slog.Info("repair scan complete", "evicted", len(evicted))
	return evicted, nil
}
