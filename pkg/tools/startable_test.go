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

	"github.com/docker/docker-agent/pkg/tools"
)

// stubDescriber implements ToolSet and Describer.
type stubDescriber struct{ desc string }

func (s *stubDescriber) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (s *stubDescriber) Describe() string                            { return s.desc }

// stubToolSet implements ToolSet only (no Describer).
type stubToolSet struct{}

func (s *stubToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

// flappyToolSet implements ToolSet + Startable with a scripted sequence of errors.
// Each call to Start() consumes the next error from errs; nil means success.
type flappyToolSet struct {
	errs     []error
	callIdx  int
	startups int // number of successful Start() calls
}

func (f *flappyToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{Name: "flappy_tool"}}, nil
}

func (f *flappyToolSet) Start(_ context.Context) error {
	if f.callIdx < len(f.errs) {
		err := f.errs[f.callIdx]
		f.callIdx++
		if err != nil {
			return err
		}
	}
	f.startups++
	return nil
}

func (f *flappyToolSet) Stop(_ context.Context) error {
	return nil
}

// listFlappyToolSet implements ToolSet with a scripted sequence of errors
// returned from Tools(). nil in the sequence means a successful listing.
type listFlappyToolSet struct {
	errs    []error
	callIdx int
}

func (f *listFlappyToolSet) Tools(context.Context) ([]tools.Tool, error) {
	if f.callIdx < len(f.errs) {
		err := f.errs[f.callIdx]
		f.callIdx++
		if err != nil {
			return nil, err
		}
	}
	return []tools.Tool{{Name: "flappy_tool"}}, nil
}

func (f *listFlappyToolSet) Stop(_ context.Context) error { return nil }

func TestDescribeToolSet_UsesDescriber(t *testing.T) {
	t.Parallel()

	ts := &stubDescriber{desc: "mcp(ref=docker:github-official)"}
	assert.Check(t, is.Equal(tools.DescribeToolSet(ts), "mcp(ref=docker:github-official)"))
}

func TestDescribeToolSet_UnwrapsStartableAndUsesDescriber(t *testing.T) {
	t.Parallel()

	inner := &stubDescriber{desc: "mcp(stdio cmd=python args=-m,srv)"}
	wrapped := tools.NewStartable(inner)
	assert.Check(t, is.Equal(tools.DescribeToolSet(wrapped), "mcp(stdio cmd=python args=-m,srv)"))
}

func TestDescribeToolSet_FallsBackToTypeName(t *testing.T) {
	t.Parallel()

	ts := &stubToolSet{}
	assert.Check(t, is.Equal(tools.DescribeToolSet(ts), "*tools_test.stubToolSet"))
}

func TestDescribeToolSet_FallsBackToTypeNameWhenDescribeEmpty(t *testing.T) {
	t.Parallel()

	ts := &stubDescriber{desc: ""}
	assert.Check(t, is.Equal(tools.DescribeToolSet(ts), "*tools_test.stubDescriber"))
}

func TestDescribeToolSet_UnwrapsStartableAndFallsBackToTypeName(t *testing.T) {
	t.Parallel()

	inner := &stubToolSet{}
	wrapped := tools.NewStartable(inner)
	assert.Check(t, is.Equal(tools.DescribeToolSet(wrapped), "*tools_test.stubToolSet"))
}

// TestStartableToolSet_ShouldReportFailure_OncePerStreak verifies that
// ShouldReportFailure returns true exactly once per failure streak,
// suppressing duplicate warnings on repeated retries.
func TestStartableToolSet_ShouldReportFailure_OncePerStreak(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	f := &flappyToolSet{errs: []error{errBoom, errBoom, nil}}
	s := tools.NewStartable(f)

	// Turn 1: first failure — should report.
	assert.Check(t, s.Start(t.Context()) != nil, "expected error on turn 1")
	assert.Check(t, is.Equal(s.ShouldReportFailure(), true), "turn 1: first failure should be reported")
	assert.Check(t, is.Equal(s.ShouldReportFailure(), false), "turn 1: second call must return false")

	// Turn 2: second failure in same streak — must NOT report again.
	assert.Check(t, s.Start(t.Context()) != nil, "expected error on turn 2")
	assert.Check(t, is.Equal(s.ShouldReportFailure(), false), "turn 2: duplicate failure must not report")

	// Turn 3: success — silent recovery, no caller-visible event.
	assert.Check(t, s.Start(t.Context()) == nil, "expected success on turn 3")
	assert.Check(t, is.Equal(s.ShouldReportFailure(), false), "turn 3: success must not report a failure")
}

