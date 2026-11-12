package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotDir(t *testing.T) {
	root := t.TempDir()
	embedded := filepath.Join(root, "pkg", "modelsdev", "embedded")
	require.NoError(t, os.MkdirAll(embedded, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(embedded, "snapshot.json"), []byte("{}"), 0o644))

	t.Run("module root", func(t *testing.T) {
		t.Chdir(root)
		path, err := snapshotDir()
		require.NoError(t, err)
		assert.Equal(t, embedded, path)
	})
	t.Run("go generate package", func(t *testing.T) {
		t.Chdir(embedded)
		path, err := snapshotDir()
		require.NoError(t, err)
		assert.Equal(t, embedded, path)
	})
	t.Run("unrelated directory", func(t *testing.T) {
		t.Chdir(t.TempDir())
		_, err := snapshotDir()
		require.ErrorContains(t, err, "could not locate pkg/modelsdev/embedded")
	})
}
