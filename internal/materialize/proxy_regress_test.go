package materialize

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/rushp4000/lazarr/internal/config"
)

// TestReadAt_Status200IgnoredRange guards the fix for the "CDN returned 200 to a
// ranged request" case. A 200 means the body starts at byte 0; copying the first
// len(p) bytes is only correct at off==0. For off>0 the engine must FAIL rather
// than silently return the file start as if it were the requested window.
func TestReadAt_Status200IgnoredRange(t *testing.T) {
	content := make([]byte, 1<<20)
	for i := range content {
		content[i] = byte(i % 251)
	}
	// A server that ignores Range and always returns 200 + the full entity.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	store := newFakeStore()
	tb := newFakeTorBox()
	u, _ := url.Parse(srv.URL)
	tb.dlURLFn = func(_ int64, fileID int) string {
		return fmt.Sprintf("%s/dl/h/%d/f.mp4?token=x&expires=9999999999", srv.URL, fileID)
	}
	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 2}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.SetLogger(quietLogger())
	m.AllowTestHost(u.Host)
	m.AllowTestHost(u.Hostname())
	defer func() { _ = m.Close() }()
	newRelease(store, "h", "magnet:?xt=urn:btih:h")

	// off==0: a 200 is acceptable (body starts where we want); bytes must match.
	p0 := make([]byte, 4096)
	n0, err := m.ReadAt("h", 0, p0, 0)
	if err != nil {
		t.Fatalf("off=0 read should succeed on 200, got: %v", err)
	}
	if !bytes.Equal(p0[:n0], content[:n0]) {
		t.Fatalf("off=0 bytes mismatch")
	}

	// off>0: must error, NOT return content[0:4096] mislabeled as content[off:off+4096].
	p1 := make([]byte, 4096)
	if _, err := m.ReadAt("h", 0, p1, 500000); err == nil {
		t.Fatalf("off>0 read on a 200 must fail (Range ignored), but it succeeded")
	}
}

// TestProbeCache_PartialPrefixIsMiss guards the all-or-nothing hit contract at the
// probeCache layer: a window larger than the cached bytes must MISS, never be served
// short (FUSE under DIRECT_IO would forward the short count to ffprobe/Plex as a
// premature EOF). Engine-level partial prefixes can no longer occur (v1.1.5 fills the
// whole region atomically), but the contract still protects corrupt/truncated cache
// files, so it is pinned here directly.
func TestProbeCache_PartialPrefixIsMiss(t *testing.T) {
	pc, err := newProbeCache(t.TempDir(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	prefix := make([]byte, 1<<10)
	for i := range prefix {
		prefix[i] = byte(i % 251)
	}
	pc.maybeStore("dd8255ec", 0, 0, prefix) // only 1 KiB cached

	big := make([]byte, 8<<10)
	if _, ok := pc.readAt("dd8255ec", 0, big, 0); ok {
		t.Fatal("window larger than cached prefix must be a MISS, not a short hit")
	}
	// A window fully inside the prefix still hits.
	small := make([]byte, 512)
	n, ok := pc.readAt("dd8255ec", 0, small, 256)
	if !ok || n != len(small) || !bytes.Equal(small, prefix[256:256+512]) {
		t.Fatal("window inside the cached prefix must hit with exact bytes")
	}
}

// TestProbeCache_SecondLargerReadServedFromRegionFill pins the v1.1.5 engine behavior
// that replaced the old partial-prefix scenario: the FIRST header read fills the whole
// region in one GET, so a later, larger read at off 0 is a full cache hit — correct
// bytes, full window, NO additional CDN traffic.
func TestProbeCache_SecondLargerReadServedFromRegionFill(t *testing.T) {
	content := make([]byte, 64<<10)
	for i := range content {
		content[i] = byte(i % 251)
	}
	m, store, _, cdn := engineWithCDN(t, content, 2, t.TempDir())
	newRelease(store, "dd8255ec", "magnet:?xt=urn:btih:dd8255ec")

	small := make([]byte, 1<<10)
	if _, err := m.ReadAt("dd8255ec", 0, small, 0); err != nil {
		t.Fatalf("first read: %v", err)
	}
	hitsAfterFirst := cdn.totalHits()

	big := make([]byte, 8<<10)
	n, err := m.ReadAt("dd8255ec", 0, big, 0)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if n != len(big) {
		t.Fatalf("short read: got %d, want %d (premature EOF)", n, len(big))
	}
	if !bytes.Equal(big[:n], content[:n]) {
		t.Fatalf("second read bytes mismatch")
	}
	if got := cdn.totalHits(); got != hitsAfterFirst {
		t.Fatalf("second read must be served from the filled region (no new GET); hits %d -> %d", hitsAfterFirst, got)
	}
}
