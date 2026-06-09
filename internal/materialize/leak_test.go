package materialize

import (
	"bytes"
	"context"
	"net/url"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/config"
)

// TestMain asserts no goroutine leaks across the whole package: reapers launched by Start
// must fully stop on Close / ctx-cancel. The httptest servers' transient goroutines are
// ignored (they are not ours).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
	)
}

// TestStartClose_NoLeak runs the reapers via Start then Close and relies on TestMain's
// goleak check to confirm the reaper goroutine exited.
func TestStartClose_NoLeak(t *testing.T) {
	content := bytes.Repeat([]byte{1}, 8<<10)
	m, store, _, _ := engineWithCDN(t, content, 3, "")
	newRelease(store, "leak", "magnet:leak")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	p := make([]byte, 256)
	if _, err := m.ReadAt("leak", 0, p, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	// Let the reaper tick at least conceptually; we don't wait the full interval.
	time.Sleep(5 * time.Millisecond)
	// Close (via t.Cleanup) stops the reaper; goleak in TestMain verifies no leak.
}

// TestStart_CtxCancelStopsReaper verifies the reaper stops on ctx cancel (independent of
// Close), then Close is still safe.
func TestStart_CtxCancelStopsReaper(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 3}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetLogger(quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	cancel()                          // reaper must observe cancellation and exit
	time.Sleep(10 * time.Millisecond) // give it a moment
	if err := m.Close(); err != nil { // Close after ctx-cancel must be safe
		t.Fatalf("Close after cancel: %v", err)
	}
}

// TestClose_ReleasesEverything asserts shutdown drains the account (ToS): all materialized
// releases are ControlDelete'd and set virtual.
func TestClose_ReleasesEverything(t *testing.T) {
	content := bytes.Repeat([]byte{2}, 8<<10)
	store := newFakeStore()
	tb := newFakeTorBox()
	cdn := newFakeCDN(content)
	tb.dlURLFn = func(id int64, fileID int) string { return cdn.url("h", fileID) }
	defer cdn.close()

	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 3}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetLogger(quietLogger())
	u, _ := url.Parse(cdn.srv.URL)
	m.AllowTestHost(u.Host)
	m.AllowTestHost(u.Hostname())

	for _, h := range []string{"x", "y"} {
		store.addRelease(&catalog.Release{Hash: h, Magnet: "m" + h, State: catalog.StateVirtual, Cached: true})
		p := make([]byte, 64)
		if _, err := m.ReadAt(h, 0, p, 0); err != nil {
			t.Fatalf("read %s: %v", h, err)
		}
	}
	if m.TrackedCount() != 2 {
		t.Fatalf("want 2 tracked, got %d", m.TrackedCount())
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if tb.deleteCount() != 2 {
		t.Fatalf("Close should ControlDelete all materialized (2), got %d", tb.deleteCount())
	}
	if store.state("x") != catalog.StateVirtual || store.state("y") != catalog.StateVirtual {
		t.Fatalf("Close should set all releases virtual")
	}
}
