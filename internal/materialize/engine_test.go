package materialize

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/config"
	"github.com/rushp4000/lazarr/internal/torbox"
)

// quietLogger discards log output so tests stay readable.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// newRelease registers a cached, virtual release in the store.
func newRelease(s *fakeStore, hash, magnet string) {
	s.addRelease(&catalog.Release{
		Hash:     hash,
		Name:     "Rel " + hash,
		Category: "radarr_hin",
		Magnet:   magnet,
		State:    catalog.StateVirtual,
		Cached:   true,
		AddedOn:  time.Now().Unix(),
	})
}

// engineWithCDN wires an engine to fakes + a fake CDN and allows the CDN host through the
// host-pin test seam. ProbeCache is off unless probeDir is non-empty.
func engineWithCDN(t *testing.T, content []byte, slots int, probeDir string) (*materializer, *fakeStore, *fakeTorBox, *fakeCDN) {
	t.Helper()
	store := newFakeStore()
	tb := newFakeTorBox()
	cdn := newFakeCDN(content)
	tb.dlURLFn = func(id int64, fileID int) string { return cdn.url("dd8255ec", fileID) }

	pol := config.Policy{ActiveSlots: slots, ProbeCache: probeDir != ""}
	m, err := New(Deps{Store: store, TorBox: tb, Policy: pol, ProbeCacheDir: probeDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.SetLogger(quietLogger())

	// Host-pin test seam: allow the httptest host (127.0.0.1:PORT) without weakening prod.
	u, _ := url.Parse(cdn.srv.URL)
	m.AllowTestHost(u.Host)
	m.AllowTestHost(u.Hostname())

	t.Cleanup(func() { _ = m.Close(); cdn.close() })
	return m, store, tb, cdn
}

// --- range-proxy correctness -------------------------------------------------------------

func TestReadAt_RangeProxy(t *testing.T) {
	content := make([]byte, 1<<20) // 1 MiB
	for i := range content {
		content[i] = byte(i % 251)
	}
	m, store, tb, _ := engineWithCDN(t, content, 3, "")
	newRelease(store, "h1", "magnet:?xt=urn:btih:h1")

	cases := []struct {
		name string
		off  int64
		size int
	}{
		{"head", 0, 4096},
		{"mid", 500000, 8192},
		{"tail-partial", int64(len(content)) - 100, 4096}, // request past EOF -> short read
		{"single-byte", 12345, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := make([]byte, tc.size)
			n, err := m.ReadAt("h1", 0, p, tc.off)
			if err != nil {
				t.Fatalf("ReadAt: %v", err)
			}
			want := content[tc.off:min64(tc.off+int64(tc.size), int64(len(content)))]
			if !bytes.Equal(p[:n], want) {
				t.Fatalf("bytes mismatch: got %d bytes, want %d; first-diff", n, len(want))
			}
		})
	}
	if store.state("h1") != catalog.StateMaterialized {
		t.Fatalf("expected materialized, got %q", store.state("h1"))
	}
	if tb.createCount() != 1 {
		t.Fatalf("expected exactly 1 CreateTorrent, got %d", tb.createCount())
	}
	if store.touches == 0 {
		t.Fatalf("expected TouchAccess on reads")
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// --- singleflight: one materialize under N concurrent first-reads -----------------------

func TestReadAt_Singleflight(t *testing.T) {
	content := bytes.Repeat([]byte{0xAB}, 64<<10)
	m, store, tb, _ := engineWithCDN(t, content, 3, "")
	newRelease(store, "sf", "magnet:?xt=urn:btih:sf")

	const N = 24
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := make([]byte, 1024)
			_, errs[i] = m.ReadAt("sf", 0, p, 0)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("reader %d: %v", i, err)
		}
	}
	if tb.createCount() != 1 {
		t.Fatalf("singleflight failed: %d CreateTorrent calls (want 1)", tb.createCount())
	}
	if !m.IsTracked("sf") {
		t.Fatalf("release not tracked after concurrent reads")
	}
}

// --- slot exhaustion + LRU eviction; never evict an actively-read release ---------------

func TestReadAt_LRUEviction(t *testing.T) {
	content := bytes.Repeat([]byte{1}, 32<<10)
	m, store, tb, _ := engineWithCDN(t, content, 2, "") // 2 slots
	for _, h := range []string{"a", "b", "c"} {
		newRelease(store, h, "magnet:"+h)
	}

	read := func(h string) {
		p := make([]byte, 512)
		if _, err := m.ReadAt(h, 0, p, 0); err != nil {
			t.Fatalf("read %s: %v", h, err)
		}
	}
	// Materialize a then b (fills both slots), each read completes so refs drop to 0.
	read("a")
	time.Sleep(2 * time.Millisecond)
	read("b")
	if m.TrackedCount() != 2 {
		t.Fatalf("want 2 tracked, got %d", m.TrackedCount())
	}
	// c must evict the LRU idle release (a).
	read("c")
	if m.IsTracked("a") {
		t.Fatalf("LRU victim 'a' should have been evicted")
	}
	if !m.IsTracked("b") || !m.IsTracked("c") {
		t.Fatalf("b and c should be tracked")
	}
	if tb.deleteCount() < 1 {
		t.Fatalf("expected at least one ControlDelete from eviction")
	}
	if store.state("a") != catalog.StateVirtual {
		t.Fatalf("evicted 'a' should be virtual, got %q", store.state("a"))
	}
}

// An actively-read release must never be evicted to admit a new one. With 1 slot and "a"
// blocked mid-read (refs=1), an attempt to materialize "b" must queue behind "a" rather
// than evict it. Once "a" finishes, "b" proceeds.
func TestReadAt_NeverEvictActiveReader(t *testing.T) {
	content := bytes.Repeat([]byte{7}, 32<<10)
	store := newFakeStore()
	tb := newFakeTorBox()

	// A CDN that blocks the first request until release is closed (gates "a" mid-read).
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var gateOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gateOnce.Do(func() {
			started <- struct{}{}
			<-release // hold the connection open -> reader keeps refs=1
		})
		// Serve the requested range (for "a"'s gated read and all of "b"ّs reads).
		total := int64(len(content))
		start, end, ok := parseRange(r.Header.Get("Range"), total)
		if !ok {
			start, end = 0, total-1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content[start : end+1])
	}))
	defer srv.Close()

	tb.dlURLFn = func(id int64, fileID int) string {
		return fmt.Sprintf("%s/dl/x/%d/f.mp4?token=t&expires=9999999999", srv.URL, fileID)
	}

	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 1}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetLogger(quietLogger())
	u, _ := url.Parse(srv.URL)
	m.AllowTestHost(u.Host)
	m.AllowTestHost(u.Hostname())
	defer func() { _ = m.Close() }()

	store.addRelease(&catalog.Release{Hash: "aaaa", Magnet: "ma", State: catalog.StateVirtual, Cached: true})
	store.addRelease(&catalog.Release{Hash: "bbbb", Magnet: "mb", State: catalog.StateVirtual, Cached: true})

	readErr := make(chan error, 1)
	go func() {
		p := make([]byte, 256)
		_, err := m.ReadAt("aaaa", 0, p, 0)
		readErr <- err
	}()
	<-started // "a" is blocked mid-read, holding the only slot with refs=1

	bDone := make(chan error, 1)
	go func() {
		p := make([]byte, 256)
		_, err := m.ReadAt("bbbb", 0, p, 0)
		bDone <- err
	}()

	// "b" must NOT be able to evict "a"; "a" stays tracked + pinned while it reads.
	time.Sleep(50 * time.Millisecond)
	if !m.IsTracked("aaaa") {
		t.Fatalf("active reader 'aaaa' was evicted while reading")
	}
	if m.Refs("aaaa") < 1 {
		t.Fatalf("active reader 'aaaa' lost its ref")
	}
	select {
	case <-bDone:
		t.Fatalf("'b' should be blocked waiting for a slot, not complete")
	default:
	}

	// Release "a": its slot frees, "b" proceeds.
	close(release)
	if err := <-readErr; err != nil {
		t.Fatalf("read a: %v", err)
	}
	if err := <-bDone; err != nil {
		t.Fatalf("read b: %v", err)
	}
}

