// Package materialize is the lazy engine: add->requestdl->proxy->idle-release, with a
// configurable slot semaphore (default 3), link-refresh-on-4xx, idle + max-hold reapers,
// the probe-header cache, and the ToS-audit loop. Phase 2; built by Agent M (docs/09).
//
// Correctness contract (docs/05 §4 + docs/11 + docs/12):
//   - NEVER adds at grab time; only on a real ReadAt. Releases via controltorrent delete
//     after idle_ttl / max_hold. The account holds ~0 at rest (ToS compliance).
//   - Concurrency is treated as adversarial: per-hash active-reader refcounts gate LRU
//     eviction so an actively-read release is never evicted; singleflight dedupes the
//     first materialize per hash; all background goroutines are ctx-cancellable and
//     leak-free (verified with goleak).
package materialize

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/config"
	"github.com/rushp4000/lazarr/internal/constants"
	"github.com/rushp4000/lazarr/internal/metrics"
	"github.com/rushp4000/lazarr/internal/torbox"
)

// ErrUncachedDisabled is returned when a release is not cached on TorBox and the policy
// forbids adding uncached torrents (the default). The read fails clearly rather than
// silently triggering a slow uncached download.
var ErrUncachedDisabled = errors.New("materialize: release not cached and allow_uncached is false")

// ErrSlotsExhausted is returned when no active slot is free and no idle materialized
// release can be evicted to make room (every slot is actively being read).
var ErrSlotsExhausted = errors.New("materialize: all active slots busy with in-use releases")

// ErrPurged is returned when TorBox reports the torrent is gone / not cached at
// materialize time (dead-cache: a stale item TorBox purged). This is DISTINCT from a
// transient presigned-link 4xx, which proxyRead recovers via refresh-on-4xx. On this
// error the engine marks the release errored (catalog.StateError) so the arr blacklists
// and re-grabs, rather than surfacing a silent EIO that the arr would retry forever.
var ErrPurged = errors.New("materialize: release purged / not cached on TorBox (dead-cache)")

// nowFunc returns the current unix time; overridable in tests.
type nowFunc func() time.Time

// materializer is the concrete Engine. main holds this concrete type (for Start/Close);
// the interface is satisfied by the ReadAt/Release/AuditTOS/Close methods.
type materializer struct {
	store  catalog.Store
	tb     torbox.Client
	policy config.Policy
	log    *slog.Logger

	probe *probeCache // nil if disabled / unwritable

	prox *proxy // SSRF-safe range proxy

	// mountHealthy is an optional broken-mount guard, set from main via SetMountHealthy.
	// When non-nil and it returns false, the idle/max-hold reapers SKIP their sweep
	// instead of calling ControlDelete — a transient FUSE blip must never trigger a mass
	// account-delete. nil => behave as before (always reap). Set once before Start; read
	// by the reaper goroutine, which is the only consumer, so no extra synchronization.
	mountHealthy func() bool

	// slots is the active-materialization budget (semaphore). Capacity is resolved once
	// in New from Policy.ActiveSlots / UserMe / DefaultActiveSlots.
	slots chan struct{}

	// idleSignal wakes admit()ers blocked behind a full semaphore when a previously-pinned
	// release becomes idle (refs->0) or is released, so they can retry LRU eviction. Buffered
	// size 1 + non-blocking send => a coalesced "something changed" notification.
	idleSignal chan struct{}

	now nowFunc

	// lastAudit is the unix time of the last completed ToS audit (0 = never). Reported by
	// the admin /health endpoint. Atomic: written by the audit loop, read by /health.
	lastAudit atomic.Int64

	// sf dedupes concurrent first-materialize per hash so exactly one CreateTorrent runs.
	sf singleflight.Group

	// mu guards the in-memory materialized-set bookkeeping (track + lru). It is never held
	// across network I/O (slot admission, GET, CreateTorrent) — see admit/lockstep helpers.
	mu    sync.Mutex
	track map[string]*entry  // hash -> live materialization state
	seen  map[int64]struct{} // torbox ids Lazarr has added this lifetime (audit scope)

	// reaper lifecycle.
	startOnce sync.Once
	closeOnce sync.Once
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closed    bool
}

