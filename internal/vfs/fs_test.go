package vfs_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/vfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeStore implements catalog.Store.  Only GetRelease is meaningfully
// implemented; all other methods are no-ops or return zero values so the
// interface is satisfied without a real SQLite database.
type fakeStore struct {
	mu       sync.RWMutex
	releases map[string]*catalog.Release
	files    map[string][]catalog.File
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		releases: make(map[string]*catalog.Release),
		files:    make(map[string][]catalog.File),
	}
}

func (s *fakeStore) addRelease(r *catalog.Release, files []catalog.File) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases[r.Hash] = r
	s.files[r.Hash] = files
}

func (s *fakeStore) UpsertRelease(r *catalog.Release, files []catalog.File) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases[r.Hash] = r
	s.files[r.Hash] = files
	return nil
}

func (s *fakeStore) GetRelease(hash string) (*catalog.Release, []catalog.File, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.releases[hash]
	if !ok {
		return nil, nil, nil
	}
	// Return a defensive copy of the files slice.
	cp := make([]catalog.File, len(s.files[hash]))
	copy(cp, s.files[hash])
	return r, cp, nil
}

func (s *fakeStore) ListByCategory(_ string) ([]*catalog.Release, error) { return nil, nil }
func (s *fakeStore) SetState(_ string, _ catalog.State, _ int64) error   { return nil }
func (s *fakeStore) TouchAccess(_ string, _ int64) error                 { return nil }
func (s *fakeStore) IdleCandidates(_ int64) ([]*catalog.Release, error)  { return nil, nil }
func (s *fakeStore) OverMaxHold(_ int64) ([]*catalog.Release, error)     { return nil, nil }
func (s *fakeStore) MaterializedIDs() ([]int64, error)                   { return nil, nil }
func (s *fakeStore) GetLink(_ string, _ int) (*catalog.DLLink, error)    { return nil, nil }
func (s *fakeStore) SetLink(_ *catalog.DLLink) error                     { return nil }
func (s *fakeStore) DeleteRelease(_ string) error                        { return nil }
func (s *fakeStore) Close() error                                        { return nil }

// fakeMat implements vfs.Materializer.  ReadAt records every call.  By
// default it fills dest with a repeated byte value (0xAB) so callers can
// verify which bytes they received.
type fakeMat struct {
	mu    sync.Mutex
	calls []readCall
	// errAfter, if > 0, makes ReadAt return an error after this many calls.
	errAfter int
	callsCnt int
}

type readCall struct {
	Hash   string
	FileID int
	Off    int64
	Len    int
}

func (m *fakeMat) ReadAt(hash string, fileID int, p []byte, off int64) (int, error) {
	m.mu.Lock()
	m.calls = append(m.calls, readCall{Hash: hash, FileID: fileID, Off: off, Len: len(p)})
	m.callsCnt++
	cnt := m.callsCnt
	ea := m.errAfter
	m.mu.Unlock()

	if ea > 0 && cnt > ea {
		return 0, errors.New("fake: read error")
	}
	for i := range p {
		p[i] = 0xAB
	}
	return len(p), nil
}

func (m *fakeMat) Release(_ string) error { return nil }

func (m *fakeMat) lastCall() (readCall, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		return readCall{}, false
	}
	return m.calls[len(m.calls)-1], true
}

func (m *fakeMat) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// skipIfNoFUSE skips the test if /dev/fuse is absent or if the current
// process cannot open it, so CI environments without FUSE still pass.
func skipIfNoFUSE(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); os.IsNotExist(err) {
		t.Skip("FUSE not available: /dev/fuse absent")
	}
}

// mountTestFS creates a temp dir, mounts the fake FS onto it, and returns
// the FS handle.  t.Cleanup registers unmount.  If mount fails (e.g. due to
// missing privileges), the test is skipped.
func mountTestFS(t *testing.T, store *fakeStore, mat *fakeMat) (mountDir string, fsys *vfs.FS) {
	t.Helper()
	skipIfNoFUSE(t)

	dir := t.TempDir()
	fsys = vfs.New(dir, store, mat)
	if err := fsys.Mount(); err != nil {
		t.Skipf("FUSE mount failed (needs SYS_ADMIN / /dev/fuse): %v", err)
	}

	t.Cleanup(func() {
		if err := fsys.Close(); err != nil {
			t.Logf("vfs close: %v", err)
		}
	})

	return dir, fsys
}

