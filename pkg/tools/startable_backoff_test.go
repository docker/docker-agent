package tools_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"

	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
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

// TestStartableToolSet_PlainTextStatusShapeDoesNotArmGate pins that a plain
// error whose message *contains* a status-shaped number (e.g. "503" in a
// URL path or "429 of 812" in a progress log) does NOT arm the gate.
// RetryableHTTPStatus may return true for these via its regex fallback, but
// startBackoffRetryable pre-filters to *modelerrors.StatusError, so the
// regex path never reaches the gate. This guards against accidental
// loosening of the classifier.
func TestStartableToolSet_PlainTextStatusShapeDoesNotArmGate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
	}{
		{"status text in message", errors.New("upstream: 503 Service Unavailable")},
		{"status in port number", errors.New("Post \"http://localhost:503/mcp\": connection refused")},
		{"status in progress counter", errors.New("chunk 429 of 812 failed")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner := &startErrToolSet{}
			inner.setErr(tc.err)
			s := newThrottledStartable(inner)
			for i := int32(1); i <= 3; i++ {
				_, err := s.TryStart(t.Context())
				assert.Check(t, err != nil)
				assert.Check(t, is.Equal(inner.starts.Load(), i),
					"status-shaped plain error must not arm gate (attempt %d)", i)
			}
		})
	}
}

// TestStartableToolSet_NonRetryablePartialStartClearsBackoffGate pins that a
// PartialStartError whose aggregated cause is NOT retryable (composite
// partially healthy) clears any active backoff window — the toolset is
// (partially) up, and its failed subset isn't a pacing candidate, so the
// cold-start gate must not suppress the composite's next recovery attempt.
//
// Scenario: window expires → partial start (plain error cause) latches and
// clears gate → immediate next TryStart must invoke the underlying without
// delay. A partial start whose cause IS retryable instead arms the gate —
// see TestStartableToolSet_RetryablePartialStartArmsBackoffGate.
func TestStartableToolSet_NonRetryablePartialStartClearsBackoffGate(t *testing.T) {
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

// TestStartableToolSet_RetryablePartialStartArmsBackoffGate is the fix for
// #4067: a PartialStartError whose aggregated cause IS retryable must arm
// the gate exactly like a total failure would, even though the wrapper
// stays latched as started (s.started=true) so the healthy subset keeps
// listing. Without this, a degraded code-mode composite retries its failed
// inner subset (e.g. a RAG toolset hitting 429s) unpaced on every turn.
func TestStartableToolSet_RetryablePartialStartArmsBackoffGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &partialGateClearToolSet{}
		inner.setErr(tools.NewPartialStartError(rateLimitErr()))
		s := newThrottledStartable(inner)

		// Attempt 1: underlying Start runs, partial failure latches started.
		_, err := s.TryStart(t.Context())
		assert.Check(t, tools.IsPartialStart(err), "expected partial start, got: %v", err)
		assert.Check(t, is.Equal(inner.starts.Load(), int32(1)))
		assert.Check(t, is.Equal(s.IsStarted(), true), "partial start must latch the wrapper")

		// Immediate retry via TryStart: gate must block it (still within window).
		_, err = s.TryStart(t.Context())
		assert.Check(t, tools.IsPartialStart(err), "gate must still report the retained partial error")
		assert.Check(t, is.Equal(inner.starts.Load(), int32(1)), "gate must not invoke underlying Start within window")
		assert.Check(t, is.Equal(s.IsStarted(), true), "healthy subset must stay listed while gated")

		// Advance the fake clock past the base window; the next TryStart must
		// reach the underlying again.
		time.Sleep(tools.ExportedStartBackoffBase + time.Millisecond) //nolint:forbidigo // inside synctest bubble
		_, err = s.TryStart(t.Context())
		assert.Check(t, tools.IsPartialStart(err), "still degraded — expected partial start after window")
		assert.Check(t, is.Equal(inner.starts.Load(), int32(2)), "underlying Start must be called again after window")
	})
}

