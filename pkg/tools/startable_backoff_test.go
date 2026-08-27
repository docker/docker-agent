package tools_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"

	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/tools"
)

// startErrToolSet is a minimal Startable whose error field controls the
// return value of Start. Setting err to nil makes Start succeed. starts
// tracks every invocation so tests can assert the gate is not bypassed.
type startErrToolSet struct {
	err    atomic.Pointer[error]
	starts atomic.Int32
}

func (s *startErrToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

func (s *startErrToolSet) Start(context.Context) error {
	s.starts.Add(1)
	if p := s.err.Load(); p != nil {
		return *p
	}
	return nil
}

func (s *startErrToolSet) Stop(context.Context) error { return nil }

func (s *startErrToolSet) setErr(err error) { s.err.Store(&err) }
func (s *startErrToolSet) clearErr()        { s.err.Store(nil) }

// retryableErr returns a *modelerrors.StatusError for status 503 (Service
// Unavailable), which RetryableHTTPStatus classifies as retryable.
func retryableErr() error {
	return &modelerrors.StatusError{StatusCode: 503, Err: errors.New("service unavailable")}
}

// rateLimitErr returns a *modelerrors.StatusError for status 429 (Too Many
// Requests), which RetryableHTTPStatus classifies as rate-limited.
func rateLimitErr() error {
	return &modelerrors.StatusError{StatusCode: 429, Err: errors.New("rate limited")}
}

// nonRetryableErr returns a *modelerrors.StatusError for status 401
// (Unauthorized), which RetryableHTTPStatus classifies as non-retryable.
func nonRetryableErr() error {
	return &modelerrors.StatusError{StatusCode: 401, Err: errors.New("unauthorized")}
}

// identityJitter is a deterministic jitter function that returns the nominal
// delay unchanged, making backoff windows exactly predictable in tests.
// Valid under the additive-jitter contract because nominal ∈ [nominal, 1.2×nominal].
func identityJitter(d time.Duration) time.Duration { return d }

// newThrottledStartable returns a StartableToolSet wrapping inner with
// identity jitter so fake-clock or synctest can drive fake time precisely.
func newThrottledStartable(inner tools.ToolSet) *tools.StartableToolSet {
	return tools.NewStartable(inner, tools.WithStartRetryJitter(identityJitter))
}

// TestStartableToolSet_RetryableFailureBacksOff verifies the core gate
// contract (via TryStart): the first attempt runs normally; within the
// backoff window the underlying Start is not invoked again; once the
// window expires the next attempt runs.
func TestStartableToolSet_RetryableFailureBacksOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &startErrToolSet{}
		inner.setErr(retryableErr())
		s := newThrottledStartable(inner)

		// Attempt 1: underlying Start runs and fails.
		_, err := s.TryStart(t.Context())
		assert.Check(t, err != nil, "expected error on first attempt")
		assert.Check(t, is.Equal(inner.starts.Load(), int32(1)), "underlying Start must be called once")

		// Immediate retry via TryStart: gate must block it (still within window).
		_, err = s.TryStart(t.Context())
		assert.Check(t, err != nil, "expected error within window")
		assert.Check(t, is.Equal(inner.starts.Load(), int32(1)), "gate must not invoke underlying Start within window")

		// Advance fake clock past the base (15s) window (identity jitter → window == base).
		time.Sleep(tools.ExportedStartBackoffBase + time.Millisecond) //nolint:forbidigo // inside synctest bubble: Sleep advances fake time

		// After the window expires the next TryStart runs the underlying attempt.
		_, err = s.TryStart(t.Context())
		assert.Check(t, err != nil, "still failing — expected error after window")
		assert.Check(t, is.Equal(inner.starts.Load(), int32(2)), "underlying Start must be called again after window")
	})
}

