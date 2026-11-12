package fsx

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestCollectFiles_ContextCancellation(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

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

		// Wait for the timeout to trigger
		<-ctx.Done()

		_, err := CollectFiles(ctx, []string{tmpDir}, nil)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestDirectoryTree_ContextCancellation(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

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

		// Wait for the timeout to trigger
		<-ctx.Done()

		_, err := DirectoryTree(ctx, tmpDir, func(string) error { return nil }, nil, 0)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
}
