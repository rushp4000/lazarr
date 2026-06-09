package symlink_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/config"
	"github.com/rushp4000/lazarr/internal/symlink"
)

// helpers

func newManager(t *testing.T) (symlink.Manager, string, string) {
	t.Helper()
	downloadDir := t.TempDir()
	fuseMount := t.TempDir()
	m := symlink.New(config.Paths{
		DownloadDir: downloadDir,
		FuseMount:   fuseMount,
	}, config.Ownership{})
	return m, downloadDir, fuseMount
}

func release(hash, name, category string) *catalog.Release {
	return &catalog.Release{Hash: hash, Name: name, Category: category}
}

func files(hash string, relPaths ...string) []catalog.File {
	out := make([]catalog.File, len(relPaths))
	for i, p := range relPaths {
		out[i] = catalog.File{Hash: hash, FileID: i, RelPath: p, Size: 1024}
	}
	return out
}

// assertSymlink checks that linkPath exists as a symlink pointing at wantTarget.
func assertSymlink(t *testing.T, linkPath, wantTarget string) {
	t.Helper()
	info, err := os.Lstat(linkPath)
	require.NoError(t, err, "lstat %q", linkPath)
	require.NotZero(t, info.Mode()&os.ModeSymlink, "expected symlink at %q", linkPath)
	got, err := os.Readlink(linkPath)
	require.NoError(t, err)
	assert.Equal(t, wantTarget, got)
}

// TestCreate_SingleFile verifies a single-file release creates the correct symlink.
func TestCreate_SingleFile(t *testing.T) {
	m, dl, fuse := newManager(t)
	r := release("aabbcc", "My.Movie.2024", "radarr_hin")
	fs_ := files("aabbcc", "My.Movie.2024.mkv")

	require.NoError(t, m.Create(r, fs_))

	linkPath := filepath.Join(dl, "radarr_hin", "My.Movie.2024", "My.Movie.2024.mkv")
	wantTarget := filepath.Join(fuse, "aabbcc", "My.Movie.2024.mkv")
	assertSymlink(t, linkPath, wantTarget)
}

// TestCreate_NestedRelPath verifies that nested subdirectories in RelPath are created.
func TestCreate_NestedRelPath(t *testing.T) {
	m, dl, fuse := newManager(t)
	r := release("nested01", "Show.S01", "sonarr_rd")
	fs_ := files("nested01", "Season 01/Episode01.mkv", "Season 01/Episode02.mkv")

	require.NoError(t, m.Create(r, fs_))

	for _, relPath := range []string{"Season 01/Episode01.mkv", "Season 01/Episode02.mkv"} {
		linkPath := filepath.Join(dl, "sonarr_rd", "Show.S01", relPath)
		wantTarget := filepath.Join(fuse, "nested01", relPath)
		assertSymlink(t, linkPath, wantTarget)
	}
}

// TestCreate_MultiFile verifies multi-file releases create all symlinks.
func TestCreate_MultiFile(t *testing.T) {
	m, dl, fuse := newManager(t)
	r := release("multi01", "Pack.2024", "radarr_rd")
	relPaths := []string{"file1.mkv", "file2.mkv", "extras/bonus.mkv"}
	fs_ := files("multi01", relPaths...)

	require.NoError(t, m.Create(r, fs_))

	for _, relPath := range relPaths {
		assertSymlink(t,
			filepath.Join(dl, "radarr_rd", "Pack.2024", relPath),
			filepath.Join(fuse, "multi01", relPath),
		)
	}
}

// TestCreate_Idempotent verifies that re-running Create on an existing tree is a no-op.
func TestCreate_Idempotent(t *testing.T) {
	m, dl, fuse := newManager(t)
	r := release("idem01", "Movie.Name", "radarr_4k")
	fs_ := files("idem01", "Movie.Name.mkv")

	require.NoError(t, m.Create(r, fs_))
	require.NoError(t, m.Create(r, fs_), "second Create must not error")

	assertSymlink(t,
		filepath.Join(dl, "radarr_4k", "Movie.Name", "Movie.Name.mkv"),
		filepath.Join(fuse, "idem01", "Movie.Name.mkv"),
	)
}