// TestStartableToolSet_RecoveryResetsStreak verifies that a successful
// Start() implicitly resets the failure streak: after a fail → succeed
// cycle, a fresh failure on the *next* streak is reported again.
func TestStartableToolSet_RecoveryResetsStreak(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	f := &flappyToolSet{errs: []error{errBoom, nil, errBoom}}
	s := tools.NewStartable(f)

	// Cycle 1: fail then recover.
	assert.Check(t, s.Start(t.Context()) != nil)
	assert.Check(t, is.Equal(s.ShouldReportFailure(), true))

	assert.Check(t, s.Start(t.Context()) == nil)

	// Stop so we can attempt to start again — a successful Start() marks
	// the toolset as started, so subsequent Start() calls short-circuit.
	assert.Check(t, s.Stop(t.Context()) == nil)

	// Cycle 2: new failure must warn again, proving the recovery reset
	// the streak even though no caller signalled it.
	assert.Check(t, s.Start(t.Context()) != nil)
	assert.Check(t, is.Equal(s.ShouldReportFailure(), true), "fresh failure after recovery must warn")
}

// TestStartableToolSet_StopResetsFailureState verifies that after a failure streak,
// an explicit Stop() clears all tracking so the next failure warns again.
func TestStartableToolSet_StopResetsFailureState(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	f := &flappyToolSet{errs: []error{errBoom, errBoom}}
	s := tools.NewStartable(f)

	// First failure: consume the warning.
	assert.Check(t, s.Start(t.Context()) != nil)
	assert.Check(t, is.Equal(s.ShouldReportFailure(), true))

	// Stop resets state.
	assert.Check(t, s.Stop(t.Context()) == nil)

	// Second failure after Stop: must warn again.
	assert.Check(t, s.Start(t.Context()) != nil)
	assert.Check(t, is.Equal(s.ShouldReportFailure(), true), "failure after Stop must produce fresh warning")
}

// TestStartableToolSet_ShouldReportListFailure_OncePerStreak verifies that
// ShouldReportListFailure returns true exactly once per Tools() failure streak,
// suppressing duplicate warnings on repeated retries.
func TestStartableToolSet_ShouldReportListFailure_OncePerStreak(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("toolset not started")
	f := &listFlappyToolSet{errs: []error{errBoom, errBoom, nil}}
	s := tools.NewStartable(f)

	// Turn 1: first failure — should report.
	_, err := s.Tools(t.Context())
	assert.Check(t, err != nil, "expected list error on turn 1")
	assert.Check(t, is.Equal(s.ShouldReportListFailure(), true), "turn 1: first failure should be reported")
	assert.Check(t, is.Equal(s.ShouldReportListFailure(), false), "turn 1: second call must return false")

	// Turn 2: second failure in same streak — must NOT report again.
	_, err = s.Tools(t.Context())
	assert.Check(t, err != nil, "expected list error on turn 2")
	assert.Check(t, is.Equal(s.ShouldReportListFailure(), false), "turn 2: duplicate failure must not report")

	// Turn 3: success — silent recovery.
	_, err = s.Tools(t.Context())
	assert.Check(t, err == nil, "expected success on turn 3")
	assert.Check(t, is.Equal(s.ShouldReportListFailure(), false), "turn 3: success must not report a failure")
}

// TestStartableToolSet_ListFailureRecoveryResetsStreak verifies that a
// successful Tools() call resets the list-failure streak: after a
// fail → succeed → fail cycle, the fresh failure is reported again.
func TestStartableToolSet_ListFailureRecoveryResetsStreak(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("toolset not started")
	f := &listFlappyToolSet{errs: []error{errBoom, nil, errBoom}}
	s := tools.NewStartable(f)

	_, err := s.Tools(t.Context())
	assert.Check(t, err != nil)
	assert.Check(t, is.Equal(s.ShouldReportListFailure(), true))

	_, err = s.Tools(t.Context())
	assert.Check(t, err == nil)

	_, err = s.Tools(t.Context())
	assert.Check(t, err != nil)
	assert.Check(t, is.Equal(s.ShouldReportListFailure(), true), "fresh failure after recovery must warn")
}

