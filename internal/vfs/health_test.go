package vfs_test

import (
	"os"
	"testing"

	"github.com/rushp4000/lazarr/internal/vfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHealthy_UnmountedIsFalse verifies an FS that was never mounted (or already
// unmounted) reports unhealthy, so the materialize reaper guard never deletes from
// the account when there is no live mount.
func TestHealthy_UnmountedIsFalse(t *testing.T) {
	f := vfs.New("/nonexistent/mount", newFakeStore(), &fakeMat{})
	assert.False(t, f.Healthy(), "an unmounted FS must report unhealthy")
}

// TestHealthy_MountedIsTrue verifies a live mount reports healthy and that after a
// clean Unmount it flips back to unhealthy. Skipped where FUSE is unavailable.
func TestHealthy_MountedIsTrue(t *testing.T) {
	store := newFakeStore()
	mat := &fakeMat{}
	rel, files := testRelease()
	store.addRelease(rel, files)

	_, fsys := mountTestFS(t, store, mat)
	assert.True(t, fsys.Healthy(), "a live mount must report healthy")

	require.NoError(t, fsys.Unmount(), "clean unmount should succeed")
	assert.False(t, fsys.Healthy(), "after unmount the FS must report unhealthy")
}

// TestUnmount_IdempotentNoMount verifies Unmount is a safe no-op when there is no
// server attached (covers the bounded-retry/detach path's nil-server guard).
func TestUnmount_IdempotentNoMount(t *testing.T) {
	f := vfs.New("/nonexistent/mount", newFakeStore(), &fakeMat{})
	require.NoError(t, f.Unmount())
	require.NoError(t, f.Unmount()) // second call still a no-op
}

// TestUnmount_CleanPathSucceeds mounts and unmounts to exercise the happy clean path
// (no EBUSY) end to end. Skipped where FUSE is unavailable.
func TestUnmount_CleanPathSucceeds(t *testing.T) {
	store := newFakeStore()
	mat := &fakeMat{}
	rel, files := testRelease()
	store.addRelease(rel, files)

	skipIfNoFUSE(t)
	dir := t.TempDir()
	fsys := vfs.New(dir, store, mat)
	if err := fsys.Mount(); err != nil {
		t.Skipf("FUSE mount failed (needs SYS_ADMIN / /dev/fuse): %v", err)
	}
	// Ensure the mount is real before we tear it down.
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("mount root not statable: %v", err)
	}
	require.NoError(t, fsys.Unmount())
}
