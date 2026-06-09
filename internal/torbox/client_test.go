package torbox_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rushp4000/lazarr/internal/config"
	"github.com/rushp4000/lazarr/internal/torbox"
)

// testdataDir is the path to the shared testdata fixtures.
const testdataDir = "../../testdata/torbox"

// readFixture reads a testdata JSON file.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testdataDir, name))
	require.NoError(t, err, "reading fixture %s", name)
	return b
}

// fakeAPIKey is used in all tests. Must never appear in logs.
const fakeAPIKey = "tb-fake-key-for-tests-only"

// newTestServer creates an httptest.Server using the provided handler.
// It also returns the client wired to point at the test server.
func newTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, torbox.Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := config.TorBox{APIKey: fakeAPIKey, APIBase: srv.URL}
	c := torbox.NewForTest(cfg) // uses withBaseURL + no redirect override
	return srv, c
}

// captureLog captures log output into a buffer for inspection.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return &buf
}

// assertNoAPIKey asserts the API key does not appear in any logged string.
func assertNoAPIKey(t *testing.T, logBuf *bytes.Buffer) {
	t.Helper()
	assert.NotContains(t, logBuf.String(), fakeAPIKey, "API key must never appear in logs")
}

// authCheckHandler wraps a handler with an auth check and records request headers.
type authCheckHandler struct {
	t       *testing.T
	handler http.HandlerFunc
	sawAuth string
}

func (a *authCheckHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.sawAuth = r.Header.Get("Authorization")
	a.handler(w, r)
}

// jsonResp writes a fixture JSON body with status 200.
func jsonResp(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// jsonRespStatus writes a fixture JSON body with an explicit status code.
func jsonRespStatus(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// ---------------------------------------------------------------------------
// CheckCached
// ---------------------------------------------------------------------------

func TestCheckCached_Hit(t *testing.T) {
	fixture := readFixture(t, "checkcached_cached.json")
	logBuf := captureLog(t)

	var lastQuery string
	ah := &authCheckHandler{
		t: t,
		handler: func(w http.ResponseWriter, r *http.Request) {
			lastQuery = r.URL.RawQuery
			jsonResp(w, fixture)
		},
	}
	srv := httptest.NewServer(ah)
	t.Cleanup(srv.Close)

	cfg := config.TorBox{APIKey: fakeAPIKey, APIBase: srv.URL}
	c := torbox.NewForTest(cfg)

	hash := "dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c"
	result, err := c.CheckCached([]string{hash})
	require.NoError(t, err)

	// Authorization header sent.
	assert.Equal(t, "Bearer "+fakeAPIKey, ah.sawAuth, "Authorization header must be set")

	// format=object&list_files=true in query.
	assert.Contains(t, lastQuery, "format=object", "format=object must be in query")
	assert.Contains(t, lastQuery, "list_files=true", "list_files=true must be in query")

	item, ok := result[hash]
	require.True(t, ok, "hash should be present in result")
	assert.Equal(t, "Big Buck Bunny (2008) [1080p]", item.Name)
	assert.Equal(t, int64(691087437), item.Size)
	require.Len(t, item.Files, 3, "expected 3 files")
	assert.Equal(t, 0, item.Files[0].ID)
	assert.Equal(t, int64(691069000), item.Files[0].Size)
	assert.Contains(t, item.Files[0].Name, "mp4")
	assert.Equal(t, 1, item.Files[1].ID)
	assert.Equal(t, 2, item.Files[2].ID)
	assert.Equal(t, int64(129), item.Files[2].Size)

	assertNoAPIKey(t, logBuf)
}

func TestCheckCached_Miss_EmptyObject(t *testing.T) {
	fixture := readFixture(t, "checkcached_miss.json")
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, fixture)
	})

	result, err := c.CheckCached([]string{"deadbeef1234"})
	require.NoError(t, err)
	assert.Empty(t, result, "no hits on miss")
}

func TestCheckCached_Miss_NullData(t *testing.T) {
	body := []byte(`{"success":true,"detail":"ok","data":null}`)
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, body)
	})

	result, err := c.CheckCached([]string{"aabbcc"})
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestCheckCached_Batching(t *testing.T) {
	// Build 150 fake hashes.
	hashes := make([]string, 150)
	for i := range hashes {
		hashes[i] = strings.Repeat("a", 39) + string(rune('0'+i%10))
	}

	callCount := 0
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		// Verify batch sizes in hash param.
		hashParam := r.URL.Query().Get("hash")
		parts := strings.Split(hashParam, ",")
		// First call = 100, second = 50.
		if callCount == 1 {
			assert.Len(t, parts, 100, "first batch must be exactly 100")
		} else {
			assert.LessOrEqual(t, len(parts), 100, "subsequent batch must be ≤100")
		}
		// Respond with empty data to avoid JSON parsing overhead.
		jsonResp(w, []byte(`{"success":true,"detail":"ok","data":{}}`))
	})

	_, err := c.CheckCached(hashes)
	require.NoError(t, err)
	assert.Equal(t, 2, callCount, "150 hashes must trigger exactly 2 API calls")
}

