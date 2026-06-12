package materialize

import (
	"errors"
	"testing"
	"time"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/config"
	"github.com/rushp4000/lazarr/internal/constants"
	"github.com/rushp4000/lazarr/internal/metrics"
	"github.com/rushp4000/lazarr/internal/torbox"
)

// --- Task 3: active_slots auto-detect from UserMe -----------------------------------------

// TestSlotCap_AutoDetectFromUserMe asserts that with Policy.ActiveSlots<=0 the engine's
// slot capacity is taken from TorBox.UserMe()'s ActiveSlots.
func TestSlotCap_AutoDetectFromUserMe(t *testing.T) {
	const want = 7
	store := newFakeStore()
	tb := newFakeTorBox()
	tb.activeSlots = want

	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 0}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	if got := m.SlotCap(); got != want {
		t.Fatalf("SlotCap() = %d, want %d (auto-detected from UserMe)", got, want)
	}
}

// TestSlotCap_ExplicitWinsOverUserMe asserts an explicit ActiveSlots is honored even when
// UserMe reports a different (higher) number.
func TestSlotCap_ExplicitWinsOverUserMe(t *testing.T) {
	const explicit = 2
	store := newFakeStore()
	tb := newFakeTorBox()
	tb.activeSlots = 9 // would win if auto-detect were used

	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: explicit}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	if got := m.SlotCap(); got != explicit {
		t.Fatalf("SlotCap() = %d, want %d (explicit overrides UserMe)", got, explicit)
	}
}

// --- Task 4: dead-cache handling -> ErrPurged + StateError --------------------------------

// TestMaterialize_DeadCacheAtCreate proves a createtorrent not-found maps to ErrPurged and
// marks the release errored (so the arr re-grabs), not a silent EIO.
func TestMaterialize_DeadCacheAtCreate(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	tb.createErr = torbox.ErrNotFound
	newRelease(store, "deadhash", "magnet:?xt=urn:btih:deadhash")

	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 3}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.SetLogger(quietLogger())
	t.Cleanup(func() { _ = m.Close() })

	p := make([]byte, 16)
	_, rerr := m.ReadAt("deadhash", 0, p, 0)
	if !errors.Is(rerr, ErrPurged) {
		t.Fatalf("ReadAt err = %v, want ErrPurged", rerr)
	}
	if st := store.state("deadhash"); st != catalog.StateError {
		t.Fatalf("release state = %q, want %q", st, catalog.StateError)
	}
	// The slot taken before the add must be returned: a fresh read still gets ErrPurged
	// (not ErrSlotsExhausted / a hang).
	if m.SlotCap() != 3 {
		t.Fatalf("SlotCap changed: %d", m.SlotCap())
	}
	if tracked := m.TrackedCount(); tracked != 0 {
		t.Fatalf("TrackedCount = %d, want 0 (purged entry must not linger)", tracked)
	}
}

// TestMaterialize_DeadCacheAtStream proves a requestdl not-found on an already-materialized
// release (TorBox purged it after the add) tears the entry down, frees the slot, marks the
// release errored, and returns ErrPurged.
func TestMaterialize_DeadCacheAtStream(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	// CreateTorrent succeeds; RequestDL then reports the torrent is gone.
	tb.requestDLErr = torbox.ErrNotFound
	newRelease(store, "gonehash", "magnet:?xt=urn:btih:gonehash")

	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 1}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.SetLogger(quietLogger())
	t.Cleanup(func() { _ = m.Close() })

	p := make([]byte, 16)
	_, rerr := m.ReadAt("gonehash", 0, p, 0)
	if !errors.Is(rerr, ErrPurged) {
		t.Fatalf("ReadAt err = %v, want ErrPurged", rerr)
	}
	if st := store.state("gonehash"); st != catalog.StateError {
		t.Fatalf("release state = %q, want %q", st, catalog.StateError)
	}
	if tracked := m.TrackedCount(); tracked != 0 {
		t.Fatalf("TrackedCount = %d, want 0 (slot must be freed after purge)", tracked)
	}
	// Slot freed: a second read on a (different) good release would admit. Re-reading the
	// purged one just re-errors without hanging.
	_, rerr2 := m.ReadAt("gonehash", 0, p, 0)
	if !errors.Is(rerr2, ErrPurged) {
		t.Fatalf("second ReadAt err = %v, want ErrPurged", rerr2)
	}
}

// --- Task 5: broken-mount guard -----------------------------------------------------------

