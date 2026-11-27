//go:build !windows

package a2astore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStoreFilesAreOwnerOnly checks a pre-existing permissive directory and
// database are tightened, and that sidecars are private while SQLite is open.
func TestStoreFilesAreOwnerOnly(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "a2a")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.Chmod(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, dbFile), nil, 0o644))
	require.NoError(t, os.Chmod(filepath.Join(dir, dbFile), 0o644))

	s, err := New(ctx, dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	_, err = s.Save(ctx, "task-1", &a2a.PushConfig{URL: "https://hook.example/cb", Token: "sekret"})
	require.NoError(t, err)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	seen := 0
	for _, p := range sqliteFiles(filepath.Join(dir, dbFile)) {
		info, err := os.Stat(p)
		if os.IsNotExist(err) {
			continue
		}
		require.NoError(t, err)
		seen++
		assert.Equalf(t, os.FileMode(0o600), info.Mode().Perm(), "%s must be owner-only", p)
	}
	if journalWAL() {
		assert.Equal(t, 3, seen, "db, -wal and -shm must exist while the store is open")
	}
}

func TestNewRejectsSymlinkedDir(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	require.NoError(t, os.Mkdir(target, 0o755))
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(target, link))

	_, err := New(t.Context(), link)
	require.Error(t, err)
}

func journalWAL() bool {
	return os.Getenv("SANDBOX_VM_ID") == ""
}
