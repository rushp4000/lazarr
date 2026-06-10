package qbit

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/rushp4000/lazarr/internal/catalog"
)

// waitPollInterval is how often the wait-poller checks TorBox's download progress
// for on_cache_miss=wait grabs. One MyListByID call per in-flight download per tick.
const waitPollInterval = 20 * time.Second

// waitProgress is the in-memory progress view for one downloading release. It is
// deliberately NOT persisted: after a restart the poller re-learns it on the first
// tick from TorBox (the catalog row in StateDownloading is the durable part).
type waitProgress struct {
	Progress float64 // 0..1
	ETA      int64   // seconds, 0 = unknown
}

// waitPool tracks on_cache_miss=wait downloads: TorBox is fetching an uncached
// torrent; we report real progress to the arr and either complete the grab when the
// content lands in TorBox's cache or bail (delete) when the ETA exceeds the budget.
//
// ToS note: a waiting download legitimately sits on the account while TorBox fetches
// it. The audit counts StateDownloading rows as believed-held, and the poller ALWAYS
// removes the torrent when done — completion flips the grab back to a normal lazy
// (released) import, because finishing the download is exactly what populates
// TorBox's cache.
type waitPool struct {
	mu   sync.Mutex
	prog map[string]waitProgress // hash -> live progress
}

func newWaitPool() *waitPool {
	return &waitPool{prog: make(map[string]waitProgress)}
}

func (w *waitPool) get(hash string) (waitProgress, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.prog[hash]
	return p, ok
}

func (w *waitPool) set(hash string, p waitProgress) {
	w.mu.Lock()
	w.prog[hash] = p
	w.mu.Unlock()
}

func (w *waitPool) drop(hash string) {
	w.mu.Lock()
	delete(w.prog, hash)
	w.mu.Unlock()
}

// startWaitDownload begins an on_cache_miss=wait grab: add the uncached torrent to
// TorBox (it starts downloading server-side) and store the release as
// StateDownloading. Returns false when the concurrent-wait cap is reached or the add
// fails — the caller falls back to error state.
func (s *server) startWaitDownload(rel *catalog.Release) bool {
	rows, err := s.deps.Store.DownloadingReleases()
	if err != nil {
		slog.Warn("qbit: wait start: list downloading", "err", err)
		return false
	}
	max := s.deps.Config.Policy.MaxWaitDownloads
	if max <= 0 {
		max = 1
	}
	if len(rows) >= max {
		slog.Info("qbit: wait cap reached, not starting another download",
			"hash", rel.Hash, "in_flight", len(rows), "max", max)
		return false
	}

	id, _, err := s.deps.TorBox.CreateTorrent(rel.Magnet, false /* uncached add */)
	if err != nil {
		slog.Warn("qbit: wait start: createtorrent", "hash", rel.Hash, "err", err)
		return false
	}
	rel.State = catalog.StateDownloading
	rel.TorBoxID = id
	rel.MaterializedAt = time.Now().Unix() // on-account-since; budget measured from here
	s.waits.set(rel.Hash, waitProgress{})
	slog.Info("qbit: cache miss → TorBox downloading (on_cache_miss=wait)",
		"hash", rel.Hash, "name", rel.Name, "torbox_id", id,
		"budget", s.deps.Config.Policy.CacheWaitBudget.D())
	return true
}

// StartWaitPoller launches the background loop that watches StateDownloading
// releases. Safe to call regardless of mode: with nothing downloading, a tick is one
// cheap store query. Stops when ctx is canceled.
func (s *server) StartWaitPoller(ctx context.Context) {
	go func() {
		t := time.NewTicker(waitPollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.pollWaitDownloads()
			}
		}
	}()
}

