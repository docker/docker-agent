//go:build !windows

package server

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsConnectionRefused_Unix(t *testing.T) {
	t.Parallel()

	assert.True(t, isConnectionRefused(syscall.ECONNREFUSED))
	assert.False(t, isConnectionRefused(syscall.ECONNRESET))
}

func TestPrepareUnixSocket_RestoresLiveReplacement(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "a.sock")
	stale, err := Listen(t.Context(), "unix://"+sockPath)
	require.NoError(t, err)
	defer stale.Close()
	staleInfo, err := os.Lstat(sockPath)
	require.NoError(t, err)

	var replacement net.Listener
	err = prepareUnixSocket(t.Context(), sockPath, func(context.Context, string) (net.Conn, error) {
		return nil, syscall.ECONNREFUSED
	}, func(oldPath, quarantined string) (os.FileInfo, error) {
		replacementPath := filepath.Join(dir, "replacement")
		var config net.ListenConfig
		replacement, err = config.Listen(t.Context(), "unix", replacementPath)
		require.NoError(t, err)
		info, err := os.Lstat(replacementPath)
		require.NoError(t, err)
		require.False(t, os.SameFile(staleInfo, info), "replacement must have a distinct inode")
		require.NoError(t, os.Rename(oldPath, filepath.Join(dir, "original")))
		require.NoError(t, os.Rename(replacementPath, quarantined))
		return info, nil
	})
	require.ErrorContains(t, err, "changed while checking it; replacement restored")
	t.Cleanup(func() { _ = replacement.Close() })

	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "unix", sockPath)
	require.NoError(t, err, "the live replacement must remain reachable at its public path")
	require.NoError(t, conn.Close())

	artifacts, err := filepath.Glob(filepath.Join(dir, ".docker-agent-stale-socket-*"))
	require.NoError(t, err)
	assert.Empty(t, artifacts)
}

func TestRestoreQuarantinedSocket_DoesNotClobberPublicPath(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	path := filepath.Join(dir, "a.sock")
	quarantined := filepath.Join(dir, "quarantined")
	require.NoError(t, os.WriteFile(path, []byte("new entry"), 0o600))
	require.NoError(t, os.WriteFile(quarantined, []byte("preserved entry"), 0o600))

	err := restoreQuarantinedSocket(path, quarantined)
	require.ErrorContains(t, err, "public path left unchanged")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new entry", string(data))
	data, err = os.ReadFile(quarantined)
	require.NoError(t, err)
	assert.Equal(t, "preserved entry", string(data))
}