// TestStartableToolSet_MixedAuthPartialStartArmsBackoffGate pins the
// ANY-cause semantics of the #4067 fix: a PartialStartError joining one
// authorization-required cause with one retryable cause must still arm the
// gate (errors.As walks the whole errors.Join tree, so one retryable cause
// is enough), even though the batch stays classified as NOT auth-only —
// mirroring the ALL-causes semantics IsAuthorizationRequired already uses
// for AuthOnly.
func TestStartableToolSet_MixedAuthPartialStartArmsBackoffGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		authErr := &tools.AuthorizationRequiredError{URL: "https://example.test/mcp"}
		inner := &partialGateClearToolSet{}
		inner.setErr(tools.NewPartialStartError(authErr, rateLimitErr()))
		s := newThrottledStartable(inner)

		_, err := s.TryStart(t.Context())
		assert.Check(t, tools.IsPartialStart(err))
		assert.Check(t, !tools.IsAuthorizationRequired(err), "mixed batch must not be classified auth-only")
		assert.Check(t, is.Equal(inner.starts.Load(), int32(1)))

		_, err = s.TryStart(t.Context())
		assert.Check(t, err != nil)
		assert.Check(t, is.Equal(inner.starts.Load(), int32(1)), "the retryable cause in the mix must still arm the gate")
	})
}

// partialGateClearToolSet is a minimal Startable + StartReporter for the
// partial-start backoff-gate tests above: IsStarted is true only when the
// last Start returned nil, matching the composite-toolset contract that
// drives the recovery path.
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

// retryAfterStatusErr returns a *modelerrors.StatusError for status 429 that
// carries a Retry-After hint, for testing that the gate honours the hint.
func retryAfterStatusErr(hint time.Duration) error {
	return &modelerrors.StatusError{
		StatusCode: 429,
		Err:        errors.New("rate limited"),
		RetryAfter: hint,
	}
}

// TestStartableToolSet_RetryAfterHonoured verifies that a Retry-After header
// larger than the computed window extends the gate to at least the hint
// duration (jittered, so actual window ∈ [hint, 1.2×hint]).
// Uses identity jitter so the window equals hint exactly.
func TestStartableToolSet_RetryAfterHonoured(t *testing.T) {
	t.Parallel()

	const hint = 60 * time.Second // much larger than the 15s base

	inner := &startErrToolSet{}
	inner.setErr(retryAfterStatusErr(hint))

	base := time.Unix(2_000_000, 0)
	now := base
	s := tools.NewStartable(inner,
		tools.WithStartRetryJitter(identityJitter), // window == hint exactly
		tools.WithStartRetryClock(func() time.Time { return now }),
	)

	// Arm the gate.
	_, err := s.TryStart(t.Context())
	assert.Check(t, err != nil)
	assert.Check(t, is.Equal(inner.starts.Load(), int32(1)))

	// 1s before the hint expires: still gated.
	now = base.Add(hint - time.Second)
	_, err = s.TryStart(t.Context())
	assert.Check(t, err != nil)
	assert.Check(t, is.Equal(inner.starts.Load(), int32(1)),
		"gate must hold until Retry-After hint expires")

	// 1ms past the hint: gate open.
	now = base.Add(hint + time.Millisecond)
	_, err = s.TryStart(t.Context())
	assert.Check(t, err != nil, "still failing — expected error")
	assert.Check(t, is.Equal(inner.starts.Load(), int32(2)),
		"gate must open once Retry-After hint elapses")
}

// TestStartableToolSet_RetryAfterJittered verifies that jitter is applied to
// the Retry-After path so concurrent clients spread their retries rather than
// all firing at the exact server-supplied deadline.
//
// Strategy: create 60 independent StartableToolSets, each armed with the same
// Retry-After hint. Probe each at hint + half-jitter-range (midpoint of
// [hint, 1.2*hint]). Roughly half should be open and half gated, demonstrating
// that windows are spread across the jitter band rather than all set to hint.
func TestStartableToolSet_RetryAfterJittered(t *testing.T) {
	t.Parallel()

	const hint = 30 * time.Second
	const midpoint = hint + hint/10 // hint + 10% ≈ midpoint of [hint, 1.2*hint]

	openCount := 0
	const samples = 60
	for range samples {
		inner := &startErrToolSet{}
		inner.setErr(retryAfterStatusErr(hint))

		base := time.Unix(7_000_000, 0)
		now := base
		s := tools.NewStartable(inner,
			tools.WithStartRetryClock(func() time.Time { return now }),
			// No WithStartRetryJitter: use real (random) jitter.
		)
		// Arm the gate.
		_, _ = s.TryStart(t.Context())

		// Probe at midpoint: gate should be open for ~half the samples.
		now = base.Add(midpoint)
		_, tryErr := s.TryStart(t.Context())
		if inner.starts.Load() > 1 || tryErr == nil {
			openCount++
		}
	}

	// With uniform jitter over [hint, 1.2*hint], the midpoint sits at the centre:
	// roughly 50% should be open. Allow wide margins to avoid flakiness.
	assert.Check(t, openCount > 5,
		"at least some samples must have windows shorter than the midpoint; got %d/%d open", openCount, samples)
	assert.Check(t, openCount < samples-5,
		"at least some samples must have windows longer than the midpoint; got %d/%d open", openCount, samples)
}