type reportingToolSet struct {
	started      bool
	startCalls   int
	restartCalls int
}

func (r *reportingToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{Name: "reporting_tool"}}, nil
}

func (r *reportingToolSet) Start(context.Context) error {
	r.startCalls++
	r.started = true
	return nil
}

func (r *reportingToolSet) Stop(context.Context) error {
	r.started = false
	return nil
}

func (r *reportingToolSet) IsStarted() bool { return r.started }

func (r *reportingToolSet) Restart(context.Context) error {
	r.restartCalls++
	r.started = true
	return nil
}

type reportingStartOnlyToolSet struct {
	started    bool
	startCalls int
}

func (r *reportingStartOnlyToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{Name: "start_only_tool"}}, nil
}

func (r *reportingStartOnlyToolSet) Start(context.Context) error {
	r.startCalls++
	r.started = true
	return nil
}

func (r *reportingStartOnlyToolSet) Stop(context.Context) error {
	r.started = false
	return nil
}

func (r *reportingStartOnlyToolSet) IsStarted() bool { return r.started }

func TestStartableToolSet_RecoversDeadUnderlyingWithRestart(t *testing.T) {
	t.Parallel()

	inner := &reportingToolSet{}
	s := tools.NewStartable(inner)

	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(s.IsStarted(), true))
	assert.Check(t, is.Equal(inner.startCalls, 1))
	assert.Check(t, is.Equal(inner.restartCalls, 0))

	inner.started = false
	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(s.IsStarted(), true))
	assert.Check(t, is.Equal(inner.startCalls, 1), "recovery should prefer Restart over Start")
	assert.Check(t, is.Equal(inner.restartCalls, 1))
}

func TestStartableToolSet_RecoversDeadUnderlyingWithStartFallback(t *testing.T) {
	t.Parallel()

	inner := &reportingStartOnlyToolSet{}
	s := tools.NewStartable(inner)

	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(inner.startCalls, 1))

	inner.started = false
	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(s.IsStarted(), true))
	assert.Check(t, is.Equal(inner.startCalls, 2))
}

func TestStartableToolSet_NoStartReporterPreservesLatchedStart(t *testing.T) {
	t.Parallel()

	inner := &flappyToolSet{}
	s := tools.NewStartable(inner)

	assert.NilError(t, s.Start(t.Context()))
	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(inner.startups, 1))
}

// recoveryFailingToolSet simulates a toolset that starts successfully on
// the first attempt (Start) and then fails on every Restart call,
// representing a toolset that was working but became unavailable.
type recoveryFailingToolSet struct {
	started    bool
	restartErr error
}

func (r *recoveryFailingToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (r *recoveryFailingToolSet) IsStarted() bool                             { return r.started }
func (r *recoveryFailingToolSet) Start(context.Context) error {
	r.started = true
	return nil
}
func (r *recoveryFailingToolSet) Restart(_ context.Context) error { return r.restartErr }
func (r *recoveryFailingToolSet) Stop(_ context.Context) error {
	r.started = false
	return nil
}

// TestStartableToolSet_ShouldReportRecoveryFailure_OncePerStreak verifies
// that ShouldReportRecoveryFailure returns true exactly once when a
// previously-started toolset fails to recover (recovering=true path), and
// is silent for subsequent calls in the same streak.
func TestStartableToolSet_ShouldReportRecoveryFailure_OncePerStreak(t *testing.T) {
	t.Parallel()

	authErr := errors.New("authentication required")
	inner := &recoveryFailingToolSet{restartErr: authErr}
	s := tools.NewStartable(inner)

	// First Start: succeeds and marks the toolset as started.
	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(s.ShouldReportRecoveryFailure(), false), "no recovery failure yet")

	// Simulate the inner toolset going down (e.g. background reconnect failed).
	inner.started = false

	// Recovery attempt 1: Restart fails → streak begins.
	assert.Check(t, s.Start(t.Context()) != nil, "expected error on recovery")
	assert.Check(t, is.Equal(s.ShouldReportRecoveryFailure(), true), "first recovery failure must be reported")
	assert.Check(t, is.Equal(s.ShouldReportRecoveryFailure(), false), "second call in same streak must be false")
}

