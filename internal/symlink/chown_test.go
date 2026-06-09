package symlink

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/config"
)

// chownRec records (path,uid,gid) calls so a test can assert what was chowned
// without needing real root.
type chownRec struct {
	calls []chownCall
}

type chownCall struct {
	path     string
	uid, gid int
}

func (r *chownRec) record(path string, uid, gid int) error {
	r.calls = append(r.calls, chownCall{path, uid, gid})
	return nil
}

func (r *chownRec) paths() []string {
	out := make([]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = c.path
	}
	sort.Strings(out)
	return out
}

// newManagerWithRecorder builds a *manager with chown/lchown wired to recorders.
func newManagerWithRecorder(t *testing.T, puid, pgid int) (*manager, *chownRec, *chownRec, string) {
	t.Helper()
	dl := t.TempDir()
	fuse := t.TempDir()
	dirRec := &chownRec{}
	linkRec := &chownRec{}
	m := New(config.Paths{DownloadDir: dl, FuseMount: fuse},
		config.Ownership{PUID: puid, PGID: pgid}).(*manager)
	m.chown = dirRec.record
	m.lchown = linkRec.record
	return m, dirRec, linkRec, dl
}

// TestCreate_ChownsDirsAndSymlinks verifies that when puid/pgid are set, each
// created directory (os.Chown) and the symlink itself (os.Lchown) are chowned to
// puid:pgid, and that the pre-existing download_dir root is NOT chowned.
func TestCreate_ChownsDirsAndSymlinks(t *testing.T) {
	const puid, pgid = 1003, 1003
	m, dirRec, linkRec, dl := newManagerWithRecorder(t, puid, pgid)

	r := &catalog.Release{Hash: "aabbcc", Name: "My.Movie.2024", Category: "radarr_hin"}
	fls := []catalog.File{{Hash: "aabbcc", FileID: 0, RelPath: "My.Movie.2024.mkv", Size: 10}}

	if err := m.Create(r, fls); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Symlink chowned exactly once via Lchown, to puid:pgid.
	if len(linkRec.calls) != 1 {
		t.Fatalf("lchown calls = %d, want 1 (%+v)", len(linkRec.calls), linkRec.calls)
	}
	wantLink := filepath.Join(dl, "radarr_hin", "My.Movie.2024", "My.Movie.2024.mkv")
	if c := linkRec.calls[0]; c.path != wantLink || c.uid != puid || c.gid != pgid {
		t.Fatalf("lchown = %+v, want {%s %d %d}", c, wantLink, puid, pgid)
	}

	// The two created dirs (category + name) are chowned; the download_dir root is NOT.
	wantCat := filepath.Join(dl, "radarr_hin")
	wantName := filepath.Join(dl, "radarr_hin", "My.Movie.2024")
	got := dirRec.paths()
	want := []string{wantCat, wantName}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("dir chown paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dir chown paths = %v, want %v", got, want)
		}
	}
	for _, c := range dirRec.calls {
		if c.path == dl {
			t.Fatalf("download_dir root %q must not be chowned", dl)
		}
		if c.uid != puid || c.gid != pgid {
			t.Fatalf("dir chown %+v, want uid=%d gid=%d", c, puid, pgid)
		}
	}
}

// TestCreate_ChownDisabled verifies puid/pgid == 0 disables all chown.
func TestCreate_ChownDisabled(t *testing.T) {
	m, dirRec, linkRec, _ := newManagerWithRecorder(t, 0, 0)
	r := &catalog.Release{Hash: "aabbcc", Name: "Movie", Category: "radarr_hin"}
	fls := []catalog.File{{Hash: "aabbcc", FileID: 0, RelPath: "movie.mkv", Size: 1}}
	if err := m.Create(r, fls); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(dirRec.calls) != 0 || len(linkRec.calls) != 0 {
		t.Fatalf("chown should be disabled: dirs=%v links=%v", dirRec.calls, linkRec.calls)
	}
}

// TestCreate_OnlyChownsNewDirs verifies pre-existing intermediate dirs are not
// re-chowned: the category dir created out-of-band keeps its ownership, only the
// freshly created name dir (and the symlink) is chowned.
func TestCreate_OnlyChownsNewDirs(t *testing.T) {
	const puid, pgid = 1003, 1003
	m, dirRec, _, dl := newManagerWithRecorder(t, puid, pgid)

	// Pre-create the category dir so Create only needs to make the name dir.
	cat := filepath.Join(dl, "radarr_hin")
	if err := os.MkdirAll(cat, 0o755); err != nil {
		t.Fatal(err)
	}

	r := &catalog.Release{Hash: "aabbcc", Name: "Movie", Category: "radarr_hin"}
	fls := []catalog.File{{Hash: "aabbcc", FileID: 0, RelPath: "movie.mkv", Size: 1}}
	if err := m.Create(r, fls); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, c := range dirRec.calls {
		if c.path == cat {
			t.Fatalf("pre-existing category dir %q must not be chowned", cat)
		}
	}
	wantName := filepath.Join(cat, "Movie")
	if got := dirRec.paths(); len(got) != 1 || got[0] != wantName {
		t.Fatalf("dir chown paths = %v, want only [%s]", got, wantName)
	}
}