// TestStartableToolSet_RetryAfterBelowComputed verifies that when the
// Retry-After hint is smaller than the computed exponential window the
// hint is ignored and the computed delay is used instead.
func TestStartableToolSet_RetryAfterBelowComputed(t *testing.T) {
	t.Parallel()

	// At attempt 2 the computed delay (identity jitter) = 2 × 15s = 30s.
	// Set hint = 5s, which is well below 30s — it must be ignored.
	const smallHint = 5 * time.Second
	inner := &startErrToolSet{}
	inner.setErr(retryAfterStatusErr(smallHint))

	base := time.Unix(8_000_000, 0)
	now := base
	s := tools.NewStartable(inner,
		tools.WithStartRetryJitter(identityJitter),
		tools.WithStartRetryClock(func() time.Time { return now }),
	)
	// Attempt 1: computed delay = base (15s). hint (5s) < 15s → ignored.
	_, _ = s.TryStart(t.Context())

	// 6s in: still gated (hint would have passed, computed window holds).
	now = base.Add(smallHint + time.Second)
	_, _ = s.TryStart(t.Context())
	assert.Check(t, is.Equal(inner.starts.Load(), int32(1)),
		"gate must hold past hint when hint < computed delay")

	// Past the computed 15s window: gate open.
	now = base.Add(tools.ExportedStartBackoffBase + time.Millisecond)
	_, _ = s.TryStart(t.Context())
	assert.Check(t, is.Equal(inner.starts.Load(), int32(2)),
		"gate must open at the computed window when hint is smaller")
}

// TestStartableToolSet_RetryAfterCapAtMax verifies that a very large
// Retry-After hint is capped at startBackoffMax so a misbehaving provider
// cannot stall the toolset indefinitely.
func TestStartableToolSet_RetryAfterCapAtMax(t *testing.T) {
	t.Parallel()

	const giantHint = 24 * time.Hour // absurdly large
	inner := &startErrToolSet{}
	inner.setErr(retryAfterStatusErr(giantHint))

	base := time.Unix(9_000_000, 0)
	now := base
	s := tools.NewStartable(inner,
		tools.WithStartRetryJitter(identityJitter), // identity → window == cap exactly
		tools.WithStartRetryClock(func() time.Time { return now }),
	)
	// Arm the gate.
	_, _ = s.TryStart(t.Context())

	// 1s before cap: still gated.
	now = base.Add(tools.ExportedStartBackoffMax - time.Second)
	_, _ = s.TryStart(t.Context())
	assert.Check(t, is.Equal(inner.starts.Load(), int32(1)),
		"gate must hold until the cap expires (not the giant hint)")

	// 1ms past cap: gate open.
	now = base.Add(tools.ExportedStartBackoffMax + time.Millisecond)
	_, _ = s.TryStart(t.Context())
	assert.Check(t, is.Equal(inner.starts.Load(), int32(2)),
		"gate must open at startBackoffMax, not at the uncapped hint")
}

// TestStartBackoffRetryable_ErrServerUnavailable verifies that a missing binary
// (ErrServerUnavailable) does NOT arm the gate — it fails promptly every turn.
func TestStartBackoffRetryable_ErrServerUnavailable(t *testing.T) {
	inner := &startErrToolSet{}
	inner.setErr(fmt.Errorf("%w: no such file or directory", lifecycle.ErrServerUnavailable))
	s := newThrottledStartable(inner)

	for range 3 {
		_, err := s.TryStart(t.Context())
		assert.Check(t, err != nil)
	}
	assert.Check(t, is.Equal(inner.starts.Load(), int32(3)),
		"missing-binary errors must not pace retries")
}

