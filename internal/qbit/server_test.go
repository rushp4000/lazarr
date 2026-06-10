package qbit_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/config"
	"github.com/rushp4000/lazarr/internal/qbit"
	"github.com/rushp4000/lazarr/internal/torbox"
)

// ── Test hashes (constants from testdata shapes) ──────────────────────────────

const (
	cachedHash   = "dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c" // cache hit
	uncachedHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // cache miss
	torrentInfoH = "08ada5a7a6183aae1e09d831df6748d566095a10" // torrentinfo fallback
)

// ── Fakes ────────────────────────────────────────────────────────────────────

// fakeStore implements catalog.Store in memory.
type fakeStore struct {
	releases map[string]*catalog.Release
	files    map[string][]catalog.File
	deleted  []string
	upserted []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		releases: make(map[string]*catalog.Release),
		files:    make(map[string][]catalog.File),
	}
}

func (f *fakeStore) UpsertRelease(r *catalog.Release, files []catalog.File) error {
	f.upserted = append(f.upserted, r.Hash)
	cp := *r
	f.releases[r.Hash] = &cp
	f.files[r.Hash] = append([]catalog.File(nil), files...)
	return nil
}

func (f *fakeStore) GetRelease(hash string) (*catalog.Release, []catalog.File, error) {
	r, ok := f.releases[hash]
	if !ok {
		return nil, nil, fmt.Errorf("not found: %s", hash)
	}
	return r, f.files[hash], nil
}