// TestStartableToolSet_SuccessfulStartClearsBackoff verifies that a
// successful TryStart resets all backoff state so the next failure starts a
// fresh (base-delay) window rather than a longer accumulated one.
func TestStartableToolSet_SuccessfulStartClearsBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &startErrToolSet{}
		inner.setErr(retryableErr())
		s := newThrottledStartable(inner)

		// Two retryable failures to advance the attempt counter.
		_, _ = s.TryStart(t.Context())
		time.Sleep(tools.ExportedStartBackoffBase + time.Millisecond) //nolint:forbidigo // inside synctest bubble
		_, _ = s.TryStart(t.Context())

		// Advance past the second window (base×2 = 30s with identity jitter).
		time.Sleep(2*tools.ExportedStartBackoffBase + time.Millisecond) //nolint:forbidigo // inside synctest bubble

		// Now let the toolset succeed.
		inner.clearErr()
		started, err := s.TryStart(t.Context())
		assert.NilError(t, err)
		assert.Check(t, started, "toolset must be started after success")
		assert.Check(t, is.Equal(s.IsStarted(), true))

		// Stop so we can drive a fresh failure.
		assert.NilError(t, s.Stop(t.Context()))

		// A fresh retryable failure starts a new base-delay window (attempt reset).
		inner.setErr(retryableErr())
		callsBefore := inner.starts.Load()
		_, _ = s.TryStart(t.Context())
		// Immediate retry must be gated (base window = 15s after reset).
		_, err = s.TryStart(t.Context())
		assert.Check(t, err != nil)
		assert.Check(t, is.Equal(inner.starts.Load(), callsBefore+1),
			"after success + stop: fresh failure must start a new base-delay window")
	})
}

// TestStartableToolSet_NoBackoffForNonRetryableFailures verifies that
// non-retryable errors (e.g. HTTP 401) do not set a backoff window —
// every TryStart reaches the underlying, preserving retry-every-turn
// behaviour for permanent config/auth errors.
func TestStartableToolSet_NoBackoffForNonRetryableFailures(t *testing.T) {
	t.Parallel()

	inner := &startErrToolSet{}
	inner.setErr(nonRetryableErr())
	s := newThrottledStartable(inner)

	for i := 1; i <= 3; i++ {
		_, err := s.TryStart(t.Context())
		assert.Check(t, err != nil, "attempt %d: expected error", i)
		assert.Check(t, is.Equal(inner.starts.Load(), int32(i)),
			"attempt %d: non-retryable error must not gate subsequent starts", i)
	}
}

// TestStartableToolSet_NoBackoffOnContextCancellation verifies that a Start
// failure caused by context cancellation does not set a backoff window.
// Context errors are shutdown signals and must never delay future attempts.
func TestStartableToolSet_NoBackoffOnContextCancellation(t *testing.T) {
	t.Parallel()

	inner := &startErrToolSet{}
	inner.setErr(context.Canceled)
	s := newThrottledStartable(inner)

	_, _ = s.TryStart(t.Context())
	assert.Check(t, is.Equal(inner.starts.Load(), int32(1)))

	// Immediate retry must proceed — no gate from a context error.
	_, _ = s.TryStart(t.Context())
	assert.Check(t, is.Equal(inner.starts.Load(), int32(2)),
		"context cancellation must not set a backoff window")
}

// TestStartableToolSet_RateLimitBacksOff verifies that HTTP 429 (rate
// limit) — the primary storm signal from issue #4060 — triggers the gate.
func TestStartableToolSet_RateLimitBacksOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &startErrToolSet{}
		inner.setErr(rateLimitErr())
		s := newThrottledStartable(inner)

		_, err := s.TryStart(t.Context())
		assert.Check(t, err != nil)
		assert.Check(t, is.Equal(inner.starts.Load(), int32(1)))

		// Immediate retry must be gated.
		_, err = s.TryStart(t.Context())
		assert.Check(t, err != nil)
		assert.Check(t, is.Equal(inner.starts.Load(), int32(1)),
			"rate-limit (429) must trigger the backoff gate in TryStart")
	})
}