// TestCreate_ReplacesStaleSymlink verifies that a symlink with a wrong target is replaced.
func TestCreate_ReplacesStaleSymlink(t *testing.T) {
	m, dl, fuse := newManager(t)
	r := release("stale01", "Movie.2023", "radarr_hin")
	fs_ := files("stale01", "Movie.2023.mkv")
	linkPath := filepath.Join(dl, "radarr_hin", "Movie.2023", "Movie.2023.mkv")

	// Pre-plant a stale symlink pointing somewhere wrong.
	require.NoError(t, os.MkdirAll(filepath.Dir(linkPath), 0o755))
	require.NoError(t, os.Symlink("/wrong/target", linkPath))

	require.NoError(t, m.Create(r, fs_))

	wantTarget := filepath.Join(fuse, "stale01", "Movie.2023.mkv")
	assertSymlink(t, linkPath, wantTarget)
}

// TestCreate_RefusesToClobberRealFile verifies that a real file at the link path causes an error.
func TestCreate_RefusesToClobberRealFile(t *testing.T) {
	m, dl, _ := newManager(t)
	r := release("clobber01", "Movie.Clobber", "radarr_hin")
	fs_ := files("clobber01", "Movie.Clobber.mkv")
	linkPath := filepath.Join(dl, "radarr_hin", "Movie.Clobber", "Movie.Clobber.mkv")

	// Pre-plant a real file at the would-be symlink path.
	require.NoError(t, os.MkdirAll(filepath.Dir(linkPath), 0o755))
	require.NoError(t, os.WriteFile(linkPath, []byte("real content"), 0o644))

	err := m.Create(r, fs_)
	require.Error(t, err, "Create must error when a real file occupies the link path")

	// The real file must still be intact.
	data, err2 := os.ReadFile(linkPath)
	require.NoError(t, err2)
	assert.Equal(t, "real content", string(data))
}

// TestRemove_DeletesOnlyTargetHash sets up two releases, removes one, and verifies
// the other is untouched.
func TestRemove_DeletesOnlyTargetHash(t *testing.T) {
	m, dl, fuse := newManager(t)

	r1 := release("hash111", "Movie.One", "radarr_hin")
	f1 := files("hash111", "Movie.One.mkv")
	r2 := release("hash222", "Movie.Two", "radarr_hin")
	f2 := files("hash222", "Movie.Two.mkv")

	require.NoError(t, m.Create(r1, f1))
	require.NoError(t, m.Create(r2, f2))

	// Remove only hash111.
	require.NoError(t, m.Remove("hash111"))

	// hash111 symlink must be gone.
	_, err := os.Lstat(filepath.Join(dl, "radarr_hin", "Movie.One", "Movie.One.mkv"))
	assert.True(t, os.IsNotExist(err), "hash111 symlink should be removed")

	// hash222 symlink must survive.
	assertSymlink(t,
		filepath.Join(dl, "radarr_hin", "Movie.Two", "Movie.Two.mkv"),
		filepath.Join(fuse, "hash222", "Movie.Two.mkv"),
	)
}

// TestRemove_PrunesEmptyDirs verifies that name and category dirs are removed when empty.
func TestRemove_PrunesEmptyDirs(t *testing.T) {
	m, dl, _ := newManager(t)
	r := release("prune01", "Prunable.Movie", "radarr_hin")
	fs_ := files("prune01", "Prunable.Movie.mkv")

	require.NoError(t, m.Create(r, fs_))
	require.NoError(t, m.Remove("prune01"))

	// Name dir should be gone.
	_, err := os.Lstat(filepath.Join(dl, "radarr_hin", "Prunable.Movie"))
	assert.True(t, os.IsNotExist(err), "name dir should be pruned")

	// Category dir should be gone (it's empty now).
	_, err = os.Lstat(filepath.Join(dl, "radarr_hin"))
	assert.True(t, os.IsNotExist(err), "category dir should be pruned when empty")
}

