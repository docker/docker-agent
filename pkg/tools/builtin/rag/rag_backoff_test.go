package rag

// Regression tests proving that the real RAG ToolSet, when wrapped in
// tools.NewStartable, correctly engages the backoff gate for structured
// HTTP-status failures (e.g. a 429 rate-limit from the embedding provider)
// and does NOT throttle plain (non-StatusError) failures.
//
// Gate enforcement is in tryStartLocked (TryStart paths only); all gate
// assertions here use TryStart, not Start. A frozen fake clock supplied via
// WithStartRetryClock gives deterministic windows without needing synctest.

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/rag"
	"github.com/docker/docker-agent/pkg/rag/strategy"
	"github.com/docker/docker-agent/pkg/tools"
)

// countingStatusErrStrategy is a mockStrategy variant whose Initialize
// returns a *modelerrors.StatusError and counts every invocation.
type countingStatusErrStrategy struct {
	mockStrategy

	statusCode int
	calls      atomic.Int32
}

func (s *countingStatusErrStrategy) Initialize(_ context.Context, _ []string, _ strategy.ChunkingConfig) error {
	s.calls.Add(1)
	return &modelerrors.StatusError{
		StatusCode: s.statusCode,
		Err:        fmt.Errorf("HTTP %d error from provider", s.statusCode),
	}
}

// countingPlainErrStrategy always returns a plain (non-StatusError) error
// so the backoff gate must never fire.
type countingPlainErrStrategy struct {
	mockStrategy

	calls atomic.Int32
}

func (s *countingPlainErrStrategy) Initialize(_ context.Context, _ []string, _ strategy.ChunkingConfig) error {
	s.calls.Add(1)
	return assert.AnError
}

func buildRAGToolSet(t *testing.T, impl strategy.Strategy) *ToolSet {
	t.Helper()
	cfg := rag.Config{
		StrategyConfigs: []strategy.Config{
			{Name: "test-strategy", Strategy: impl},
		},
	}
	mgr, err := rag.New(t.Context(), "test-rag", cfg, nil)
	require.NoError(t, err)
	return &ToolSet{manager: mgr, toolName: "test-rag"}
}

// TestRAGStartableBackoff_StatusErrorEngagesGate proves that the real RAG
// ToolSet, when it fails with a *modelerrors.StatusError, arms the backoff
// gate in TryStart: the immediate retry is gated and the underlying is not
// called again until the window expires.
//
// A frozen fake clock (WithStartRetryClock) and identity jitter give exact,
// deterministic windows without synctest complications from the RAG manager's
// internal goroutines.
func TestRAGStartableBackoff_StatusErrorEngagesGate(t *testing.T) {
	t.Parallel()

	counting := &countingStatusErrStrategy{statusCode: 429}
	toolset := buildRAGToolSet(t, counting)

	// Frozen clock: window = base + 0% jitter = exactly base.
	// We advance the clock manually to control expiry.
	now := time.Unix(1_000_000, 0)
	s := tools.NewStartable(toolset,
		tools.WithStartRetryJitter(func(d time.Duration) time.Duration { return d }), // identity
		tools.WithStartRetryClock(func() time.Time { return now }),
	)

	// Attempt 1: TryStart invokes Initialize and arms the gate.
	_, err := s.TryStart(t.Context())
	require.Error(t, err, "expected failure on attempt 1")
	assert.Equal(t, int32(1), counting.calls.Load(), "Initialize must be called on attempt 1")

	// Immediate TryStart: gate must suppress it (clock not advanced).
	_, err = s.TryStart(t.Context())
	require.Error(t, err)
	assert.Equal(t, int32(1), counting.calls.Load(),
		"gate must suppress TryStart within the window")

	// Advance clock past base window (base × 1.0 with identity jitter).
	// Use 6 minutes to exceed any possible jittered window for any attempt.
	now = now.Add(6 * time.Minute)

	// Gate expired: TryStart must invoke Initialize again.
	_, err = s.TryStart(t.Context())
	require.Error(t, err, "still failing — expected error")
	assert.Equal(t, int32(2), counting.calls.Load(),
		"Initialize must be called again once the backoff window expires")
}

// TestRAGStartableBackoff_PlainErrorNoGate proves that a plain (non-StatusError)
// failure does NOT arm the gate: every TryStart reaches Initialize.
func TestRAGStartableBackoff_PlainErrorNoGate(t *testing.T) {
	t.Parallel()

	counting := &countingPlainErrStrategy{}
	toolset := buildRAGToolSet(t, counting)
	s := tools.NewStartable(toolset)

	for i := int32(1); i <= 3; i++ {
		_, err := s.TryStart(t.Context())
		require.Error(t, err)
		assert.Equal(t, i, counting.calls.Load(),
			"plain error must not gate subsequent TryStart calls (attempt %d)", i)
	}
}