// TestStartableToolSet_StopClearsBackoff verifies that an explicit Stop
// resets the backoff window so the next TryStart runs immediately.
func TestStartableToolSet_StopClearsBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &startErrToolSet{}
		inner.setErr(retryableErr())
		s := newThrottledStartable(inner)

		// Arm the backoff gate.
		_, _ = s.TryStart(t.Context())
		assert.Check(t, is.Equal(inner.starts.Load(), int32(1)))

		// Immediate TryStart is gated.
		_, _ = s.TryStart(t.Context())
		assert.Check(t, is.Equal(inner.starts.Load(), int32(1)))

		// Stop must reset the gate.
		assert.NilError(t, s.Stop(t.Context()))

		// The next TryStart must run the underlying attempt immediately.
		_, _ = s.TryStart(t.Context())
		assert.Check(t, is.Equal(inner.starts.Load(), int32(2)),
			"Stop must clear the backoff window so the next TryStart runs")
	})
}

// TestStartableToolSet_BackoffNoDoubleStartWithinWindow verifies that many
// concurrent TryStart calls against a failing toolset do not multiply the
// underlying retry activity. The single-flight TryLock serialises them: the
// first goroutine runs the underlying Start (failing), arms the gate, then
// releases the lock. All subsequent callers either skip the lock (TryLock
// already held → return (false,nil)) or hit the gate and receive the cached
// error — in both cases without invoking the underlying Start.
func TestStartableToolSet_BackoffNoDoubleStartWithinWindow(t *testing.T) {
	t.Parallel()

	inner := &startErrToolSet{}
	inner.setErr(retryableErr())
	s := newThrottledStartable(inner) // identity jitter → base window == 15s

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
		"concurrent TryStart calls must invoke the underlying Start exactly once")
}

// TestStartableToolSet_BackoffAtBoundary pins the window-expiry contract
// using a manual fake clock: a TryStart strictly before expiry is gated, while
// one at or after expiry is allowed through.
func TestStartableToolSet_BackoffAtBoundary(t *testing.T) {
	t.Parallel()

	inner := &startErrToolSet{}
	inner.setErr(retryableErr())

	var now time.Time
	now = time.Unix(1_000_000, 0) // arbitrary fixed start
	clock := func() time.Time { return now }

	s := tools.NewStartable(inner,
		tools.WithStartRetryJitter(identityJitter),
		tools.WithStartRetryClock(clock),
	)

	// Arm the gate: first TryStart fails, window = [now, now+base].
	_, err := s.TryStart(t.Context())
	assert.Check(t, err != nil)
	assert.Check(t, is.Equal(inner.starts.Load(), int32(1)))

	// One nanosecond before expiry: gate must hold.
	now = now.Add(tools.ExportedStartBackoffBase - time.Nanosecond)
	_, err = s.TryStart(t.Context())
	assert.Check(t, err != nil)
	assert.Check(t, is.Equal(inner.starts.Load(), int32(1)),
		"TryStart 1ns before window expiry must still be gated")

	// Exactly at expiry (clock == backoffUntil → not-before → gate open).
	now = now.Add(time.Nanosecond)
	_, err = s.TryStart(t.Context())
	assert.Check(t, err != nil, "still failing — expected error")
	assert.Check(t, is.Equal(inner.starts.Load(), int32(2)),
		"TryStart at expiry must be allowed through the gate")
}

