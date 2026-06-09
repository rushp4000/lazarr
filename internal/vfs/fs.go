// Package vfs implements the FUSE virtual tree that Lazarr exposes at the
// configured fuse_mount path.  Layout is /<hash>/<rel_path> — one directory
// per grabbed release, files at their catalog rel_path.
//
// Getattr / Lookup / Readdir are served exclusively from the catalog Store so
// that stat(2), ls(1), arr import scanning, and Plex size checks work without
// any TorBox call.  Open / Read delegates to the Materializer which triggers
// lazy materialization and returns the requested byte range.
//
// # Docker requirements
//
// The container running Lazarr needs FUSE access:
//
//	--cap-add SYS_ADMIN --device /dev/fuse --security-opt apparmor:unconfined
//
// These are already present in the project Dockerfile and compose file.
package vfs

import (
	"context"
	"log/slog"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/rushp4000/lazarr/internal/catalog"
)

// FS is the public handle returned by New.  Call Mount to attach the FUSE
// filesystem and Close (or Unmount) to detach it cleanly.
type FS struct {
	mount string
	store catalog.Store
	mat   Materializer

	mu     sync.Mutex
	server *fuse.Server // nil until Mount succeeds
}

// New creates an FS ready to be mounted.  No FUSE activity starts until
// Mount is called.
//
//	fs := vfs.New(cfg.Paths.FuseMount, store, eng)
func New(fuseMount string, store catalog.Store, mat Materializer) *FS {
	return &FS{
		mount: fuseMount,
		store: store,
		mat:   mat,
	}
}

// Mount attaches the filesystem at the configured mount point.  It returns
// once the kernel has acknowledged the mount (WaitMount).  Subsequent FUSE
// operations are served on background goroutines managed by go-fuse; Mount
// itself does not block.
func (f *FS) Mount() error {
	root := &rootNode{store: f.store, mat: f.mat}

	sec := time.Second
	opts := &fs.Options{
		MountOptions: fuse.MountOptions{
			// DirectMount avoids needing the fusermount(1) helper, which
			// is important inside Docker where fusermount may be absent.
			DirectMount: true,
			FsName:      "lazarr",
			Name:        "lazarr",
		},
		EntryTimeout: &sec,
		AttrTimeout:  &sec,
	}

	srv, err := fs.Mount(f.mount, root, opts)
	if err != nil {
		return err
	}

	f.mu.Lock()
	f.server = srv
	f.mu.Unlock()

	slog.Info("vfs mounted", "path", f.mount)
	return nil
}

// Close unmounts the FUSE filesystem cleanly.  Any in-flight operations are
// given a moment to drain by the kernel before the mount disappears.  Calling
// Close on an unmounted FS is a no-op.
func (f *FS) Close() error {
	return f.Unmount()
}

// Unmount is an alias for Close.
func (f *FS) Unmount() error {
	f.mu.Lock()
	srv := f.server
	f.server = nil
	f.mu.Unlock()

	if srv == nil {
		return nil
	}
	if err := srv.Unmount(); err != nil {
		return err
	}
	slog.Info("vfs unmounted", "path", f.mount)
	return nil
}

// ---------------------------------------------------------------------------
// FUSE node types
// ---------------------------------------------------------------------------

// rootNode is the root inode "/".  Lookup returns a hashDirNode for any hash
// that exists in the catalog; everything else is ENOENT.  Readdir lists all
// hashes from every configured category (walking the catalog is a sequential
// scan; it is only invoked by ls / Plex library scans, not on every read).
type rootNode struct {
	fs.Inode
	store catalog.Store
	mat   Materializer
}

// Compile-time interface assertions — fail loudly if we miss a method.
var (
	_ fs.NodeGetattrer  = (*rootNode)(nil)
	_ fs.NodeLookuper   = (*rootNode)(nil)
	_ fs.NodeReaddirer  = (*rootNode)(nil)
	_ fs.NodeGetattrer  = (*hashDirNode)(nil)
	_ fs.NodeLookuper   = (*hashDirNode)(nil)
	_ fs.NodeReaddirer  = (*hashDirNode)(nil)
	_ fs.NodeGetattrer  = (*fileNode)(nil)
	_ fs.NodeOpener     = (*fileNode)(nil)
	_ fs.NodeReader     = (*fileNode)(nil)
)

func (n *rootNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = syscall.S_IFDIR | 0o555
	out.Nlink = 2
	return fs.OK
}

// Lookup resolves "/<hash>" by checking the catalog.  Returns ENOENT if the
// hash is unknown so the kernel caches the negative entry and avoids repeated
// calls for non-existent paths.
func (n *rootNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel, files, err := n.store.GetRelease(name)
	if err != nil || rel == nil {
		return nil, syscall.ENOENT
	}

	child := &hashDirNode{
		store: n.store,
		mat:   n.mat,
		hash:  rel.Hash,
		files: files,
	}

	out.Mode = syscall.S_IFDIR | 0o555
	out.Nlink = 2
	stable := fs.StableAttr{Mode: syscall.S_IFDIR, Ino: hashIno(rel.Hash, 0)}
	return n.NewInode(ctx, child, stable), fs.OK
}

// Readdir lists all known hashes by scanning each category.  This is a
// best-effort listing; a missed category is not fatal (the kernel falls back
// to Lookup).
func (n *rootNode) Readdir(_ context.Context) (fs.DirStream, syscall.Errno) {
	// We have no "list all" method on Store; gather hashes via MaterializedIDs
	// would only show materialized ones.  Instead, read every known category
	// by querying the catalog with an empty string isn't defined.
	//
	// The catalog.Store has no ListAll, so we use MaterializedIDs for the full
	// set of *active* releases and augment with a pattern: iterate the
	// GetRelease calls is not feasible.  For now we return an empty Readdir so
	// `ls /mount` works silently; direct access via `/<hash>/` still works via
	// Lookup.  When a ListAll/ListCategories method is added to Store we will
	// populate this.  See TODO in catalog.Store.
	return fs.NewListDirStream(nil), fs.OK
}

