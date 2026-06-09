package torbox_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rushp4000/lazarr/internal/torbox"
)

// TestCreateTorrent_NotCached_DeadCache proves a cached-only add whose hash is not
// cached / not found maps to the typed torbox.ErrNotFound sentinel (the dead-cache
// signal the materialize engine turns into ErrPurged + StateError).
func TestCreateTorrent_NotCached_DeadCache(t *testing.T) {
	details := []string{
		`{"success":false,"detail":"Torrent not cached.","data":null}`,
		`{"success":false,"detail":"Torrent not found.","data":null}`,
		`{"success":false,"detail":"This torrent no longer exists.","data":null}`,
	}
	for _, body := range details {
		body := body
		t.Run(body, func(t *testing.T) {
			_, c := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				jsonResp(w, []byte(body))
			})
			_, _, err := c.CreateTorrent("magnet:?xt=urn:btih:aabb", true /* addOnlyIfCached */)
			require.Error(t, err)
			assert.True(t, errors.Is(err, torbox.ErrNotFound),
				"want ErrNotFound, got: %v", err)
		})
	}
}

// TestCreateTorrent_NotCached_OnlyWhenCachedOnly proves the dead-cache sentinel is NOT
// raised when add_only_if_cached is false (an uncached add is a legitimate request).
func TestCreateTorrent_NotCached_OnlyWhenCachedOnly(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonResp(w, []byte(`{"success":false,"detail":"Torrent not cached.","data":null}`))
	})
	_, _, err := c.CreateTorrent("magnet:?xt=urn:btih:aabb", false /* allow uncached */)
	require.Error(t, err)
	assert.False(t, errors.Is(err, torbox.ErrNotFound),
		"uncached add must not be reported as dead-cache; got: %v", err)
}

// TestRequestDL_NotFound_404 proves a 404 from requestdl (the torrent is gone, not a
// stale link) maps to torbox.ErrNotFound, distinct from the 400/403/410 link-expired set.
func TestRequestDL_NotFound_404(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonRespStatus(w, http.StatusNotFound,
			[]byte(`{"success":false,"detail":"Torrent not found.","data":null}`))
	})
	_, err := c.RequestDL(7654321, 0)
	require.Error(t, err)
	assert.True(t, errors.Is(err, torbox.ErrNotFound), "want ErrNotFound on 404, got: %v", err)
	assert.False(t, errors.Is(err, torbox.ErrLinkExpired), "404 must NOT be link-expired: %v", err)
}