func (f *fakeStore) ListByCategory(cat string) ([]*catalog.Release, error) {
	var out []*catalog.Release
	for _, r := range f.releases {
		if r.Category == cat {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStore) SetState(hash string, st catalog.State, id int64) error {
	if r, ok := f.releases[hash]; ok {
		r.State = st
		r.TorBoxID = id
	}
	return nil
}

func (f *fakeStore) TouchAccess(hash string, ts int64) error {
	if r, ok := f.releases[hash]; ok {
		r.LastAccess = ts
	}
	return nil
}

func (f *fakeStore) IdleCandidates(before int64) ([]*catalog.Release, error) { return nil, nil }
func (f *fakeStore) OverMaxHold(before int64) ([]*catalog.Release, error)    { return nil, nil }
func (f *fakeStore) ListReleases(_ catalog.ReleaseFilter) ([]*catalog.Release, int, error) {
	return nil, 0, nil
}
func (f *fakeStore) MaterializedIDs() ([]int64, error)                 { return nil, nil }
func (f *fakeStore) MaterializedReleases() ([]*catalog.Release, error) { return nil, nil }
func (f *fakeStore) DownloadingReleases() ([]*catalog.Release, error) {
	var out []*catalog.Release
	for _, r := range f.releases {
		if r.State == catalog.StateDownloading {
			cp := *r
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (f *fakeStore) GetLink(hash string, fileID int) (*catalog.DLLink, error) { return nil, nil }
func (f *fakeStore) SetLink(l *catalog.DLLink) error                          { return nil }

func (f *fakeStore) DeleteRelease(hash string) error {
	f.deleted = append(f.deleted, hash)
	delete(f.releases, hash)
	delete(f.files, hash)
	return nil
}

func (f *fakeStore) Close() error                                                  { return nil }
func (f *fakeStore) ListAllHashes() ([]string, error)                              { return nil, nil }
func (f *fakeStore) SetCacheStatus(_ string, _ catalog.CacheStatus, _ int64) error { return nil }
func (f *fakeStore) ListEvicted() ([]*catalog.Release, error)                      { return nil, nil }

// fakeTorBox implements torbox.Client with canned responses.
type fakeTorBox struct {
	checkCachedCalls [][]string
	torrentInfoCalls []string
	createCalls      []string

	// wait-mode seams
	createOK     bool                                 // CreateTorrent succeeds with id 9001
	createCached []bool                               // recorded addOnlyIfCached args
	nowCached    map[string]bool                      // extra CheckCached hits (post-download)
	myListByIDFn func(id int64) *torbox.TorrentDetail // poller progress responses
	deletedIDs   []int64                              // ControlDelete calls
}

func (f *fakeTorBox) CheckCached(hashes []string) (map[string]torbox.CachedItem, error) {
	f.checkCachedCalls = append(f.checkCachedCalls, hashes)
	result := make(map[string]torbox.CachedItem)
	for _, h := range hashes {
		switch h {
		case cachedHash:
			result[h] = torbox.CachedItem{
				Hash: h,
				Name: "Big Buck Bunny (2008) [1080p]",
				Size: 691087437,
				Files: []torbox.CachedFile{
					{ID: 0, Name: "Big Buck Bunny (2008) [1080p]/Big.Buck.Bunny.2008.1080p.mp4", Size: 691069000},
					{ID: 1, Name: "Big Buck Bunny (2008) [1080p]/Big.Buck.Bunny.2008.1080p.en.srt", Size: 18308},
					{ID: 2, Name: "Big Buck Bunny (2008) [1080p]/movie.nfo", Size: 129},
				},
			}
		case torrentInfoH:
			// Also cached for the torrentinfo hash in the CheckCached test.
			result[h] = torbox.CachedItem{
				Hash: h,
				Name: "Sintel (2010) [1080p]",
				Size: 1136785408,
				Files: []torbox.CachedFile{
					{ID: 0, Name: "Sintel (2010) [1080p]/Sintel.2010.1080p.mkv", Size: 1136766000},
					{ID: 1, Name: "Sintel (2010) [1080p]/Sintel.2010.1080p.en.srt", Size: 19279},
				},
			}
			// uncachedHash → no entry in result (miss)
		}
		if f.nowCached != nil && f.nowCached[h] {
			result[h] = torbox.CachedItem{
				Hash: h,
				Name: "Waited Release",
				Size: 4096,
				Files: []torbox.CachedFile{
					{ID: 0, Name: "Waited Release/file.mkv", Size: 4096},
				},
			}
		}
	}
	return result, nil
}

func (f *fakeTorBox) TorrentInfo(hash string) (*torbox.CachedItem, error) {
	f.torrentInfoCalls = append(f.torrentInfoCalls, hash)
	if hash == torrentInfoH {
		return &torbox.CachedItem{
			Hash: hash,
			Name: "Sintel (2010) [1080p]",
			Size: 1136785408,
			Files: []torbox.CachedFile{
				{ID: 0, Name: "Sintel (2010) [1080p]/Sintel.2010.1080p.mkv", Size: 1136766000},
				{ID: 1, Name: "Sintel (2010) [1080p]/Sintel.2010.1080p.en.srt", Size: 19279},
			},
		}, nil
	}
	return nil, fmt.Errorf("torrentinfo: not found")
}

func (f *fakeTorBox) CreateTorrent(magnet string, addOnlyIfCached bool) (int64, string, error) {
	f.createCalls = append(f.createCalls, magnet)
	f.createCached = append(f.createCached, addOnlyIfCached)
	if f.createOK {
		return 9001, "", nil
	}
	return 0, "", fmt.Errorf("not implemented in fake")
}

func (f *fakeTorBox) RequestDL(torrentID int64, fileID int) (string, error) { return "", nil }
func (f *fakeTorBox) ControlDelete(torrentID int64) error {
	f.deletedIDs = append(f.deletedIDs, torrentID)
	return nil
}
func (f *fakeTorBox) MyList(offset int) ([]torbox.TorrentDetail, error) { return nil, nil }
func (f *fakeTorBox) MyListByID(id int64) (*torbox.TorrentDetail, error) {
	if f.myListByIDFn != nil {
		return f.myListByIDFn(id), nil
	}
	return nil, nil
}
func (f *fakeTorBox) UserMe() (*torbox.Account, error) { return &torbox.Account{Plan: 1}, nil }

// fakeSymlink implements symlink.Manager, recording calls.
type fakeSymlink struct {
	created []string
	removed []string
}

func (f *fakeSymlink) Create(r *catalog.Release, files []catalog.File) error {
	f.created = append(f.created, r.Hash)
	return nil
}

func (f *fakeSymlink) Remove(hash string) error {
	f.removed = append(f.removed, hash)
	return nil
}

// ── Test setup ───────────────────────────────────────────────────────────────

type testEnv struct {
	store   *fakeStore
	torbox  *fakeTorBox
	symlink *fakeSymlink
	handler http.Handler
	cfg     *config.Config
}

func newTestEnv(allowUncached bool) *testEnv {
	cfg := config.Default()
	cfg.Paths.DownloadDir = "/data/symlinks"
	cfg.Paths.FuseMount = "/data/torbox"
	cfg.Categories = []string{"radarr_hin", "sonarr_rd"}
	cfg.Policy.AllowUncached = allowUncached

	st := newFakeStore()
	tb := &fakeTorBox{}
	sl := &fakeSymlink{}

	h := qbit.New(qbit.Deps{
		Config:  cfg,
		Store:   st,
		TorBox:  tb,
		Symlink: sl,
	})

	return &testEnv{store: st, torbox: tb, symlink: sl, handler: h, cfg: cfg}
}

// do sends a request to the handler and returns the response.
func (e *testEnv) do(method, path string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func formBody(kv ...string) (io.Reader, string) {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return strings.NewReader(v.Encode()), "application/x-www-form-urlencoded"
}

func magnetURI(hash, name string) string {
	return fmt.Sprintf("magnet:?xt=urn:btih:%s&dn=%s&tr=udp%%3A%%2F%%2Ftracker.example.com", hash, url.QueryEscape(name))
}

// ── Tests ────────────────────────────────────────────────────────────────────

// TestLogin verifies the auth/login endpoint returns "Ok." for any credentials.
func TestLogin(t *testing.T) {
	e := newTestEnv(false)
	body, ct := formBody("username", "admin", "password", "secret")
	rec := e.do("POST", "/api/v2/auth/login", body, ct)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "Ok.", rec.Body.String())
}

// TestLoginEmptyCreds verifies login accepts empty credentials.
func TestLoginEmptyCreds(t *testing.T) {
	e := newTestEnv(false)
	body, ct := formBody()
	rec := e.do("POST", "/api/v2/auth/login", body, ct)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "Ok.", rec.Body.String())
}

// TestWebapiVersion verifies the connection-test endpoint.
func TestWebapiVersion(t *testing.T) {
	e := newTestEnv(false)
	rec := e.do("GET", "/api/v2/app/webapiVersion", nil, "")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "2.9.3", rec.Body.String())
}

// TestAppVersion verifies the app/version endpoint.
func TestAppVersion(t *testing.T) {
	e := newTestEnv(false)
	rec := e.do("GET", "/api/v2/app/version", nil, "")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "v4.6.0", rec.Body.String())
}

// TestPreferences verifies save_path is present in preferences.
func TestPreferences(t *testing.T) {
	e := newTestEnv(false)
	rec := e.do("GET", "/api/v2/app/preferences", nil, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var prefs map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &prefs))
	assert.Equal(t, "/data/symlinks", prefs["save_path"])
}

// TestArrLifecycle is the full end-to-end lifecycle test:
// login → webapiVersion → add (cached) → info → delete.
func TestArrLifecycle(t *testing.T) {
	e := newTestEnv(false)

	// 1. Login
	body, ct := formBody("username", "admin", "password", "pass")
	rec := e.do("POST", "/api/v2/auth/login", body, ct)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "Ok.", rec.Body.String())

	// 2. Connection test
	rec = e.do("GET", "/api/v2/app/webapiVersion", nil, "")
	require.Equal(t, http.StatusOK, rec.Code)

	// 3. Add torrent (magnet, cached hash)
	magnet := magnetURI(cachedHash, "Big+Buck+Bunny+(2008)+[1080p]")
	body, ct = formBody("urls", magnet, "category", "radarr_hin")
	rec = e.do("POST", "/api/v2/torrents/add", body, ct)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "Ok.", rec.Body.String())

	// Assert CheckCached was called (not CreateTorrent).
	require.Len(t, e.torbox.checkCachedCalls, 1)
	assert.Contains(t, e.torbox.checkCachedCalls[0], cachedHash)
	assert.Empty(t, e.torbox.createCalls, "TorBox CreateTorrent must NOT be called")

	// Assert UpsertRelease called.
	assert.Contains(t, e.store.upserted, cachedHash)

	// Assert Symlink.Create called.
	assert.Contains(t, e.symlink.created, cachedHash)

	// 4. torrents/info — must show progress=1.0, state=pausedUP, correct content_path.
	rec = e.do("GET", "/api/v2/torrents/info?category=radarr_hin", nil, "")
	require.Equal(t, http.StatusOK, rec.Code)

	var infos []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &infos))
	require.Len(t, infos, 1)

	info := infos[0]
	assert.Equal(t, cachedHash, info["hash"])
	assert.InDelta(t, 1.0, info["progress"], 0.001)
	assert.Equal(t, "pausedUP", info["state"])
	assert.Equal(t, float64(691087437), info["size"])
	assert.Equal(t, float64(0), info["amount_left"])
	assert.Equal(t, "/data/symlinks/radarr_hin", info["save_path"])
	// content_path must be set (non-empty and under download dir)
	cp, ok := info["content_path"].(string)
	require.True(t, ok)
	assert.True(t, strings.HasPrefix(cp, "/data/symlinks/radarr_hin/"), "content_path: %s", cp)

	// completion_on and added_on must be set (non-zero)
	assert.NotZero(t, info["completion_on"])
	assert.NotZero(t, info["added_on"])

	// speed/ratio/seq fields
	assert.Equal(t, float64(0), info["dlspeed"])
	assert.Equal(t, float64(0), info["upspeed"])
	assert.Equal(t, float64(0), info["eta"])
	assert.Equal(t, float64(0), info["ratio"])
	assert.Equal(t, false, info["seq_dl"])
	assert.Equal(t, false, info["f_l_piece_prio"])

	// 5. Delete torrent
	body, ct = formBody("hashes", cachedHash, "deleteFiles", "true")
	rec = e.do("POST", "/api/v2/torrents/delete", body, ct)
	require.Equal(t, http.StatusOK, rec.Code)

	// Assert Symlink.Remove called.
	assert.Contains(t, e.symlink.removed, cachedHash)
	// Assert DeleteRelease called.
	assert.Contains(t, e.store.deleted, cachedHash)
}