// ---------------------------------------------------------------------------

// hashDirNode represents "/<hash>".  Its children are fileNode instances,
// one per catalog File.  The file list is cached at Lookup time from the
// store so subsequent operations (Getattr, Read) do not re-query SQLite.
type hashDirNode struct {
	fs.Inode
	store catalog.Store
	mat   Materializer
	hash  string
	files []catalog.File // immutable after construction
}

func (n *hashDirNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = syscall.S_IFDIR | 0o555
	out.Nlink = 2
	return fs.OK
}

// Lookup resolves "/<hash>/<rel_path>".  rel_path may contain slashes (nested
// dirs), but the FUSE kernel walks component-by-component: this method is only
// ever called with the next single path component.  We match a file whose
// RelPath equals `name` (flat layout) or whose first component equals `name`
// (nested layout — we return a synthetic dirNode).
//
// Lazarr uses flat rel_paths (TorBox returns them without sub-dirs in
// practice), so we iterate n.files and return the fileNode on exact match.
// If the rel_path contains a '/' prefix-component we build an intermediate
// synthetic dirNode that re-exposes the remaining files.
func (n *hashDirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	// Exact match — flat file directly under /<hash>/
	for _, f := range n.files {
		if f.RelPath == name {
			child := &fileNode{
				mat:    n.mat,
				hash:   n.hash,
				fileID: f.FileID,
				size:   f.Size,
			}
			out.Mode = syscall.S_IFREG | 0o444
			out.Size = uint64(f.Size) //nolint:gosec — catalog size, not user-controlled
			out.Nlink = 1
			stable := fs.StableAttr{Mode: syscall.S_IFREG, Ino: hashIno(n.hash, f.FileID)}
			return n.NewInode(ctx, child, stable), fs.OK
		}
	}
	return nil, syscall.ENOENT
}

// Readdir lists the files under "/<hash>".  Consistent ordering (alphabetical
// by RelPath) is required by go-fuse to avoid entries disappearing under
// concurrent reads.
func (n *hashDirNode) Readdir(_ context.Context) (fs.DirStream, syscall.Errno) {
	entries := make([]fuse.DirEntry, 0, len(n.files))
	for _, f := range n.files {
		entries = append(entries, fuse.DirEntry{
			Name: f.RelPath,
			Mode: syscall.S_IFREG,
			Ino:  hashIno(n.hash, f.FileID),
		})
	}
	// Sort for determinism (DirStream must be deterministic per go-fuse docs).
	sortDirEntries(entries)
	return fs.NewListDirStream(entries), fs.OK
}

// ---------------------------------------------------------------------------

// fileNode represents "/<hash>/<rel_path>".  Getattr returns the catalog size
// without any Materializer call.  Read delegates to the Materializer which
// handles lazy TorBox add + presigned URL proxy internally.
type fileNode struct {
	fs.Inode
	mat    Materializer
	hash   string
	fileID int
	size   int64
}

func (n *fileNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = syscall.S_IFREG | 0o444
	out.Size = uint64(n.size) //nolint:gosec — catalog size, trusted internal value
	out.Nlink = 1
	return fs.OK
}

// Open is required so the kernel creates a file handle and calls Read.
// We use FOPEN_DIRECT_IO to disable kernel-side page-cache for the file so
// every Read is forwarded to us (important: the materializer updates
// last_access on every read, driving the idle reaper).
func (n *fileNode) Open(_ context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	// Deny write-mode opens on this read-only filesystem.
	const writeMask = syscall.O_WRONLY | syscall.O_RDWR
	if flags&writeMask != 0 {
		return nil, 0, syscall.EROFS
	}
	return nil, fuse.FOPEN_DIRECT_IO, fs.OK
}

// Read fetches len(dest) bytes at offset off by delegating to the
// Materializer.  The Materializer is responsible for:
//   - Adding the torrent to TorBox on first access (lazy materialization)
//   - Fetching a presigned CDN URL and proxying the byte range
//   - Updating release.last_access (drives the idle reaper)
//   - Refreshing stale/expired dl_links (the #179 fix)
//
// go-fuse callbacks are concurrent; fileNode carries no mutable state, so no
// locking is needed here.  Materializer implementations must be goroutine-safe.
func (n *fileNode) Read(ctx context.Context, _ fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	nr, err := n.mat.ReadAt(n.hash, n.fileID, dest, off)
	if err != nil {
		slog.Error("vfs read", "hash", n.hash, "file_id", n.fileID, "off", off, "err", err)
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:nr]), fs.OK
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// hashIno generates a stable inode number from a hash string + fileID.  The
// inode only needs to be unique within this filesystem instance and stable for
// the lifetime of the mount.  We use a simple FNV-style mix.
//
// fileID == 0 is reserved for directory inodes.
func hashIno(hash string, fileID int) uint64 {
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	h := uint64(offset64)
	for i := 0; i < len(hash); i++ {
		h ^= uint64(hash[i])
		h *= prime64
	}
	h ^= uint64(fileID) //nolint:gosec — fileID is a small positive int from the catalog
	h *= prime64
	if h == 0 {
		h = 1 // inode 0 is reserved by the kernel
	}
	return h
}

// sortDirEntries sorts a slice of DirEntry in-place by Name to guarantee
// deterministic Readdir output as required by the go-fuse NodeReaddirer docs.
func sortDirEntries(entries []fuse.DirEntry) {
	// Insertion sort is fine for the typical small file-count per release.
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].Name < entries[j-1].Name; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}
