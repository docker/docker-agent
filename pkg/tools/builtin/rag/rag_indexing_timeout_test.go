package rag

// Regression tests for #4073: RAG indexing must be decoupled from the
// caller's tools.DefaultStartTimeout wait budget and bounded only by the
// new indexing_timeout (WithIndexingTimeout) and Stop.

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/rag"
	"github.com/docker/docker-agent/pkg/rag/strategy"
	"github.com/docker/docker-agent/pkg/tools"
)

// blockingIndexStrategy blocks in Initialize until either release is closed
// (success) or the supplied ctx is done (the caller's cancellation/timeout).
// It captures the ctx it was called with on ctxCh so tests can assert on its
// lifetime without racing the goroutine that invokes Initialize.
type blockingIndexStrategy struct {
	mockStrategy

	release chan struct{}
	ctxCh   chan context.Context
	calls   atomic.Int32
}

func (s *blockingIndexStrategy) Initialize(ctx context.Context, _ []string, _ strategy.ChunkingConfig) error {
	s.calls.Add(1)
	select {
	case s.ctxCh <- ctx:
	default:
	}
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newTestRAGToolSet(t *testing.T, impl strategy.Strategy, opts ...Option) *ToolSet {
	t.Helper()
	cfg := rag.Config{
		StrategyConfigs: []strategy.Config{
			{Name: "test-strategy", Strategy: impl},
		},
	}
	mgr, err := rag.New(t.Context(), "test-rag", cfg, nil)
	require.NoError(t, err)
	return New(mgr, "test-rag", opts...)
}

// TestRAGIndexingTimeout_WaitBudgetDoesNotCancelIndexing proves the core
// #4073 fix: when tools.DefaultStartTimeout-style wait budget expires,
// indexing (Initialize) keeps running on a context that is NOT canceled by
// that budget, and a later attempt picks up the completed result without
// calling Initialize again.
func TestRAGIndexingTimeout_WaitBudgetDoesNotCancelIndexing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		strategyMock := &blockingIndexStrategy{
			release: make(chan struct{}),
			ctxCh:   make(chan context.Context, 1),
		}
		// indexingTimeout left at its zero value (unbounded): only the
		// caller's wait budget below should ever be observed expiring.
		toolset := newTestRAGToolSet(t, strategyMock)
		s := tools.NewStartable(toolset)

		started, err := s.TryStartWithTimeout(t.Context(), time.Second)
		assert.False(t, started)
		require.ErrorIs(t, err, context.DeadlineExceeded, "the caller's wait budget must expire")

		var capturedCtx context.Context
		select {
		case capturedCtx = <-strategyMock.ctxCh:
		default:
			t.Fatal("Initialize was never called")
		}
		require.NoError(t, capturedCtx.Err(), "indexing's context must survive the caller's expired wait budget")

		// Let indexing finish; the abandoned background attempt still holds
		// the single-flight lock and settles once Initialize returns.
		close(strategyMock.release)
		synctest.Wait()

		started, err = s.TryStart(t.Context())
		require.NoError(t, err)
		assert.True(t, started, "a later TryStart must pick up the completed background start")
		assert.Equal(t, int32(1), strategyMock.calls.Load(), "Initialize must not be called a second time")
	})
}

// TestRAGIndexingTimeout_Applied proves that a positive indexing_timeout
// bounds Initialize itself (independent of any caller wait budget) and that
// the resulting Start error is context.DeadlineExceeded.
func TestRAGIndexingTimeout_Applied(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		strategyMock := &blockingIndexStrategy{
			release: make(chan struct{}),
			ctxCh:   make(chan context.Context, 1),
		}
		toolset := newTestRAGToolSet(t, strategyMock, WithIndexingTimeout(5*time.Second))

		err := toolset.Start(t.Context())
		require.Error(t, err)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, int32(1), strategyMock.calls.Load())
	})
}

// TestRAGIndexingTimeout_ContextDeadline covers indexing_timeout resolution
// at the ToolSet level: the context handed to Initialize carries a deadline
// only when a positive indexing_timeout was configured.
func TestRAGIndexingTimeout_ContextDeadline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		timeout      time.Duration
		wantDeadline bool
	}{
		{name: "unbounded (zero value, e.g. omitted config)", timeout: 0, wantDeadline: false},
		{name: "bounded", timeout: 5 * time.Minute, wantDeadline: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctxCh := make(chan context.Context, 1)
			strategyMock := &ctxCapturingStrategy{ctxCh: ctxCh}

			var opts []Option
			if tt.timeout != 0 {
				opts = append(opts, WithIndexingTimeout(tt.timeout))
			}
			toolset := newTestRAGToolSet(t, strategyMock, opts...)

			require.NoError(t, toolset.Start(t.Context()))
			defer func() { require.NoError(t, toolset.Stop(t.Context())) }()

			var capturedCtx context.Context
			select {
			case capturedCtx = <-ctxCh:
			default:
				t.Fatal("Initialize was never called")
			}
			_, hasDeadline := capturedCtx.Deadline()
			assert.Equal(t, tt.wantDeadline, hasDeadline)
		})
	}
}

// ctxCapturingStrategy returns immediately from Initialize after handing the
// ctx it received to the test, for synchronous deadline assertions.
type ctxCapturingStrategy struct {
	mockStrategy

	ctxCh chan context.Context
}

func (s *ctxCapturingStrategy) Initialize(ctx context.Context, _ []string, _ strategy.ChunkingConfig) error {
	select {
	case s.ctxCh <- ctx:
	default:
	}
	return nil
}

// closeCountingStrategy counts Close calls so Stop's exactly-once contract
// can be asserted after a detached Start completes.
type closeCountingStrategy struct {
	mockStrategy

	closes atomic.Int32
}

func (s *closeCountingStrategy) Close() error {
	s.closes.Add(1)
	return nil
}

// TestRAGIndexingTimeout_StopAfterDetachedStartClosesOnce proves Stop's
// wg/cancelWatcher accounting is unaffected by decoupling indexing from the
// caller's ctx: a completed detached Start still closes the manager exactly
// once and Stop does not deadlock.
func TestRAGIndexingTimeout_StopAfterDetachedStartClosesOnce(t *testing.T) {
	t.Parallel()

	strategyMock := &closeCountingStrategy{}
	toolset := newTestRAGToolSet(t, strategyMock, WithIndexingTimeout(0))

	require.NoError(t, toolset.Start(t.Context()))

	done := make(chan struct{})
	go func() {
		_ = toolset.Stop(t.Context())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop deadlocked after a detached Start completed")
	}

	assert.Equal(t, int32(1), strategyMock.closes.Load())
}
