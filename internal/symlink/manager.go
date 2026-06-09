// Package symlink manages the category symlink tree the arr imports from.
// Layout: <download_dir>/<category>/<name>/<rel_path> -> <fuse_mount>/<hash>/<rel_path>.
// See docs/03 (Path model) and docs/05 §5.
package symlink

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/rushp4000/lazarr/internal/catalog"
	"github.com/rushp4000/lazarr/internal/config"
)

// manager is the concrete implementation of Manager.
type manager struct {
	downloadDir string // abs path: arr's qBit save path root
	fuseMount   string // abs path: FUSE virtual tree root
}

// New returns a Manager that writes symlinks under paths.DownloadDir pointing
// into paths.FuseMount. Both paths should be absolute; they are cleaned but
// not validated against the filesystem — the caller must ensure they exist.
func New(paths config.Paths) Manager {
	return &manager{
		downloadDir: filepath.Clean(paths.DownloadDir),
		fuseMount:   filepath.Clean(paths.FuseMount),
	}
}

// Create builds the symlink tree for every file in the release.
//
// For each file f, it creates:
//
//	<DownloadDir>/<Category>/<Name>/<f.RelPath>  ->  <FuseMount>/<hash>/<f.RelPath>
//
// The function is idempotent:
//   - If a symlink already exists with the correct target, it is left in place.
//   - If a symlink exists with a wrong target, it is atomically replaced.
//   - If a real file/directory exists at the destination path, an error is returned
//     (we refuse to clobber non-symlink filesystem objects we didn't create).
//
// Path-traversal in Category, Name, or RelPath is rejected: any component
// that, after joining and cleaning, escapes DownloadDir is an error.
func (m *manager) Create(r *catalog.Release, files []catalog.File) error {
	if r == nil {
		return fmt.Errorf("symlink.Create: nil release")
	}

	// Validate and sanitize the release-level path components.
	category, err := safeComponent(r.Category)
	if err != nil {
		return fmt.Errorf("symlink.Create: category: %w", err)
	}
	name, err := safeComponent(r.Name)
	if err != nil {
		return fmt.Errorf("symlink.Create: name: %w", err)
	}

	releaseDir := filepath.Join(m.downloadDir, category, name)

	for _, f := range files {
		relPath, err := safeRelPath(f.RelPath)
		if err != nil {
			return fmt.Errorf("symlink.Create: file %q: %w", f.RelPath, err)
		}

		linkPath := filepath.Join(releaseDir, relPath)

		// Guard: linkPath must stay inside DownloadDir.
		if err := mustBeUnder(m.downloadDir, linkPath); err != nil {
			return fmt.Errorf("symlink.Create: file %q: %w", f.RelPath, err)
		}

		target := filepath.Join(m.fuseMount, r.Hash, relPath)

		if err := m.createSymlink(linkPath, target); err != nil {
			return fmt.Errorf("symlink.Create: %q -> %q: %w", linkPath, target, err)
		}
	}
	return nil
}

// createSymlink creates the symlink at linkPath pointing to target, creating
// intermediate directories as needed. Idempotency rules:
//   - Correct symlink already present → no-op.
//   - Wrong symlink present → remove and recreate.
//   - Real file/dir present → error (refuse to clobber).
func (m *manager) createSymlink(linkPath, target string) error {
	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	info, err := os.Lstat(linkPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("lstat: %w", err)
	}

	if err == nil {
		// Something exists at linkPath.
		if info.Mode()&fs.ModeSymlink == 0 {
			// It is a real file or directory — we must not clobber it.
			return fmt.Errorf("path exists and is not a symlink (mode %s); refusing to replace", info.Mode())
		}
		// It is a symlink. Check the current target.
		current, err := os.Readlink(linkPath)
		if err != nil {
			return fmt.Errorf("readlink: %w", err)
		}
		if current == target {
			// Already correct — idempotent no-op.
			return nil
		}
		// Wrong target — remove the stale symlink and fall through to create.
		if err := os.Remove(linkPath); err != nil {
			return fmt.Errorf("remove stale symlink: %w", err)
		}
	}

	return os.Symlink(target, linkPath)
}