// --- createtorrent rate-limit + not-cached paths ----------------------------------------

func TestReadAt_RateLimited(t *testing.T) {
	m, store, tb, _ := engineWithCDN(t, []byte("x"), 3, "")
	tb.createErr = torbox.ErrRateLimited
	newRelease(store, "rl", "magnet:rl")

	p := make([]byte, 1)
	_, err := m.ReadAt("rl", 0, p, 0)
	if !errors.Is(err, torbox.ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
	if tb.createCount() != 1 {
		t.Fatalf("rate-limit must NOT retry-loop: %d create calls", tb.createCount())
	}
	if m.IsTracked("rl") {
		t.Fatalf("failed materialize should not be tracked")
	}
}

func TestReadAt_NotCachedUncachedDisabled(t *testing.T) {
	m, store, tb, _ := engineWithCDN(t, []byte("x"), 3, "")
	tb.createErr = errors.New("torbox: item not cached")
	newRelease(store, "nc", "magnet:nc")

	p := make([]byte, 1)
	_, err := m.ReadAt("nc", 0, p, 0)
	if !errors.Is(err, ErrUncachedDisabled) {
		t.Fatalf("want ErrUncachedDisabled, got %v", err)
	}
	if store.state("nc") != catalog.StateError {
		t.Fatalf("want state=error, got %q", store.state("nc"))
	}
	if tb.createCount() != 1 {
		t.Fatalf("must not retry: %d create calls", tb.createCount())
	}
}

// --- dl_link cache + refresh-on-4xx (exactly one re-RequestDL + one retry) ---------------

func TestReadAt_RefreshOn4xx(t *testing.T) {
	for _, status := range []int{400, 403, 410} {
		t.Run(fmt.Sprintf("status-%d", status), func(t *testing.T) {
			content := bytes.Repeat([]byte{9}, 16<<10)
			m, store, tb, cdn := engineWithCDN(t, content, 3, "")
			newRelease(store, "rf", "magnet:rf")

			// First read materializes + caches a link (1 RequestDL).
			p := make([]byte, 1024)
			if _, err := m.ReadAt("rf", 0, p, 0); err != nil {
				t.Fatalf("warmup read: %v", err)
			}
			base := tb.requestDLCount()

			// Next read: CDN returns ONE 4xx, then serves bytes. Expect exactly one extra
			// RequestDL and one retry that succeeds.
			cdn.setExpire(1, status)
			n, err := m.ReadAt("rf", 0, p, 0)
			if err != nil {
				t.Fatalf("refresh read: %v", err)
			}
			if !bytes.Equal(p[:n], content[:n]) {
				t.Fatalf("bytes mismatch after refresh")
			}
			if got := tb.requestDLCount() - base; got != 1 {
				t.Fatalf("want exactly 1 extra RequestDL on refresh, got %d", got)
			}
		})
	}
}

func TestReadAt_RepeatedExpiryNoInfiniteLoop(t *testing.T) {
	content := bytes.Repeat([]byte{9}, 16<<10)
	m, store, tb, cdn := engineWithCDN(t, content, 3, "")
	newRelease(store, "rf2", "magnet:rf2")

	p := make([]byte, 1024)
	if _, err := m.ReadAt("rf2", 0, p, 0); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	base := tb.requestDLCount()

	// CDN expires for many requests in a row: the engine must refresh ONCE, retry ONCE,
	// then surface an error (no infinite loop).
	cdn.setExpire(10, 403)
	_, err := m.ReadAt("rf2", 0, p, 0)
	if err == nil {
		t.Fatalf("expected error on repeated expiry")
	}
	if got := tb.requestDLCount() - base; got != 1 {
		t.Fatalf("want exactly 1 refresh attempt, got %d", got)
	}
}

// dl_link cache: a second read of the same window does NOT re-RequestDL.
func TestReadAt_LinkCached(t *testing.T) {
	content := bytes.Repeat([]byte{3}, 16<<10)
	m, store, tb, _ := engineWithCDN(t, content, 3, "")
	newRelease(store, "lc", "magnet:lc")

	p := make([]byte, 1024)
	_, _ = m.ReadAt("lc", 0, p, 0)
	first := tb.requestDLCount()
	_, _ = m.ReadAt("lc", 0, p, 0)
	if tb.requestDLCount() != first {
		t.Fatalf("link not cached: re-requested DL (%d -> %d)", first, tb.requestDLCount())
	}
}

// --- idle reaper + max-hold reaper -------------------------------------------------------

func TestReaper_Idle(t *testing.T) {
	content := bytes.Repeat([]byte{5}, 8<<10)
	m, store, tb, _ := engineWithCDN(t, content, 3, "")
	newRelease(store, "id", "magnet:id")

	p := make([]byte, 256)
	if _, err := m.ReadAt("id", 0, p, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	// Jump the clock far past idle_ttl.
	m.policy.IdleTTL = config.Duration(time.Minute)
	future := time.Now().Add(time.Hour)
	m.SetNow(func() time.Time { return future })

	m.reapOnce()
	if m.IsTracked("id") {
		t.Fatalf("idle release not reaped")
	}
	if store.state("id") != catalog.StateVirtual {
		t.Fatalf("want virtual after idle reap, got %q", store.state("id"))
	}
	if tb.deleteCount() < 1 {
		t.Fatalf("expected ControlDelete on idle reap")
	}
}

func TestReaper_MaxHold(t *testing.T) {
	content := bytes.Repeat([]byte{6}, 8<<10)
	m, store, tb, _ := engineWithCDN(t, content, 3, "")
	// AddedOn far in the past so OverMaxHold matches regardless of access.
	store.addRelease(&catalog.Release{
		Hash: "mh", Magnet: "magnet:mh", State: catalog.StateVirtual, Cached: true,
		AddedOn: time.Now().Add(-48 * time.Hour).Unix(),
	})

	p := make([]byte, 256)
	if _, err := m.ReadAt("mh", 0, p, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	m.policy.MaxHold = config.Duration(24 * time.Hour)
	m.reapOnce()
	if m.IsTracked("mh") {
		t.Fatalf("over-max-hold release not reaped")
	}
	if tb.deleteCount() < 1 {
		t.Fatalf("expected ControlDelete on max-hold reap")
	}
}

// Reaper must skip a release that is actively being read.
func TestReaper_SkipsActiveReader(t *testing.T) {
	content := bytes.Repeat([]byte{8}, 8<<10)
	m, store, _, _ := engineWithCDN(t, content, 3, "")
	newRelease(store, "ar", "magnet:ar")

	// Materialize and manually pin (simulate an in-flight read) by bumping refs.
	p := make([]byte, 64)
	if _, err := m.ReadAt("ar", 0, p, 0); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.track["ar"].refs = 1 // pretend a reader is in-flight
	m.mu.Unlock()

	m.policy.IdleTTL = config.Duration(time.Minute)
	future := time.Now().Add(time.Hour)
	m.SetNow(func() time.Time { return future })
	m.reapOnce()

	if !m.IsTracked("ar") {
		t.Fatalf("reaper must not release an actively-read release")
	}
	// Unpin so Close can release cleanly.
	m.mu.Lock()
	m.track["ar"].refs = 0
	m.mu.Unlock()
}

// --- AuditTOS: leak detection + decypharr ids NOT flagged --------------------------------

func TestAuditTOS_LeakAndScope(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 3}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetLogger(quietLogger())
	defer func() { _ = m.Close() }()

	// Simulate: Lazarr added id 1000 then "released" it (so it's NOT in MaterializedIDs),
	// but the account still holds it (a leak). decypharr owns 7777 (out of scope).
	m.mu.Lock()
	m.seen[1000] = struct{}{} // Lazarr added 1000 this lifetime
	m.mu.Unlock()
	tb.myList = []torbox.TorrentDetail{
		{ID: 1000, Hash: "leaked"},     // Lazarr leak (in scope, held, not believed)
		{ID: 7777, Hash: "decypharr"},  // decypharr-owned, NOT in Lazarr scope
		{ID: 8888, Hash: "decypharr2"}, // decypharr-owned
	}

	// Capture logs to assert the leak is flagged and decypharr ids are not.
	var buf bytes.Buffer
	m.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if err := m.AuditTOS(); err != nil {
		t.Fatalf("AuditTOS: %v", err)
	}
	out := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte("torbox_id=1000")) {
		t.Fatalf("expected leak alarm for id 1000; logs:\n%s", out)
	}
	if bytes.Contains(buf.Bytes(), []byte("7777")) || bytes.Contains(buf.Bytes(), []byte("8888")) {
		t.Fatalf("decypharr ids must NOT be flagged; logs:\n%s", out)
	}
}

func TestAuditTOS_Clean(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	// Believed-held 1000 and the account holds exactly 1000 (in scope) + decypharr noise.
	store.addRelease(&catalog.Release{Hash: "ok", State: catalog.StateMaterialized, TorBoxID: 1000})
	tb.myList = []torbox.TorrentDetail{{ID: 1000}, {ID: 7777}}
	m, _ := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 3}})
	m.SetLogger(quietLogger())
	defer func() { _ = m.Close() }()
	if err := m.AuditTOS(); err != nil {
		t.Fatalf("AuditTOS: %v", err)
	}
}

