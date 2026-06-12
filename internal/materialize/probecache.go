package materialize

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Probe-cache defaults (docs/05 §6). The header region is the first few MiB that ffprobe /
// Plex read; caching it on disk lets a freshly imported item's metadata scan be served
// locally so it does NOT trigger a second TorBox CreateTorrent against the ~55/hr budget.
const (
	defaultProbeRegionBytes = int64(4 << 20)   // 4 MiB header region per file
	defaultProbeCacheBytes  = int64(512 << 20) // 512 MiB total on-disk budget (bounded)

	// Footer region bounds (riven lesson): MKV cues and MP4 moov atoms live at the END
	// of the file, so every playback start and every ffprobe reads the tail too. The
	// region scales with file size (cue size grows with duration), clamped to sane bounds.
	probeFooterMin   = int64(1 << 20) // 1 MiB
	probeFooterMax   = int64(8 << 20) // 8 MiB
	probeFooterRatio = int64(500)     // region = size/500 = 0.2% of the file
)

// footerRegionFor sizes the cached tail region for a file. 0 when size is unknown.
func footerRegionFor(size int64) int64 {
	if size <= 0 {
		return 0
	}
	r := size / probeFooterRatio
	if r < probeFooterMin {
		r = probeFooterMin
	}
	if r > probeFooterMax {
		r = probeFooterMax
	}
	if r > size {
		r = size
	}
	return r
}

// probeCache is a bounded on-disk cache of file-header regions, keyed by (hash,fileID).
// Reads that fall entirely within [0, region) for a cached key are served from disk.
// Total size is bounded; the oldest (least-recently inserted) entries are evicted.
//
// All exported methods are safe for concurrent use and degrade gracefully on I/O errors
// (the engine falls back to a live proxy read).
type probeCache struct {
	dir        string
	region     int64 // header region size captured per file
	maxBytes   int64 // total on-disk budget
	mu         sync.Mutex
	order      []string         // insertion order of keys, for eviction
	sizes      map[string]int64 // key -> bytes on disk
	footStarts map[string]int64 // footer key -> absolute start offset of the cached tail
	totalBytes int64
}

// newProbeCache creates (and validates writability of) the cache directory.
func newProbeCache(dir string, maxBytes, region int64) (*probeCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("probe cache: mkdir: %w", err)
	}
	// Writability probe.
	probe := filepath.Join(dir, ".writable")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return nil, fmt.Errorf("probe cache: not writable: %w", err)
	}
	_ = os.Remove(probe)

	if region <= 0 {
		region = defaultProbeRegionBytes
	}
	if maxBytes <= 0 {
		maxBytes = defaultProbeCacheBytes
	}
	return &probeCache{
		dir:        dir,
		region:     region,
		maxBytes:   maxBytes,
		sizes:      make(map[string]int64),
		footStarts: make(map[string]int64),
	}, nil
}

// covers reports whether a read at [off, off+length) lies entirely within the header region
// and is therefore a candidate for the probe cache.
func (c *probeCache) covers(off, length int64) bool {
	return off >= 0 && length >= 0 && off+length <= c.region
}

// key builds the on-disk filename for a (hash,fileID). hash is already a validated infohash
// (40 hex / 32 base32 by Phase-1's qbit layer); fileID is an int. We still sanitize to keep
// the path strictly within dir (defense in depth — no separators, no traversal).
func (c *probeCache) key(hash string, fileID int) string {
	safe := make([]rune, 0, len(hash))
	for _, r := range hash {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			safe = append(safe, r)
		default:
			safe = append(safe, '_')
		}
	}
	return fmt.Sprintf("%s.%d", string(safe), fileID)
}

// readAt serves a header-region read from disk if present. A hit is all-or-nothing:
// it returns (len(p), true) ONLY when the cached prefix fully covers [off, off+len(p)).
// A partial cached prefix is a MISS (returns false) so the caller does a full live read
// instead — a short count here would be forwarded to FUSE under FOPEN_DIRECT_IO and read
// by ffprobe/Plex as a premature EOF, truncating the very header scan the cache serves.
func (c *probeCache) readAt(hash string, fileID int, p []byte, off int64) (int, bool) {
	k := c.key(hash, fileID)

	c.mu.Lock()
	sz, ok := c.sizes[k]
	c.mu.Unlock()
	// Require the cached region to fully cover the requested window.
	if !ok || off < 0 || off+int64(len(p)) > sz {
		return 0, false
	}

	f, err := os.Open(filepath.Join(c.dir, k))
	if err != nil {
		return 0, false
	}
	defer f.Close()

	n, err := f.ReadAt(p, off)
	if err != nil || n < len(p) {
		// Incomplete read (truncated/corrupt cache file) -> miss; fall through to live.
		return 0, false
	}
	return n, true
}

