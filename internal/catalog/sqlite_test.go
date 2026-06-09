package catalog

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openTestDB opens a fresh SQLite database in a temp dir for each test.
func openTestDB(t *testing.T) Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog_test.db")
	s, err := OpenSQLite(path)
	require.NoError(t, err, "OpenSQLite should succeed")
	t.Cleanup(func() { s.Close() })
	return s
}

// sampleRelease returns a minimal valid Release for testing.
func sampleRelease(hash string) *Release {
	return &Release{
		Hash:       hash,
		Name:       "Test Release " + hash,
		Category:   "radarr_hin",
		Magnet:     "magnet:?xt=urn:btih:" + hash,
		TotalSize:  1234567,
		State:      StateVirtual,
		Cached:     true,
		TorBoxID:   0,
		AddedOn:    1000,
		LastAccess: 1000,
		CreatedAt:  1000,
	}
}

// sampleFiles returns two File rows for a given hash.
func sampleFiles(hash string) []File {
	return []File{
		{Hash: hash, FileID: 1, RelPath: "movie/movie.mkv", Size: 1000000},
		{Hash: hash, FileID: 2, RelPath: "movie/subs.srt", Size: 12345},
	}
}

// ---- Migrations idempotency ------------------------------------------------

func TestMigrationsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "idem.db")

	s1, err := OpenSQLite(path)
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	// Open the same file a second time; migrations must not fail.
	s2, err := OpenSQLite(path)
	require.NoError(t, err, "second open of same DB should not fail")
	require.NoError(t, s2.Close())
}

// ---- UpsertRelease + GetRelease round-trip ---------------------------------

func TestUpsertGetRoundTrip(t *testing.T) {
	s := openTestDB(t)
	hash := "aabbcc001122334455667788990011223344556677"
	r := sampleRelease(hash)
	files := sampleFiles(hash)

	require.NoError(t, s.UpsertRelease(r, files))

	got, gotFiles, err := s.GetRelease(hash)
	require.NoError(t, err)

	// Release fields
	assert.Equal(t, r.Hash, got.Hash)
	assert.Equal(t, r.Name, got.Name)
	assert.Equal(t, r.Category, got.Category)
	assert.Equal(t, r.Magnet, got.Magnet)
	assert.Equal(t, r.TotalSize, got.TotalSize)
	assert.Equal(t, r.State, got.State)
	assert.Equal(t, r.Cached, got.Cached)
	assert.Equal(t, r.TorBoxID, got.TorBoxID)
	assert.Equal(t, r.AddedOn, got.AddedOn)
	assert.Equal(t, r.LastAccess, got.LastAccess)
	assert.Equal(t, r.CreatedAt, got.CreatedAt)

	// File rows
	require.Len(t, gotFiles, 2)
	assert.Equal(t, files[0].FileID, gotFiles[0].FileID)
	assert.Equal(t, files[0].RelPath, gotFiles[0].RelPath)
	assert.Equal(t, files[0].Size, gotFiles[0].Size)
	assert.Equal(t, files[1].FileID, gotFiles[1].FileID)
}

func TestUpsertCachedFalse(t *testing.T) {
	s := openTestDB(t)
	hash := "deadbeef01020304050607080900010203040506"
	r := sampleRelease(hash)
	r.Cached = false

	require.NoError(t, s.UpsertRelease(r, nil))

	got, _, err := s.GetRelease(hash)
	require.NoError(t, err)
	assert.False(t, got.Cached)
}

// ---- UpsertRelease replaces files ------------------------------------------

func TestUpsertReplacesFiles(t *testing.T) {
	s := openTestDB(t)
	hash := "11223344556677889900aabbccddeeff00112233"
	r := sampleRelease(hash)

	// First insert with 2 files.
	require.NoError(t, s.UpsertRelease(r, sampleFiles(hash)))

	// Re-upsert with only 1 file — stale row must be gone.
	newFiles := []File{
		{Hash: hash, FileID: 10, RelPath: "movie/new.mkv", Size: 999},
	}
	r.Name = "Updated Name"
	require.NoError(t, s.UpsertRelease(r, newFiles))

	got, gotFiles, err := s.GetRelease(hash)
	require.NoError(t, err)
	assert.Equal(t, "Updated Name", got.Name)
	require.Len(t, gotFiles, 1, "stale files must be removed on upsert")
	assert.Equal(t, 10, gotFiles[0].FileID)
}

