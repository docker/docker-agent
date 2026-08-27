package tools_test

// Regression suite for the StartableToolSet backoff gate (issue #4060).
//
// Gate enforcement is in tryStartLocked, called only from TryStart/TryStartWithTimeout.
// Blocking Start() bypasses the gate and always reaches the underlying toolset —
// that is intentional (mcpcatalog enable, skill sub-session startup must be immediate).
// Gate-assertion tests therefore drive TryStart; compatibility tests use Start or
// TryStart interchangeably (the gate never fires for non-StatusError errors).
//
// The helpers startErrToolSet / retryableErr / rateLimitErr / nonRetryableErr /
// identityJitter / newThrottledStartable are defined in startable_backoff_test.go
// (same package) and are reused directly here.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"

	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

// ragWrappedStatusErr returns a StatusError wrapped through two levels of
// fmt.Errorf, approximating the real RAG toolset error chain
// (Initialize → Manager → ToolSet.Start).
func ragWrappedStatusErr(code int) error {
	base := &modelerrors.StatusError{
		StatusCode: code,
		Err:        fmt.Errorf("HTTP %d from provider", code),
	}
	aborted := fmt.Errorf("indexing aborted due to non-retryable model error: %w", base)
	return fmt.Errorf("failed to initialize RAG manager %q: %w", "knowledge-base", aborted)
}

// TestBackoffRegression_RAGShapedFailureAndRecovery verifies the full
// backoff-then-recovery cycle for an error shaped like a real RAG toolset
// hitting a rate-limit (429) during indexing.
func TestBackoffRegression_RAGShapedFailureAndRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &startErrToolSet{}
		inner.setErr(ragWrappedStatusErr(429))
		s := newThrottledStartable(inner)

		// Attempt 1: underlying Start runs via TryStart and fails.
		_, err := s.TryStart(t.Context())
		assert.Check(t, err != nil, "expected failure on attempt 1")
		assert.Check(t, is.Equal(inner.starts.Load(), int32(1)), "attempt 1 must invoke the underlying")

		// Immediate TryStart retry: gate must suppress it (within the base window).
		_, err = s.TryStart(t.Context())
		assert.Check(t, err != nil)
		assert.Check(t, is.Equal(inner.starts.Load(), int32(1)), "immediate TryStart must be gated within the window")

		// Advance fake clock past the base window (identity jitter ⇒ window == base exactly).
		time.Sleep(tools.ExportedStartBackoffBase + time.Millisecond) //nolint:forbidigo // synctest bubble

		// Gate expired: TryStart must invoke the underlying again.
		_, err = s.TryStart(t.Context())
		assert.Check(t, err != nil, "still failing — expected error")
		assert.Check(t, is.Equal(inner.starts.Load(), int32(2)), "TryStart must re-run after the window expires")

		// Recovery: advance past the second window (base×2 with identity jitter)
		// then clear the error so the next TryStart succeeds.
		time.Sleep(2*tools.ExportedStartBackoffBase + time.Millisecond) //nolint:forbidigo // synctest bubble
		inner.clearErr()
		started, err := s.TryStart(t.Context())
		assert.NilError(t, err, "recovery TryStart must succeed")
		assert.Check(t, started)
		assert.Check(t, is.Equal(s.IsStarted(), true))

		// After recovery, stop and verify the next failure starts a fresh base window.
		assert.NilError(t, s.Stop(t.Context()))
		inner.setErr(ragWrappedStatusErr(503))
		callsBefore := inner.starts.Load()
		_, _ = s.TryStart(t.Context()) // arms fresh gate at base
		// Immediate TryStart must be gated (fresh base window after reset).
		_, _ = s.TryStart(t.Context())
		assert.Check(t, is.Equal(inner.starts.Load(), callsBefore+1),
			"after recovery, the next retryable failure must start a fresh base-delay window")
	})
}