// TestAddUncachedDisallowed verifies that an uncached hash with AllowUncached=false
// results in an error-state release that the arr can move on from.
func TestAddUncachedDisallowed(t *testing.T) {
	e := newTestEnv(false)

	magnet := magnetURI(uncachedHash, "Some+Random+Release")
	body, ct := formBody("urls", magnet, "category", "radarr_hin")
	rec := e.do("POST", "/api/v2/torrents/add", body, ct)
	require.Equal(t, http.StatusOK, rec.Code) // arr gets 200

	// Release should be in error state.
	rel, _, err := e.store.GetRelease(uncachedHash)
	require.NoError(t, err)
	assert.Equal(t, catalog.StateError, rel.State)

	// Symlink.Create must NOT have been called for an errored release.
	assert.NotContains(t, e.symlink.created, uncachedHash)

	// /info should reflect error state (arr moves on).
	rec = e.do("GET", "/api/v2/torrents/info?category=radarr_hin", nil, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var infos []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &infos))
	require.Len(t, infos, 1)
	assert.Equal(t, "error", infos[0]["state"])
	assert.InDelta(t, 0.0, infos[0]["progress"], 0.001)
}

// TestAddUncachedAllowed verifies AllowUncached=true falls back to TorrentInfo.
func TestAddUncachedAllowed(t *testing.T) {
	e := newTestEnv(true)

	magnet := magnetURI(torrentInfoH, "Sintel+(2010)+[1080p]")
	body, ct := formBody("urls", magnet, "category", "sonarr_rd")
	rec := e.do("POST", "/api/v2/torrents/add", body, ct)
	require.Equal(t, http.StatusOK, rec.Code)

	// TorrentInfo fallback should be called (CheckCached misses for this hash in
	// the fake because torrentInfoH is NOT in the cached set here — we need a
	// version of the fake that misses it; let's inject a miss by using a fresh hash).
	//
	// Actually in our fake, torrentInfoH IS a cache hit in CheckCached.
	// Test the uncached path with a truly uncached hash + AllowUncached=true
	// but TorrentInfo also fails → error state.
	e2 := newTestEnv(true)
	magnet2 := magnetURI(uncachedHash, "Ghost+Release")
	body2, ct2 := formBody("urls", magnet2, "category", "radarr_hin")
	rec2 := e2.do("POST", "/api/v2/torrents/add", body2, ct2)
	require.Equal(t, http.StatusOK, rec2.Code)

	rel, _, err := e2.store.GetRelease(uncachedHash)
	require.NoError(t, err)
	// TorrentInfo also fails for uncachedHash → error state.
	assert.Equal(t, catalog.StateError, rel.State)
	assert.Contains(t, e2.torbox.torrentInfoCalls, uncachedHash)
}

