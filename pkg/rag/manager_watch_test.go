package rag

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/rag/strategy"
)

type watchStrategy struct {
	mockStrategy

	watch func(context.Context, []string, strategy.ChunkingConfig) error
}

func (s *watchStrategy) WatchFiles(ctx context.Context, paths []string, chunking strategy.ChunkingConfig) error {
	return s.watch(ctx, paths, chunking)
}

func TestWatchFilesRunsStrategiesTogetherAndJoins(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		started := make(chan string, 2)
		release := make(chan struct{})
		var finished atomic.Int32
		configs := make([]strategy.Config, 0, 2)
		for _, name := range []string{"a", "b"} {
			configs = append(configs, strategy.Config{Name: name, Docs: []string{name}, Chunking: strategy.ChunkingConfig{Size: 42}, Strategy: &watchStrategy{
				watch: func(ctx context.Context, paths []string, chunking strategy.ChunkingConfig) error {
					assert.Equal(t, []string{name}, paths)
					assert.Equal(t, 42, chunking.Size)
					started <- name
					<-ctx.Done()
					<-release
					finished.Add(1)
					return ctx.Err()
				},
			}})
		}
		mgr, err := New(t.Context(), "test", Config{StrategyConfigs: configs}, nil)
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() { done <- mgr.WatchFiles(ctx) }()
		assert.ElementsMatch(t, []string{"a", "b"}, []string{<-started, <-started})
		cancel()
		synctest.Wait()
		assert.Empty(t, done, "must join both strategies")
		close(release)
		require.ErrorIs(t, <-done, context.Canceled)
		assert.Equal(t, int32(2), finished.Load())
	})
}

func TestWatchFilesFailureCancelsAndJoinsPeer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		failure := errors.New("watch failed")
		mgr, err := New(t.Context(), "test", Config{StrategyConfigs: []strategy.Config{
			{Name: "failed", Strategy: &watchStrategy{watch: func(context.Context, []string, strategy.ChunkingConfig) error {
				<-started
				return failure
			}}},
			{Name: "peer", Strategy: &watchStrategy{watch: func(ctx context.Context, _ []string, _ strategy.ChunkingConfig) error {
				close(started)
				<-ctx.Done()
				<-release
				return ctx.Err()
			}}},
		}}, nil)
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() { done <- mgr.WatchFiles(t.Context()) }()
		synctest.Wait()
		assert.Empty(t, done)
		close(release)
		err = <-done
		require.ErrorIs(t, err, failure)
		require.ErrorContains(t, err, "failed")
	})
}

func TestWatchFilesNoOpDoesNotCancelActivePeer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		started := make(chan context.Context, 1)
		manager, err := New(t.Context(), "test", Config{StrategyConfigs: []strategy.Config{
			{Name: "noop", Strategy: &mockStrategy{}},
			{Name: "active", Strategy: &watchStrategy{watch: func(ctx context.Context, _ []string, _ strategy.ChunkingConfig) error {
				started <- ctx
				<-ctx.Done()
				return ctx.Err()
			}}},
		}}, nil)
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() { done <- manager.WatchFiles(ctx) }()
		running := <-started
		synctest.Wait()
		require.NoError(t, running.Err())
		assert.Empty(t, done)
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}
