package vfs

import "testing"

// TestIsStaleMount_NormalDir confirms a live, healthy directory is not flagged stale, and a
// missing path (ENOENT) is not mistaken for a dead FUSE mount (ENOTCONN). The actual
// ENOTCONN crash-recovery path is validated end-to-end by the live canary.
func TestIsStaleMount_NormalDir(t *testing.T) {
	if isStaleMount(t.TempDir()) {
		t.Fatal("a normal directory must not be reported as a stale mount")
	}
	if isStaleMount("/nonexistent/path/xyz") {
		t.Fatal("a missing path is ENOENT, not ENOTCONN — must not be reported stale")
	}
}