// TestStartBackoffRetryable_ErrTransport verifies that a network-level failure
// (connection refused, no such host) does NOT arm the gate.
func TestStartBackoffRetryable_ErrTransport(t *testing.T) {
	inner := &startErrToolSet{}
	inner.setErr(fmt.Errorf("%w: connection refused", lifecycle.ErrTransport))
	s := newThrottledStartable(inner)

	for range 3 {
		_, _ = s.TryStart(t.Context())
	}
	assert.Check(t, is.Equal(inner.starts.Load(), int32(3)),
		"transport errors must not pace retries")
}

// TestStartBackoffRetryable_ErrAuthRequired verifies that a permanent auth
// failure (OAuth required / invalid token) does NOT arm the gate.
func TestStartBackoffRetryable_ErrAuthRequired(t *testing.T) {
	inner := &startErrToolSet{}
	inner.setErr(fmt.Errorf("%w: token expired", lifecycle.ErrAuthRequired))
	s := newThrottledStartable(inner)

	for range 3 {
		_, _ = s.TryStart(t.Context())
	}
	assert.Check(t, is.Equal(inner.starts.Load(), int32(3)),
		"auth-required errors must not pace retries")
}

// TestStartBackoffRetryable_4xxStatusDoesNotArm verifies that a structured
// 4xx HTTP error (client error, not a rate-limit) wraps as *StatusError but
// does not arm the gate.
func TestStartBackoffRetryable_4xxStatusDoesNotArm(t *testing.T) {
	inner := &startErrToolSet{}
	inner.setErr(&modelerrors.StatusError{StatusCode: 400, Err: errors.New("bad request")})
	s := newThrottledStartable(inner)

	for range 3 {
		_, _ = s.TryStart(t.Context())
	}
	assert.Check(t, is.Equal(inner.starts.Load(), int32(3)),
		"400 client errors must not arm the backoff gate")
}

// TestStartBackoffRetryable_ErrServerCrashed verifies that a bare
// lifecycle.ErrServerCrashed (a single crash, not escalated to a loop) does
// NOT arm the gate: it is the supervisor's own restart policy's job, not
// this gate's.
func TestStartBackoffRetryable_ErrServerCrashed(t *testing.T) {
	inner := &startErrToolSet{}
	inner.setErr(fmt.Errorf("%w: exit status 1", lifecycle.ErrServerCrashed))
	s := newThrottledStartable(inner)

	for range 3 {
		_, err := s.TryStart(t.Context())
		assert.Check(t, err != nil)
	}
	assert.Check(t, is.Equal(inner.starts.Load(), int32(3)),
		"a one-off server crash must not pace retries")
}

// TestStartBackoffRetryable_ErrCrashLooping verifies that
// lifecycle.ErrCrashLooping — the supervisor's own verdict that a crash
// loop is underway — arms the gate exactly like a retryable HTTP status,
// via TryStart end-to-end.
func TestStartBackoffRetryable_ErrCrashLooping(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &startErrToolSet{}
		inner.setErr(fmt.Errorf("%w: %w", lifecycle.ErrCrashLooping, lifecycle.ErrServerCrashed))
		s := newThrottledStartable(inner)

		// Attempt 1: underlying Start runs and fails.
		_, err := s.TryStart(t.Context())
		assert.Check(t, err != nil)
		assert.Check(t, is.Equal(inner.starts.Load(), int32(1)))

		// Immediate retry: gate must block it (still within the window).
		_, err = s.TryStart(t.Context())
		assert.Check(t, err != nil)
		assert.Check(t, is.Equal(inner.starts.Load(), int32(1)),
			"a crash loop must arm the gate like a retryable HTTP status")

		// After the window expires the next TryStart runs the underlying attempt.
		time.Sleep(tools.ExportedStartBackoffBase + time.Millisecond) //nolint:forbidigo // inside synctest bubble
		_, err = s.TryStart(t.Context())
		assert.Check(t, err != nil)
		assert.Check(t, is.Equal(inner.starts.Load(), int32(2)))
	})
}
