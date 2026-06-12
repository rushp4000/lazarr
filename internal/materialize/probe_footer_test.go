package materialize

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rushp4000/lazarr/internal/config"
	"github.com/rushp4000/lazarr/internal/version"
)

// TestReadAt_ProbeHitServesVirtual_NoAdd guards the v1.1.4 reorder: the probe cache is
// consulted BEFORE materializing, so a metadata scan of a RELEASED (virtual) item is
// served from disk with zero TorBox adds. Pre-1.1.4 the lookup sat after the materialize
// call and every scan of a released item burned a createtorrent against the hourly budget.
func TestReadAt_ProbeHitServesVirtual_NoAdd(t *testing.T) {
	content := make([]byte, 64<<10)
	for i := range content {
		content[i] = byte(i % 241)
	}
	m, store, tb, _ := engineWithCDN(t, content, 2, t.TempDir())
	newReleaseWithFile(store, "h", int64(len(content)))

	// First header read: materializes (1 add) and populates the header cache (off==0).
	p := make([]byte, 8<<10)
	if _, err := m.ReadAt("h", 0, p, 0); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if got := tb.createCount(); got != 1 {
		t.Fatalf("first read should materialize exactly once, got %d adds", got)
	}

	// Idle-release the item: back to virtual, account copy gone.
	if err := m.Release("h"); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Re-scan the header: must be served from the probe cache with NO new add.
	q := make([]byte, 8<<10)
	n, err := m.ReadAt("h", 0, q, 0)
	if err != nil {
		t.Fatalf("post-release header read: %v", err)
	}
	if !bytes.Equal(q[:n], content[:n]) {
		t.Fatal("probe-cache bytes mismatch")
	}
	if got := tb.createCount(); got != 1 {
		t.Fatalf("post-release header scan must not re-materialize; adds = %d", got)
	}
	if m.IsTracked("h") {
		t.Fatal("release must stay virtual after a probe-cache hit")
	}
}

// TestReadAt_FooterCachedAndServedAfterRelease guards the footer probe region (riven
// lesson: MKV cues / MP4 moov live at EOF, so players and ffprobe always read the tail).
// The first tail read fetches the whole footer region in ONE ranged GET and caches it;
// after release, tail reads are served from cache with no re-materialize.
func TestReadAt_FooterCachedAndServedAfterRelease(t *testing.T) {
	content := make([]byte, 6<<20) // > header region (4 MiB) so the footer is distinct
	for i := range content {
		content[i] = byte((i * 7) % 251)
	}
	size := int64(len(content))
	m, store, tb, _ := engineWithCDN(t, content, 2, t.TempDir())
	newReleaseWithFile(store, "h", size)

	// Tail read (last 4 KiB) -> materialize once + whole-region footer fill.
	p := make([]byte, 4<<10)
	off := size - int64(len(p))
	n, err := m.ReadAt("h", 0, p, off)
	if err != nil {
		t.Fatalf("first tail read: %v", err)
	}
	if !bytes.Equal(p[:n], content[off:off+int64(n)]) {
		t.Fatal("footer fill bytes mismatch")
	}
	if got := tb.createCount(); got != 1 {
		t.Fatalf("expected exactly 1 add, got %d", got)
	}

	if err := m.Release("h"); err != nil {
		t.Fatalf("release: %v", err)
	}

	// A DIFFERENT window inside the cached footer region must hit the cache, virtual.
	q := make([]byte, 8<<10)
	off2 := size - footerRegionFor(size) + 512 // near the region start, not the same window
	n2, err := m.ReadAt("h", 0, q, off2)
	if err != nil {
		t.Fatalf("post-release footer read: %v", err)
	}
	if !bytes.Equal(q[:n2], content[off2:off2+int64(n2)]) {
		t.Fatal("cached footer bytes mismatch")
	}
	if got := tb.createCount(); got != 1 {
		t.Fatalf("post-release footer scan must not re-materialize; adds = %d", got)
	}
}

// TestFooterRegionFor covers the clamp behavior.
func TestFooterRegionFor(t *testing.T) {
	cases := []struct {
		size, want int64
	}{
		{0, 0},
		{-1, 0},
		{512 << 10, 512 << 10},       // smaller than min -> whole file
		{6 << 20, 1 << 20},           // 0.2% tiny -> min clamp 1 MiB
		{10 << 30, 8 << 20},          // 10 GiB * 0.2% = 20 MiB -> max clamp 8 MiB
		{1 << 30, 1073741824 / 500},  // 1 GiB -> ~2.1 MiB (within clamps)
	}
	for _, c := range cases {
		if got := footerRegionFor(c.size); got != c.want {
			t.Errorf("footerRegionFor(%d) = %d, want %d", c.size, got, c.want)
		}
	}
}