// TestStartableToolSet_ShouldReportRecoveryFailure_NotFiredForInitialStartup
// verifies that ShouldReportRecoveryFailure is NOT triggered for initial-
// startup failures (toolset was never started before). Only recovery
// failures (toolset was working, then failed) should trigger the notice.
func TestStartableToolSet_ShouldReportRecoveryFailure_NotFiredForInitialStartup(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("startup error")
	f := &flappyToolSet{errs: []error{errBoom, errBoom}}
	s := tools.NewStartable(f)

	// Turn 1: initial startup failure (never started before).
	assert.Check(t, s.Start(t.Context()) != nil)
	assert.Check(t, is.Equal(s.ShouldReportRecoveryFailure(), false),
		"initial-startup failure must NOT trigger recovery notice")

	// Turn 2: second startup failure.
	assert.Check(t, s.Start(t.Context()) != nil)
	assert.Check(t, is.Equal(s.ShouldReportRecoveryFailure(), false),
		"repeated initial-startup failure must NOT trigger recovery notice")
}

// TestStartableToolSet_ShouldReportRecoveryFailure_ResetsOnSuccess verifies
// that a successful recovery clears the streak so a future failure is
// reported as fresh.
func TestStartableToolSet_ShouldReportRecoveryFailure_ResetsOnSuccess(t *testing.T) {
	t.Parallel()

	authErr := errors.New("authentication required")
	inner := &recoveryFailingToolSet{restartErr: authErr}
	s := tools.NewStartable(inner)

	// Initial start succeeds (Start always returns nil for recoveryFailingToolSet).
	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(s.IsStarted(), true))
	assert.Check(t, is.Equal(s.ShouldReportRecoveryFailure(), false), "no recovery failure yet")

	// Background failure: inner loses its connection.
	inner.started = false

	// Recovery fails: Restart returns authErr.
	err := s.Start(t.Context())
	assert.Check(t, err != nil, "expected error on recovery failure")
	assert.Check(t, is.Equal(s.ShouldReportRecoveryFailure(), true), "first recovery failure must be reported")
	assert.Check(t, is.Equal(s.ShouldReportRecoveryFailure(), false), "second call in same streak must return false (dedup)")

	// Successful recovery: clear the error so the next Start goes through and
	// resets the streak. Because s.started==false after the failed Restart,
	// Start takes the non-recovery path (inner.Start), which succeeds.
	inner.restartErr = nil
	assert.NilError(t, s.Start(t.Context()), "recovery with nil restartErr must succeed")
	assert.Check(t, is.Equal(s.IsStarted(), true))
	assert.Check(t, is.Equal(s.ShouldReportRecoveryFailure(), false),
		"after successful recovery, the streak must be reset")

	// A subsequent background failure after the reset is a fresh streak.
	inner.restartErr = authErr
	inner.started = false
	_ = s.Start(t.Context())
	assert.Check(t, is.Equal(s.ShouldReportRecoveryFailure(), true),
		"fresh failure after streak reset must be reported")
}

// TestStartableToolSet_ShouldReportRecoveryFailure_ResetsOnStop verifies
// that Stop clears the recovery streak.
func TestStartableToolSet_ShouldReportRecoveryFailure_ResetsOnStop(t *testing.T) {
	t.Parallel()

	authErr := errors.New("authentication required")
	inner := &recoveryFailingToolSet{restartErr: authErr}
	s := tools.NewStartable(inner)

	// Initial start → recovery failure → consume the once-report.
	assert.NilError(t, s.Start(t.Context()))
	inner.started = false
	assert.Check(t, s.Start(t.Context()) != nil)
	assert.Check(t, is.Equal(s.ShouldReportRecoveryFailure(), true), "must report once")

	// Stop resets all streaks.
	assert.NilError(t, s.Stop(t.Context()))

	// A new recovery cycle after Stop must report again.
	inner.started = false // inner Stop set it false, but simulate inner starting first
	inner.restartErr = nil
	assert.NilError(t, s.Start(t.Context())) // inner Start succeeds (restartErr cleared)
	inner.started = false
	inner.restartErr = authErr

	assert.Check(t, s.Start(t.Context()) != nil)
	assert.Check(t, is.Equal(s.ShouldReportRecoveryFailure(), true), "fresh recovery after Stop must report again")
}