// TestCategoryFiltering verifies /info?category= only returns releases of that category.
func TestCategoryFiltering(t *testing.T) {
	e := newTestEnv(false)

	// Add one release to each category.
	magnet1 := magnetURI(cachedHash, "Big+Buck+Bunny+(2008)+[1080p]")
	body, ct := formBody("urls", magnet1, "category", "radarr_hin")
	rec := e.do("POST", "/api/v2/torrents/add", body, ct)
	require.Equal(t, http.StatusOK, rec.Code)

	// Query only radarr_hin.
	rec = e.do("GET", "/api/v2/torrents/info?category=radarr_hin", nil, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var infos []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &infos))
	assert.Len(t, infos, 1)
	assert.Equal(t, "radarr_hin", infos[0]["category"])

	// Query sonarr_rd — should be empty.
	rec = e.do("GET", "/api/v2/torrents/info?category=sonarr_rd", nil, "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &infos))
	assert.Len(t, infos, 0)
}

// TestHashesFilter verifies ?hashes= filtering in /torrents/info.
func TestHashesFilter(t *testing.T) {
	e := newTestEnv(false)

	// Add one release.
	magnet := magnetURI(cachedHash, "Big+Buck+Bunny+(2008)+[1080p]")
	body, ct := formBody("urls", magnet, "category", "radarr_hin")
	e.do("POST", "/api/v2/torrents/add", body, ct)

	// Query with matching hash.
	rec := e.do("GET", "/api/v2/torrents/info?category=radarr_hin&hashes="+cachedHash, nil, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var infos []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &infos))
	assert.Len(t, infos, 1)

	// Query with non-matching hash.
	rec = e.do("GET", "/api/v2/torrents/info?category=radarr_hin&hashes="+uncachedHash, nil, "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &infos))
	assert.Len(t, infos, 0)
}

