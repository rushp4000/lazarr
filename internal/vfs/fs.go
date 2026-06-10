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
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
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
			// DirectMountStrict mounts via the raw mount(2) syscall and never
			// falls back to the fusermount3 suid helper. This matters in a
			// container running as an arbitrary uid (e.g. 1003) that is absent
			// from /etc/passwd: fusermount3 aborts with "could not determine
			// username", whereas the raw syscall just needs CAP_SYS_ADMIN.
			DirectMount:       true,
			DirectMountStrict: true,
			// AllowOther lets processes other than the mounting uid read the
			// tree — required so Plex/the *arr suite (often a different uid)
			// can stat and stream files, mirroring decypharr's rclone mount.
			AllowOther: true,
			// max_read for the raw mount is derived from MaxWrite; 0 makes the
			// kernel reject the mount with EINVAL. 1 MiB matches decypharr's
			// rclone mount (max_read=1048576).
			MaxWrite: 1 << 20,
			FsName:   "lazarr",
			Name:     "lazarr",
		},
		EntryTimeout: &sec,
		AttrTimeout:  &sec,
	}

	srv, err := fs.Mount(f.mount, root, opts)
	if err != nil {
		// A prior instance killed with SIGKILL (or any unclean exit) leaves a stale FUSE
		// mount whose kernel connection is dead — stat returns ENOTCONN ("transport endpoint
		// is not connected") and fs.Mount cannot mount over it, so the daemon would
		// crash-loop on every restart. Detach the dead mount (lazy) and retry once so a
		// hard-crashed daemon recovers automatically. This is critical for B2: boot
		// reconciliation (which releases TorBox leftovers) runs only AFTER a successful
		// mount, so a wedged remount would otherwise pin the very leak reconcile must clear.
		if isStaleMount(f.mount) {
			slog.Warn("vfs: clearing stale FUSE mount left by a prior unclean exit, retrying",
				"path", f.mount)
			_ = syscall.Unmount(f.mount, syscall.MNT_DETACH)
			srv, err = fs.Mount(f.mount, root, opts)
		}
		if err != nil {
			return err
		}
	}

	f.mu.Lock()
	f.server = srv
	f.mu.Unlock()

	slog.Info("vfs mounted", "path", f.mount)
	return nil
}

// isStaleMount reports whether path is a dead/stale FUSE mount whose kernel connection is
// gone — the state a SIGKILL'd FUSE daemon leaves behind. The kernel surfaces it as ENOTCONN
// ("transport endpoint is not connected") on any stat of the mount root.
func isStaleMount(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, syscall.ENOTCONN)
}

// Close unmounts the FUSE filesystem cleanly.  Any in-flight operations are
// given a moment to drain by the kernel before the mount disappears.  Calling
// Close on an unmounted FS is a no-op.
func (f *FS) Close() error {
	return f.Unmount()
}

// unmountRetries / unmountBackoff bound the EBUSY retry loop in Unmount before we
// fall back to a lazy (detach) unmount, so shutdown can never hang forever.
const (
	unmountRetries = 5
	unmountBackoff = 200 * time.Millisecond
)

// Unmount is an alias for Close. It first tries the clean go-fuse unmount; if that
// reports EBUSY (in-flight reads still draining), it retries with bounded backoff,
// and as a last resort performs a lazy/detach unmount (MNT_DETACH) so a stuck reader
// can never wedge shutdown. Calling Unmount on an unmounted FS is a no-op.
func (f *FS) Unmount() error {
	f.mu.Lock()
	srv := f.server
	f.server = nil
	mount := f.mount
	f.mu.Unlock()

	if srv == nil {
		return nil
	}

	var lastErr error
	for attempt := 0; attempt < unmountRetries; attempt++ {
		err := srv.Unmount()
		if err == nil {
			slog.Info("vfs unmounted", "path", mount)
			return nil
		}
		lastErr = err
		// Only EBUSY is worth retrying (in-flight reads draining); anything else is
		// terminal for the clean path and we go straight to the lazy fallback.
		if !errors.Is(err, syscall.EBUSY) {
			break
		}
		slog.Warn("vfs unmount busy, retrying", "path", mount, "attempt", attempt+1, "err", err)
		time.Sleep(unmountBackoff)
	}

	// Fallback: lazy/detach unmount. MNT_DETACH detaches the filesystem from the tree
	// immediately and tears it down once the last reference is gone, so shutdown does
	// not block on a stuck reader. This is best-effort and the recovery of last resort.
	slog.Warn("vfs unmount falling back to lazy detach", "path", mount, "err", lastErr)
	if derr := syscall.Unmount(mount, syscall.MNT_DETACH); derr != nil {
		return errors.Join(lastErr, derr)
	}
	slog.Info("vfs lazily unmounted (detached)", "path", mount)
	return nil
}