// blockingToolSet blocks in Start until release is closed. entered is closed
// on the first Start call so tests can line up with the in-flight attempt.
type blockingToolSet struct {
	entered  chan struct{}
	release  chan struct{}
	startErr error
	calls    atomic.Int32
	stops    atomic.Int32
}

func (b *blockingToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

func (b *blockingToolSet) Start(context.Context) error {
	if b.calls.Add(1) == 1 {
		close(b.entered)
	}
	<-b.release
	return b.startErr
}

func (b *blockingToolSet) Stop(context.Context) error {
	b.stops.Add(1)
	return nil
}

// TestStartableToolSet_TryStartSkipsInFlightStart pins the TryStart contract
// for #4001: while another Start holds the single-flight lock, TryStart
// returns (false, nil) immediately — no second underlying Start, no failure
// streak — and TryIsStarted reports not-ready without blocking.
func TestStartableToolSet_TryStartSkipsInFlightStart(t *testing.T) {
	t.Parallel()

	inner := &blockingToolSet{entered: make(chan struct{}), release: make(chan struct{})}
	s := tools.NewStartable(inner)

	assert.Check(t, is.Equal(s.TryIsStarted(), false), "TryIsStarted is false while unstarted")

	// Always unblock the wedged Start so no goroutine outlives the test.
	release := sync.OnceFunc(func() { close(inner.release) })
	t.Cleanup(release)

	startDone := make(chan error, 1)
	go func() { startDone <- s.Start(t.Context()) }()
	<-inner.entered

	started, err := s.TryStart(t.Context())
	assert.NilError(t, err, "an in-flight start must not surface as a failure")
	assert.Check(t, !started, "TryStart must not join an in-flight start")
	assert.Check(t, is.Equal(inner.calls.Load(), int32(1)), "TryStart must not run a second underlying Start")
	assert.Check(t, is.Equal(s.TryIsStarted(), false), "TryIsStarted is false while a lifecycle operation is in flight")

	release()
	assert.NilError(t, <-startDone)

	// The skipped TryStart must not have begun a failure streak.
	assert.Check(t, is.Equal(s.ShouldReportFailure(), false), "skipped TryStart must not record a failure")
	assert.Check(t, is.Equal(s.IsStarted(), true))
	assert.Check(t, is.Equal(s.TryIsStarted(), true), "TryIsStarted reads the settled state once the lock is free")

	// Latched: a later TryStart reports started without another underlying call.
	started, err = s.TryStart(t.Context())
	assert.NilError(t, err)
	assert.Check(t, started)
	assert.Check(t, is.Equal(inner.calls.Load(), int32(1)))
}

// TestStartableToolSet_TryStartRunsStartLogic verifies that when the lock is
// free, TryStart behaves exactly like Start: a failure records the
// once-per-streak warning and a subsequent success latches the started state.
func TestStartableToolSet_TryStartRunsStartLogic(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	f := &flappyToolSet{errs: []error{errBoom, nil}}
	s := tools.NewStartable(f)

	started, err := s.TryStart(t.Context())
	assert.Check(t, errors.Is(err, errBoom), "TryStart must surface the underlying start error")
	assert.Check(t, !started)
	assert.Check(t, is.Equal(s.ShouldReportFailure(), true), "a failed TryStart attempt reports like Start")

	started, err = s.TryStart(t.Context())
	assert.NilError(t, err)
	assert.Check(t, started)
	assert.Check(t, is.Equal(s.IsStarted(), true))
	assert.Check(t, is.Equal(f.startups, 1))
}

// TestStartableToolSet_TryStartWithTimeoutAbandonsWedgedStart pins the shared
// bounded-start helper used by the agent turn path and the runtime startup
// probe: a wedged Start (one that ignores cancellation) is abandoned exactly
// at the bound with the bound's context error, an attempt already in flight
// is skipped rather than joined, and the abandoned attempt keeps running in
// the background under the single-flight lock so a later call picks up its
// outcome.
func TestStartableToolSet_TryStartWithTimeoutAbandonsWedgedStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &blockingToolSet{entered: make(chan struct{}), release: make(chan struct{})}
		s := tools.NewStartable(inner)

		begin := time.Now()
		started, err := s.TryStartWithTimeout(t.Context(), time.Second)
		assert.Check(t, errors.Is(err, context.DeadlineExceeded), "a wedged attempt must be abandoned with the bound's error: %v", err)
		assert.Check(t, !started)
		assert.Check(t, is.Equal(time.Since(begin), time.Second), "the attempt must be abandoned exactly at the bound (fake time)")

		// While the abandoned attempt holds the single-flight lock, a second
		// bounded call skips it immediately instead of joining it.
		started, err = s.TryStartWithTimeout(t.Context(), time.Second)
		assert.NilError(t, err, "an in-flight start must not surface as a failure")
		assert.Check(t, !started, "a bounded call must not join an in-flight start")
		assert.Check(t, is.Equal(inner.calls.Load(), int32(1)), "the skip must not run a second underlying Start")

		// The abandoned attempt settles in the background; a later call
		// reports the latched start without another underlying Start.
		close(inner.release)
		synctest.Wait()
		started, err = s.TryStartWithTimeout(t.Context(), time.Second)
		assert.NilError(t, err)
		assert.Check(t, started)
		assert.Check(t, is.Equal(inner.calls.Load(), int32(1)))
	})
}

