package materialize

import (
	"bytes"
	"net/url"
	"testing"
	"time"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/config"
)

// engineWithReadahead is engineWithCDN with policy.readahead_windows enabled.
func engineWithReadahead(t *testing.T, content []byte, windows int) (*materializer, *fakeStore, *fakeCDN) {
	t.Helper()
	store := newFakeStore()
	tb := newFakeTorBox()
	cdn := newFakeCDN(content)
	tb.dlURLFn = func(id int64, fileID int) string { return cdn.url("ra", fileID) }

	pol := config.Policy{ActiveSlots: 2, ReadaheadWindows: windows}
	m, err := New(Deps{Store: store, TorBox: tb, Policy: pol})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.SetLogger(quietLogger())
	u, _ := url.Parse(cdn.srv.URL)
	m.AllowTestHost(u.Host)
	m.AllowTestHost(u.Hostname())
	t.Cleanup(func() { _ = m.Close(); cdn.close() })
	return m, store, cdn
}

// newReleaseWithFile registers a release whose catalog file size matches content, so
// the prefetcher's EOF clamp has the same view a real grab would have.
func newReleaseWithFile(s *fakeStore, hash string, size int64) {
	newRelease(s, hash, "magnet:?xt=urn:btih:"+hash)
	s.mu.Lock()
	s.files[hash] = []catalog.File{{Hash: hash, FileID: 0, RelPath: "f.mkv", Size: size}}
	s.mu.Unlock()
}

// TestReadahead_SequentialBytesCorrect reads a file sequentially in odd-sized,
// chunk-misaligned windows and asserts byte-exactness — the chunk assembly must be
// invisible to the reader.
func TestReadahead_SequentialBytesCorrect(t *testing.T) {
	content := make([]byte, 5<<20+12345) // ~5 MiB, non-aligned tail
	for i := range content {
		content[i] = byte((i * 7) % 251)
	}
	m, store, _ := engineWithReadahead(t, content, 4)
	newReleaseWithFile(store, "ra", int64(len(content)))

	pos := int64(0)
	buf := make([]byte, 700_001) // deliberately not chunk-aligned
	for pos < int64(len(content)) {
		n, err := m.ReadAt("ra", 0, buf, pos)
		if err != nil {
			t.Fatalf("ReadAt off=%d: %v", pos, err)
		}
		if n == 0 {
			break
		}
		if !bytes.Equal(buf[:n], content[pos:pos+int64(n)]) {
			t.Fatalf("bytes mismatch at off=%d len=%d", pos, n)
		}
		pos += int64(n)
	}
	if pos != int64(len(content)) {
		t.Fatalf("read %d bytes, want %d", pos, len(content))
	}
}

// TestReadahead_PrefetchesAhead asserts that a sequential pattern triggers background
// fetches beyond what the reader asked for.
func TestReadahead_PrefetchesAhead(t *testing.T) {
	content := make([]byte, 16<<20)
	m, store, cdn := engineWithReadahead(t, content, 4)
	newReleaseWithFile(store, "ra", int64(len(content)))

	buf := make([]byte, 1<<20)
	// Two sequential reads establish the pattern and schedule readahead.
	if _, err := m.ReadAt("ra", 0, buf, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadAt("ra", 0, buf, 1<<20); err != nil {
		t.Fatal(err)
	}

	// Eventually the CDN must have served more chunks than the 2 we consumed.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cdn.totalHits() >= 4 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no readahead observed: CDN hits = %d, want >= 4", cdn.totalHits())
}

// TestReadahead_CachedChunksNotRefetched: re-reading a window served moments ago must
// come from memory, not the CDN.
func TestReadahead_CachedChunksNotRefetched(t *testing.T) {
	content := make([]byte, 4<<20)
	for i := range content {
		content[i] = byte(i % 251)
	}
	m, store, cdn := engineWithReadahead(t, content, 2)
	newReleaseWithFile(store, "ra", int64(len(content)))

	// The file is exactly 4 chunks. Sequential reads + readahead must fetch each
	// chunk EXACTLY once; once all 4 are cached, any amount of re-reading must not
	// produce a single extra CDN hit.
	buf := make([]byte, 1<<20)
	if _, err := m.ReadAt("ra", 0, buf, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadAt("ra", 0, buf, 1<<20); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for cdn.totalHits() < 4 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := cdn.totalHits(); got != 4 {
		t.Fatalf("expected all 4 chunks fetched once, got %d hits", got)
	}
	for i := 0; i < 4; i++ { // re-read every window from the in-memory cache
		off := int64(i) << 20
		if _, err := m.ReadAt("ra", 0, buf, off); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf, content[off:off+int64(len(buf))]) {
			t.Fatalf("cached bytes mismatch at window %d", i)
		}
	}
	if got := cdn.totalHits(); got != 4 {
		t.Fatalf("re-reads refetched from CDN: hits = %d, want exactly 4", got)
	}
}

// TestReadahead_SeekAndTail: a random seek mid-file crossing a chunk boundary and a
// short read at EOF both return exact bytes.
func TestReadahead_SeekAndTail(t *testing.T) {
	content := make([]byte, 3<<20+777)
	for i := range content {
		content[i] = byte((i * 13) % 251)
	}
	m, store, _ := engineWithReadahead(t, content, 4)
	newReleaseWithFile(store, "ra", int64(len(content)))

	// Crosses the chunk-1/chunk-2 boundary.
	p := make([]byte, 512<<10)
	off := int64(1<<21 - 100_000)
	n, err := m.ReadAt("ra", 0, p, off)
	if err != nil {
		t.Fatalf("seek read: %v", err)
	}
	if !bytes.Equal(p[:n], content[off:off+int64(n)]) {
		t.Fatalf("seek bytes mismatch")
	}

	// Tail: request past EOF -> short read of exactly the remainder.
	tailOff := int64(len(content)) - 500
	n, err = m.ReadAt("ra", 0, p, tailOff)
	if err != nil {
		t.Fatalf("tail read: %v", err)
	}
	if n != 500 || !bytes.Equal(p[:n], content[tailOff:]) {
		t.Fatalf("tail read = %d bytes, want 500 exact", n)
	}
}