// TestStartBackoffBoundedAndJittered validates the pure mathematical
// properties of computeStartBackoff without a full Start cycle:
//   - delay is always in [nominal, nominal+nominal/5] (additive 0–20% jitter)
//   - delay is always <= startBackoffMax
//   - no overflow or panic for large attempt counts
//   - attempt=1 uses the base delay as nominal
func TestStartBackoffBoundedAndJittered(t *testing.T) {
	t.Parallel()

	base := tools.ExportedStartBackoffBase
	backoffMax := tools.ExportedStartBackoffMax

	// Verify bounds across a range of attempt counts including a huge one.
	attempts := []int{1, 2, 3, 4, 5, 6, 7, 8, 100, 1000}
	for _, attempt := range attempts {
		// nominal is base * 2^(attempt-1), capped at max.
		nominal := base
		for i := 1; i < attempt; i++ {
			nominal *= 2
			if nominal >= backoffMax {
				nominal = backoffMax
				break
			}
		}

		// Call the pure helper multiple times to sample the jitter distribution.
		for range 20 {
			d := tools.ExportedComputeStartBackoff(attempt, nil)
			assert.Check(t, d > 0, "attempt %d: delay must be positive, got %s", attempt, d)
			// Additive jitter adds 0–20% of nominal; nominal is capped at
			// backoffMax, so the jittered delay is bounded by 1.2×backoffMax.
			assert.Check(t, d <= backoffMax+backoffMax/5,
				"attempt %d: delay %s exceeds 1.2×cap %s", attempt, d, backoffMax+backoffMax/5)
			assert.Check(t, d >= nominal,
				"attempt %d: delay %s is below additive-jitter floor %s", attempt, d, nominal)
			assert.Check(t, d <= nominal+nominal/5,
				"attempt %d: delay %s exceeds 1.2×nominal %s", attempt, d, nominal+nominal/5)
		}
	}

	// Identity jitter must return the nominal exactly.
	d := tools.ExportedComputeStartBackoff(1, identityJitter)
	assert.Check(t, is.Equal(d, base), "attempt 1 with identity jitter must equal base")

	d = tools.ExportedComputeStartBackoff(100, identityJitter)
	assert.Check(t, is.Equal(d, backoffMax), "large attempt with identity jitter must equal max")
}

// TestStartBackoffExponentialGrowth verifies that the nominal delay doubles
// with each attempt up to the cap (using identity jitter for precision).
func TestStartBackoffExponentialGrowth(t *testing.T) {
	t.Parallel()

	base := tools.ExportedStartBackoffBase
	backoffMax := tools.ExportedStartBackoffMax

	prev := time.Duration(0)
	for attempt := 1; attempt <= 8; attempt++ {
		d := tools.ExportedComputeStartBackoff(attempt, identityJitter)
		assert.Check(t, d >= prev, "attempt %d: delay must not decrease (got %s < %s)", attempt, d, prev)
		assert.Check(t, d <= backoffMax, "attempt %d: delay %s exceeds cap", attempt, d)
		if prev > 0 && prev < backoffMax {
			assert.Check(t, d == prev*2 || d == backoffMax,
				"attempt %d: expected double (%s) or cap (%s), got %s", attempt, prev*2, backoffMax, d)
		}
		prev = d
		if d == backoffMax {
			break // further attempts stay capped; no need to keep checking
		}
	}

	// Base case.
	assert.Check(t, is.Equal(tools.ExportedComputeStartBackoff(1, identityJitter), base))
}

// TestStartableToolSet_BackoffDormantForPlainErrors confirms that plain
// network errors (errors.New("boom") style, with no HTTP status code) never
// engage the backoff gate — the gate requires a retryable HTTP status code.
func TestStartableToolSet_BackoffDormantForPlainErrors(t *testing.T) {
	t.Parallel()

	// errors.New("boom") has no HTTP status code → no gate.
	inner := &startErrToolSet{}
	inner.setErr(errors.New("boom"))
	s := newThrottledStartable(inner)

	for i := 1; i <= 5; i++ {
		_, err := s.TryStart(t.Context())
		assert.Check(t, err != nil)
		assert.Check(t, is.Equal(inner.starts.Load(), int32(i)),
			"plain error must not engage backoff: attempt %d should have run", i)
	}
}