// testRelease is a convenience fixture: one release with two files.
func testRelease() (*catalog.Release, []catalog.File) {
	rel := &catalog.Release{
		Hash:      "aabbccddeeff00112233445566778899aabbccdd",
		Name:      "Big Buck Bunny",
		Category:  "radarr_hin",
		TotalSize: 2048,
		State:     catalog.StateVirtual,
	}
	files := []catalog.File{
		{Hash: rel.Hash, FileID: 1, RelPath: "bbb.mkv", Size: 1500},
		{Hash: rel.Hash, FileID: 2, RelPath: "subs.srt", Size: 548},
	}
	return rel, files
}

// ---------------------------------------------------------------------------
// Unit tests that do NOT require a real FUSE mount
// ---------------------------------------------------------------------------

// TestNew_ReturnsNonNil verifies that New does not panic and returns a
// non-nil FS even when the mount directory does not exist (no mount).
func TestNew_ReturnsNonNil(t *testing.T) {
	store := newFakeStore()
	mat := &fakeMat{}
	f := vfs.New("/nonexistent/mount", store, mat)
	require.NotNil(t, f)
}

// TestClose_UnmountedNoError ensures Close on an unmounted FS is a no-op.
func TestClose_UnmountedNoError(t *testing.T) {
	f := vfs.New("/nonexistent/mount", newFakeStore(), &fakeMat{})
	require.NoError(t, f.Close())
	// Idempotent.
	require.NoError(t, f.Close())
}

// ---------------------------------------------------------------------------
// FUSE mount tests (skipped if /dev/fuse absent or unprivileged)
// ---------------------------------------------------------------------------

// TestStat_FileReturnsCatalogSize asserts (a): stat of a known file returns
// the catalog size WITHOUT calling the Materializer.
func TestStat_FileReturnsCatalogSize(t *testing.T) {
	store := newFakeStore()
	mat := &fakeMat{}
	rel, files := testRelease()
	store.addRelease(rel, files)

	mnt, _ := mountTestFS(t, store, mat)

	// Stat the file.
	target := filepath.Join(mnt, rel.Hash, "bbb.mkv")
	info, err := os.Stat(target)
	require.NoError(t, err, "stat should succeed for a known file")

	assert.Equal(t, int64(1500), info.Size(), "size must come from the catalog, not TorBox")
	assert.False(t, info.IsDir())

	// Materializer must NOT have been called.
	assert.Zero(t, mat.callCount(), "stat must not trigger the Materializer")
}

// TestReaddir_ListsFiles asserts (c): readdir of /<hash> returns the correct
// file names as reported by the catalog.
func TestReaddir_ListsFiles(t *testing.T) {
	store := newFakeStore()
	mat := &fakeMat{}
	rel, files := testRelease()
	store.addRelease(rel, files)

	mnt, _ := mountTestFS(t, store, mat)

	entries, err := os.ReadDir(filepath.Join(mnt, rel.Hash))
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	assert.Contains(t, names, "bbb.mkv")
	assert.Contains(t, names, "subs.srt")
	assert.Len(t, names, 2)

	// Readdir must not have triggered the Materializer.
	assert.Zero(t, mat.callCount(), "readdir must not trigger the Materializer")
}

// TestRead_DelegatesToMaterializerWithCorrectArgs asserts (b): a read on a
// file delegates to the Materializer with the correct (hash, fileID, offset,
// length).
func TestRead_DelegatesToMaterializerWithCorrectArgs(t *testing.T) {
	store := newFakeStore()
	mat := &fakeMat{}
	rel, files := testRelease()
	store.addRelease(rel, files)

	mnt, _ := mountTestFS(t, store, mat)

	target := filepath.Join(mnt, rel.Hash, "bbb.mkv")
	f, err := os.Open(target)
	require.NoError(t, err)
	defer f.Close()

	buf := make([]byte, 128)
	// Read 128 bytes at offset 64.
	n, err := f.ReadAt(buf, 64)
	require.NoError(t, err)
	assert.Equal(t, 128, n)

	call, ok := mat.lastCall()
	require.True(t, ok, "Materializer must have been called")

	assert.Equal(t, rel.Hash, call.Hash, "hash must match")
	assert.Equal(t, 1, call.FileID, "fileID must match file 'bbb.mkv' (fileID=1)")
	assert.Equal(t, int64(64), call.Off, "offset must be forwarded")
	assert.Equal(t, 128, call.Len, "length must match buf size")
}