// TestTorrentsFiles verifies /torrents/files returns per-file list with progress=1.
func TestTorrentsFiles(t *testing.T) {
	e := newTestEnv(false)

	magnet := magnetURI(cachedHash, "Big+Buck+Bunny")
	body, ct := formBody("urls", magnet, "category", "radarr_hin")
	e.do("POST", "/api/v2/torrents/add", body, ct)

	rec := e.do("GET", "/api/v2/torrents/files?hash="+cachedHash, nil, "")
	require.Equal(t, http.StatusOK, rec.Code)

	var files []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &files))
	assert.Len(t, files, 3) // 3 files in fake
	for _, f := range files {
		assert.Equal(t, float64(1), f["progress"])
		assert.NotEmpty(t, f["name"])
	}
}

// TestTorrentsProperties verifies /torrents/properties returns content_path.
func TestTorrentsProperties(t *testing.T) {
	e := newTestEnv(false)

	magnet := magnetURI(cachedHash, "Big+Buck+Bunny")
	body, ct := formBody("urls", magnet, "category", "radarr_hin")
	e.do("POST", "/api/v2/torrents/add", body, ct)

	rec := e.do("GET", "/api/v2/torrents/properties?hash="+cachedHash, nil, "")
	require.Equal(t, http.StatusOK, rec.Code)

	var props map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &props))
	assert.Equal(t, "/data/symlinks/radarr_hin", props["save_path"])
	cp, ok := props["content_path"].(string)
	require.True(t, ok)
	assert.True(t, strings.HasPrefix(cp, "/data/symlinks/radarr_hin/"))
}