// TestStartableToolSet_PartialStartClearsBackoffGate pins that a
// PartialStartError (composite partially healthy) clears any active backoff
// window — the toolset is (partially) up, so the cold-start gate must not
// suppress the composite's next recovery attempt for the failed subset.
//
// Scenario: window expires → partial start latches and clears gate →
// immediate next TryStart must invoke the underlying without delay.
func TestStartableToolSet_PartialStartClearsBackoffGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &partialGateClearToolSet{}

		// Step 1: retryable failure arms the gate.
		inner.setErr(retryableErr())
		s := newThrottledStartable(inner)

		_, _ = s.TryStart(t.Context())
		assert.Check(t, is.Equal(inner.starts.Load(), int32(1)))

		// Step 2: advance past the window, then produce a partial start.
		time.Sleep(tools.ExportedStartBackoffBase + time.Millisecond) //nolint:forbidigo // inside synctest bubble
		partialErr := &tools.PartialStartError{Err: errors.New("inner-b broken")}
		inner.setErr(partialErr)

		_, err := s.TryStart(t.Context())
		assert.Check(t, tools.IsPartialStart(err), "expected partial start, got: %v", err)
		assert.Check(t, is.Equal(inner.starts.Load(), int32(2)))
		assert.Check(t, is.Equal(s.IsStarted(), true), "partial start must latch the wrapper")

		// Step 3: gate must now be cleared — a non-retryable error must
		// invoke the underlying immediately (no residual backoff window).
		inner.setErr(nonRetryableErr())
		_, _ = s.TryStart(t.Context())
		assert.Check(t, is.Equal(inner.starts.Load(), int32(3)),
			"after partial start clears the gate, next TryStart must reach the underlying")
	})
}

// partialGateClearToolSet is a minimal Startable + StartReporter for
// TestStartableToolSet_PartialStartClearsBackoffGate: IsStarted is true
// only when the last Start returned nil, matching the composite-toolset
// contract that drives the recovery path.
type partialGateClearToolSet struct {
	err    atomic.Pointer[error]
	starts atomic.Int32
	stable atomic.Bool
}