// TestStartableToolSet_StopIfStartedLeavesNeverStartedUntouched pins the
// shutdown intent carried over from Agent.StopToolSets: a toolset that was
// never started must not have its underlying Stop called.
func TestStartableToolSet_StopIfStartedLeavesNeverStartedUntouched(t *testing.T) {
	t.Parallel()

	inner := &blockingToolSet{entered: make(chan struct{}), release: make(chan struct{})}
	s := tools.NewStartable(inner)

	assert.NilError(t, s.StopIfStarted(t.Context()))
	assert.Check(t, is.Equal(inner.stops.Load(), int32(0)), "a toolset that was never started must not be stopped")
}

// TestStartableToolSet_StopIfStartedStopsStarted verifies the settled fast
// path: a started toolset is stopped and unlatched.
func TestStartableToolSet_StopIfStartedStopsStarted(t *testing.T) {
	t.Parallel()

	inner := &blockingToolSet{entered: make(chan struct{}), release: make(chan struct{})}
	close(inner.release) // Start completes immediately
	s := tools.NewStartable(inner)

	assert.NilError(t, s.Start(t.Context()))
	assert.NilError(t, s.StopIfStarted(t.Context()))
	assert.Check(t, is.Equal(inner.stops.Load(), int32(1)))
	assert.Check(t, is.Equal(s.IsStarted(), false))
}

// TestStartableToolSet_StopIfStartedExpiredContextStopsResponsiveToolset pins
// the deliberate fast path in lifecycleMutex.LockContext: an uncontended lock
// is acquired even when ctx is already done, so a shutdown arriving with an
// expired deadline still stops a responsive started toolset instead of
// leaking it.
func TestStartableToolSet_StopIfStartedExpiredContextStopsResponsiveToolset(t *testing.T) {
	t.Parallel()

	inner := &blockingToolSet{entered: make(chan struct{}), release: make(chan struct{})}
	close(inner.release) // Start completes immediately, Stop never blocks
	s := tools.NewStartable(inner)
	assert.NilError(t, s.Start(t.Context()))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.NilError(t, s.StopIfStarted(ctx), "an already-canceled ctx must still stop an uncontended started toolset")
	assert.Check(t, is.Equal(inner.stops.Load(), int32(1)))
	assert.Check(t, is.Equal(s.IsStarted(), false))
}

// TestStartableToolSet_StopIfStartedHonorsContext pins the deadline half of
// the shutdown contract: while a wedged Start holds the single-flight lock,
// StopIfStarted gives up with ctx.Err() instead of blocking, leaves the
// underlying Stop unrun, and leaves the request pending for the wedged
// attempt — whose lock release reaps the toolset when the attempt settles
// successfully. A consumed request must not affect a later deliberate start.
func TestStartableToolSet_StopIfStartedHonorsContext(t *testing.T) {
	t.Parallel()

	inner := &blockingToolSet{entered: make(chan struct{}), release: make(chan struct{})}
	s := tools.NewStartable(inner)

	// Always unblock the wedged Start so no goroutine outlives the test.
	release := sync.OnceFunc(func() { close(inner.release) })
	t.Cleanup(release)

	startDone := make(chan error, 1)
	go func() { startDone <- s.Start(t.Context()) }()
	<-inner.entered

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := s.StopIfStarted(ctx)
	assert.Check(t, errors.Is(err, context.Canceled), "StopIfStarted must give up when ctx ends while Start is wedged: %v", err)
	assert.Check(t, is.Equal(inner.stops.Load(), int32(0)), "the underlying Stop must not run while Start is wedged")

	// The wedged Start settles: the abandoned request reaps the toolset.
	release()
	assert.NilError(t, <-startDone)
	assert.Check(t, is.Equal(inner.stops.Load(), int32(1)), "a start that settles after the abandoned request must be stopped")
	assert.Check(t, is.Equal(s.IsStarted(), false))

	// The request was consumed by the reap: a later deliberate start must
	// stay up.
	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(s.IsStarted(), true))
	assert.Check(t, is.Equal(inner.stops.Load(), int32(1)), "a later deliberate start must not be reaped")
}

