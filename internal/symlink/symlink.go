// Package symlink manages the category symlink tree the arr imports from.
// <download_dir>/<category>/<name>/<rel_path> -> <fuse_mount>/<hash>/<rel_path>.
// See docs/03 (Path model) + docs/05 §5. Built by Agent S (docs/09).
package symlink

import "github.com/rushp4000/lazarr/internal/catalog"

// Manager owns the symlink tree.
type Manager interface {
	// Create makes the category symlinks for a release's files, pointing into the
	// FUSE virtual tree. Must be idempotent and create nested dirs as needed.
	Create(r *catalog.Release, files []catalog.File) error
	// Remove deletes a release's symlinks (never follows into real files).
	Remove(hash string) error
}
