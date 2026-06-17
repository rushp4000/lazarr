package materialize

import (
	"context"
	"sync"
)

// chunkKey identifies one cached window of one file.
type chunkKey struct {
	hash   string
	fileID int
	idx    int64 // chunk index = offset / p.chunkSize
}

// streamKey identifies one logical open stream for sequential-pattern tracking.
type streamKey struct {
	hash   string
	fileID int
}

// streamState tracks the sequential-read frontier of one stream.
type streamState struct {
	lastEnd int64 // byte offset just past the last served read
	eofIdx  int64 // first chunk index known to be at/past EOF (-1 = unknown)
	size    int64 // file size from the catalog (0 = not looked up, -1 = unknown)
}

// prefetcher implements parallel readahead over the range-proxy. Rationale: the FUSE
// layer runs with DIRECT_IO (no kernel page cache, no kernel readahead), so without
// this every 1 MiB window is one serial CDN round-trip ≈ 5-8 MB/s — not enough for
// high-bitrate 4K. With N windows prefetched in parallel a sequential reader sees
// roughly N× that. Costs: ReadaheadWindows MiB of RAM per active stream (bounded
// globally) and up to N MiB of discarded transfer when a viewer stops/seeks, which
// counts against TorBox's rolling bandwidth — keep N modest (4-8).
type prefetcher struct {
	mu        sync.Mutex
	cache     map[chunkKey][]byte
	order     []chunkKey // FIFO eviction order
	inflight  map[chunkKey]chan struct{}
	streams   map[streamKey]*streamState
	windows   int   // prefetch depth (config policy.readahead_windows)
	capacity  int   // max cached chunks (global bound)
	chunkSize int64 // bytes per readahead window (config policy.readahead_chunk_mib × 1 MiB)

	// sem caps CONCURRENT background fetches. TorBox's CDN 429s parallel bursts
	// (observed live at ~18 MB/s with unbounded parallelism); three in-flight
	// prefetches + the foreground read stay under the limit while still pipelining.
	sem chan struct{}

	// fetch retrieves one whole chunk (short at EOF). Wired to the engine's
	// proxyRead; the entry must be pinned by the caller for the fetch duration.
	fetch func(ctx context.Context, ent *entry, fileID int, buf []byte, off int64) (int, error)
}

func newPrefetcher(windows int, chunkMiB int, fetch func(ctx context.Context, ent *entry, fileID int, buf []byte, off int64) (int, error)) *prefetcher {
	conc := 3
	if windows < conc {
		conc = windows
	}
	cs := int64(chunkMiB) << 20
	return &prefetcher{
		cache:     make(map[chunkKey][]byte),
		inflight:  make(map[chunkKey]chan struct{}),
		streams:   make(map[streamKey]*streamState),
		windows:   windows,
		capacity:  windows * 8,
		chunkSize: cs,
		sem:       make(chan struct{}, conc),
		fetch:     fetch,
	}
}

// read serves p from chunk-aligned windows (cache-first), then schedules readahead
// when the access pattern is sequential. Returns bytes read (short only at EOF).
func (p *prefetcher) read(ctx context.Context, m *materializer, ent *entry, fileID int, dst []byte, off int64) (int, error) {
	sk := streamKey{ent.hash, fileID}
	want := int64(len(dst))
	n := 0

	for n < len(dst) {
		cur := off + int64(n)
		idx := cur / p.chunkSize
		inner := cur - idx*p.chunkSize

		data, err := p.getChunk(ctx, ent, fileID, idx)
		if err != nil {
			if n > 0 {
				break // partial success; surface what we have
			}
			return 0, err
		}
		if int64(len(data)) <= inner {
			break // EOF inside this chunk
		}
		c := copy(dst[n:], data[inner:])
		n += c
		if int64(len(data)) < p.chunkSize {
			p.noteEOF(sk, idx)
			break // short chunk = EOF
		}
	}

	// Sequential-pattern detection + readahead scheduling. The file size (from the
	// catalog) caps the readahead frontier so a chunk-aligned file never triggers a
	// probe past EOF (which would be a wasted CDN request every time).
	p.mu.Lock()
	st, ok := p.streams[sk]
	if !ok {
		st = &streamState{eofIdx: -1}
		p.streams[sk] = st
	}
	needSize := st.size == 0
	p.mu.Unlock()
	if needSize {
		sz := fileSizeFromStore(m, sk.hash, sk.fileID)
		p.mu.Lock()
		if st.size == 0 {
			st.size = sz
		}
		p.mu.Unlock()
	}

	p.mu.Lock()
	sequential := ok && off >= st.lastEnd-4*p.chunkSize && off <= st.lastEnd+p.chunkSize
	st.lastEnd = off + int64(n)
	eofIdx := st.eofIdx
	if st.size > 0 {
		if lastChunk := (st.size - 1) / p.chunkSize; eofIdx < 0 || lastChunk+1 < eofIdx {
			eofIdx = lastChunk + 1
		}
	}
	p.mu.Unlock()

	if sequential && p.windows > 0 && n > 0 {
		lastIdx := (off + want - 1) / p.chunkSize
		for i := int64(1); i <= int64(p.windows); i++ {
			idx := lastIdx + i
			if eofIdx >= 0 && idx >= eofIdx {
				break
			}
			p.prefetchAsync(m, ent.hash, fileID, idx)
		}
	}
	return n, nil
}