// TestReapers_SkipWhenMountUnhealthy proves the idle + max-hold sweeps are skipped (no
// ControlDelete) when the mount-health guard reports unhealthy, and proceed when healthy.
func TestReapers_SkipWhenMountUnhealthy(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	mk := func(healthy bool) (*materializer, *fakeStore, *fakeTorBox) {
		store := newFakeStore()
		tb := newFakeTorBox()
		m, err := New(Deps{
			Store:  store,
			TorBox: tb,
			Policy: config.Policy{ActiveSlots: 3, IdleTTL: config.Duration(time.Minute), MaxHold: config.Duration(time.Hour)},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		m.SetLogger(quietLogger())
		m.SetNow(func() time.Time { return now })
		m.SetMountHealthy(func() bool { return healthy })
		t.Cleanup(func() { _ = m.Close() })

		// Register a materialized, long-idle release that a healthy reaper would delete.
		store.addRelease(&catalog.Release{
			Hash: "h1", Name: "Rel", Category: "radarr_hin",
			State: catalog.StateMaterialized, TorBoxID: 42,
			AddedOn: now.Add(-2 * time.Hour).Unix(), LastAccess: now.Add(-time.Hour).Unix(),
		})
		// Track it in memory so Release would actually call ControlDelete.
		m.register("h1", 42)
		return m, store, tb
	}

	t.Run("unhealthy skips delete", func(t *testing.T) {
		m, _, tb := mk(false)
		m.runReapOnceGuarded()
		if tb.deleteCount() != 0 {
			t.Fatalf("ControlDelete called %d times while mount unhealthy; want 0", tb.deleteCount())
		}
		if !m.IsTracked("h1") {
			t.Fatal("release should remain tracked when reaping is skipped")
		}
	})

	t.Run("healthy proceeds", func(t *testing.T) {
		m, _, tb := mk(true)
		m.runReapOnceGuarded()
		if tb.deleteCount() != 1 {
			t.Fatalf("ControlDelete called %d times while mount healthy; want 1", tb.deleteCount())
		}
	})

	// S3: a skipped sweep increments lazarr_reaper_skipped_total so an operator can alert on
	// "reaping paused" (items held past max-hold while the mount is wedged).
	t.Run("unhealthy increments skip metric", func(t *testing.T) {
		before, err := metrics.GatherSummary()
		if err != nil {
			t.Fatalf("GatherSummary: %v", err)
		}
		m, _, _ := mk(false)
		m.runReapOnceGuarded()
		after, err := metrics.GatherSummary()
		if err != nil {
			t.Fatalf("GatherSummary: %v", err)
		}
		if after.ReaperSkippedTotal != before.ReaperSkippedTotal+1 {
			t.Fatalf("reaper_skipped_total = %v, want %v (one skip)",
				after.ReaperSkippedTotal, before.ReaperSkippedTotal+1)
		}
	})
}

// --- B2: untracked-release leak + boot reconciliation -------------------------------------

// TestRelease_UntrackedLeftover proves Release deletes a TorBox item the store still
// believes is materialized but that this process does not track (a crash/restart leftover),
// instead of the old silent no-op that left it on the account forever (B2).
func TestRelease_UntrackedLeftover(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 3}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.SetLogger(quietLogger())
	t.Cleanup(func() { _ = m.Close() })

	store.addRelease(&catalog.Release{Hash: "leak", State: catalog.StateMaterialized, TorBoxID: 99})
	if m.IsTracked("leak") {
		t.Fatal("precondition: leftover must not be tracked")
	}

	if err := m.Release("leak"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if tb.deleteCount() != 1 {
		t.Fatalf("want 1 ControlDelete on untracked leftover, got %d", tb.deleteCount())
	}
	if got := store.state("leak"); got != catalog.StateVirtual {
		t.Fatalf("want virtual after untracked release, got %q", got)
	}
}

// TestReconcile_ReleasesLeftovers proves the boot sweep releases every store-believed
// materialized row not tracked in memory, and leaves virtual rows untouched (B2).
func TestReconcile_ReleasesLeftovers(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 3}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.SetLogger(quietLogger())
	t.Cleanup(func() { _ = m.Close() })

	store.addRelease(&catalog.Release{Hash: "a", State: catalog.StateMaterialized, TorBoxID: 1})
	store.addRelease(&catalog.Release{Hash: "b", State: catalog.StateMaterialized, TorBoxID: 2})
	store.addRelease(&catalog.Release{Hash: "v", State: catalog.StateVirtual}) // already released

	m.Reconcile()

	if tb.deleteCount() != 2 {
		t.Fatalf("want 2 ControlDelete (a,b), got %d", tb.deleteCount())
	}
	if store.state("a") != catalog.StateVirtual || store.state("b") != catalog.StateVirtual {
		t.Fatalf("leftovers must be flipped to virtual: a=%q b=%q", store.state("a"), store.state("b"))
	}
	if store.state("v") != catalog.StateVirtual {
		t.Fatalf("virtual row must be untouched, got %q", store.state("v"))
	}
}

// TestRelease_InFlightMaterializeNotDeleted proves releaseUntracked defers to a concurrent
// materialize: a store-only row whose hash is marked in-flight must NOT be deleted, so a
// boot-reconcile / reaper never tears down an item a first-read is mid-materializing (B2).
func TestRelease_InFlightMaterializeNotDeleted(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 3}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.SetLogger(quietLogger())
	t.Cleanup(func() { _ = m.Close() })

	store.addRelease(&catalog.Release{Hash: "x", State: catalog.StateMaterialized, TorBoxID: 7})
	m.mu.Lock()
	m.inflight["x"] = struct{}{} // a materialize for "x" is in flight
	m.mu.Unlock()

	if err := m.Release("x"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if tb.deleteCount() != 0 {
		t.Fatalf("must NOT delete an in-flight materialize, got %d ControlDelete", tb.deleteCount())
	}
	if store.state("x") != catalog.StateMaterialized {
		t.Fatalf("state must stay materialized, got %q", store.state("x"))
	}

	m.mu.Lock()
	delete(m.inflight, "x")
	m.mu.Unlock()
}

// --- B3: graceful shutdown force-releases pinned entries -----------------------------------

// TestClose_ForceReleasesPinnedEntry proves Close tears down an entry that still has active
// readers (refs>0) after the drain window: the mount is gone, so leaving the item on the
// account (a ToS leak that B2 would make permanent post-restart) is the worse outcome (B3).
func TestClose_ForceReleasesPinnedEntry(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 3}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.SetLogger(quietLogger())
	m.SetDrainTimeout(50 * time.Millisecond) // refs never drain; force after the short window

	store.addRelease(&catalog.Release{Hash: "pin", State: catalog.StateMaterialized, TorBoxID: 55})
	m.slots <- struct{}{} // the slot this materialization holds
	m.register("pin", 55)
	m.mu.Lock()
	m.track["pin"].refs = 1 // pretend an in-flight reader is pinning it
	m.mu.Unlock()

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if tb.deleteCount() != 1 {
		t.Fatalf("Close must force-release the pinned entry (1 ControlDelete), got %d", tb.deleteCount())
	}
	if store.state("pin") != catalog.StateVirtual {
		t.Fatalf("want virtual after force-release, got %q", store.state("pin"))
	}
	if m.IsTracked("pin") {
		t.Fatal("entry must be dropped from track after force-release")
	}
}