// Healthy reports whether the FUSE mount is up and serving: the server must be
// attached AND a cheap stat of the mount root must succeed. The materialize engine
// consults this before the idle/max-hold reapers delete from the TorBox account, so
// a transient mount blip can never trigger a mass account-delete (see SetMountHealthy
// in internal/materialize). It is safe to call concurrently.
func (f *FS) Healthy() bool {
	f.mu.Lock()
	srv := f.server
	mount := f.mount
	f.mu.Unlock()

	if srv == nil {
		return false
	}
	// A stat of the mount root reaches the kernel FUSE layer; if the connection is
	// dead/stale the kernel returns an error (e.g. ENOTCONN) rather than succeeding.
	//
	// But a *wedged* FUSE mount (server goroutine stuck, kernel waiting on a reply) can
	// make os.Stat block forever. Healthy() is called from the single reaper goroutine and
	// from /health, so a blocking stat would hang reaping (no idle/max-hold deletes → items
	// held past 30d, a ToS risk) and the admin endpoint. Run the stat off-goroutine under a
	// short deadline; a timeout counts as unhealthy. The goroutine leaks only on a truly
	// wedged stat (it unblocks if/when the mount recovers or is force-unmounted) — an
	// acceptable, bounded leak versus wedging the daemon's liveness path (S3).
	done := make(chan error, 1) // buffered so the goroutine never blocks on send after timeout
	go func() {
		_, err := os.Stat(mount)
		done <- err
	}()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(healthStatTimeout):
		return false
	}
}

// healthStatTimeout bounds the mount-root stat in Healthy() so a wedged FUSE mount cannot
// hang the reaper goroutine or the /health endpoint.
const healthStatTimeout = 2 * time.Second

// ---------------------------------------------------------------------------
// FUSE node types
// ---------------------------------------------------------------------------

// rootNode is the root inode "/".  Lookup returns a dirNode for any hash
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
	_ fs.NodeGetattrer = (*rootNode)(nil)
	_ fs.NodeLookuper  = (*rootNode)(nil)
	_ fs.NodeReaddirer = (*rootNode)(nil)
	_ fs.NodeGetattrer = (*dirNode)(nil)
	_ fs.NodeLookuper  = (*dirNode)(nil)
	_ fs.NodeReaddirer = (*dirNode)(nil)
	_ fs.NodeGetattrer = (*fileNode)(nil)
	_ fs.NodeOpener    = (*fileNode)(nil)
	_ fs.NodeReader    = (*fileNode)(nil)
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

	child := &dirNode{
		mat:    n.mat,
		hash:   rel.Hash,
		prefix: "",
		files:  files,
	}

	out.Mode = syscall.S_IFDIR | 0o555
	out.Nlink = 2
	stable := fs.StableAttr{Mode: syscall.S_IFDIR, Ino: dirIno(rel.Hash, "")}
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

// dirNode represents a directory in the virtual tree: either "/<hash>" (when
// prefix == "") or a synthetic intermediate directory "/<hash>/<prefix>" when a
// release's files live under sub-directories (e.g. season packs, the common
// "<Movie Name>/<file>" layout TorBox returns).  go-fuse walks paths one
// component at a time, so Lookup only ever sees the next single component; we
// resolve it against the release's full file list (rel_paths are stored
// hash-root-relative and may contain '/').  The file list is captured at
// construction from the store so Getattr/Read never re-query SQLite.
type dirNode struct {
	fs.Inode
	mat    Materializer
	hash   string
	prefix string         // path from the hash root to this dir ("" at the hash root)
	files  []catalog.File // all files of the release; immutable after construction
}

func (n *dirNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = syscall.S_IFDIR | 0o555
	out.Nlink = 2
	return fs.OK
}