func TestAuditTOS_APIError(t *testing.T) {
	store := newFakeStore()
	tb := &errTorBox{}
	m, _ := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 3}})
	m.SetLogger(quietLogger())
	defer func() { _ = m.Close() }()
	if err := m.AuditTOS(); err == nil {
		t.Fatalf("expected error on MyList API failure")
	}
}

// errTorBox fails MyList (and UserMe returns default) for the audit API-error path.
type errTorBox struct{ fakeTorBox }

func (e *errTorBox) MyList(offset int) ([]torbox.TorrentDetail, error) {
	return nil, errors.New("boom")
}
func (e *errTorBox) UserMe() (*torbox.Account, error) { return &torbox.Account{ActiveSlots: 3}, nil }

// --- host-pin: rejects non-tb-cdn + private/loopback BEFORE any GET ----------------------

func TestHostPin_RejectsBeforeGET(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	// Production proxy: NO test allowlist -> only *.tb-cdn.io passes.
	m, _ := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 3}})
	m.SetLogger(quietLogger())
	defer func() { _ = m.Close() }()

	bad := []string{
		"http://nexus-138.snam.tb-cdn.io/dl/x",         // http (not https)
		"https://evil.example.com/dl/x",                // wrong host
		"https://127.0.0.1/dl/x",                       // loopback literal
		"https://10.0.0.5/dl/x",                        // private literal
		"https://169.254.169.254/latest/meta",          // link-local metadata
		"https://192.168.1.10/dl/x",                    // private literal
		"https://evil.com.tb-cdn.io.attacker.net/dl/x", // suffix-trick
		"ftp://nexus.tb-cdn.io/dl/x",                   // wrong scheme
	}
	for _, raw := range bad {
		if err := m.ValidateURLForTest(raw); err == nil {
			t.Errorf("expected SSRF rejection for %q", raw)
		} else if !errors.Is(err, errSSRFBlocked) {
			t.Errorf("expected errSSRFBlocked for %q, got %v", raw, err)
		}
	}

	// A genuine *.tb-cdn.io https URL passes the static gate.
	if err := m.ValidateURLForTest("https://nexus-138.snam.tb-cdn.io/dl/x?token=abc"); err != nil {
		t.Errorf("legitimate tb-cdn URL rejected: %v", err)
	}

	// And critically: a ReadAt against a private URL must error WITHOUT the CDN ever being
	// hit. We point RequestDL at a private URL and assert no panic / clean error.
	tb.dlURLFn = func(id int64, fileID int) string { return "https://169.254.169.254/dl" }
	store.addRelease(&catalog.Release{Hash: "ssrf", Magnet: "m", State: catalog.StateVirtual, Cached: true})
	p := make([]byte, 16)
	if _, err := m.ReadAt("ssrf", 0, p, 0); err == nil {
		t.Fatalf("ReadAt to private URL should fail")
	}
}