// ---------------------------------------------------------------------------
// TorrentInfo
// ---------------------------------------------------------------------------

func TestTorrentInfo(t *testing.T) {
	fixture := readFixture(t, "torrentinfo.json")
	ah := &authCheckHandler{
		t: t,
		handler: func(w http.ResponseWriter, r *http.Request) {
			jsonResp(w, fixture)
		},
	}
	srv := httptest.NewServer(ah)
	t.Cleanup(srv.Close)

	cfg := config.TorBox{APIKey: fakeAPIKey, APIBase: srv.URL}
	c := torbox.NewForTest(cfg)

	item, err := c.TorrentInfo("08ada5a7a6183aae1e09d831df6748d566095a10")
	require.NoError(t, err)
	require.NotNil(t, item)
	assert.Equal(t, "Sintel (2010) [1080p]", item.Name)
	assert.Equal(t, int64(1136785408), item.Size)
	require.Len(t, item.Files, 3)
	assert.Equal(t, "Bearer "+fakeAPIKey, ah.sawAuth)
}

// ---------------------------------------------------------------------------
// CreateTorrent
// ---------------------------------------------------------------------------

func TestCreateTorrent_Success(t *testing.T) {
	fixture := readFixture(t, "createtorrent_cached.json")
	ah := &authCheckHandler{
		t: t,
		handler: func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			// Verify multipart body contains magnet and add_only_if_cached.
			require.NoError(t, r.ParseMultipartForm(1<<20))
			assert.Equal(t, "magnet:?xt=urn:btih:dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c", r.FormValue("magnet"))
			assert.Equal(t, "true", r.FormValue("add_only_if_cached"))
			jsonResp(w, fixture)
		},
	}
	srv := httptest.NewServer(ah)
	t.Cleanup(srv.Close)

	cfg := config.TorBox{APIKey: fakeAPIKey, APIBase: srv.URL}
	c := torbox.NewForTest(cfg)

	id, hash, err := c.CreateTorrent("magnet:?xt=urn:btih:dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c", true)
	require.NoError(t, err)
	assert.Equal(t, int64(7654321), id)
	assert.Equal(t, "dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c", hash)
	assert.Equal(t, "Bearer "+fakeAPIKey, ah.sawAuth)
}

func TestCreateTorrent_RateLimit(t *testing.T) {
	fixture := readFixture(t, "createtorrent_ratelimited.json")
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, fixture)
	})

	_, _, err := c.CreateTorrent("magnet:?xt=urn:btih:aabb", true)
	require.Error(t, err)
	assert.True(t, errors.Is(err, torbox.ErrRateLimited), "must return ErrRateLimited, got: %v", err)
}

// ---------------------------------------------------------------------------
// RequestDL
// ---------------------------------------------------------------------------

func TestRequestDL_Success(t *testing.T) {
	fixture := readFixture(t, "requestdl.json")
	logBuf := captureLog(t)

	ah := &authCheckHandler{
		t: t,
		handler: func(w http.ResponseWriter, r *http.Request) {
			// Verify query params.
			q := r.URL.Query()
			assert.Equal(t, fakeAPIKey, q.Get("token"), "token param must match API key")
			assert.Equal(t, "7654321", q.Get("torrent_id"))
			assert.Equal(t, "0", q.Get("file_id"))
			assert.Equal(t, "false", q.Get("redirect"))
			jsonResp(w, fixture)
		},
	}
	srv := httptest.NewServer(ah)
	t.Cleanup(srv.Close)

	cfg := config.TorBox{APIKey: fakeAPIKey, APIBase: srv.URL}
	c := torbox.NewForTest(cfg)

	dlURL, err := c.RequestDL(7654321, 0)
	require.NoError(t, err)
	assert.Contains(t, dlURL, "tb-cdn.io", "expected CDN URL")
	assert.Equal(t, "Bearer "+fakeAPIKey, ah.sawAuth)
	assertNoAPIKey(t, logBuf)
}

func TestRequestDL_LinkExpired_400(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		jsonRespStatus(w, http.StatusBadRequest, []byte(`{"success":false,"detail":"link expired","data":null}`))
	})

	_, err := c.RequestDL(7654321, 0)
	require.Error(t, err)
	assert.True(t, errors.Is(err, torbox.ErrLinkExpired), "must return ErrLinkExpired on 400, got: %v", err)
}

func TestRequestDL_LinkExpired_403(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		jsonRespStatus(w, http.StatusForbidden, []byte(`{"success":false,"detail":"forbidden","data":null}`))
	})

	_, err := c.RequestDL(1, 1)
	require.Error(t, err)
	assert.True(t, errors.Is(err, torbox.ErrLinkExpired), "must return ErrLinkExpired on 403, got: %v", err)
}

func TestRequestDL_LinkExpired_410(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		jsonRespStatus(w, http.StatusGone, []byte(`{"success":false,"detail":"gone","data":null}`))
	})

	_, err := c.RequestDL(1, 1)
	require.Error(t, err)
	assert.True(t, errors.Is(err, torbox.ErrLinkExpired), "must return ErrLinkExpired on 410, got: %v", err)
}

