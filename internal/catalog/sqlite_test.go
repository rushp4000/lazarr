package catalog

import (
	"database/sql"
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

// TestMigrateAddsMaterializedAt simulates the canary's pre-existing DB: a release table
// created BEFORE the materialized_at column (B1). OpenSQLite must add the column
// idempotently and backfill pre-existing materialized rows to NOW so they are not instantly
// reap-eligible (materialized_at=0 would read as the epoch).
func TestMigrateAddsMaterializedAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	// Build a legacy DB: the old release schema (no materialized_at) with one materialized
	// row and one virtual row.
	raw, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	_, err = raw.Exec(`
		CREATE TABLE release (
		    hash TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', category TEXT NOT NULL DEFAULT '',
		    magnet TEXT NOT NULL DEFAULT '', total_size INTEGER NOT NULL DEFAULT 0,
		    state TEXT NOT NULL DEFAULT 'virtual', cached INTEGER NOT NULL DEFAULT 0,
		    torbox_id INTEGER NOT NULL DEFAULT 0, added_on INTEGER NOT NULL DEFAULT 0,
		    last_access INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL DEFAULT 0
		);
		INSERT INTO release (hash, state, torbox_id, added_on) VALUES ('mat', 'materialized', 5, 100);
		INSERT INTO release (hash, state) VALUES ('virt', 'virtual');`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	// Open via the production path → migration adds the column + backfills.
	s, err := OpenSQLite(path)
	require.NoError(t, err, "opening a legacy DB must migrate cleanly")
	t.Cleanup(func() { s.Close() })

	mat, _, err := s.GetRelease("mat")
	require.NoError(t, err)
	assert.Greater(t, mat.MaterializedAt, int64(0), "pre-existing materialized row backfilled to NOW")

	virt, _, err := s.GetRelease("virt")
	require.NoError(t, err)
	assert.Equal(t, int64(0), virt.MaterializedAt, "virtual row stays at 0")

	// The just-backfilled row must NOT be over-max-hold against a recent cutoff (it would be
	// if materialized_at were left at the epoch) — the whole point of the backfill.
	over, err := s.OverMaxHold(50) // cutoff well before NOW
	require.NoError(t, err)
	assert.Empty(t, over, "freshly-backfilled row is not instantly reap-eligible")

	// Re-open to prove the ALTER is idempotent on an already-migrated DB.
	require.NoError(t, s.Close())
	s2, err := OpenSQLite(path)
	require.NoError(t, err, "re-open of migrated DB must not fail")
	require.NoError(t, s2.Close())
}

// ---- UpsertRelease + GetRelease round-trip ---------------------------------

func TestUpsertGetRoundTrip(t *testing.T) {
	s := openTestDB(t)
	hash := "aabbcc001122334455667788990011223344556677"
	r := sampleRelease(hash)
	r.MaterializedAt = 1717000000 // persisted verbatim by UpsertRelease (B1 column)
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
	assert.Equal(t, r.MaterializedAt, got.MaterializedAt)
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

// ---- OverMaxHold (B1: measured from materialized_at, not added_on) ----------

func TestOverMaxHold(t *testing.T) {
	s := openTestDB(t)

	// materialized_at ascends while added_on DESCENDS — proving the filter keys off
	// materialized_at (grab order is the inverse and must be ignored). Rows are upserted
	// directly as materialized with explicit materialized_at (SetState would stamp NOW).
	releases := []struct {
		hash           string
		addedOn        int64
		materializedAt int64
	}{
		{"hold0000000000000000000000000000000001", 9000, 1000},
		{"hold0000000000000000000000000000000002", 8000, 2000},
		{"hold0000000000000000000000000000000003", 7000, 3000},
	}
	for _, tc := range releases {
		r := sampleRelease(tc.hash)
		r.AddedOn = tc.addedOn
		r.State = StateMaterialized
		r.TorBoxID = 200
		r.MaterializedAt = tc.materializedAt
		require.NoError(t, s.UpsertRelease(r, nil))
	}

	// Virtual release must NOT appear, even with an ancient materialized_at.
	virt := "hold_virt00000000000000000000000000001"
	rv := sampleRelease(virt)
	rv.MaterializedAt = 1
	require.NoError(t, s.UpsertRelease(rv, nil))

	// A materialized row with materialized_at=0 must NOT appear (the > 0 guard): without it
	// the epoch reads as ancient and the release would be reaped mid-grab (B1).
	guard := "hold_guard0000000000000000000000000001"
	rg := sampleRelease(guard)
	rg.State = StateMaterialized
	rg.TorBoxID = 201
	rg.MaterializedAt = 0
	require.NoError(t, s.UpsertRelease(rg, nil))

	// before=2500 → materialized_at 1000 and 2000 are < 2500 (added_on order is opposite).
	got, err := s.OverMaxHold(2500)
	require.NoError(t, err)
	assert.Len(t, got, 2)

	// before=1000 → strict less-than; none returned.
	got, err = s.OverMaxHold(1000)
	require.NoError(t, err)
	assert.Empty(t, got)

	// Far-future cutoff still excludes the virtual row and the materialized_at=0 guard row.
	got, err = s.OverMaxHold(1 << 40)
	require.NoError(t, err)
	assert.Len(t, got, 3, "only the three stamped materialized rows are candidates")
}

// TestMaterializedAtStampedBySetState proves SetState stamps materialized_at on entry to
// StateMaterialized and zeroes it on exit (B1), so the max-hold window is measured from
// materialize time and a released row is never an over-max-hold candidate.
func TestMaterializedAtStampedBySetState(t *testing.T) {
	s := openTestDB(t)
	hash := "matat000000000000000000000000000000001"
	r := sampleRelease(hash)
	r.AddedOn = 1 // ancient grab
	require.NoError(t, s.UpsertRelease(r, nil))

	got, _, err := s.GetRelease(hash)
	require.NoError(t, err)
	assert.Equal(t, int64(0), got.MaterializedAt, "virtual release has no materialized_at")

	require.NoError(t, s.SetState(hash, StateMaterialized, 7))
	got, _, err = s.GetRelease(hash)
	require.NoError(t, err)
	assert.Greater(t, got.MaterializedAt, int64(0), "materialized_at stamped on entry")

	// Leaving materialized zeroes it; OverMaxHold then never returns it.
	require.NoError(t, s.SetState(hash, StateVirtual, 0))
	got, _, err = s.GetRelease(hash)
	require.NoError(t, err)
	assert.Equal(t, int64(0), got.MaterializedAt, "materialized_at zeroed on exit")

	over, err := s.OverMaxHold(1 << 40)
	require.NoError(t, err)
	assert.Empty(t, over, "released (virtual) release is never over-max-hold")
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

// ---- ListReleases -----------------------------------------------------------

func TestListReleases_All(t *testing.T) {
	s := openTestDB(t)
	hashes := []string{
		"list0000000000000000000000000000000001",
		"list0000000000000000000000000000000002",
		"list0000000000000000000000000000000003",
	}
	for i, h := range hashes {
		r := sampleRelease(h)
		r.Name = "Movie " + string(rune('A'+i))
		require.NoError(t, s.UpsertRelease(r, nil))
	}

	got, total, err := s.ListReleases(ReleaseFilter{})
	require.NoError(t, err)
	assert.Equal(t, 3, total)
	assert.Len(t, got, 3)
}

func TestListReleases_QueryFilter(t *testing.T) {
	s := openTestDB(t)
	r1 := sampleRelease("listq000000000000000000000000000000001")
	r1.Name = "Big Buck Bunny"
	r2 := sampleRelease("listq000000000000000000000000000000002")
	r2.Name = "Elephants Dream"
	require.NoError(t, s.UpsertRelease(r1, nil))
	require.NoError(t, s.UpsertRelease(r2, nil))

	got, total, err := s.ListReleases(ReleaseFilter{Q: "buck"})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, got, 1)
	assert.Equal(t, "Big Buck Bunny", got[0].Name)
}

func TestListReleases_StateFilter(t *testing.T) {
	s := openTestDB(t)
	rv := sampleRelease("lists000000000000000000000000000000001")
	require.NoError(t, s.UpsertRelease(rv, nil)) // virtual (default)
	rm := sampleRelease("lists000000000000000000000000000000002")
	require.NoError(t, s.UpsertRelease(rm, nil))
	require.NoError(t, s.SetState(rm.Hash, StateMaterialized, 999))

	got, total, err := s.ListReleases(ReleaseFilter{State: StateMaterialized})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, got, 1)
	assert.Equal(t, StateMaterialized, got[0].State)
}

func TestListReleases_CategoryFilter(t *testing.T) {
	s := openTestDB(t)
	r1 := sampleRelease("listc000000000000000000000000000000001")
	r1.Category = "sonarr_hd"
	r2 := sampleRelease("listc000000000000000000000000000000002")
	r2.Category = "radarr_hin"
	require.NoError(t, s.UpsertRelease(r1, nil))
	require.NoError(t, s.UpsertRelease(r2, nil))

	got, total, err := s.ListReleases(ReleaseFilter{Category: "sonarr_hd"})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, got, 1)
	assert.Equal(t, "sonarr_hd", got[0].Category)
}

func TestListReleases_Pagination(t *testing.T) {
	s := openTestDB(t)
	for i := 0; i < 5; i++ {
		r := sampleRelease("listp" + string(rune('0'+i)) + "000000000000000000000000000000001")
		require.NoError(t, s.UpsertRelease(r, nil))
	}

	// page 1
	got, total, err := s.ListReleases(ReleaseFilter{Limit: 3, Offset: 0})
	require.NoError(t, err)
	assert.Equal(t, 5, total)
	assert.Len(t, got, 3)

	// page 2
	got2, total2, err := s.ListReleases(ReleaseFilter{Limit: 3, Offset: 3})
	require.NoError(t, err)
	assert.Equal(t, 5, total2)
	assert.Len(t, got2, 2)
}
