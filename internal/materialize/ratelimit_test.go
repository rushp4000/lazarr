package materialize

import (
	"errors"
	"testing"
	"time"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/config"
	"github.com/rushp4000/lazarr/internal/constants"
	"github.com/rushp4000/lazarr/internal/torbox"
)

// --- v1.1.7: createtorrent budget protection (Fire 1) -------------------------------------
//
// These tests pin the two fixes that stop a single stuck item (read every ~60s by an arr
// import loop) from burning TorBox's account-wide "60 per 1 hour" createtorrent budget:
//   - a GLOBAL backoff after a 429, so subsequent materialize calls make NO TorBox call
//     until the window clears; and
//   - a fast-fail when the catalog already has the release in StateError, so a known
//     not-cached/purged item never re-issues createtorrent.

// TestMaterialize_RateLimit_GlobalBackoff proves the first 429 arms a global backoff: a
// second read of the SAME hash inside the window fails fast with ErrRateLimited and issues
// NO additional createtorrent call (the bug was one call per read => budget exhaustion).
func TestMaterialize_RateLimit_GlobalBackoff(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	tb.createErr = torbox.ErrRateLimited
	newRelease(store, "rlhash", "magnet:?xt=urn:btih:rlhash")

	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 3}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.SetLogger(quietLogger())
	t.Cleanup(func() { _ = m.Close() })

	p := make([]byte, 16)
	if _, rerr := m.ReadAt("rlhash", 0, p, 0); !errors.Is(rerr, torbox.ErrRateLimited) {
		t.Fatalf("1st ReadAt err = %v, want ErrRateLimited", rerr)
	}
	if got := tb.createCount(); got != 1 {
		t.Fatalf("after 1st read createCount = %d, want 1", got)
	}

	// Second read inside the backoff window must NOT touch TorBox.
	if _, rerr := m.ReadAt("rlhash", 0, p, 0); !errors.Is(rerr, torbox.ErrRateLimited) {
		t.Fatalf("2nd ReadAt err = %v, want ErrRateLimited (fast-fail)", rerr)
	}
	if got := tb.createCount(); got != 1 {
		t.Fatalf("after 2nd read createCount = %d, want 1 (global backoff must skip the call)", got)
	}
	// The slot taken before the failed add must have been returned.
	if m.SlotCap() != 3 {
		t.Fatalf("SlotCap changed: %d", m.SlotCap())
	}
}

// TestMaterialize_RateLimit_BackoffIsGlobal proves the backoff is account-wide, not
// per-hash: after one hash trips the 429, a DIFFERENT hash also fast-fails with no call.
func TestMaterialize_RateLimit_BackoffIsGlobal(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	tb.createErr = torbox.ErrRateLimited
	newRelease(store, "hashA", "magnet:?xt=urn:btih:hashA")
	newRelease(store, "hashB", "magnet:?xt=urn:btih:hashB")

	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 3}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.SetLogger(quietLogger())
	t.Cleanup(func() { _ = m.Close() })

	p := make([]byte, 16)
	if _, rerr := m.ReadAt("hashA", 0, p, 0); !errors.Is(rerr, torbox.ErrRateLimited) {
		t.Fatalf("hashA ReadAt err = %v, want ErrRateLimited", rerr)
	}
	if _, rerr := m.ReadAt("hashB", 0, p, 0); !errors.Is(rerr, torbox.ErrRateLimited) {
		t.Fatalf("hashB ReadAt err = %v, want ErrRateLimited (global backoff)", rerr)
	}
	if got := tb.createCount(); got != 1 {
		t.Fatalf("createCount = %d, want 1 (only hashA's call should reach TorBox)", got)
	}
}

// TestMaterialize_RateLimit_BackoffExpires proves the backoff is temporary: once the clock
// advances past RateLimitBackoff, a fresh read issues a real createtorrent again.
func TestMaterialize_RateLimit_BackoffExpires(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	tb.createErr = torbox.ErrRateLimited
	newRelease(store, "exphash", "magnet:?xt=urn:btih:exphash")

	base := time.Now()
	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 3}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.SetLogger(quietLogger())
	m.SetNow(func() time.Time { return base })
	t.Cleanup(func() { _ = m.Close() })

	p := make([]byte, 16)
	if _, rerr := m.ReadAt("exphash", 0, p, 0); !errors.Is(rerr, torbox.ErrRateLimited) {
		t.Fatalf("ReadAt err = %v, want ErrRateLimited", rerr)
	}
	if got := tb.createCount(); got != 1 {
		t.Fatalf("createCount = %d, want 1", got)
	}

	// Jump past the backoff window; the next read must reach TorBox again.
	m.SetNow(func() time.Time { return base.Add(constants.RateLimitBackoff + time.Minute) })
	if _, rerr := m.ReadAt("exphash", 0, p, 0); !errors.Is(rerr, torbox.ErrRateLimited) {
		t.Fatalf("post-window ReadAt err = %v, want ErrRateLimited (real call)", rerr)
	}
	if got := tb.createCount(); got != 2 {
		t.Fatalf("createCount = %d, want 2 (backoff must expire and allow a retry)", got)
	}
}

// TestMaterialize_StateError_FastFail proves a release already in StateError never issues a
// createtorrent: it fails fast with ErrPurged so the qbit layer keeps reporting the torrent
// errored and the arr removes it instead of looping reads forever.
func TestMaterialize_StateError_FastFail(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	// A release a prior attempt already marked errored (not cached / purged).
	store.addRelease(&catalog.Release{
		Hash:    "errhash",
		Name:    "Rel errhash",
		Magnet:  "magnet:?xt=urn:btih:errhash",
		State:   catalog.StateError,
		AddedOn: time.Now().Unix(),
	})

	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 3}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.SetLogger(quietLogger())
	t.Cleanup(func() { _ = m.Close() })

	p := make([]byte, 16)
	if _, rerr := m.ReadAt("errhash", 0, p, 0); !errors.Is(rerr, ErrPurged) {
		t.Fatalf("ReadAt err = %v, want ErrPurged", rerr)
	}
	if got := tb.createCount(); got != 0 {
		t.Fatalf("createCount = %d, want 0 (errored release must never call createtorrent)", got)
	}
	if m.TrackedCount() != 0 {
		t.Fatalf("TrackedCount = %d, want 0", m.TrackedCount())
	}
}