// TestCategories verifies /torrents/categories returns configured categories.
func TestCategories(t *testing.T) {
	e := newTestEnv(false)
	rec := e.do("GET", "/api/v2/torrents/categories", nil, "")
	require.Equal(t, http.StatusOK, rec.Code)

	var cats map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cats))
	assert.Contains(t, cats, "radarr_hin")
	assert.Contains(t, cats, "sonarr_rd")
}

// TestNoopEndpoints verifies pause/resume/topPrio return 200.
func TestNoopEndpoints(t *testing.T) {
	e := newTestEnv(false)
	for _, ep := range []string{"/api/v2/torrents/pause", "/api/v2/torrents/resume", "/api/v2/torrents/topPrio"} {
		body, ct := formBody("hashes", cachedHash)
		rec := e.do("POST", ep, body, ct)
		assert.Equal(t, http.StatusOK, rec.Code, "endpoint: %s", ep)
	}
}

// TestTransferInfo verifies transfer/info returns zeros.
func TestTransferInfo(t *testing.T) {
	e := newTestEnv(false)
	rec := e.do("GET", "/api/v2/transfer/info", nil, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var info map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &info))
	assert.Equal(t, float64(0), info["dl_info_speed"])
	assert.Equal(t, float64(0), info["up_info_speed"])
}

// TestMaindata verifies sync/maindata returns torrents keyed by hash.
func TestMaindata(t *testing.T) {
	e := newTestEnv(false)

	magnet := magnetURI(cachedHash, "Big+Buck+Bunny")
	body, ct := formBody("urls", magnet, "category", "radarr_hin")
	e.do("POST", "/api/v2/torrents/add", body, ct)

	rec := e.do("GET", "/api/v2/sync/maindata?category=radarr_hin", nil, "")
	require.Equal(t, http.StatusOK, rec.Code)

	var md map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &md))
	torrents, ok := md["torrents"].(map[string]any)
	require.True(t, ok)
	_, exists := torrents[cachedHash]
	assert.True(t, exists)
}

// TestDeleteMultipleHashes verifies pipe-separated hashes in delete.
func TestDeleteMultipleHashes(t *testing.T) {
	e := newTestEnv(false)

	// Add two releases using two different fakes.
	h1 := cachedHash
	h2 := torrentInfoH

	for _, h := range []string{h1, h2} {
		magnet := magnetURI(h, "Release+"+h[:8])
		body, ct := formBody("urls", magnet, "category", "radarr_hin")
		e.do("POST", "/api/v2/torrents/add", body, ct)
	}

	// Delete both in one call.
	body, ct := formBody("hashes", h1+"|"+h2)
	rec := e.do("POST", "/api/v2/torrents/delete", body, ct)
	require.Equal(t, http.StatusOK, rec.Code)

	assert.Contains(t, e.store.deleted, h1)
	assert.Contains(t, e.store.deleted, h2)
	assert.Contains(t, e.symlink.removed, h1)
	assert.Contains(t, e.symlink.removed, h2)
}