// fileSizeFromStore looks up the per-file size in the catalog. Returns -1 when it
// cannot be determined (readahead then falls back to short-chunk EOF detection).
func fileSizeFromStore(m *materializer, hash string, fileID int) int64 {
	_, files, err := m.store.GetRelease(hash)
	if err != nil {
		return -1
	}
	for _, f := range files {
		if f.FileID == fileID {
			return f.Size
		}
	}
	return -1
}

// getChunk returns the chunk's bytes from cache, an in-flight fetch, or a live fetch.
// The returned slice is shared — callers must only read it.
func (p *prefetcher) getChunk(ctx context.Context, ent *entry, fileID int, idx int64) ([]byte, error) {
	key := chunkKey{ent.hash, fileID, idx}
	for {
		p.mu.Lock()
		if data, ok := p.cache[key]; ok {
			p.mu.Unlock()
			return data, nil
		}
		if ch, ok := p.inflight[key]; ok {
			p.mu.Unlock()
			select {
			case <-ch:
				continue // re-check cache (fetch may have failed -> we retry ourselves)
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		ch := make(chan struct{})
		p.inflight[key] = ch
		p.mu.Unlock()

		buf := make([]byte, p.chunkSize)
		n, err := p.fetch(ctx, ent, fileID, buf, idx*p.chunkSize)

		p.mu.Lock()
		delete(p.inflight, key)
		close(ch)
		if err != nil {
			p.mu.Unlock()
			return nil, err
		}
		p.store(key, buf[:n])
		data := p.cache[key]
		p.mu.Unlock()
		return data, nil
	}
}

// prefetchAsync fetches a chunk in the background (dedupes against cache/in-flight).
// The entry is re-pinned for the fetch so a concurrent release can't yank the
// TorBox item from under the read.
func (p *prefetcher) prefetchAsync(m *materializer, hash string, fileID int, idx int64) {
	// CDN-throttle breaker: while a 429 window is open, schedule NOTHING speculative.
	// Foreground reads keep going (the player needs them); readahead resumes when the
	// window elapses. Without this, prefetch competes with the blocked foreground read
	// for the same per-IP budget and prolongs the throttle.
	if m.throttledNow() {
		return
	}
	key := chunkKey{hash, fileID, idx}
	p.mu.Lock()
	if _, ok := p.cache[key]; ok {
		p.mu.Unlock()
		return
	}
	if _, ok := p.inflight[key]; ok {
		p.mu.Unlock()
		return
	}
	ch := make(chan struct{})
	p.inflight[key] = ch
	p.mu.Unlock()

	go func() {
		defer func() {
			p.mu.Lock()
			delete(p.inflight, key)
			close(ch)
			p.mu.Unlock()
		}()
		p.sem <- struct{}{} // concurrency cap (CDN 429s parallel bursts)
		defer func() { <-p.sem }()
		ctx := context.Background()
		ent, err := m.ensureMaterialized(ctx, hash)
		if err != nil {
			return // best-effort: the foreground read path reports real errors
		}
		defer m.unpin(hash)
		buf := make([]byte, p.chunkSize)
		n, err := p.fetch(ctx, ent, fileID, buf, idx*p.chunkSize)
		if err != nil || n == 0 {
			if err == nil {
				p.noteEOF(streamKey{hash, fileID}, idx)
			}
			return
		}
		p.mu.Lock()
		p.store(key, buf[:n])
		p.mu.Unlock()
		if int64(n) < p.chunkSize {
			p.noteEOF(streamKey{hash, fileID}, idx+1)
		}
	}()
}

// store inserts a chunk and FIFO-evicts past capacity. Caller holds p.mu.
func (p *prefetcher) store(key chunkKey, data []byte) {
	if _, ok := p.cache[key]; ok {
		return
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	p.cache[key] = cp
	p.order = append(p.order, key)
	for len(p.order) > p.capacity {
		old := p.order[0]
		p.order = p.order[1:]
		delete(p.cache, old)
	}
}

// noteEOF records the first chunk index at/past EOF so readahead stops there.
func (p *prefetcher) noteEOF(sk streamKey, idx int64) {
	p.mu.Lock()
	if st, ok := p.streams[sk]; ok {
		if st.eofIdx < 0 || idx < st.eofIdx {
			st.eofIdx = idx
		}
	} else {
		p.streams[sk] = &streamState{eofIdx: idx}
	}
	p.mu.Unlock()
}

// invalidate drops all cached chunks + stream state for a hash (called on release,
// purge, and Close so memory is returned promptly).
func (p *prefetcher) invalidate(hash string) {
	p.mu.Lock()
	kept := p.order[:0]
	for _, k := range p.order {
		if k.hash == hash {
			delete(p.cache, k)
		} else {
			kept = append(kept, k)
		}
	}
	p.order = kept
	for sk := range p.streams {
		if sk.hash == hash {
			delete(p.streams, sk)
		}
	}
	p.mu.Unlock()
}