// --- probe cache: hit avoids a second CreateTorrent (and a second read served from disk) --

func TestProbeCache_AvoidsLiveRefetch(t *testing.T) {
	dir := t.TempDir()
	content := make([]byte, 1<<20)
	for i := range content {
		content[i] = byte(i)
	}
	m, store, tb, cdn := engineWithCDN(t, content, 3, dir)
	newRelease(store, "pc", "magnet:pc")

	p := make([]byte, 4096)
	// First header read: materializes (1 add) + populates probe cache.
	if _, err := m.ReadAt("pc", 0, p, 0); err != nil {
		t.Fatalf("read1: %v", err)
	}
	hitsAfterFirst := cdn.totalHits()

	// Second header read of the same region: must be served from the probe cache (no new
	// CDN hit) and must NOT trigger a second CreateTorrent.
	q := make([]byte, 4096)
	if _, err := m.ReadAt("pc", 0, q, 0); err != nil {
		t.Fatalf("read2: %v", err)
	}
	if !bytes.Equal(q, content[:4096]) {
		t.Fatalf("probe-cache bytes mismatch")
	}
	if cdn.totalHits() != hitsAfterFirst {
		t.Fatalf("probe-cache miss: extra CDN hit (%d -> %d)", hitsAfterFirst, cdn.totalHits())
	}
	if tb.createCount() != 1 {
		t.Fatalf("probe cache should keep CreateTorrent at 1, got %d", tb.createCount())
	}
}

// --- slot budget resolution --------------------------------------------------------------

func TestSlotBudgetResolution(t *testing.T) {
	store := newFakeStore()

	// Explicit policy wins.
	tb := newFakeTorBox()
	tb.activeSlots = 9
	m, _ := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 5}})
	if m.SlotCap() != 5 {
		t.Fatalf("explicit slots: want 5, got %d", m.SlotCap())
	}
	_ = m.Close()

	// 0 => auto-detect from UserMe.
	m2, _ := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 0}})
	if m2.SlotCap() != 9 {
		t.Fatalf("auto-detect slots: want 9, got %d", m2.SlotCap())
	}
	_ = m2.Close()

	// 0 + UserMe reports 0 => DefaultActiveSlots (3).
	tb2 := newFakeTorBox() // activeSlots default 0
	m3, _ := New(Deps{Store: store, TorBox: tb2, Policy: config.Policy{ActiveSlots: 0}})
	if m3.SlotCap() != 3 {
		t.Fatalf("default slots: want 3, got %d", m3.SlotCap())
	}
	_ = m3.Close()
}