// Remove deletes all symlinks whose target resolves under <FuseMount>/<hash>/,
// then prunes any now-empty name and category directories.
//
// Approach (documented): Remove only receives the hash. To locate its links we
// walk <DownloadDir>/*/*/ — two levels of subdirectory (category/name) — and
// inspect every symlink target. Any symlink whose target has the prefix
// <FuseMount>/<hash>/ is removed. We never follow symlinks into real content
// and never delete real files or non-empty directories.
//
// Removing a hash that has no symlinks (absent or already cleaned up) is a
// no-op and returns nil.
func (m *manager) Remove(hash string) error {
	if hash == "" {
		return fmt.Errorf("symlink.Remove: empty hash")
	}

	// The prefix that every target for this hash must start with.
	// Include trailing separator to prevent prefix collision between e.g.
	// "abc" and "abcdef".
	fuseHashDir := filepath.Join(m.fuseMount, hash) + string(filepath.Separator)

	// Walk DownloadDir at depth ≤ 3 (category / name / ...files...).
	// We use filepath.WalkDir which gives us the entry type without following
	// symlinks for directories.
	var toRemove []string
	err := filepath.WalkDir(m.downloadDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Ignore permission / transient errors on individual entries.
			return nil
		}
		if path == m.downloadDir {
			return nil
		}

		// Compute depth relative to DownloadDir.
		rel, err := filepath.Rel(m.downloadDir, path)
		if err != nil {
			return nil
		}
		depth := strings.Count(rel, string(filepath.Separator)) + 1

		// Directories beyond depth 2 (category/name) are NOT descended into
		// by normal means — but WalkDir already descends. We only care about
		// symlinks inside name dirs (depth >= 3). Skip descending into
		// directories beyond depth 2? No — files may be nested deeper.
		// We allow the walk to descend freely; we only act on symlinks.

		// We only process symlinks.
		if d.Type()&fs.ModeSymlink == 0 {
			// Skip non-symlinks. Let WalkDir continue descending dirs.
			return nil
		}

		// Depth guard: symlinks must be at depth >= 3 (category/name/relpath).
		if depth < 3 {
			return nil
		}

		// Read the symlink target without following it.
		target, err := os.Readlink(path)
		if err != nil {
			return nil // skip unreadable
		}

		// Normalise the target before prefix-checking.
		target = filepath.Clean(target)
		targetWithSep := target + string(filepath.Separator)

		// Match: target is exactly fuseHashDir (minus trailing sep) or under it.
		if target == filepath.Join(m.fuseMount, hash) || strings.HasPrefix(targetWithSep, fuseHashDir) {
			toRemove = append(toRemove, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("symlink.Remove: walk: %w", err)
	}

	// Remove matching symlinks.
	for _, p := range toRemove {
		info, err := os.Lstat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("symlink.Remove: lstat %q: %w", p, err)
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			// Safety: something changed it to a real file between walk and now.
			return fmt.Errorf("symlink.Remove: %q is no longer a symlink; refusing to delete", p)
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("symlink.Remove: remove %q: %w", p, err)
		}
	}

	// Prune empty name dirs (depth 2), then empty category dirs (depth 1).
	pruneEmptyDirs(m.downloadDir, 2)
	pruneEmptyDirs(m.downloadDir, 1)

	return nil
}

// pruneEmptyDirs removes empty directories at exactly targetDepth levels below root.
// It never removes root itself. Errors are silently ignored — pruning is best-effort.
func pruneEmptyDirs(root string, targetDepth int) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		depth := strings.Count(rel, string(filepath.Separator)) + 1
		if depth == targetDepth {
			// Attempt to remove only if empty; os.Remove fails on non-empty dirs.
			_ = os.Remove(path)
			return fs.SkipDir // don't descend into (possibly already removed) dir
		}
		if depth > targetDepth {
			return fs.SkipDir
		}
		return nil
	})
}

// safeComponent rejects a single path component (category or name) that would
// allow traversal outside DownloadDir. It must be a single, clean segment with
// no path separator and no ".." trickery.
func safeComponent(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("empty component")
	}
	// Reject anything containing a path separator.
	if strings.ContainsRune(s, filepath.Separator) || strings.ContainsRune(s, '/') {
		return "", fmt.Errorf("component %q contains path separator", s)
	}
	clean := filepath.Clean(s)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("component %q traverses parent directory", s)
	}
	return clean, nil
}

// safeRelPath validates a relative path from a File record. It is allowed to
// have subdirectory segments but must not escape via ".." and must not be
// absolute.
func safeRelPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty rel_path")
	}
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("rel_path %q is absolute", p)
	}
	clean := filepath.Clean(p)
	// After cleaning, the path must not start with ".." (which would escape).
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("rel_path %q traverses parent directory", p)
	}
	return clean, nil
}

// mustBeUnder returns an error if path is not under (or equal to) root.
// Both must already be cleaned absolute paths.
func mustBeUnder(root, path string) error {
	rootWithSep := root + string(filepath.Separator)
	if path != root && !strings.HasPrefix(path, rootWithSep) {
		return fmt.Errorf("path %q escapes root %q", path, root)
	}
	return nil
}