// fakeReleaser records Release calls for the S2 delete-release test.
type fakeReleaser struct{ released []string }

func (f *fakeReleaser) Release(hash string) error {
	f.released = append(f.released, hash)
	return nil
}

// TestDeleteReleasesEngine proves torrents/delete calls the engine's Release exactly once
// per hash (S2) so a delete during playback frees the TorBox item + slot instead of
// leaking it. Nil-safe: the other delete tests pass no engine and must still pass.
func TestDeleteReleasesEngine(t *testing.T) {
	cfg := config.Default()
	cfg.Categories = []string{"radarr_hin"}
	rel := &fakeReleaser{}
	h := qbit.New(qbit.Deps{
		Config: cfg, Store: newFakeStore(), TorBox: &fakeTorBox{},
		Symlink: &fakeSymlink{}, Engine: rel,
	})

	body, ct := formBody("hashes", cachedHash)
	req := httptest.NewRequest("POST", "/api/v2/torrents/delete", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Len(t, rel.released, 1, "engine.Release must be called exactly once")
	assert.Equal(t, cachedHash, rel.released[0])
}

// TestAddSetsContentPathUnderDownloadDir ensures content_path starts with DownloadDir/category.
func TestAddSetsContentPathUnderDownloadDir(t *testing.T) {
	e := newTestEnv(false)
	magnet := magnetURI(cachedHash, "Big+Buck+Bunny+(2008)+[1080p]")
	body, ct := formBody("urls", magnet, "category", "radarr_hin")
	e.do("POST", "/api/v2/torrents/add", body, ct)

	rec := e.do("GET", "/api/v2/torrents/info?category=radarr_hin", nil, "")
	var infos []map[string]any
	json.Unmarshal(rec.Body.Bytes(), &infos) //nolint:errcheck
	require.Len(t, infos, 1)

	cp := infos[0]["content_path"].(string)
	assert.True(t, strings.HasPrefix(cp, "/data/symlinks/radarr_hin/"),
		"content_path should be under DownloadDir/category, got: %s", cp)
}

// TestAddDoesNotCallCreateTorrent is an explicit assertion that TorBox add is never called.
func TestAddDoesNotCallCreateTorrent(t *testing.T) {
	e := newTestEnv(false)
	magnet := magnetURI(cachedHash, "Big+Buck+Bunny")
	body, ct := formBody("urls", magnet, "category", "radarr_hin")
	e.do("POST", "/api/v2/torrents/add", body, ct)

	assert.Empty(t, e.torbox.createCalls,
		"TorBox CreateTorrent must never be called during Phase 1 add")
}

// TestSizeIsNonZeroForCachedRelease checks the "trick" — size must be real.
func TestSizeIsNonZeroForCachedRelease(t *testing.T) {
	e := newTestEnv(false)
	magnet := magnetURI(cachedHash, "Big+Buck+Bunny")
	body, ct := formBody("urls", magnet, "category", "radarr_hin")
	e.do("POST", "/api/v2/torrents/add", body, ct)

	rec := e.do("GET", "/api/v2/torrents/info?category=radarr_hin", nil, "")
	var infos []map[string]any
	json.Unmarshal(rec.Body.Bytes(), &infos) //nolint:errcheck
	require.Len(t, infos, 1)
	size := infos[0]["size"].(float64)
	assert.Greater(t, size, float64(0), "size must be >0 so arr doesn't treat it as a sample")
}

// TestAddedOnTimestamp verifies added_on is a recent unix timestamp.
func TestAddedOnTimestamp(t *testing.T) {
	before := time.Now().Unix() - 2
	e := newTestEnv(false)
	magnet := magnetURI(cachedHash, "Big+Buck+Bunny")
	body, ct := formBody("urls", magnet, "category", "radarr_hin")
	e.do("POST", "/api/v2/torrents/add", body, ct)

	rec := e.do("GET", "/api/v2/torrents/info?category=radarr_hin", nil, "")
	var infos []map[string]any
	json.Unmarshal(rec.Body.Bytes(), &infos) //nolint:errcheck
	require.Len(t, infos, 1)
	addedOn := int64(infos[0]["added_on"].(float64))
	assert.Greater(t, addedOn, before)
}

// TestTorrentFileUpload verifies .torrent upload path (multipart form).
// We craft a minimal valid bencode torrent for the test.
func TestTorrentFileUpload(t *testing.T) {
	// Build a minimal bencode torrent: d4:infod4:name8:TestFile12:piece lengthi262144e6:pieces20:<20 bytes>ee
	// We encode just enough to get a parseable info dict.
	torrentBytes := buildMinimalTorrent("TestMovie")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("category", "radarr_hin")
	fw, err := mw.CreateFormFile("torrents", "test.torrent")
	require.NoError(t, err)
	_, _ = fw.Write(torrentBytes)
	_ = mw.Close()

	e := newTestEnv(false)
	req := httptest.NewRequest("POST", "/api/v2/torrents/add", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)

	// The torrent's hash won't be in the fake cache, so it ends up error state,
	// but the endpoint itself must return 200 (not 400).
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "Ok.", rec.Body.String())
}