func (p *partialGateClearToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

func (p *partialGateClearToolSet) Start(context.Context) error {
	p.starts.Add(1)
	p.stable.Store(false) // degraded by default; only fully healthy on nil error
	if ptr := p.err.Load(); ptr != nil {
		return *ptr // PartialStartError keeps stable=false (still degraded)
	}
	p.stable.Store(true)
	return nil
}

func (p *partialGateClearToolSet) Stop(context.Context) error {
	p.stable.Store(false)
	return nil
}

func (p *partialGateClearToolSet) IsStarted() bool  { return p.stable.Load() }
func (p *partialGateClearToolSet) setErr(err error) { p.err.Store(&err) }

// reporterRecoveryToolSet is a minimal Startable + StartReporter whose
// Start() always increments a counter and returns the configured error.
// IsStarted() returns whatever the caller sets in the started field, so a
// test can simulate an external restart (e.g. via /toolset-restart) by
// setting started = true while the wrapper's s.started is still false.
type reporterRecoveryToolSet struct {
	starts    atomic.Int32
	startErr  atomic.Pointer[error]
	isStarted atomic.Bool
}

func (r *reporterRecoveryToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (r *reporterRecoveryToolSet) Start(context.Context) error {
	r.starts.Add(1)
	if p := r.startErr.Load(); p != nil {
		return *p
	}
	r.isStarted.Store(true)
	return nil
}

func (r *reporterRecoveryToolSet) Stop(context.Context) error {
	r.isStarted.Store(false)
	return nil
}
func (r *reporterRecoveryToolSet) IsStarted() bool       { return r.isStarted.Load() }
func (r *reporterRecoveryToolSet) setStartErr(err error) { r.startErr.Store(&err) }
func (r *reporterRecoveryToolSet) clearStartErr()        { r.startErr.Store(nil) }

// TestStartableToolSet_ExternalRecoveryClearsBackoffGate verifies that a
// live StartReporter clears a stale backoff window in tryStartLocked so a
// successful /toolset-restart is immediately visible via the next TryStart,
// without waiting for the window to expire.
//
// Scenario:
//  1. TryStart fails with a retryable error (gate armed).
//  2. External restart succeeds: inner toolset's IsStarted() flips to true
//     while the wrapper's started flag is still false.
//  3. TryStart sees the live reporter, adopts the state, clears the window,
//     and returns (true, nil) WITHOUT invoking the underlying Start.
func TestStartableToolSet_ExternalRecoveryClearsBackoffGate(t *testing.T) {
	t.Parallel()

	inner := &reporterRecoveryToolSet{}
	inner.setStartErr(retryableErr())
	s := newThrottledStartable(inner)

	// Step 1: arm the gate — TryStart fails, window = base (15 s with identity jitter).
	started, err := s.TryStart(t.Context())
	assert.Check(t, err != nil, "expected error arming the gate")
	assert.Check(t, !started)
	assert.Check(t, is.Equal(inner.starts.Load(), int32(1)))

	// Step 2: simulate an external restart (e.g. via /toolset-restart that
	// called the inner Restartable directly). The inner reports IsStarted=true
	// while the wrapper still has started=false and an active backoff window.
	inner.clearStartErr()       // no more start error
	inner.isStarted.Store(true) // inner reports live

	// Step 3: TryStart must detect the live reporter, adopt the state,
	// clear the gate, and return (true, nil) — without calling underlying Start.
	callsBefore := inner.starts.Load()
	started, err = s.TryStart(t.Context())
	assert.NilError(t, err, "TryStart must succeed after external recovery")
	assert.Check(t, started, "wrapper must be latched as started after reporter-based recovery")
	assert.Check(t, is.Equal(s.IsStarted(), true), "IsStarted must report started after recovery")
	assert.Check(t, is.Equal(inner.starts.Load(), callsBefore),
		"external recovery must not invoke underlying Start: the wrapper adopted the reporter's state")

	// Subsequent TryStart must short-circuit (latched healthy) without another Start.
	started, err = s.TryStart(t.Context())
	assert.NilError(t, err)
	assert.Check(t, started)
	assert.Check(t, is.Equal(inner.starts.Load(), callsBefore),
		"latched toolset must not be restarted on subsequent TryStart")
}

// TestStartableToolSet_BlockingStartSkipsGate pins that blocking Start() is
// never gated — it always reaches the underlying toolset — while a concurrent
// TryStart() within the same window is correctly throttled. This invariant
// keeps mcpcatalog enable and skill sub-session startup responsive regardless
// of backoff state.
func TestStartableToolSet_BlockingStartSkipsGate(t *testing.T) {
	t.Parallel()

	inner := &startErrToolSet{}
	inner.setErr(retryableErr())

	// Freeze the clock so the window stays active for the duration of the test.
	now := time.Unix(1_000_000, 0)
	s := tools.NewStartable(inner,
		tools.WithStartRetryJitter(identityJitter),
		tools.WithStartRetryClock(func() time.Time { return now }),
	)

	// Arm the gate via TryStart.
	_, err := s.TryStart(t.Context())
	assert.Check(t, err != nil)
	assert.Check(t, is.Equal(inner.starts.Load(), int32(1)))

	// Blocking Start must invoke the underlying even while the gate is active.
	err = s.Start(t.Context())
	assert.Check(t, err != nil, "blocking Start must fail (retryableErr still set)")
	assert.Check(t, is.Equal(inner.starts.Load(), int32(2)),
		"blocking Start must bypass the gate and reach the underlying")

	// TryStart within the (now re-armed) window must still be gated.
	_, err = s.TryStart(t.Context())
	assert.Check(t, err != nil)
	assert.Check(t, is.Equal(inner.starts.Load(), int32(2)),
		"TryStart within window must be gated: underlying must not be invoked again")
}
