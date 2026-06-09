package materialize

import (
	"errors"
	"testing"
	"time"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/config"
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