// TestBackoffRegression_MCPShapedCompatibility verifies that Start failures
// shaped like a real MCP toolset (plain connection error, no StatusError)
// do NOT engage the backoff gate. Every call reaches the underlying.
func TestBackoffRegression_MCPShapedCompatibility(t *testing.T) {
	t.Parallel()

	inner := &startErrToolSet{}
	inner.setErr(errors.New("connection refused: dial tcp 127.0.0.1:9999"))
	s := newThrottledStartable(inner)

	// Three consecutive TryStart calls: each must invoke the underlying (no gate).
	for i := int32(1); i <= 3; i++ {
		_, err := s.TryStart(t.Context())
		assert.Check(t, err != nil)
		assert.Check(t, is.Equal(inner.starts.Load(), i),
			"MCP connection-error failure must not engage backoff (attempt %d)", i)
	}

	inner.clearErr()
	assert.NilError(t, s.Start(t.Context()), "recovery must work without a backoff window")
	assert.Check(t, is.Equal(s.IsStarted(), true))
}

// TestBackoffRegression_LSPShapedCompatibility mirrors the MCP test for the
// LSP toolset error shape (lifecycle.ErrServerUnavailable, not a StatusError).
func TestBackoffRegression_LSPShapedCompatibility(t *testing.T) {
	t.Parallel()

	inner := &startErrToolSet{}
	inner.setErr(fmt.Errorf("binary not found: %w", lifecycle.ErrServerUnavailable))
	s := newThrottledStartable(inner)

	for i := int32(1); i <= 3; i++ {
		_, err := s.TryStart(t.Context())
		assert.Check(t, err != nil)
		assert.Check(t, is.Equal(inner.starts.Load(), i),
			"LSP ErrServerUnavailable must not engage backoff (attempt %d)", i)
	}

	inner.clearErr()
	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(s.IsStarted(), true))
}

// TestBackoffRegression_BlockingStartIsNeverGated verifies that blocking
// Start() always reaches the underlying toolset even while a TryStart backoff
// window is active. This is intentional: mcpcatalog enable and skill
// sub-session startup use Start() and must be immediate.
func TestBackoffRegression_BlockingStartIsNeverGated(t *testing.T) {
	t.Parallel()

	inner := &startErrToolSet{}
	inner.setErr(retryableErr())

	// Freeze the clock so the window stays active for the test duration.
	now := time.Unix(1_000_000, 0)
	s := tools.NewStartable(inner,
		tools.WithStartRetryJitter(identityJitter),
		tools.WithStartRetryClock(func() time.Time { return now }),
	)

	// Arm the gate via TryStart.
	_, err := s.TryStart(t.Context())
	assert.Check(t, err != nil)
	assert.Check(t, is.Equal(inner.starts.Load(), int32(1)))

	// Blocking Start() must bypass the gate and invoke the underlying.
	err = s.Start(t.Context())
	assert.Check(t, err != nil, "blocking Start must fail (retryableErr still set)")
	assert.Check(t, is.Equal(inner.starts.Load(), int32(2)),
		"blocking Start must bypass the gate and reach the underlying")

	// TryStart within the (re-armed) window must still be gated.
	_, err = s.TryStart(t.Context())
	assert.Check(t, err != nil)
	assert.Check(t, is.Equal(inner.starts.Load(), int32(2)),
		"TryStart within window must be gated even after a blocking Start")
}

// TestBackoffRegression_ConcurrentStartsNoMultiplication verifies that many
// concurrent TryStart calls against a failing toolset do not multiply the
// underlying retry activity. The single-flight TryLock serialises them: the
// first goroutine runs and arms the gate; the rest either skip the lock or
// hit the gate — in both cases without invoking the underlying.
func TestBackoffRegression_ConcurrentStartsNoMultiplication(t *testing.T) {
	t.Parallel()

	inner := &startErrToolSet{}
	inner.setErr(retryableErr())
	s := newThrottledStartable(inner) // identity jitter → base window stays active

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			_, _ = s.TryStart(t.Context())
		}()
	}
	wg.Wait()

	assert.Check(t, is.Equal(inner.starts.Load(), int32(1)),
		"concurrent TryStart calls must invoke the underlying exactly once (single-flight + gate)")
}