// maybeStore writes the header region for a key once, bounded by the region size. body is
// the bytes read starting at off; we only persist when off==0 (the true header start) to
// keep the cache file a contiguous [0,region) prefix. Best-effort: errors are swallowed.
func (c *probeCache) maybeStore(hash string, fileID int, off int64, body []byte) {
	if off != 0 || len(body) == 0 {
		return
	}
	k := c.key(hash, fileID)

	c.mu.Lock()
	if _, exists := c.sizes[k]; exists {
		c.mu.Unlock()
		return // already cached -> never re-add (avoids churn)
	}
	c.mu.Unlock()

	n := int64(len(body))
	if n > c.region {
		n = c.region
		body = body[:n]
	}

	tmp := filepath.Join(c.dir, k+".tmp")
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return
	}
	final := filepath.Join(c.dir, k)
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return
	}

	c.mu.Lock()
	c.sizes[k] = n
	c.order = append(c.order, k)
	c.totalBytes += n
	c.evictLocked()
	c.mu.Unlock()
}

// footerKey is the on-disk filename for a (hash,fileID) tail region. The ".f" suffix
// cannot collide with header keys: header keys end in the numeric fileID.
func (c *probeCache) footerKey(hash string, fileID int) string {
	return c.key(hash, fileID) + ".f"
}

// coversFooter reports whether a read at [off, off+length) lies entirely within the
// file's footer region [size-footerRegionFor(size), size). size==0 (unknown) never covers.
func (c *probeCache) coversFooter(size, off, length int64) bool {
	r := footerRegionFor(size)
	return r > 0 && off >= size-r && length >= 0 && off+length <= size
}

// readAtFooter serves a footer-region read from disk if present. Same all-or-nothing
// contract as readAt: a window not fully covered by the cached tail is a MISS.
func (c *probeCache) readAtFooter(hash string, fileID int, p []byte, off int64) (int, bool) {
	k := c.footerKey(hash, fileID)

	c.mu.Lock()
	sz, ok := c.sizes[k]
	start, sok := c.footStarts[k]
	c.mu.Unlock()
	if !ok || !sok || off < start || off+int64(len(p)) > start+sz {
		return 0, false
	}

	f, err := os.Open(filepath.Join(c.dir, k))
	if err != nil {
		return 0, false
	}
	defer f.Close()

	n, err := f.ReadAt(p, off-start)
	if err != nil || n < len(p) {
		return 0, false
	}
	return n, true
}

// storeFooter persists a file's tail region once. start is the absolute offset of body[0]
// (= size - footerRegionFor(size), computed by the engine, which fetches the whole region
// in one GET on the first footer miss). Best-effort, same contract as maybeStore.
func (c *probeCache) storeFooter(hash string, fileID int, start int64, body []byte) {
	if start < 0 || len(body) == 0 {
		return
	}
	k := c.footerKey(hash, fileID)

	c.mu.Lock()
	if _, exists := c.sizes[k]; exists {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	tmp := filepath.Join(c.dir, k+".tmp")
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return
	}
	final := filepath.Join(c.dir, k)
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return
	}

	c.mu.Lock()
	c.sizes[k] = int64(len(body))
	c.footStarts[k] = start
	c.order = append(c.order, k)
	c.totalBytes += int64(len(body))
	c.evictLocked()
	c.mu.Unlock()
}

// evictLocked drops oldest entries until total size is within budget. Caller holds c.mu.
func (c *probeCache) evictLocked() {
	for c.totalBytes > c.maxBytes && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		if sz, ok := c.sizes[oldest]; ok {
			c.totalBytes -= sz
			delete(c.sizes, oldest)
			delete(c.footStarts, oldest)
			_ = os.Remove(filepath.Join(c.dir, oldest))
		}
	}
}

// close is a no-op placeholder for symmetry (files persist; dir is bounded).
func (c *probeCache) close() {}
