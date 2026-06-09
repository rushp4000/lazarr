// Package vfs is the FUSE virtual tree: /<hash>/<rel_path>. Getattr/Lookup/Readdir
// serve names+sizes from the catalog (no TorBox call), so stat/ls/import work without
// materializing. Read triggers the Materializer. Phase 2; built by Agent V (docs/09).
package vfs

// Materializer is what vfs needs from internal/materialize (consumer-defined interface).
type Materializer interface {
	// ReadAt fetches len(p) bytes at off for (hash,fileID), materializing on first
	// access and proxying the byte range. Updates last_access.
	ReadAt(hash string, fileID int, p []byte, off int64) (int, error)
	// Release force-releases a materialized item (used by reapers/shutdown).
	Release(hash string) error
}