// TestEnoent_UnknownHash asserts (d): accessing an unknown hash returns ENOENT.
func TestEnoent_UnknownHash(t *testing.T) {
	store := newFakeStore()
	mat := &fakeMat{}

	mnt, _ := mountTestFS(t, store, mat)

	_, err := os.Stat(filepath.Join(mnt, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrNotExist), "unknown hash must give ENOENT, got: %v", err)
}

// TestEnoent_UnknownRelPath asserts (d): accessing an unknown rel_path under a
// known hash returns ENOENT.
func TestEnoent_UnknownRelPath(t *testing.T) {
	store := newFakeStore()
	mat := &fakeMat{}
	rel, files := testRelease()
	store.addRelease(rel, files)

	mnt, _ := mountTestFS(t, store, mat)

	_, err := os.Stat(filepath.Join(mnt, rel.Hash, "does_not_exist.mkv"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrNotExist), "unknown file must give ENOENT, got: %v", err)
}

// TestHashDir_IsDirectory verifies that /<hash> reports as a directory.
func TestHashDir_IsDirectory(t *testing.T) {
	store := newFakeStore()
	mat := &fakeMat{}
	rel, files := testRelease()
	store.addRelease(rel, files)

	mnt, _ := mountTestFS(t, store, mat)

	info, err := os.Stat(filepath.Join(mnt, rel.Hash))
	require.NoError(t, err)
	assert.True(t, info.IsDir(), "/<hash> must be reported as a directory")
}

// TestWriteDenied verifies that opening a file for writing returns EROFS.
func TestWriteDenied(t *testing.T) {
	store := newFakeStore()
	mat := &fakeMat{}
	rel, files := testRelease()
	store.addRelease(rel, files)

	mnt, _ := mountTestFS(t, store, mat)

	target := filepath.Join(mnt, rel.Hash, "bbb.mkv")
	_, err := os.OpenFile(target, os.O_WRONLY, 0)
	require.Error(t, err, "write-mode open must be denied")

	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		errno := func() syscall.Errno {
			var target syscall.Errno
			_ = errors.As(pathErr.Err, &target)
			return target
		}() //nolint:forcetypeassert — we check the type
		assert.Equal(t, syscall.EROFS, errno, "expected EROFS for write attempt")
	}
}

// TestMultipleFiles_ReaddirDeterministic checks that concurrent Readdir calls
// return a consistent, sorted listing.
func TestMultipleFiles_ReaddirDeterministic(t *testing.T) {
	store := newFakeStore()
	mat := &fakeMat{}
	rel := &catalog.Release{
		Hash:     "1234567890abcdef1234567890abcdef12345678",
		Name:     "Multi",
		Category: "radarr_hin",
		State:    catalog.StateVirtual,
	}
	files := []catalog.File{
		{Hash: rel.Hash, FileID: 3, RelPath: "zebra.mkv", Size: 100},
		{Hash: rel.Hash, FileID: 1, RelPath: "alpha.mkv", Size: 200},
		{Hash: rel.Hash, FileID: 2, RelPath: "middle.mkv", Size: 150},
	}
	store.addRelease(rel, files)

	mnt, _ := mountTestFS(t, store, mat)

	hashDir := filepath.Join(mnt, rel.Hash)

	// Read the directory twice and assert same order.
	var results [2][]string
	for i := 0; i < 2; i++ {
		entries, err := os.ReadDir(hashDir)
		require.NoError(t, err)
		for _, e := range entries {
			results[i] = append(results[i], e.Name())
		}
	}

	require.Equal(t, results[0], results[1], "Readdir must be deterministic")
	// Should be sorted alphabetically.
	assert.Equal(t, []string{"alpha.mkv", "middle.mkv", "zebra.mkv"}, results[0])
}