func TestUpsertNoFiles(t *testing.T) {
	s := openTestDB(t)
	hash := "cafebabe0000000000000000000000000000cafe"
	r := sampleRelease(hash)

	require.NoError(t, s.UpsertRelease(r, nil))

	_, files, err := s.GetRelease(hash)
	require.NoError(t, err)
	assert.Empty(t, files)
}

// ---- GetRelease not found --------------------------------------------------

func TestGetReleaseNotFound(t *testing.T) {
	s := openTestDB(t)
	_, _, err := s.GetRelease("doesnotexist")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound))
}

// ---- ListByCategory --------------------------------------------------------

func TestListByCategory(t *testing.T) {
	s := openTestDB(t)

	hashes := []string{
		"aaaa000000000000000000000000000000000001",
		"aaaa000000000000000000000000000000000002",
		"bbbb000000000000000000000000000000000003",
	}

	for i, h := range hashes {
		r := sampleRelease(h)
		if i == 2 {
			r.Category = "sonarr_hin"
		}
		require.NoError(t, s.UpsertRelease(r, nil))
	}

	radarr, err := s.ListByCategory("radarr_hin")
	require.NoError(t, err)
	assert.Len(t, radarr, 2)

	sonarr, err := s.ListByCategory("sonarr_hin")
	require.NoError(t, err)
	assert.Len(t, sonarr, 1)

	empty, err := s.ListByCategory("unknown")
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// ---- SetState transitions --------------------------------------------------

func TestSetStateTransitions(t *testing.T) {
	s := openTestDB(t)
	hash := "statetest0000000000000000000000000000001"
	r := sampleRelease(hash)
	require.NoError(t, s.UpsertRelease(r, nil))

	// virtual → materialized
	require.NoError(t, s.SetState(hash, StateMaterialized, 42))
	got, _, err := s.GetRelease(hash)
	require.NoError(t, err)
	assert.Equal(t, StateMaterialized, got.State)
	assert.Equal(t, int64(42), got.TorBoxID)

	// materialized → virtual (clear torbox_id)
	require.NoError(t, s.SetState(hash, StateVirtual, 0))
	got, _, err = s.GetRelease(hash)
	require.NoError(t, err)
	assert.Equal(t, StateVirtual, got.State)
	assert.Equal(t, int64(0), got.TorBoxID)

	// virtual → error
	require.NoError(t, s.SetState(hash, StateError, 0))
	got, _, err = s.GetRelease(hash)
	require.NoError(t, err)
	assert.Equal(t, StateError, got.State)
}

func TestSetStateNotFound(t *testing.T) {
	s := openTestDB(t)
	err := s.SetState("nosuchhash", StateMaterialized, 1)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound))
}

// ---- TouchAccess -----------------------------------------------------------

func TestTouchAccess(t *testing.T) {
	s := openTestDB(t)
	hash := "touchtest000000000000000000000000000001"
	r := sampleRelease(hash)
	r.LastAccess = 500
	require.NoError(t, s.UpsertRelease(r, nil))

	require.NoError(t, s.TouchAccess(hash, 9999))

	got, _, err := s.GetRelease(hash)
	require.NoError(t, err)
	assert.Equal(t, int64(9999), got.LastAccess)
}

func TestTouchAccessNotFound(t *testing.T) {
	s := openTestDB(t)
	err := s.TouchAccess("nosuch", 1)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound))
}

// ---- IdleCandidates --------------------------------------------------------