// entry is the in-memory record of a materialized release. It exists only while the
// release is materialized (holds a slot). refs counts active ReadAt calls; a release with
// refs>0 is pinned and must never be evicted/released by the LRU or the reapers.
type entry struct {
	hash     string
	torboxID int64
	refs     int   // active readers; >0 => pinned
	lastUsed int64 // unix nanos of last admit/read; drives in-memory LRU
}

// New builds the engine. It resolves the slot budget and the SSRF-safe HTTP proxy, and
// (best-effort) opens the probe-header cache. It does NOT start the reapers — call Start.
//
// Returns the concrete *materializer, which satisfies the frozen Engine interface.
func New(d Deps) (*materializer, error) {
	if d.Store == nil {
		return nil, errors.New("materialize: nil Store")
	}
	if d.TorBox == nil {
		return nil, errors.New("materialize: nil TorBox")
	}

	m := &materializer{
		store:  d.Store,
		tb:     d.TorBox,
		policy: d.Policy,
		log:    slog.Default(),
		now:    time.Now,
		track:  make(map[string]*entry),
		seen:   make(map[int64]struct{}),
		prox:   newProxy(),
	}

	// Resolve the active-slot budget: explicit policy > UserMe() > DefaultActiveSlots.
	n := d.Policy.ActiveSlots
	if n <= 0 {
		if acct, err := d.TorBox.UserMe(); err == nil && acct != nil && acct.ActiveSlots > 0 {
			n = acct.ActiveSlots
		}
	}
	if n <= 0 {
		n = constants.DefaultActiveSlots
	}
	m.slots = make(chan struct{}, n)
	m.idleSignal = make(chan struct{}, 1)

	// Probe-header cache is best-effort: if disabled or the dir is unwritable we degrade
	// to live-proxy only (the docs require graceful degradation, not a hard failure).
	if d.Policy.ProbeCache && d.ProbeCacheDir != "" {
		pc, err := newProbeCache(d.ProbeCacheDir, defaultProbeCacheBytes, defaultProbeRegionBytes)
		if err != nil {
			m.log.Warn("materialize: probe cache disabled (dir unwritable)", "dir", d.ProbeCacheDir, "err", err)
		} else {
			m.probe = pc
		}
	}

	return m, nil
}

// SetLogger overrides the engine's logger (used by main and tests). Not part of the
// frozen Engine interface; safe to call before Start.
func (m *materializer) SetLogger(l *slog.Logger) {
	if l != nil {
		m.log = l
	}
}

// SetMountHealthy installs the broken-mount guard. fn should be a cheap probe of the
// FUSE mount (e.g. vfs.FS.Healthy). When it returns false the idle/max-hold reapers
// skip their sweep so a transient mount blip never mass-deletes from the account. Set
// once before Start; nil restores the default (always reap). Not part of the frozen
// Engine interface.
func (m *materializer) SetMountHealthy(fn func() bool) { m.mountHealthy = fn }

// mountIsHealthy reports whether reaping is currently allowed. No guard installed =>
// always allowed (preserves pre-Phase-3 behaviour).
func (m *materializer) mountIsHealthy() bool {
	return m.mountHealthy == nil || m.mountHealthy()
}

// slotCap reports the configured active-slot budget.
func (m *materializer) slotCap() int { return cap(m.slots) }

// SlotsInUse and SlotsTotal report the live and total active-materialize slots; LastAuditUnix
// is the unix time of the last completed ToS audit (0 = never). These feed the admin /health
// endpoint and are not part of the frozen Engine interface (called on the concrete type).
func (m *materializer) SlotsInUse() int      { return len(m.slots) }
func (m *materializer) SlotsTotal() int      { return cap(m.slots) }
func (m *materializer) LastAuditUnix() int64 { return m.lastAudit.Load() }

