//go:build !windows

package plans

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// symlinkedDestination plants a regular file and a symlink pointing at it,
// returning both paths.
func symlinkedDestination(t *testing.T, targetContent string) (link, target string) {
	t.Helper()
	dir := t.TempDir()
	target = filepath.Join(dir, "target.md")
	require.NoError(t, os.WriteFile(target, []byte(targetContent), 0o600))
	link = filepath.Join(dir, "link.md")
	require.NoError(t, os.Symlink(target, link))
	return link, target
}

// TestService_ExportRefusesSymlinkDestination proves a non-force export
// treats an existing symlink like any existing destination: refused with a
// typed already-exists *ValidationError, leaving both the link entry and the
// file it points to untouched.
func TestService_ExportRefusesSymlinkDestination(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	mustCreate(t, svc, "p", "new body")
	link, target := symlinkedDestination(t, "precious")

	var invalid *ValidationError
	_, err := svc.Export(t.Context(), ExportRequest{Ref: SharedRef("p"), Path: link})
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Message, "already exists")

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the refused export must leave the symlink a symlink")
	resolved, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, target, resolved)

	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "precious", string(data), "the refused export must not write through the symlink")
}

// TestService_ExportRefusesDanglingSymlinkDestination proves the no-replace
// publication refuses any existing directory entry, not only what the stat
// pre-check can see: a dangling symlink at the destination is stat-invisible
// yet still fails as already-exists, is left in place, and no temp file is
// left behind.
func TestService_ExportRefusesDanglingSymlinkDestination(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	mustCreate(t, svc, "p", "new body")

	dir := t.TempDir()
	link := filepath.Join(dir, "link.md")
	require.NoError(t, os.Symlink(filepath.Join(dir, "missing.md"), link))

	var invalid *ValidationError
	_, err := svc.Export(t.Context(), ExportRequest{Ref: SharedRef("p"), Path: link})
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Message, "already exists")

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the refused export must leave the dangling link entry in place")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "the refused export must clean up its temp file")
}

// TestService_ExportForceReplacesSymlinkEntryNotTarget proves a force export
// publishes by rename: the symlink entry itself becomes a regular file with
// the exported body, and the file the link pointed to is never modified.
func TestService_ExportForceReplacesSymlinkEntryNotTarget(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	mustCreate(t, svc, "p", "new body")
	link, target := symlinkedDestination(t, "precious")

	result, err := svc.Export(t.Context(), ExportRequest{Ref: SharedRef("p"), Path: link, Force: true})
	require.NoError(t, err)
	assert.Equal(t, len("new body"), result.BytesWritten)

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular(), "force must replace the symlink entry itself, not write through it")
	data, err := os.ReadFile(link)
	require.NoError(t, err)
	assert.Equal(t, "new body", string(data))

	data, err = os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "precious", string(data), "the symlink target must be untouched")
}