// TestCreate_ReassertsOwnershipOnIdempotentRun verifies a second Create over an
// already-correct symlink re-chowns the link (so links predating puid config
// converge) without recreating it.
func TestCreate_ReassertsOwnershipOnIdempotentRun(t *testing.T) {
	const puid, pgid = 1003, 1003
	m, _, linkRec, _ := newManagerWithRecorder(t, puid, pgid)
	r := &catalog.Release{Hash: "aabbcc", Name: "Movie", Category: "radarr_hin"}
	fls := []catalog.File{{Hash: "aabbcc", FileID: 0, RelPath: "movie.mkv", Size: 1}}

	if err := m.Create(r, fls); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if err := m.Create(r, fls); err != nil {
		t.Fatalf("second Create: %v", err)
	}
	// Two runs -> two lchown calls (create + idempotent reassert).
	if len(linkRec.calls) != 2 {
		t.Fatalf("lchown calls = %d, want 2 (create + reassert)", len(linkRec.calls))
	}
}

// TestChownDirNoFollow_RejectsSymlink proves the chown primitive refuses to follow a
// symlink swapped in place of a directory (S4a): the O_NOFOLLOW open fails with ELOOP
// before any fchown, so root never chowns the attacker-chosen target. No root needed —
// the rejection happens at the open stage.
func TestChownDirNoFollow_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "real")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "swapped")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	err := chownDirNoFollow(link, 12345, 12345)
	if err == nil {
		t.Fatal("chownDirNoFollow must refuse to chown through a symlink")
	}
	// The open is rejected at the open stage (ELOOP from O_NOFOLLOW, or ENOTDIR because a
	// symlink inode is not a directory under O_DIRECTORY) — either way before any fchown, so
	// the symlink's target is never chowned. Crucially it is NOT EPERM, which would mean the
	// open succeeded (followed the link) and fchown was attempted.
	if !errors.Is(err, syscall.ELOOP) && !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("want ELOOP/ENOTDIR (no-follow rejected the symlink before fchown), got %v", err)
	}
}

// TestChownDirNoFollow_RejectsNonDir proves O_DIRECTORY rejects a regular file (a created
// "dir" replaced by a file): the open fails with ENOTDIR before any fchown.
func TestChownDirNoFollow_RejectsNonDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := chownDirNoFollow(file, 12345, 12345)
	if err == nil {
		t.Fatal("chownDirNoFollow must refuse a non-directory")
	}
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("want ENOTDIR (O_DIRECTORY), got %v", err)
	}
}

// TestCreate_DoesNotChownAboveDownloadDir proves that when download_dir does not pre-exist,
// MkdirAll creates it but the chown loop never touches download_dir itself or any ancestor
// above it (S4b) — only the category/name tree strictly below it is chowned.
func TestCreate_DoesNotChownAboveDownloadDir(t *testing.T) {
	const puid, pgid = 1003, 1003
	parent := t.TempDir()
	dd := filepath.Join(parent, "downloads") // deliberately does NOT exist yet
	fuse := t.TempDir()

	dirRec := &chownRec{}
	linkRec := &chownRec{}
	m := New(config.Paths{DownloadDir: dd, FuseMount: fuse},
		config.Ownership{PUID: puid, PGID: pgid}).(*manager)
	m.chown = dirRec.record
	m.lchown = linkRec.record

	r := &catalog.Release{Hash: "aabbcc", Name: "Movie", Category: "radarr_hin"}
	fls := []catalog.File{{Hash: "aabbcc", FileID: 0, RelPath: "movie.mkv", Size: 1}}
	if err := m.Create(r, fls); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ddSep := dd + string(filepath.Separator)
	for _, c := range dirRec.calls {
		if c.path == dd {
			t.Fatalf("download_dir %q itself must not be chowned", dd)
		}
		if !strings.HasPrefix(c.path, ddSep) {
			t.Fatalf("chowned a path not strictly under download_dir: %q", c.path)
		}
	}
	// The category + name dirs (strictly under dd) ARE chowned.
	wantCat := filepath.Join(dd, "radarr_hin")
	wantName := filepath.Join(dd, "radarr_hin", "Movie")
	got := dirRec.paths()
	want := []string{wantCat, wantName}
	sort.Strings(want)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("dir chowns = %v, want %v", got, want)
	}
}

// TestChownRealRoot does a real chown when the test runs as root, proving the
// production os.Chown/os.Lchown wiring sets ownership end to end. Skipped otherwise
// (the injected-recorder tests above cover the id/path plumbing without root).
func TestChownRealRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("not root; chown plumbing is covered by the injected-recorder tests")
	}
	dl := t.TempDir()
	fuse := t.TempDir()
	const puid, pgid = 12345, 12345
	m := New(config.Paths{DownloadDir: dl, FuseMount: fuse},
		config.Ownership{PUID: puid, PGID: pgid})
	r := &catalog.Release{Hash: "aabbcc", Name: "Movie", Category: "radarr_hin"}
	fls := []catalog.File{{Hash: "aabbcc", FileID: 0, RelPath: "movie.mkv", Size: 1}}
	if err := m.Create(r, fls); err != nil {
		t.Fatalf("Create: %v", err)
	}
	link := filepath.Join(dl, "radarr_hin", "Movie", "movie.mkv")
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no syscall.Stat_t on this platform")
	}
	if int(st.Uid) != puid || int(st.Gid) != pgid {
		t.Fatalf("symlink owner = %d:%d, want %d:%d", st.Uid, st.Gid, puid, pgid)
	}
}