// Start launches the idle and max-hold reapers. Non-blocking; idempotent. The reapers stop
// on ctx cancel or on Close (whichever comes first), and are awaited by Close — no leaks.
func (m *materializer) Start(ctx context.Context) {
	m.startOnce.Do(func() {
		rctx, cancel := context.WithCancel(ctx)
		m.cancel = cancel
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.runReapers(rctx)
		}()
	})
}

// Close stops the reapers, force-releases everything still materialized (ToS: leave the
// account clean on shutdown), and closes the proxy's idle connections. Safe to call once;
// safe after Start or without Start. Implements Engine.Close.
func (m *materializer) Close() error {
	var err error
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		// Snapshot the live set; release outside the lock (Release takes mu itself).
		hashes := make([]string, 0, len(m.track))
		for h := range m.track {
			hashes = append(hashes, h)
		}
		m.mu.Unlock()

		if m.cancel != nil {
			m.cancel()
		}
		m.wg.Wait() // reapers fully stopped before we touch shared state below

		// Best-effort clean release of anything still held. Errors are aggregated but do
		// not block shutdown.
		for _, h := range hashes {
			if rerr := m.Release(h); rerr != nil {
				err = errors.Join(err, rerr)
			}
		}

		m.prox.close()
		if m.probe != nil {
			m.probe.close()
		}
	})
	return err
}

// ReadAt is the materialize trigger. See package doc + docs/05 §4 for the full sequence:
// slot admission (+ LRU evict), singleflight first-materialize, fresh dl_link, SSRF-safe
// range-proxy with one refresh-on-4xx retry, and TouchAccess on every read.
func (m *materializer) ReadAt(hash string, fileID int, p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	ctx := context.Background()

	// 1+2. Ensure the release is materialized and pinned (refs++). Singleflight dedupes
	// concurrent first-reads; admit() handles the slot semaphore + LRU eviction.
	ent, err := m.ensureMaterialized(ctx, hash)
	if err != nil {
		return 0, err
	}
	// From here the release is pinned (refs incremented). Always unpin.
	defer m.unpin(hash)

	// 5. Record access for the idle reaper (every read).
	_ = m.store.TouchAccess(hash, m.now().Unix())

	// Probe-header cache: serve metadata-region reads from disk so Plex's header scan of a
	// freshly imported item doesn't re-hit the CDN (and never triggers a 2nd CreateTorrent;
	// the add already happened above). Falls through to live proxy on a miss.
	if m.probe != nil && m.probe.covers(off, int64(len(p))) {
		if n, ok := m.probe.readAt(hash, fileID, p, off); ok {
			metrics.IncProbeHit()
			return n, nil
		}
		metrics.IncProbeMiss()
	}

	// 3+4. Resolve a fresh link and range-proxy the window (with one refresh-on-4xx retry).
	n, body, err := m.proxyRead(ctx, ent, fileID, p, off)
	if err != nil {
		// Dead-cache at stream time: TorBox purged a release we believed materialized
		// (requestdl returns not-found, NOT a recoverable stale-link 4xx). Tear the entry
		// down and mark it errored so the arr re-grabs, instead of looping on EIO.
		if errors.Is(err, torbox.ErrNotFound) {
			return 0, m.markPurged(hash)
		}
		return n, err
	}

	// Populate the probe cache from the header region on first sight (best-effort).
	if m.probe != nil && body != nil && m.probe.covers(off, int64(len(p))) {
		m.probe.maybeStore(hash, fileID, off, body)
	}
	return n, nil
}

