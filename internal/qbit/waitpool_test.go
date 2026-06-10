package qbit_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/config"
	"github.com/rushp4000/lazarr/internal/qbit"
	"github.com/rushp4000/lazarr/internal/torbox"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newWaitEnv is newTestEnv with on_cache_miss configured.
func newWaitEnv(mode string) *testEnv {
	e := newTestEnv(false)
	e.cfg.Policy.OnCacheMiss = mode
	e.cfg.Policy.CacheWaitBudget = config.Duration(15 * time.Minute)
	e.cfg.Policy.MaxWaitDownloads = 1
	return e
}

// TestCacheMiss_Reject: the add is refused ("Fails.") so the arr instantly falls
// back to its next release candidate; nothing is stored or symlinked.
func TestCacheMiss_Reject(t *testing.T) {
	e := newWaitEnv("reject")
	body, ct := formBody("urls", magnetURI(uncachedHash, "Some+Release"), "category", "radarr_hin")
	rec := e.do("POST", "/api/v2/torrents/add", body, ct)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "Fails.", rec.Body.String())

	_, _, err := e.store.GetRelease(uncachedHash)
	assert.Error(t, err, "rejected add must not be stored")
	assert.Empty(t, e.symlink.created)
}

// TestCacheMiss_WaitHappyPath: miss -> uncached TorBox add -> StateDownloading with
// live progress in torrents/info -> poller sees it finished -> account copy released,
// grab flipped to a normal cached virtual import with symlinks.
func TestCacheMiss_WaitHappyPath(t *testing.T) {
	e := newWaitEnv("wait")
	e.torbox.createOK = true

	body, ct := formBody("urls", magnetURI(uncachedHash, "Waited+Release"), "category", "radarr_hin")
	rec := e.do("POST", "/api/v2/torrents/add", body, ct)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "Ok.", rec.Body.String())
	require.NotEmpty(t, e.torbox.createCached)
	assert.False(t, e.torbox.createCached[0], "wait mode must add WITHOUT add_only_if_cached")

	rel, _, err := e.store.GetRelease(uncachedHash)
	require.NoError(t, err)
	assert.Equal(t, catalog.StateDownloading, rel.State)
	assert.EqualValues(t, 9001, rel.TorBoxID)

	// While downloading: poller observes 40% / 60s ETA -> torrents/info shows it.
	e.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, Hash: uncachedHash, DownloadState: "downloading", Progress: 0.4, ETA: 60}
	}
	qbit.PollWaitDownloadsForTest(e.handler.(qbit.Server))

	rec = e.do("GET", "/api/v2/torrents/info?category=radarr_hin", nil, "")
	var infos []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &infos))
	require.Len(t, infos, 1)
	assert.Equal(t, "downloading", infos[0]["state"])
	assert.InDelta(t, 0.4, infos[0]["progress"], 0.001)

	// Download completes: poller must delete the account copy, re-checkcached, and
	// flip to a normal virtual import.
	e.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, Hash: uncachedHash, DownloadState: "completed", DownloadFinished: true, Progress: 1}
	}
	e.torbox.nowCached = map[string]bool{uncachedHash: true}
	qbit.PollWaitDownloadsForTest(e.handler.(qbit.Server))

	assert.Contains(t, e.torbox.deletedIDs, int64(9001), "account copy must be released on completion")
	rel, files, err := e.store.GetRelease(uncachedHash)
	require.NoError(t, err)
	assert.Equal(t, catalog.StateVirtual, rel.State)
	assert.True(t, rel.Cached)
	assert.EqualValues(t, 0, rel.TorBoxID)
	assert.NotEmpty(t, files)
	assert.Contains(t, e.symlink.created, uncachedHash)
}

// TestCacheMiss_WaitBailsWhenETAOverBudget: TorBox predicts the download cannot
// finish inside cache_wait_budget -> delete + error state.
func TestCacheMiss_WaitBailsWhenETAOverBudget(t *testing.T) {
	e := newWaitEnv("wait")
	e.torbox.createOK = true
	body, ct := formBody("urls", magnetURI(uncachedHash, "Slow+Release"), "category", "radarr_hin")
	e.do("POST", "/api/v2/torrents/add", body, ct)

	e.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, Hash: uncachedHash, DownloadState: "downloading", Progress: 0.01, ETA: 7200}
	}
	qbit.PollWaitDownloadsForTest(e.handler.(qbit.Server))

	assert.Contains(t, e.torbox.deletedIDs, int64(9001))
	rel, _, err := e.store.GetRelease(uncachedHash)
	require.NoError(t, err)
	assert.Equal(t, catalog.StateError, rel.State)
}

// TestCacheMiss_WaitCapFallsBackToError: a second miss while one wait download is
// in flight must not start another TorBox download.
func TestCacheMiss_WaitCapFallsBackToError(t *testing.T) {
	e := newWaitEnv("wait")
	e.torbox.createOK = true

	body, ct := formBody("urls", magnetURI(uncachedHash, "First"), "category", "radarr_hin")
	e.do("POST", "/api/v2/torrents/add", body, ct)

	second := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	body, ct = formBody("urls", magnetURI(second, "Second"), "category", "radarr_hin")
	e.do("POST", "/api/v2/torrents/add", body, ct)

	assert.Len(t, e.torbox.createCalls, 1, "cap=1: only the first miss may start a download")
	rel, _, err := e.store.GetRelease(second)
	require.NoError(t, err)
	assert.Equal(t, catalog.StateError, rel.State)
}