// relUnder returns the portion of relPath below prefix and whether relPath is a
// descendant of prefix.  prefix == "" is the hash root (everything is under it).
func relUnder(relPath, prefix string) (string, bool) {
	if prefix == "" {
		return relPath, true
	}
	if rest, ok := strings.CutPrefix(relPath, prefix+"/"); ok {
		return rest, true
	}
	return "", false
}

// Lookup resolves the next path component `name` under this directory.  A file
// whose remaining rel_path equals `name` resolves to a fileNode (the leaf); a
// file whose remaining rel_path is `name/...` resolves to a synthetic child
// dirNode.  Unknown names are ENOENT so the kernel caches the negative entry.
func (n *dirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	var childDir bool
	for _, f := range n.files {
		rest, ok := relUnder(f.RelPath, n.prefix)
		if !ok || rest == "" {
			continue
		}
		head, _, nested := strings.Cut(rest, "/")
		if head != name {
			continue
		}
		if !nested {
			// Leaf file directly under this directory.
			child := &fileNode{
				mat:    n.mat,
				hash:   n.hash,
				fileID: f.FileID,
				size:   f.Size,
			}
			out.Mode = syscall.S_IFREG | 0o444
			out.Size = uint64(f.Size) //nolint:gosec — catalog size, not user-controlled
			out.Nlink = 1
			stable := fs.StableAttr{Mode: syscall.S_IFREG, Ino: fileIno(n.hash, f.FileID)}
			return n.NewInode(ctx, child, stable), fs.OK
		}
		// `name` is an intermediate directory for this file; keep scanning in
		// case a sibling file is a leaf with the same name (files win), but
		// remember that a directory match exists.
		childDir = true
	}
	if childDir {
		childPrefix := name
		if n.prefix != "" {
			childPrefix = n.prefix + "/" + name
		}
		child := &dirNode{mat: n.mat, hash: n.hash, prefix: childPrefix, files: n.files}
		out.Mode = syscall.S_IFDIR | 0o555
		out.Nlink = 2
		stable := fs.StableAttr{Mode: syscall.S_IFDIR, Ino: dirIno(n.hash, childPrefix)}
		return n.NewInode(ctx, child, stable), fs.OK
	}
	return nil, syscall.ENOENT
}

// Readdir lists the immediate children of this directory: leaf files directly
// under it plus the distinct first-level sub-directory names of any nested
// files.  Consistent (alphabetical) ordering is required by go-fuse to avoid
// entries disappearing under concurrent reads.
func (n *dirNode) Readdir(_ context.Context) (fs.DirStream, syscall.Errno) {
	entries := make([]fuse.DirEntry, 0, len(n.files))
	seenDir := make(map[string]struct{})
	for _, f := range n.files {
		rest, ok := relUnder(f.RelPath, n.prefix)
		if !ok || rest == "" {
			continue
		}
		head, _, nested := strings.Cut(rest, "/")
		if nested {
			if _, dup := seenDir[head]; dup {
				continue
			}
			seenDir[head] = struct{}{}
			childPrefix := head
			if n.prefix != "" {
				childPrefix = n.prefix + "/" + head
			}
			entries = append(entries, fuse.DirEntry{
				Name: head,
				Mode: syscall.S_IFDIR,
				Ino:  dirIno(n.hash, childPrefix),
			})
			continue
		}
		entries = append(entries, fuse.DirEntry{
			Name: head,
			Mode: syscall.S_IFREG,
			Ino:  fileIno(n.hash, f.FileID),
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

// inoFromKey generates a stable inode number from an arbitrary key via an
// FNV-1a mix.  The inode only needs to be unique within this filesystem
// instance and stable for the lifetime of the mount.
func inoFromKey(key string) uint64 {
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	h := uint64(offset64)
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= prime64
	}
	if h == 0 {
		h = 1 // inode 0 is reserved by the kernel
	}
	return h
}

// fileIno and dirIno derive inode numbers in disjoint keyspaces so a file and a
// directory can never collide (e.g. file_id 0 vs the hash root, which the old
// scheme mapped to the same inode).
func fileIno(hash string, fileID int) uint64 {
	return inoFromKey(hash + "\x00f" + strconv.Itoa(fileID))
}
func dirIno(hash, prefix string) uint64 { return inoFromKey(hash + "\x00d" + prefix) }

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