// TestReadAt_416IsEOF guards the riven lesson: 416 Range Not Satisfiable = reads at/past
// the entity's true end (catalog size drift) = clean EOF, never EIO.
func TestReadAt_416IsEOF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	defer srv.Close()

	store := newFakeStore()
	tb := newFakeTorBox()
	tb.dlURLFn = func(_ int64, _ int) string { return srv.URL + "/dl/f.mkv?token=x" }
	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 2}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetLogger(quietLogger())
	u, _ := url.Parse(srv.URL)
	m.AllowTestHost(u.Host)
	m.AllowTestHost(u.Hostname())
	defer func() { _ = m.Close() }()
	newRelease(store, "h", "magnet:?xt=urn:btih:h")

	p := make([]byte, 4096)
	n, err := m.ReadAt("h", 0, p, 1<<40) // way past EOF
	if err != nil {
		t.Fatalf("416 must be EOF (nil error), got: %v", err)
	}
	if n != 0 {
		t.Fatalf("416 must yield 0 bytes, got %d", n)
	}
}

// TestStallGuard_AbortsTrickleAndRefreshes guards the min-throughput stall guard: a node
// that returns 206 then trickles must be aborted within the stall budget and recovered
// via the existing refresh/re-pin path — not tolerated for the full 5-minute timeout.
// It also asserts the CDN request carries Lazarr's identifying User-Agent.
func TestStallGuard_AbortsTrickleAndRefreshes(t *testing.T) {
	old := stallFloor
	stallFloor = 300 * time.Millisecond
	t.Cleanup(func() { stallFloor = old })

	content := make([]byte, 32<<10)
	for i := range content {
		content[i] = byte(i % 233)
	}
	var calls atomic.Int32
	var sawUA atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUA.Store(r.Header.Get("User-Agent"))
		if calls.Add(1) == 1 {
			// Sick node: valid 206 headers, then a stalled body.
			w.Header().Set("Content-Range", "bytes 0-4095/32768")
			w.WriteHeader(http.StatusPartialContent)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(1500 * time.Millisecond) // > stallFloor; body never arrives in time
			return
		}
		var a, b int64
		_, _ = fmtSscanfRange(r.Header.Get("Range"), &a, &b)
		if b >= int64(len(content)) {
			b = int64(len(content)) - 1
		}
		w.Header().Set("Content-Range", "bytes 0-4095/32768")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content[a : b+1])
	}))
	defer srv.Close()

	store := newFakeStore()
	tb := newFakeTorBox()
	tb.dlURLFn = func(_ int64, _ int) string { return srv.URL + "/dl/f.mkv?token=x" }
	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 2}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetLogger(quietLogger())
	u, _ := url.Parse(srv.URL)
	m.AllowTestHost(u.Host)
	m.AllowTestHost(u.Hostname())
	defer func() { _ = m.Close() }()
	newRelease(store, "h", "magnet:?xt=urn:btih:h")

	p := make([]byte, 4096)
	n, err := m.ReadAt("h", 0, p, 0)
	if err != nil {
		t.Fatalf("stalled node must be aborted + refreshed + retried, got: %v", err)
	}
	if !bytes.Equal(p[:n], content[:n]) {
		t.Fatal("bytes mismatch after stall recovery")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 CDN GETs (stall then success), got %d", got)
	}
	if got := tb.requestDLCount(); got != 2 {
		t.Fatalf("stall must trigger exactly one link refresh; RequestDL = %d", got)
	}
	ua, _ := sawUA.Load().(string)
	if !strings.HasPrefix(ua, "Lazarr/") {
		t.Fatalf("CDN request User-Agent = %q, want Lazarr/...", ua)
	}
}

// TestUserAgentFormat pins the UA shape used on API + CDN requests.
func TestUserAgentFormat(t *testing.T) {
	ua := version.UserAgent()
	if !strings.HasPrefix(ua, "Lazarr/") || !strings.Contains(ua, "(") {
		t.Fatalf("unexpected UserAgent format: %q", ua)
	}
}