// TestStartableToolSet_StopIfStartedRequestDiscardedOnFailedStart verifies
// that when the wedged Start fails after the stop request was abandoned,
// there is nothing to reap: the request is consumed on lock release without
// stopping anything, rather than left to stop an instance a later caller
// deliberately starts.
func TestStartableToolSet_StopIfStartedRequestDiscardedOnFailedStart(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	inner := &blockingToolSet{entered: make(chan struct{}), release: make(chan struct{}), startErr: errBoom}
	s := tools.NewStartable(inner)

	// Always unblock the wedged Start so no goroutine outlives the test.
	release := sync.OnceFunc(func() { close(inner.release) })
	t.Cleanup(release)

	startDone := make(chan error, 1)
	go func() { startDone <- s.Start(t.Context()) }()
	<-inner.entered

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.Check(t, errors.Is(s.StopIfStarted(ctx), context.Canceled))

	// The wedged Start fails: nothing started, nothing to stop.
	release()
	assert.Check(t, errors.Is(<-startDone, errBoom))
	assert.Check(t, is.Equal(inner.stops.Load(), int32(0)))

	inner.startErr = nil
	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(s.IsStarted(), true))
	assert.Check(t, is.Equal(inner.stops.Load(), int32(0)), "the discarded request must not reap the fresh start")
}

// barrierReporterToolSet is a startable toolset whose IsStarted reporter can
// be armed to block once. startLocked probes the reporter (while holding the
// lifecycle lock) when the wrapper is already latched started, so the barrier
// keeps a non-starting lock holder busy through public behavior alone — long
// enough for a StopIfStarted deadline to expire behind it.
type barrierReporterToolSet struct {
	entered chan struct{} // closed when the armed probe begins blocking
	release chan struct{} // closed by the test to let the probe return
	armed   atomic.Bool
	started atomic.Bool
	stops   atomic.Int32
}