// pollWaitDownloads advances every in-flight wait download by one observation.
func (s *server) pollWaitDownloads() {
	rows, err := s.deps.Store.DownloadingReleases()
	if err != nil {
		slog.Warn("qbit: wait-poll: list downloading", "err", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	budget := s.deps.Config.Policy.CacheWaitBudget.D()
	now := time.Now()

	for _, rel := range rows {
		detail, err := s.deps.TorBox.MyListByID(rel.TorBoxID)
		if err != nil {
			slog.Warn("qbit: wait-poll: mylist byid", "hash", rel.Hash, "torbox_id", rel.TorBoxID, "err", err)
			continue // transient; try again next tick (budget still applies)
		}
		if detail == nil {
			// Deleted out-of-band (user, TorBox). Nothing to release; surface as error
			// so the arr/Cleanuparr can re-search.
			slog.Warn("qbit: wait download vanished from account", "hash", rel.Hash, "torbox_id", rel.TorBoxID)
			s.failWaitDownload(rel, "vanished")
			continue
		}

		if detail.DownloadFinished || detail.DownloadState == "cached" ||
			detail.DownloadState == "completed" || detail.DownloadState == "uploading" {
			s.finishWaitDownload(rel)
			continue
		}

		// Still downloading: refresh the live progress for torrents/info.
		s.waits.set(rel.Hash, waitProgress{Progress: detail.Progress, ETA: detail.ETA})

		// Budget enforcement, measured from when the download started
		// (MaterializedAt doubles as on-account-since for downloading rows):
		//  - hard: the budget window has fully elapsed;
		//  - predictive: TorBox's ETA says it cannot finish inside the window.
		deadline := time.Unix(rel.MaterializedAt, 0).Add(budget)
		switch {
		case now.After(deadline):
			slog.Info("qbit: wait download over budget, bailing",
				"hash", rel.Hash, "progress", detail.Progress, "budget", budget)
			s.bailWaitDownload(rel, "budget elapsed")
		case detail.ETA > 0 && now.Add(time.Duration(detail.ETA)*time.Second).After(deadline):
			slog.Info("qbit: wait download ETA exceeds budget, bailing",
				"hash", rel.Hash, "eta_s", detail.ETA, "budget", budget)
			s.bailWaitDownload(rel, "eta over budget")
		}
	}
}

// finishWaitDownload completes a wait grab: the content is now in TorBox's cache, so
// release the account copy and flip the grab into the normal lazy-virtual shape
// (checkcached file list -> symlinks -> StateVirtual). Playback later re-adds it
// instantly, exactly like any cached grab.
func (s *server) finishWaitDownload(rel *catalog.Release) {
	if err := s.deps.TorBox.ControlDelete(rel.TorBoxID); err != nil {
		// Keep state=downloading and retry next tick — never leave the account copy
		// behind silently (the audit counts downloading as held, so no false leak).
		slog.Warn("qbit: wait finish: release account copy failed (will retry)",
			"hash", rel.Hash, "torbox_id", rel.TorBoxID, "err", err)
		return
	}

	cachedMap, err := s.deps.TorBox.CheckCached([]string{rel.Hash})
	item, ok := cachedMap[rel.Hash]
	if err != nil || !ok || len(item.Files) == 0 {
		// Downloaded but checkcached can't see it yet — flip to error only after the
		// budget window; before that, retry (cache index can lag).
		slog.Warn("qbit: wait finish: checkcached after download empty",
			"hash", rel.Hash, "err", err)
		s.failWaitDownload(rel, "not cached after download")
		return
	}

	rel.Name = item.Name
	rel.TotalSize = item.Size
	rel.Cached = true
	rel.State = catalog.StateVirtual
	rel.TorBoxID = 0
	rel.MaterializedAt = 0
	files := toCatalogFiles(rel.Hash, item.Files)
	if err := s.deps.Store.UpsertRelease(rel, files); err != nil {
		slog.Error("qbit: wait finish: upsert", "hash", rel.Hash, "err", err)
		return
	}
	if err := s.deps.Symlink.Create(rel, files); err != nil {
		slog.Warn("qbit: wait finish: symlink", "hash", rel.Hash, "err", err)
	}
	s.waits.drop(rel.Hash)
	slog.Info("qbit: wait download complete → cached, released, import ready",
		"hash", rel.Hash, "name", rel.Name, "size", rel.TotalSize, "files", len(files))
}

// bailWaitDownload aborts a wait download over budget: delete from the account and
// surface the grab as an error.
func (s *server) bailWaitDownload(rel *catalog.Release, reason string) {
	if err := s.deps.TorBox.ControlDelete(rel.TorBoxID); err != nil {
		slog.Warn("qbit: wait bail: delete failed (will retry next tick)",
			"hash", rel.Hash, "torbox_id", rel.TorBoxID, "err", err)
		return // stay in downloading; retried next tick
	}
	s.failWaitDownload(rel, reason)
}

// failWaitDownload marks the grab errored (no account copy remains).
func (s *server) failWaitDownload(rel *catalog.Release, reason string) {
	if err := s.deps.Store.SetState(rel.Hash, catalog.StateError, 0); err != nil {
		slog.Error("qbit: wait fail: set state", "hash", rel.Hash, "err", err)
	}
	s.waits.drop(rel.Hash)
	slog.Info("qbit: wait download failed", "hash", rel.Hash, "reason", reason)
}