// ensureMaterialized guarantees the release identified by hash is added to TorBox and
// tracked in memory, and returns its entry with refs incremented (pinned). The caller MUST
// call m.unpin(hash) when done.
func (m *materializer) ensureMaterialized(ctx context.Context, hash string) (*entry, error) {
	// Fast path: already tracked -> pin and reuse (no new slot, no singleflight).
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("materialize: engine closed")
	}
	if ent, ok := m.track[hash]; ok {
		ent.refs++
		ent.lastUsed = m.now().UnixNano()
		m.mu.Unlock()
		return ent, nil
	}
	m.mu.Unlock()

	// Slow path: dedupe the materialize across concurrent first-readers for this hash.
	// Exactly one goroutine runs materializeLocked; the rest share its result.
	v, err, _ := m.sf.Do(hash, func() (any, error) {
		// Re-check under lock: a previous singleflight winner may have just finished.
		m.mu.Lock()
		if ent, ok := m.track[hash]; ok {
			m.mu.Unlock()
			return ent, nil
		}
		m.mu.Unlock()
		return m.materialize(ctx, hash)
	})
	if err != nil {
		return nil, err
	}
	_ = v // the canonical entry is re-fetched under lock below (eviction-safe)

	// Pin AFTER materialize. Because singleflight shares one *entry across all waiters, we
	// must increment refs once per ReadAt (here), not inside the shared func.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("materialize: engine closed")
	}
	// The entry could have been evicted between materialize and now only if refs==0; since
	// we are about to pin it, re-fetch the canonical entry to avoid pinning a stale one.
	cur, ok := m.track[hash]
	if !ok {
		// Lost to an eviction race; redo the whole thing.
		m.mu.Unlock()
		return m.ensureMaterialized(ctx, hash)
	}
	cur.refs++
	cur.lastUsed = m.now().UnixNano()
	m.mu.Unlock()
	return cur, nil
}

// materialize admits a slot (evicting an idle LRU release if needed), creates the torrent
// on TorBox, persists the state, and registers the in-memory entry. Runs under
// singleflight so it executes at most once per hash concurrently.
func (m *materializer) materialize(ctx context.Context, hash string) (*entry, error) {
	rel, _, err := m.store.GetRelease(hash)
	if err != nil {
		return nil, fmt.Errorf("materialize: get release %s: %w", short(hash), err)
	}
	if rel == nil {
		return nil, fmt.Errorf("materialize: unknown release %s", short(hash))
	}

	// If the catalog already says materialized (e.g. recovered from a prior run), adopt the
	// existing torbox id without a new add — but still take a slot to bound concurrency.
	if rel.State == catalog.StateMaterialized && rel.TorBoxID != 0 {
		if err := m.admit(ctx, hash); err != nil {
			return nil, err
		}
		return m.register(hash, rel.TorBoxID), nil
	}

	// Admit a slot BEFORE adding (so we never exceed the budget on TorBox).
	if err := m.admit(ctx, hash); err != nil {
		return nil, err
	}

	id, _, err := m.tb.CreateTorrent(rel.Magnet, !m.policy.AllowUncached)
	if err != nil {
		// Release the slot we took; the add did not land.
		m.releaseSlot()
		switch {
		case errors.Is(err, torbox.ErrRateLimited):
			// Do NOT spin/retry — surface a clear, wrapped error for the caller to back off.
			metrics.IncCreateRateLimited()
			return nil, fmt.Errorf("materialize: createtorrent rate limited for %s: %w", short(hash), err)
		case errors.Is(err, torbox.ErrNotFound):
			// Dead-cache: TorBox purged this item, so it can never materialize. Mark it
			// errored (permanent) and surface ErrPurged so the arr blacklists + re-grabs,
			// rather than a silent EIO it would retry forever.
			return nil, m.markPurged(hash)
		case isNotCached(err) && !m.policy.AllowUncached:
			// Mark the release errored so the catalog reflects reality; the read fails clearly.
			_ = m.store.SetState(hash, catalog.StateError, 0)
			return nil, fmt.Errorf("materialize: %s: %w", short(hash), ErrUncachedDisabled)
		default:
			return nil, fmt.Errorf("materialize: createtorrent %s: %w", short(hash), err)
		}
	}

	if err := m.store.SetState(hash, catalog.StateMaterialized, id); err != nil {
		// Persisted state failed but TorBox now holds the torrent: release it to avoid a
		// leak (ToS), free the slot, and fail the read.
		_ = m.tb.ControlDelete(id)
		m.releaseSlot()
		return nil, fmt.Errorf("materialize: persist state %s: %w", short(hash), err)
	}

	m.log.Info("materialized", "hash", short(hash), "torbox_id", id)
	metrics.IncMaterializes()
	return m.register(hash, id), nil
}