// nestedRelease fixtures a release whose files live under a sub-directory, the
// common "<Name>/<file>" layout TorBox returns (and exactly what the live
// canary movie uses).  Before the nested-rel_path fix these all returned
// ENOENT through the FUSE mount.
func nestedRelease() (*catalog.Release, []catalog.File) {
	rel := &catalog.Release{
		Hash:     "dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c",
		Name:     "Big Buck Bunny",
		Category: "radarr_hin",
		State:    catalog.StateVirtual,
	}
	files := []catalog.File{
		{Hash: rel.Hash, FileID: 2, RelPath: "Big Buck Bunny/Big Buck Bunny.mp4", Size: 276134947},
		{Hash: rel.Hash, FileID: 0, RelPath: "Big Buck Bunny/Big Buck Bunny.en.srt", Size: 140},
		{Hash: rel.Hash, FileID: 1, RelPath: "Big Buck Bunny/poster.jpg", Size: 310380},
	}
	return rel, files
}

// TestNestedRelPath_StatAndRead asserts that a file at a nested rel_path is
// reachable through the synthetic intermediate directory: the sub-dir lists,
// stat returns the catalog size, and a read delegates to the Materializer with
// the correct fileID.  This is the regression test for deferred fix #3.
func TestNestedRelPath_StatAndRead(t *testing.T) {
	store := newFakeStore()
	mat := &fakeMat{}
	rel, files := nestedRelease()
	store.addRelease(rel, files)

	mnt, _ := mountTestFS(t, store, mat)

	// The intermediate dir lists under /<hash>.
	top, err := os.ReadDir(filepath.Join(mnt, rel.Hash))
	require.NoError(t, err)
	require.Len(t, top, 1)
	assert.Equal(t, "Big Buck Bunny", top[0].Name())
	assert.True(t, top[0].IsDir(), "the rel_path prefix must appear as a directory")

	// The leaves list inside the synthetic dir.
	subDir := filepath.Join(mnt, rel.Hash, "Big Buck Bunny")
	leaves, err := os.ReadDir(subDir)
	require.NoError(t, err)
	names := make([]string, 0, len(leaves))
	for _, e := range leaves {
		names = append(names, e.Name())
		assert.False(t, e.IsDir())
	}
	assert.ElementsMatch(t, []string{"Big Buck Bunny.mp4", "Big Buck Bunny.en.srt", "poster.jpg"}, names)
	assert.Zero(t, mat.callCount(), "listing must not materialize")

	// Stat the nested .mp4 — catalog size, no Materializer call.
	mp4 := filepath.Join(subDir, "Big Buck Bunny.mp4")
	info, err := os.Stat(mp4)
	require.NoError(t, err)
	assert.Equal(t, int64(276134947), info.Size())
	assert.Zero(t, mat.callCount(), "stat must not materialize")

	// Read forwards to the Materializer with the nested file's fileID (2).
	f, err := os.Open(mp4)
	require.NoError(t, err)
	defer f.Close()
	buf := make([]byte, 64)
	n, err := f.ReadAt(buf, 1024)
	require.NoError(t, err)
	assert.Equal(t, 64, n)

	call, ok := mat.lastCall()
	require.True(t, ok)
	assert.Equal(t, rel.Hash, call.Hash)
	assert.Equal(t, 2, call.FileID, "must resolve to the nested .mp4 (fileID 2)")
	assert.Equal(t, int64(1024), call.Off)
}

// TestNestedRelPath_BogusPathENOENT asserts unknown components at any depth
// still give ENOENT (negative-cache friendly) rather than resolving spuriously.
func TestNestedRelPath_BogusPathENOENT(t *testing.T) {
	store := newFakeStore()
	mat := &fakeMat{}
	rel, files := nestedRelease()
	store.addRelease(rel, files)

	mnt, _ := mountTestFS(t, store, mat)

	_, err := os.Stat(filepath.Join(mnt, rel.Hash, "Big Buck Bunny", "nope.mkv"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrNotExist))

	_, err = os.Stat(filepath.Join(mnt, rel.Hash, "Wrong Dir", "x.mkv"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrNotExist))
}

// TestStatDoesNotMaterialize_RaceDetector runs concurrent stats and verifies
// the materializer is never called.  Run with -race to confirm there are no
// data races.
func TestStatDoesNotMaterialize_RaceDetector(t *testing.T) {
	store := newFakeStore()
	mat := &fakeMat{}
	rel, files := testRelease()
	store.addRelease(rel, files)

	mnt, _ := mountTestFS(t, store, mat)
	target := filepath.Join(mnt, rel.Hash, "bbb.mkv")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_, _ = os.Stat(target)
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()

	assert.Zero(t, mat.callCount(), "concurrent stats must not trigger the Materializer")
}