// TestBackoffRegression_GateSpawnsNoTimersOrGoroutines verifies that the
// backoff gate is purely a wall-clock check with no background resources.
func TestBackoffRegression_GateSpawnsNoTimersOrGoroutines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &startErrToolSet{}
		inner.setErr(retryableErr())
		s := newThrottledStartable(inner)

		// Arm the gate via TryStart.
		_, _ = s.TryStart(t.Context())

		// Several gated TryStart retries — if any created a timer the bubble stalls.
		for range 5 {
			_, _ = s.TryStart(t.Context())
		}
		// Bubble settling proves no leaked goroutines or timers.
	})
}

// TestBackoffRegression_CancellationNoWindowSet verifies that context
// cancellation does not arm the backoff gate.
func TestBackoffRegression_CancellationNoWindowSet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &startErrToolSet{}
		inner.setErr(context.Canceled)
		s := newThrottledStartable(inner)

		_, _ = s.TryStart(t.Context())
		assert.Check(t, is.Equal(inner.starts.Load(), int32(1)))

		// No window set: next TryStart must reach the underlying immediately.
		_, _ = s.TryStart(t.Context())
		assert.Check(t, is.Equal(inner.starts.Load(), int32(2)),
			"context cancellation must not arm the backoff gate")
	})
}

// TestBackoffRegression_JitterDeSynchronizesRetries verifies that additive
// jitter ([nominal, 1.2×nominal]) produces a spread of distinct durations,
// preventing concurrent toolsets from synchronising retries into a burst.
func TestBackoffRegression_JitterDeSynchronizesRetries(t *testing.T) {
	t.Parallel()

	const samples = 100
	attempt := 1
	nominal := tools.ExportedStartBackoffBase // 15s

	seen := make(map[time.Duration]bool, samples)
	for range samples {
		d := tools.ExportedComputeStartBackoff(attempt, nil) // real (random) additive jitter
		assert.Check(t, d >= nominal,
			"additive jitter floor must be the nominal itself, got %s < %s", d, nominal)
		assert.Check(t, d <= nominal+nominal/5,
			"additive jitter ceiling must be 1.2×nominal, got %s > %s", d, nominal+nominal/5)
		seen[d] = true
	}
	// With 100 samples over a 3s range in 1ns increments, identical values
	// would be astronomically improbable — assert meaningful spread.
	assert.Check(t, len(seen) > 5,
		"additive jitter must produce meaningful spread; got only %d distinct values", len(seen))
}

// TestBackoffRegression_AlreadyStartedNotRestarted pins that a latched
// toolset is not restarted by subsequent Start or TryStart calls.
func TestBackoffRegression_AlreadyStartedNotRestarted(t *testing.T) {
	t.Parallel()

	inner := &startErrToolSet{}
	s := newThrottledStartable(inner)

	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(inner.starts.Load(), int32(1)))
	assert.Check(t, is.Equal(s.IsStarted(), true))

	for range 3 {
		assert.NilError(t, s.Start(t.Context()))
	}
	assert.Check(t, is.Equal(inner.starts.Load(), int32(1)),
		"a latched toolset must not be restarted by subsequent Start calls")
}

func http408Err() error {
	return &modelerrors.StatusError{StatusCode: 408, Err: errors.New("request timeout")}
}

// TestBackoffRegression_HTTP408EngagesGate verifies that HTTP 408 (Request
// Timeout) arms the backoff gate via TryStart.
func TestBackoffRegression_HTTP408EngagesGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &startErrToolSet{}
		inner.setErr(http408Err())
		s := newThrottledStartable(inner)

		_, err := s.TryStart(t.Context())
		assert.Check(t, err != nil, "expected failure on attempt 1")
		assert.Check(t, is.Equal(inner.starts.Load(), int32(1)))

		// Immediate TryStart retry: gate must suppress it.
		_, err = s.TryStart(t.Context())
		assert.Check(t, err != nil)
		assert.Check(t, is.Equal(inner.starts.Load(), int32(1)),
			"HTTP 408 must arm the backoff gate: TryStart must not reach the underlying")
	})
}