func newBarrierReporterToolSet() *barrierReporterToolSet {
	return &barrierReporterToolSet{entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *barrierReporterToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

func (b *barrierReporterToolSet) Start(context.Context) error {
	b.started.Store(true)
	return nil
}

func (b *barrierReporterToolSet) Stop(context.Context) error {
	b.stops.Add(1)
	b.started.Store(false)
	return nil
}

func (b *barrierReporterToolSet) IsStarted() bool {
	if b.armed.CompareAndSwap(true, false) {
		close(b.entered)
		<-b.release
	}
	return b.started.Load()
}

// TestStartableToolSet_StopIfStartedRequestOutlivesLatchedStart pins the
// removal of the stale-request compromise: a stop request that times out
// while the lifecycle lock is held by a short, non-starting holder — here a
// latched Start blocked in its reporter probe — must stop the still-started
// toolset as soon as that holder releases the lock, and must not linger to
// reap a later deliberate start.
func TestStartableToolSet_StopIfStartedRequestOutlivesLatchedStart(t *testing.T) {
	t.Parallel()

	inner := newBarrierReporterToolSet()
	s := tools.NewStartable(inner)
	assert.NilError(t, s.Start(t.Context()))

	// Wedge a latched Start inside the reporter probe: the lifecycle lock is
	// held but nothing is being started.
	inner.armed.Store(true)
	releaseProbe := sync.OnceFunc(func() { close(inner.release) })
	t.Cleanup(releaseProbe)
	startDone := make(chan error, 1)
	go func() { startDone <- s.Start(t.Context()) }()
	<-inner.entered

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.Check(t, errors.Is(s.StopIfStarted(ctx), context.Canceled))
	assert.Check(t, is.Equal(inner.stops.Load(), int32(0)), "the underlying Stop must not run while the probe holds the lock")

	// The probe returns: the holder consumes the abandoned request on lock
	// release and stops the still-started toolset before Start returns.
	releaseProbe()
	assert.NilError(t, <-startDone)
	assert.Check(t, is.Equal(inner.stops.Load(), int32(1)), "the abandoned request must stop the toolset when the lock holder releases")
	assert.Check(t, is.Equal(s.IsStarted(), false))

	// The request was consumed: a later deliberate start stays up.
	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(s.IsStarted(), true))
	assert.Check(t, is.Equal(inner.stops.Load(), int32(1)), "a later deliberate start must not be reaped")
}

// TestStartableToolSet_TryStartReportsReapedStart pins TryStart's return
// value against the release handshake: when a shutdown request abandoned
// during the attempt is consumed as TryStart releases the lock, the toolset
// it just confirmed started is stopped again, and TryStart must report
// (false, nil) rather than a started toolset that no longer is.
func TestStartableToolSet_TryStartReportsReapedStart(t *testing.T) {
	t.Parallel()

	inner := newBarrierReporterToolSet()
	s := tools.NewStartable(inner)
	assert.NilError(t, s.Start(t.Context()))

	inner.armed.Store(true)
	releaseProbe := sync.OnceFunc(func() { close(inner.release) })
	t.Cleanup(releaseProbe)

	type result struct {
		started bool
		err     error
	}
	tryDone := make(chan result, 1)
	go func() {
		started, err := s.TryStart(t.Context())
		tryDone <- result{started: started, err: err}
	}()
	<-inner.entered

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.Check(t, errors.Is(s.StopIfStarted(ctx), context.Canceled))

	releaseProbe()
	res := <-tryDone
	assert.NilError(t, res.err)
	assert.Check(t, !res.started, "TryStart must not report started after the pending request reaped the toolset")
	assert.Check(t, is.Equal(inner.stops.Load(), int32(1)))
	assert.Check(t, is.Equal(s.IsStarted(), false))
}

// TestStartableToolSet_TryIsStartedReportsReapedStart is the TryIsStarted
// analogue of the TryStart test above: when a pending shutdown request is
// consumed as TryIsStarted releases the lock, the toolset it just observed
// started is stopped again, and TryIsStarted must report false rather than
// a started toolset that no longer is. The pending request is planted
// directly because the window it models — a StopIfStarted that published its
// request but lost the lifecycle-lock race to TryIsStarted before giving up
// on its already-done context — cannot be sequenced deterministically through
// the public API.
func TestStartableToolSet_TryIsStartedReportsReapedStart(t *testing.T) {
	t.Parallel()

	inner := &blockingToolSet{entered: make(chan struct{}), release: make(chan struct{})}
	close(inner.release) // Start completes immediately
	s := tools.NewStartable(inner)
	assert.NilError(t, s.Start(t.Context()))

	s.ExportedPublishStopRequest(t.Context())

	assert.Check(t, is.Equal(s.TryIsStarted(), false), "TryIsStarted must not report started after its own release reaped the toolset")
	assert.Check(t, is.Equal(inner.stops.Load(), int32(1)), "the pending request must stop the toolset on release")
	assert.Check(t, is.Equal(s.IsStarted(), false))
}

// TestStartableToolSet_ZeroValueIsUsable pins that a StartableToolSet
// constructed without NewStartable — zero-value lock fields — does not
// deadlock: the lifecycle mutex initializes lazily on first use, including
// concurrent first use.
func TestStartableToolSet_ZeroValueIsUsable(t *testing.T) {
	t.Parallel()

	s := &tools.StartableToolSet{ToolSet: &stubToolSet{}}

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			_ = s.IsStarted()
		})
	}
	wg.Wait()

	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(s.IsStarted(), true))

	started, err := s.TryStart(t.Context())
	assert.NilError(t, err)
	assert.Check(t, started)
	assert.Check(t, is.Equal(s.TryIsStarted(), true))

	_, err = s.Tools(t.Context())
	assert.NilError(t, err)

	assert.NilError(t, s.StopIfStarted(t.Context()))
	assert.Check(t, is.Equal(s.IsStarted(), false))
}