// register inserts a freshly materialized release into the in-memory track map. Assumes a
// slot has already been admitted for this hash.
func (m *materializer) register(hash string, id int64) *entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	ent := &entry{hash: hash, torboxID: id, lastUsed: m.now().UnixNano()}
	m.track[hash] = ent
	if id != 0 {
		m.seen[id] = struct{}{} // remember for the ToS audit scope
	}
	metrics.SetMaterializedCount(len(m.track))
	return ent
}

// unpin decrements the active-reader count for a hash. When a release becomes idle
// (refs hits 0) it may now be an eviction candidate, so wake any admit()ers waiting behind
// a full semaphore.
func (m *materializer) unpin(hash string) {
	m.mu.Lock()
	nowIdle := false
	if ent, ok := m.track[hash]; ok && ent.refs > 0 {
		ent.refs--
		nowIdle = ent.refs == 0
	}
	m.mu.Unlock()
	if nowIdle {
		m.notifyIdle()
	}
}

// notifyIdle coalesces a non-blocking wakeup to one admit()er blocked on the semaphore.
func (m *materializer) notifyIdle() {
	select {
	case m.idleSignal <- struct{}{}:
	default:
	}
}

// admit acquires an active slot for a NOT-yet-materialized release. Re-reads of an already
// materialized release do not call admit.
//
// Strategy (liveness-safe): on each iteration try to grab a slot; if full, LRU-release the
// least-recently-used IDLE (refs==0) release to free one and retry. If nothing is evictable
// (every slot is pinned by an active reader), wait for either a slot to free OR an idle
// signal (a previously-pinned release went idle => newly evictable), then loop. ctx
// cancellation always unblocks. This guarantees a queued admit re-attempts eviction once a
// slot-holder becomes idle, instead of blocking forever behind held-but-idle slots.
func (m *materializer) admit(ctx context.Context, incoming string) error {
	for {
		// Try to acquire without evicting.
		select {
		case m.slots <- struct{}{}:
			metrics.SetSlotsInUse(len(m.slots))
			return nil
		case <-ctx.Done():
			return fmt.Errorf("materialize: admit %s: %w", short(incoming), ctx.Err())
		default:
		}

		// Full: evict one idle LRU release if any, freeing a slot, then loop to grab it.
		if victim := m.pickLRUIdle(incoming); victim != "" {
			if err := m.Release(victim); err != nil {
				m.log.Warn("materialize: LRU release failed", "hash", short(victim), "err", err)
			}
			continue
		}

		// Nothing evictable right now (all slots pinned). Wait for a slot to free directly,
		// or for an idle signal indicating a release became evictable, then re-evaluate.
		select {
		case m.slots <- struct{}{}:
			metrics.SetSlotsInUse(len(m.slots))
			return nil
		case <-m.idleSignal:
			// A release went idle; loop to attempt eviction.
		case <-ctx.Done():
			return fmt.Errorf("materialize: admit %s: %w", short(incoming), ctx.Err())
		}
	}
}

// pickLRUIdle returns the hash of the least-recently-used materialized release that has no
// active readers (refs==0), excluding `incoming`. Returns "" if none is evictable.
func (m *materializer) pickLRUIdle(incoming string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var victim string
	var oldest int64
	for h, ent := range m.track {
		if h == incoming || ent.refs > 0 {
			continue // pinned or self -> never evict
		}
		if victim == "" || ent.lastUsed < oldest {
			victim, oldest = h, ent.lastUsed
		}
	}
	return victim
}

// releaseSlot frees one slot in the semaphore. Must be paired with a prior successful admit.
// It also nudges a blocked admit()er (covers the coalescing window where a waiter is between
// a failed pickLRUIdle and its blocking select).
func (m *materializer) releaseSlot() {
	select {
	case <-m.slots:
	default:
		// Defensive: never block / never go negative. A missing token here would indicate a
		// bookkeeping bug; we log rather than panic in production.
		m.log.Warn("materialize: releaseSlot with empty semaphore (bug)")
	}
	metrics.SetSlotsInUse(len(m.slots))
	m.notifyIdle()
}