func TestIdleCandidates(t *testing.T) {
	s := openTestDB(t)

	// Three releases, all materialized but different last_access times.
	releases := []struct {
		hash       string
		lastAccess int64
	}{
		{"idle0000000000000000000000000000000001", 100},
		{"idle0000000000000000000000000000000002", 200},
		{"idle0000000000000000000000000000000003", 300},
	}
	for _, tc := range releases {
		r := sampleRelease(tc.hash)
		r.LastAccess = tc.lastAccess
		require.NoError(t, s.UpsertRelease(r, nil))
		require.NoError(t, s.SetState(tc.hash, StateMaterialized, 100))
	}

	// A virtual release — must NOT appear in results.
	virt := "idle_virt000000000000000000000000000001"
	rv := sampleRelease(virt)
	rv.LastAccess = 50
	require.NoError(t, s.UpsertRelease(rv, nil))

	// before=250 → should return hashes with last_access 100 and 200.
	candidates, err := s.IdleCandidates(250)
	require.NoError(t, err)
	require.Len(t, candidates, 2)

	// before=100 → strict less-than; hash with last_access==100 must NOT be returned.
	candidates, err = s.IdleCandidates(100)
	require.NoError(t, err)
	assert.Empty(t, candidates)

	// before=101 → hash with last_access 100 returned.
	candidates, err = s.IdleCandidates(101)
	require.NoError(t, err)
	assert.Len(t, candidates, 1)
}

// ---- OverMaxHold -----------------------------------------------------------

func TestOverMaxHold(t *testing.T) {
	s := openTestDB(t)

	releases := []struct {
		hash    string
		addedOn int64
	}{
		{"hold0000000000000000000000000000000001", 1000},
		{"hold0000000000000000000000000000000002", 2000},
		{"hold0000000000000000000000000000000003", 3000},
	}
	for _, tc := range releases {
		r := sampleRelease(tc.hash)
		r.AddedOn = tc.addedOn
		require.NoError(t, s.UpsertRelease(r, nil))
		require.NoError(t, s.SetState(tc.hash, StateMaterialized, 200))
	}

	// Virtual release must NOT appear.
	virt := "hold_virt00000000000000000000000000001"
	rv := sampleRelease(virt)
	rv.AddedOn = 500
	require.NoError(t, s.UpsertRelease(rv, nil))

	// before=2500 → hash with added_on 1000 and 2000 are < 2500.
	got, err := s.OverMaxHold(2500)
	require.NoError(t, err)
	assert.Len(t, got, 2)

	// before=1000 → strict less-than; none returned.
	got, err = s.OverMaxHold(1000)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// ---- MaterializedIDs -------------------------------------------------------

func TestMaterializedIDs(t *testing.T) {
	s := openTestDB(t)

	releases := []struct {
		hash     string
		torboxID int64
		state    State
	}{
		{"matid000000000000000000000000000000001", 111, StateMaterialized},
		{"matid000000000000000000000000000000002", 222, StateMaterialized},
		{"matid000000000000000000000000000000003", 333, StateVirtual},
	}
	for _, tc := range releases {
		r := sampleRelease(tc.hash)
		require.NoError(t, s.UpsertRelease(r, nil))
		require.NoError(t, s.SetState(tc.hash, tc.state, tc.torboxID))
	}

	ids, err := s.MaterializedIDs()
	require.NoError(t, err)
	assert.Len(t, ids, 2)
	assert.ElementsMatch(t, []int64{111, 222}, ids)
}

func TestMaterializedIDsEmpty(t *testing.T) {
	s := openTestDB(t)
	ids, err := s.MaterializedIDs()
	require.NoError(t, err)
	assert.Empty(t, ids)
}

// ---- GetLink / SetLink -----------------------------------------------------

func TestGetSetLink(t *testing.T) {
	s := openTestDB(t)
	hash := "link0000000000000000000000000000000001"
	require.NoError(t, s.UpsertRelease(sampleRelease(hash), sampleFiles(hash)))

	l := &DLLink{
		Hash:      hash,
		FileID:    1,
		URL:       "https://tb-cdn.io/test?token=abc",
		FetchedAt: 5000,
		ExpiresAt: 15000,
	}
	require.NoError(t, s.SetLink(l))

	got, err := s.GetLink(hash, 1)
	require.NoError(t, err)
	assert.Equal(t, l.Hash, got.Hash)
	assert.Equal(t, l.FileID, got.FileID)
	assert.Equal(t, l.URL, got.URL)
	assert.Equal(t, l.FetchedAt, got.FetchedAt)
	assert.Equal(t, l.ExpiresAt, got.ExpiresAt)
}

func TestSetLinkUpsert(t *testing.T) {
	s := openTestDB(t)
	hash := "link0000000000000000000000000000000002"
	require.NoError(t, s.UpsertRelease(sampleRelease(hash), sampleFiles(hash)))

	l := &DLLink{Hash: hash, FileID: 1, URL: "https://old.url", FetchedAt: 100, ExpiresAt: 200}
	require.NoError(t, s.SetLink(l))

	// Overwrite with new URL.
	l.URL = "https://new.url"
	l.FetchedAt = 999
	l.ExpiresAt = 9999
	require.NoError(t, s.SetLink(l))

	got, err := s.GetLink(hash, 1)
	require.NoError(t, err)
	assert.Equal(t, "https://new.url", got.URL)
	assert.Equal(t, int64(999), got.FetchedAt)
}

func TestGetLinkNotFound(t *testing.T) {
	s := openTestDB(t)
	hash := "link0000000000000000000000000000000003"
	require.NoError(t, s.UpsertRelease(sampleRelease(hash), sampleFiles(hash)))

	_, err := s.GetLink(hash, 99)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound))
}

