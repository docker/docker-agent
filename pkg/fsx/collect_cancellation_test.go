package fsx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectFiles_ContextCancellation(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create a large directory structure to ensure context cancellation has time to kick in
	for i := range 100 {
		subDir := filepath.Join(tmpDir, "dir", "subdir", "deepdir", fmt.Sprintf("dir%d", i))
		require.NoError(t, os.MkdirAll(subDir, 0o755))
		for j := range 10 {
			filePath := filepath.Join(subDir, fmt.Sprintf("file%d.txt", j))
			require.NoError(t, os.WriteFile(filePath, []byte("test content"), 0o644))
		}
	}

	t.Run("respects context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())

		// Cancel context immediately
		cancel()

		_, err := CollectFiles(ctx, []string{tmpDir}, nil)
		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("respects context timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 1*time.Nanosecond)
		defer cancel()

		// Give time for timeout to trigger
		time.Sleep(10 * time.Millisecond)

		_, err := CollectFiles(ctx, []string{tmpDir}, nil)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestDirectoryTree_ContextCancellation(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create a large directory structure
	for i := range 100 {
		subDir := filepath.Join(tmpDir, "dir", "subdir", fmt.Sprintf("dir%d", i))
		require.NoError(t, os.MkdirAll(subDir, 0o755))
		for j := range 10 {
			filePath := filepath.Join(subDir, fmt.Sprintf("file%d.txt", j))
			require.NoError(t, os.WriteFile(filePath, []byte("test content"), 0o644))
		}
	}

	t.Run("respects context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())

		// Cancel context immediately
		cancel()

		_, err := DirectoryTree(ctx, tmpDir, func(string) error { return nil }, nil, 0)
		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("respects context timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 1*time.Nanosecond)
		defer cancel()

		// Give time for timeout to trigger
		time.Sleep(10 * time.Millisecond)

		_, err := DirectoryTree(ctx, tmpDir, func(string) error { return nil }, nil, 0)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
}