// Release force-releases a materialized item: controltorrent delete on TorBox, state ->
// virtual in the catalog, drop the in-memory entry, and free its slot. Used by the LRU
// path, the reapers, and shutdown. Skips releases that are currently being read (refs>0)
// so an in-use stream is never torn down. Implements Engine.Release.
func (m *materializer) Release(hash string) error {
	m.mu.Lock()
	ent, ok := m.track[hash]
	if !ok {
		m.mu.Unlock()
		return nil // already released / never materialized -> no-op
	}
	if ent.refs > 0 {
		m.mu.Unlock()
		return nil // pinned by an active reader -> do not release
	}
	// Remove from tracking under the lock so a concurrent admit/read re-materializes
	// cleanly instead of racing on a half-torn-down entry.
	delete(m.track, hash)
	id := ent.torboxID
	metrics.SetMaterializedCount(len(m.track))
	m.mu.Unlock()

	// Network + persistence happen OUTSIDE the lock.
	var err error
	if id != 0 {
		if derr := m.tb.ControlDelete(id); derr != nil {
			err = fmt.Errorf("materialize: controldelete %s (id=%d): %w", short(hash), id, derr)
		}
	}
	if serr := m.store.SetState(hash, catalog.StateVirtual, 0); serr != nil {
		err = errors.Join(err, fmt.Errorf("materialize: set virtual %s: %w", short(hash), serr))
	}

	// Free the slot this release was holding.
	m.releaseSlot()
	if err == nil {
		metrics.IncReleases()
		m.log.Info("released", "hash", short(hash), "torbox_id", id)
	}
	return err
}

// markPurged handles the dead-cache case: TorBox reports the torrent is gone. It marks
// the release errored in the catalog (a permanent state the arr can act on), tears down
// any in-memory entry, and frees the slot it held — then returns ErrPurged.
//
// Safety vs. the active reader: when called from ReadAt the entry has refs>0 and ReadAt
// has a deferred unpin(hash). We remove the entry here regardless (the torrent no longer
// exists on TorBox, so there is nothing to protect from teardown); the later unpin then
// finds no entry and is a safe no-op. The in-memory slot is freed exactly once, here.
func (m *materializer) markPurged(hash string) error {
	m.mu.Lock()
	ent, ok := m.track[hash]
	if ok {
		delete(m.track, hash)
	}
	m.mu.Unlock()

	// Persist the permanent errored state so the arr blacklists + re-grabs.
	if serr := m.store.SetState(hash, catalog.StateError, 0); serr != nil {
		m.log.Warn("materialize: persist purged state failed", "hash", short(hash), "err", serr)
	}

	// If it was tracked it held a slot (and, best-effort, may still have a TorBox id we can
	// attempt to clean up — harmless if already gone). Free the slot exactly once.
	if ok {
		if ent.torboxID != 0 {
			if derr := m.tb.ControlDelete(ent.torboxID); derr != nil {
				m.log.Debug("materialize: controldelete on purged release (ignored)",
					"hash", short(hash), "id", ent.torboxID, "err", derr)
			}
		}
		m.releaseSlot()
	}

	m.log.Warn("materialize: release purged on TorBox (dead-cache), marked errored", "hash", short(hash))
	return fmt.Errorf("materialize: %s: %w", short(hash), ErrPurged)
}

// short truncates an infohash for logs (never log full secrets/paths unnecessarily).
func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// isNotCached reports whether a CreateTorrent error indicates the item is not cached on
// TorBox. The torbox client surfaces this in the error string ("not cached"/"not found");
// we match conservatively so the uncached-disabled path triggers correctly.
func isNotCached(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return containsFold(s, "not cached") || containsFold(s, "uncached") || containsFold(s, "not found")
}