func TestGetLinkHashNotFound(t *testing.T) {
	s := openTestDB(t)
	_, err := s.GetLink("nosuchhash", 1)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound))
}

// ---- DeleteRelease cascades ------------------------------------------------

func TestDeleteReleaseCascade(t *testing.T) {
	s := openTestDB(t)
	hash := "del00000000000000000000000000000000001"
	require.NoError(t, s.UpsertRelease(sampleRelease(hash), sampleFiles(hash)))

	// Add a DLLink too.
	require.NoError(t, s.SetLink(&DLLink{
		Hash: hash, FileID: 1, URL: "https://x", FetchedAt: 1, ExpiresAt: 2,
	}))

	// Delete the release.
	require.NoError(t, s.DeleteRelease(hash))

	// Release gone.
	_, _, err := s.GetRelease(hash)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound))

	// Files cascade-deleted; we can verify by re-inserting the release (no FK error)
	// and checking no stale files exist.
	require.NoError(t, s.UpsertRelease(sampleRelease(hash), nil))
	_, files, err := s.GetRelease(hash)
	require.NoError(t, err)
	assert.Empty(t, files)

	// DLLink cascade-deleted: link must be gone.
	_, err = s.GetLink(hash, 1)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound))
}

func TestDeleteReleaseNotFound(t *testing.T) {
	s := openTestDB(t)
	err := s.DeleteRelease("nosuch")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound))
}

// ---- errors.Is sentinel check -----------------------------------------------

func TestErrNotFoundSentinel(t *testing.T) {
	s := openTestDB(t)

	t.Run("GetRelease", func(t *testing.T) {
		_, _, err := s.GetRelease("x")
		assert.True(t, errors.Is(err, ErrNotFound))
	})
	t.Run("SetState", func(t *testing.T) {
		err := s.SetState("x", StateVirtual, 0)
		assert.True(t, errors.Is(err, ErrNotFound))
	})
	t.Run("TouchAccess", func(t *testing.T) {
		err := s.TouchAccess("x", 1)
		assert.True(t, errors.Is(err, ErrNotFound))
	})
	t.Run("GetLink", func(t *testing.T) {
		_, err := s.GetLink("x", 1)
		assert.True(t, errors.Is(err, ErrNotFound))
	})
	t.Run("DeleteRelease", func(t *testing.T) {
		err := s.DeleteRelease("x")
		assert.True(t, errors.Is(err, ErrNotFound))
	})
}