// TestRemove_DoesNotPruneDirWithOtherContent verifies that a category dir is NOT removed
// when another release still exists under it.
func TestRemove_DoesNotPruneDirWithOtherContent(t *testing.T) {
	m, dl, _ := newManager(t)

	r1 := release("keepa", "Movie.Keep", "radarr_hin")
	f1 := files("keepa", "Movie.Keep.mkv")
	r2 := release("gone1", "Movie.Gone", "radarr_hin")
	f2 := files("gone1", "Movie.Gone.mkv")

	require.NoError(t, m.Create(r1, f1))
	require.NoError(t, m.Create(r2, f2))
	require.NoError(t, m.Remove("gone1"))

	// Category dir must still exist because Movie.Keep is still there.
	info, err := os.Lstat(filepath.Join(dl, "radarr_hin"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

// TestRemove_AbsentHashIsNoOp verifies that removing a hash with no symlinks returns nil.
func TestRemove_AbsentHashIsNoOp(t *testing.T) {
	m, _, _ := newManager(t)
	require.NoError(t, m.Remove("deadbeef"), "removing absent hash must be a no-op")
}

// TestRemove_NeverDeletesRealFile plants a real file in the tree and confirms
// Remove leaves it untouched (it cannot match the symlink-target prefix check).
func TestRemove_NeverDeletesRealFile(t *testing.T) {
	m, dl, _ := newManager(t)

	// Create a release so the category/name dirs exist.
	r := release("realfile01", "Movie.RealFile", "radarr_hin")
	fs_ := files("realfile01", "Movie.RealFile.mkv")
	require.NoError(t, m.Create(r, fs_))

	// Plant a real file inside the name dir (not a symlink).
	realFilePath := filepath.Join(dl, "radarr_hin", "Movie.RealFile", "README.txt")
	require.NoError(t, os.WriteFile(realFilePath, []byte("do not delete"), 0o644))

	require.NoError(t, m.Remove("realfile01"))

	// The real file must still be intact.
	data, err := os.ReadFile(realFilePath)
	require.NoError(t, err)
	assert.Equal(t, "do not delete", string(data))
}

// TestCreate_PathTraversal_Category rejects a category with ".." traversal.
func TestCreate_PathTraversal_Category(t *testing.T) {
	m, _, _ := newManager(t)
	r := release("trav01", "Movie", "../escaped_category")
	fs_ := files("trav01", "Movie.mkv")
	err := m.Create(r, fs_)
	require.Error(t, err, "category with path separator must be rejected")
}

// TestCreate_PathTraversal_Name rejects a name with ".." traversal.
func TestCreate_PathTraversal_Name(t *testing.T) {
	m, _, _ := newManager(t)
	r := release("trav02", "../../escaped_name", "radarr_hin")
	fs_ := files("trav02", "file.mkv")
	err := m.Create(r, fs_)
	require.Error(t, err, "name with .. must be rejected")
}

// TestCreate_PathTraversal_RelPath rejects a RelPath with ".." escaping.
func TestCreate_PathTraversal_RelPath(t *testing.T) {
	m, _, _ := newManager(t)
	r := release("trav03", "Movie", "radarr_hin")
	fs_ := []catalog.File{
		{Hash: "trav03", FileID: 0, RelPath: "../../etc/passwd", Size: 1},
	}
	err := m.Create(r, fs_)
	require.Error(t, err, "RelPath traversing parent must be rejected")
}

// TestCreate_AbsoluteRelPath rejects a RelPath that is absolute.
func TestCreate_AbsoluteRelPath(t *testing.T) {
	m, _, _ := newManager(t)
	r := release("abs01", "Movie", "radarr_hin")
	fs_ := []catalog.File{
		{Hash: "abs01", FileID: 0, RelPath: "/etc/passwd", Size: 1},
	}
	err := m.Create(r, fs_)
	require.Error(t, err, "absolute RelPath must be rejected")
}

// TestCreate_NilRelease returns an error for a nil release pointer.
func TestCreate_NilRelease(t *testing.T) {
	m, _, _ := newManager(t)
	err := m.Create(nil, nil)
	require.Error(t, err)
}

// TestRemove_EmptyHashErrors verifies Remove with an empty hash is an error.
func TestRemove_EmptyHashErrors(t *testing.T) {
	m, _, _ := newManager(t)
	err := m.Remove("")
	require.Error(t, err)
}