// TestRequestDL_DeadCacheOrdering proves a gone torrent reported as a 400/403/410 whose
// detail says "not found" maps to ErrNotFound, not ErrLinkExpired (S6): the not-found check
// must precede the link-refresh-status branch, else a dead torrent drives an endless
// refresh→fail→EIO loop instead of erroring the release for re-grab.
func TestRequestDL_DeadCacheOrdering(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		detail  string
		wantErr error
	}{
		{"400 not found => dead-cache", http.StatusBadRequest, "torrent not found", torbox.ErrNotFound},
		{"403 not found => dead-cache", http.StatusForbidden, "file not found", torbox.ErrNotFound},
		{"410 no longer => dead-cache", http.StatusGone, "torrent no longer exists", torbox.ErrNotFound},
		{"400 other detail => link expired", http.StatusBadRequest, "link expired", torbox.ErrLinkExpired},
		{"403 other detail => link expired", http.StatusForbidden, "forbidden", torbox.ErrLinkExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"success":false,"detail":"` + tc.detail + `","data":null}`)
			_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				jsonRespStatus(w, tc.status, body)
			})
			_, err := c.RequestDL(1, 0)
			require.Error(t, err)
			assert.True(t, errors.Is(err, tc.wantErr),
				"status %d detail %q: want %v, got %v", tc.status, tc.detail, tc.wantErr, err)
		})
	}
}

// ---------------------------------------------------------------------------
// ControlDelete
// ---------------------------------------------------------------------------

func TestControlDelete_PostsCorrectJSON(t *testing.T) {
	fixture := readFixture(t, "controltorrent_delete_ok.json")

	var receivedBody []byte
	ah := &authCheckHandler{
		t: t,
		handler: func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
			var err error
			receivedBody, err = io.ReadAll(r.Body)
			require.NoError(t, err)
			jsonResp(w, fixture)
		},
	}
	srv := httptest.NewServer(ah)
	t.Cleanup(srv.Close)

	cfg := config.TorBox{APIKey: fakeAPIKey, APIBase: srv.URL}
	c := torbox.NewForTest(cfg)

	err := c.ControlDelete(7654321)
	require.NoError(t, err)

	// Inspect the JSON body.
	var body struct {
		TorrentID int64  `json:"torrent_id"`
		Operation string `json:"operation"`
	}
	require.NoError(t, json.Unmarshal(receivedBody, &body))
	assert.Equal(t, int64(7654321), body.TorrentID)
	assert.Equal(t, "delete", body.Operation, "operation must be lowercase 'delete'")
	assert.Equal(t, "Bearer "+fakeAPIKey, ah.sawAuth)
}

// ---------------------------------------------------------------------------
// MyList
// ---------------------------------------------------------------------------

func TestMyList(t *testing.T) {
	fixture := readFixture(t, "mylist.json")
	ah := &authCheckHandler{
		t: t,
		handler: func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			assert.Equal(t, "0", q.Get("offset"))
			assert.Equal(t, "1000", q.Get("limit"))
			assert.Equal(t, "true", q.Get("bypass_cache"))
			jsonResp(w, fixture)
		},
	}
	srv := httptest.NewServer(ah)
	t.Cleanup(srv.Close)

	cfg := config.TorBox{APIKey: fakeAPIKey, APIBase: srv.URL}
	c := torbox.NewForTest(cfg)

	items, err := c.MyList(0)
	require.NoError(t, err)
	require.Len(t, items, 2)

	bbb := items[0]
	assert.Equal(t, int64(7654321), bbb.ID)
	assert.Equal(t, "dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c", bbb.Hash)
	assert.Equal(t, "Big Buck Bunny (2008) [1080p]", bbb.Name)
	assert.True(t, bbb.DownloadFinished)
	require.Len(t, bbb.Files, 3)

	sintel := items[1]
	assert.Equal(t, int64(7654322), sintel.ID)
	assert.False(t, sintel.DownloadFinished)
	assert.Equal(t, "Bearer "+fakeAPIKey, ah.sawAuth)
}

// ---------------------------------------------------------------------------
// UserMe
// ---------------------------------------------------------------------------

func TestUserMe(t *testing.T) {
	fixture := readFixture(t, "user_me.json")
	logBuf := captureLog(t)

	ah := &authCheckHandler{
		t: t,
		handler: func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "true", r.URL.Query().Get("settings"))
			jsonResp(w, fixture)
		},
	}
	srv := httptest.NewServer(ah)
	t.Cleanup(srv.Close)

	cfg := config.TorBox{APIKey: fakeAPIKey, APIBase: srv.URL}
	c := torbox.NewForTest(cfg)

	acc, err := c.UserMe()
	require.NoError(t, err)
	require.NotNil(t, acc)
	assert.Equal(t, 1, acc.Plan)
	// Essential base 3 + additional_concurrent_slots 0 = 3.
	assert.Equal(t, 3, acc.ActiveSlots)
	assert.Equal(t, "2026-06-10T01:08:06Z", acc.CooldownUntil)
	assert.False(t, acc.LongTermStore)
	assert.Equal(t, "Bearer "+fakeAPIKey, ah.sawAuth)
	assertNoAPIKey(t, logBuf)
}