// TestMountIsHealthy_NilGuard proves the default (no guard installed) always reaps.
func TestMountIsHealthy_NilGuard(t *testing.T) {
	m, err := New(Deps{Store: newFakeStore(), TorBox: newFakeTorBox(), Policy: config.Policy{ActiveSlots: 1}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if !m.mountIsHealthy() {
		t.Fatal("nil mount-health guard must report healthy (reap as before)")
	}
}

// TestMaterialize_QueuedUpstreamDefersRetries proves a createtorrent answered with
// "Download already queued." (TorBox parked the add server-side: account cooldown /
// slots full) defers the hash: reads inside the deferral window fail fast with NO
// further CreateTorrent call, and a read after the window retries the add. Live
// failure mode 2026-06-12: an arr import loop re-POSTed createtorrent ~120×/hour
// against a cooldown-locked account.
func TestMaterialize_QueuedUpstreamDefersRetries(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	tb.createErrOnce = torbox.ErrAlreadyQueued
	newRelease(store, "queuedhash", "magnet:?xt=urn:btih:queuedhash")

	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 1}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.SetLogger(quietLogger())
	now := time.Unix(1_000_000, 0)
	m.SetNow(func() time.Time { return now })
	t.Cleanup(func() { _ = m.Close() })

	p := make([]byte, 16)
	_, rerr := m.ReadAt("queuedhash", 0, p, 0)
	if !errors.Is(rerr, torbox.ErrAlreadyQueued) {
		t.Fatalf("first ReadAt err = %v, want ErrAlreadyQueued", rerr)
	}
	if got := tb.createCount(); got != 1 {
		t.Fatalf("createCount after first read = %d, want 1", got)
	}
	// The release must NOT be marked errored — queued upstream is recoverable.
	if st := store.state("queuedhash"); st == catalog.StateError {
		t.Fatalf("release state = %q; queued upstream must not be permanent", st)
	}

	// Inside the deferral window: fail fast, no new CreateTorrent.
	_, rerr2 := m.ReadAt("queuedhash", 0, p, 0)
	if !errors.Is(rerr2, torbox.ErrAlreadyQueued) {
		t.Fatalf("second ReadAt err = %v, want ErrAlreadyQueued", rerr2)
	}
	if got := tb.createCount(); got != 1 {
		t.Fatalf("createCount inside deferral window = %d, want 1 (no hot retry)", got)
	}

	// After the window: the next read retries the add (createErrOnce is consumed, so
	// the fake now succeeds). The read may still fail later in the chain (no dlURLFn),
	// but the deferral must be lifted and CreateTorrent attempted again.
	m.SetNow(func() time.Time { return now.Add(constants.QueuedDeferral + time.Second) })
	_, rerr3 := m.ReadAt("queuedhash", 0, p, 0)
	if errors.Is(rerr3, torbox.ErrAlreadyQueued) {
		t.Fatalf("third ReadAt err = %v; deferral must lift after the window", rerr3)
	}
	if got := tb.createCount(); got != 2 {
		t.Fatalf("createCount after deferral window = %d, want 2 (retry attempted)", got)
	}
}