// buildMinimalTorrent creates a bencode-encoded .torrent with the given name.
// bencode dict keys must appear in lexicographic order.
func buildMinimalTorrent(name string) []byte {
	// info dict keys (sorted): "name", "piece length", "pieces", "size"
	pieces := strings.Repeat("x", 20) // 20-byte SHA-1 block (1 piece)
	infoDict := fmt.Sprintf("d4:name%d:%s12:piece lengthi262144e6:pieces20:%s4:sizei1000000ee",
		len(name), name, pieces)
	// outer dict keys (sorted): "info"
	outer := fmt.Sprintf("d4:info%d:%se", len(infoDict), infoDict)
	return []byte(outer)
}

// TestBuildInfo verifies app/buildInfo returns valid JSON.
func TestBuildInfo(t *testing.T) {
	e := newTestEnv(false)
	rec := e.do("GET", "/api/v2/app/buildInfo", nil, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var bi map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &bi))
	assert.NotEmpty(t, bi)
}

// TestLogout returns 200.
func TestLogout(t *testing.T) {
	e := newTestEnv(false)
	rec := e.do("POST", "/api/v2/auth/logout", nil, "")
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestInfoByHashWithoutCategory guards the canary-discovered fix: *arr clients
// poll torrents/info by hash with NO category (and sometimes with no filter at
// all). The server must still resolve the release, or the arr never imports.
func TestInfoByHashWithoutCategory(t *testing.T) {
	e := newTestEnv(false)
	rel := &catalog.Release{
		Hash: cachedHash, Name: "Big Buck Bunny", Category: "radarr_hin",
		TotalSize: 100, State: catalog.StateVirtual,
	}
	files := []catalog.File{{Hash: cachedHash, FileID: 0, RelPath: "Big Buck Bunny/x.mp4", Size: 100}}
	require.NoError(t, e.store.UpsertRelease(rel, files))

	// (a) hashes filter, NO category — how radarr polls a tracked download.
	rec := e.do("GET", "/api/v2/torrents/info?hashes="+cachedHash, nil, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var arr []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &arr))
	require.Len(t, arr, 1)
	assert.Equal(t, cachedHash, arr[0]["hash"])

	// (b) no filter at all — must return all configured categories' releases.
	rec2 := e.do("GET", "/api/v2/torrents/info", nil, "")
	var arr2 []map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &arr2))
	require.Len(t, arr2, 1)
}
