package lifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"

	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

// fakeSession is a controllable session: its Wait blocks until either
// Close is called or fail is invoked.
type fakeSession struct {
	mu       sync.Mutex
	closed   bool
	closedCh chan struct{} // closed once Close has run
	waitDone atomic.Bool   // set true after Wait returns
	waiting  chan struct{} // closed once Wait has parked on failCh
	waitOnce sync.Once
	failCh   chan error
}

func newFakeSession() *fakeSession {
	return &fakeSession{
		waiting:  make(chan struct{}),
		closedCh: make(chan struct{}),
		failCh:   make(chan error, 1),
	}
}

func (f *fakeSession) Wait() error {
	f.waitOnce.Do(func() { close(f.waiting) })
	err := <-f.failCh
	f.waitDone.Store(true)
	return err
}

// waitParked blocks until the watcher goroutine has entered sess.Wait().
// Used by tests that need to exercise Stop against an actively-blocking
// watcher rather than the racy connect-then-stop path where the watcher
// could exit before parking.
func (f *fakeSession) waitParked(t *testing.T) {
	t.Helper()
	select {
	case <-f.waiting:
	case <-time.After(time.Second):
		t.Fatal("watcher did not enter Wait()")
	}
}

func (f *fakeSession) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.closedCh)
		// Closed sessions return nil from Wait by convention.
		select {
		case f.failCh <- nil:
		default:
		}
	}
	return nil
}

// waitClosed blocks until Close has been called on the session.
func (f *fakeSession) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-f.closedCh:
	case <-time.After(time.Second):
		t.Fatal("session was not closed")
	}
}

func (f *fakeSession) fail(err error) {
	select {
	case f.failCh <- err:
	default:
	}
}

// scriptedConnector returns sessions and errors from a scripted slice.
type scriptedConnector struct {
	mu        sync.Mutex
	scripts   []scriptStep
	idx       int
	calls     atomic.Int32
	delivered chan *fakeSession
}

type scriptStep struct {
	err     error
	session *fakeSession
}

func newScriptedConnector(steps ...scriptStep) *scriptedConnector {
	return &scriptedConnector{
		scripts:   steps,
		delivered: make(chan *fakeSession, len(steps)),
	}
}

func (c *scriptedConnector) Connect(context.Context) (lifecycle.Session, error) {
	c.calls.Add(1)
	c.mu.Lock()
	if c.idx >= len(c.scripts) {
		c.mu.Unlock()
		return nil, errors.New("scripted connector exhausted")
	}
	step := c.scripts[c.idx]
	c.idx++
	c.mu.Unlock()

	if step.err != nil {
		return nil, step.err
	}
	c.delivered <- step.session
	return step.session, nil
}

func (c *scriptedConnector) Calls() int { return int(c.calls.Load()) }

// blockingConnector blocks in Connect until release is closed, then returns
// the configured session/err. It lets tests exercise the startup-timeout path
// where Connect is slow rather than failing outright.
type blockingConnector struct {
	release chan struct{}
	session *fakeSession
	err     error
	calls   atomic.Int32
}

func (c *blockingConnector) Connect(ctx context.Context) (lifecycle.Session, error) {
	c.calls.Add(1)
	select {
	case <-c.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if c.err != nil {
		return nil, c.err
	}
	return c.session, nil
}

// fastBackoff is a minimal backoff for tests so we don't sit in time.Sleep.
var fastBackoff = lifecycle.Backoff{
	Initial:    1 * time.Millisecond,
	Max:        2 * time.Millisecond,
	Multiplier: 2,
}

func TestSupervisor_StartFailurePropagates(t *testing.T) {
	t.Parallel()

	want := errors.New("boom")
	c := newScriptedConnector(scriptStep{err: want})
	s := lifecycle.New("test", c, lifecycle.Policy{})

	err := s.Start(t.Context())
	assert.Check(t, errors.Is(err, want))
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateStopped))
}

func TestSupervisor_StartSucceedsAndReadies(t *testing.T) {
	t.Parallel()

	sess := newFakeSession()
	c := newScriptedConnector(scriptStep{session: sess})
	s := lifecycle.New("test", c, lifecycle.Policy{})

	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateReady))
	assert.Check(t, s.IsReady())
	assert.NilError(t, s.Stop(t.Context()))
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateStopped))
}

// TestSupervisor_StartTimesOutOnSlowConnect verifies that a Connect that
// hangs past StartupTimeout causes Start to return ErrInitTimeout and leaves
// the supervisor in StateStopped so the caller can retry.
func TestSupervisor_StartTimesOutOnSlowConnect(t *testing.T) {
	t.Parallel()

	c := &blockingConnector{release: make(chan struct{}), session: newFakeSession()}
	s := lifecycle.New("test", c, lifecycle.Policy{StartupTimeout: 20 * time.Millisecond})

	err := s.Start(t.Context())
	assert.Check(t, errors.Is(err, lifecycle.ErrInitTimeout))
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateStopped))

	// Only one Connect is ever launched, even though it is still wedged.
	assert.Check(t, is.Equal(c.calls.Load(), int32(1)))
}

// TestSupervisor_StartAdoptsLateConnect verifies that a Connect that finishes
// after the first Start timed out is adopted by the next Start (reusing the
// same in-flight goroutine) rather than launching a second, concurrent
// Connect.
func TestSupervisor_StartAdoptsLateConnect(t *testing.T) {
	t.Parallel()

	c := &blockingConnector{release: make(chan struct{}), session: newFakeSession()}
	s := lifecycle.New("test", c, lifecycle.Policy{StartupTimeout: 20 * time.Millisecond})

	assert.Check(t, errors.Is(s.Start(t.Context()), lifecycle.ErrInitTimeout))

	// The handshake completes; the next Start adopts it.
	close(c.release)
	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateReady))
	// Still only one Connect was ever launched.
	assert.Check(t, is.Equal(c.calls.Load(), int32(1)))

	assert.NilError(t, s.Stop(t.Context()))
}

// TestSupervisor_StartAdoptsLateConnectAfterFirstCtxCancelled verifies that
// the in-flight Connect is detached from the first caller's context: when that
// context is cancelled after the first Start times out, a connector that
// respects ctx cancellation still completes, and the adopting Start does not
// receive a stale context.Canceled.
func TestSupervisor_StartAdoptsLateConnectAfterFirstCtxCancelled(t *testing.T) {
	t.Parallel()

	c := &blockingConnector{release: make(chan struct{}), session: newFakeSession()}
	s := lifecycle.New("test", c, lifecycle.Policy{StartupTimeout: 20 * time.Millisecond})

	ctx, cancel := context.WithCancel(t.Context())
	assert.Check(t, errors.Is(s.Start(ctx), lifecycle.ErrInitTimeout))
	// Cancelling the first caller's context must not poison the detached
	// in-flight Connect: blockingConnector returns ctx.Err() if its ctx is done.
	cancel()

	close(c.release)
	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateReady))
	assert.Check(t, is.Equal(c.calls.Load(), int32(1)))

	assert.NilError(t, s.Stop(t.Context()))
}

// TestSupervisor_StopReapsLateConnect verifies that when Stop is called while a
// timed-out Connect is still in flight, the session it eventually produces is
// closed rather than leaked.
func TestSupervisor_StopReapsLateConnect(t *testing.T) {
	t.Parallel()

	sess := newFakeSession()
	c := &blockingConnector{release: make(chan struct{}), session: sess}
	s := lifecycle.New("test", c, lifecycle.Policy{StartupTimeout: 20 * time.Millisecond})

	assert.Check(t, errors.Is(s.Start(t.Context()), lifecycle.ErrInitTimeout))

	// Stop while the connect is still wedged, then let it complete.
	assert.NilError(t, s.Stop(t.Context()))
	close(c.release)

	// The reaper must close the late session.
	sess.waitClosed(t)
}

// TestSupervisor_StartWithinTimeoutSucceeds verifies that a Connect that
// completes before StartupTimeout readies the supervisor normally.
func TestSupervisor_StartWithinTimeoutSucceeds(t *testing.T) {
	t.Parallel()

	c := &blockingConnector{release: make(chan struct{}), session: newFakeSession()}
	close(c.release) // Connect returns immediately
	s := lifecycle.New("test", c, lifecycle.Policy{StartupTimeout: time.Second})

	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateReady))
	assert.NilError(t, s.Stop(t.Context()))
}

func TestSupervisor_StartIsIdempotent(t *testing.T) {
	t.Parallel()

	sess := newFakeSession()
	c := newScriptedConnector(scriptStep{session: sess})
	s := lifecycle.New("test", c, lifecycle.Policy{})

	assert.NilError(t, s.Start(t.Context()))
	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(c.Calls(), 1))
	assert.NilError(t, s.Stop(t.Context()))
}

func TestSupervisor_RestartAfterDisconnect(t *testing.T) {
	t.Parallel()

	sess1 := newFakeSession()
	sess2 := newFakeSession()
	c := newScriptedConnector(
		scriptStep{session: sess1},
		scriptStep{session: sess2},
	)

	restarted := make(chan struct{}, 1)
	s := lifecycle.New("test", c, lifecycle.Policy{
		Backoff: fastBackoff,
		OnRestart: func(context.Context) {
			select {
			case restarted <- struct{}{}:
			default:
			}
		},
	})

	assert.NilError(t, s.Start(t.Context()))

	// Make session 1 fail; supervisor should reconnect to session 2.
	sess1.fail(errors.New("crash"))

	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not restart")
	}

	assert.Check(t, is.Equal(s.State().State, lifecycle.StateReady))
	assert.Check(t, is.Equal(c.Calls(), 2))

	assert.NilError(t, s.Stop(t.Context()))
}

func TestSupervisor_GivesUpAfterMaxAttempts(t *testing.T) {
	t.Parallel()

	sess1 := newFakeSession()
	c := newScriptedConnector(
		scriptStep{session: sess1},
		scriptStep{err: errors.New("fail-1")},
		scriptStep{err: errors.New("fail-2")},
		scriptStep{err: errors.New("fail-3")},
	)

	failed := make(chan error, 1)
	s := lifecycle.New("test", c, lifecycle.Policy{
		MaxAttempts: 3,
		Backoff:     fastBackoff,
		OnFailed: func(err error) {
			select {
			case failed <- err:
			default:
			}
		},
	})

	assert.NilError(t, s.Start(t.Context()))
	sess1.fail(errors.New("crash"))

	select {
	case <-failed:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not call OnFailed")
	}

	assert.Check(t, is.Equal(s.State().State, lifecycle.StateFailed))
}

func TestSupervisor_RestartNeverGoesToFailed(t *testing.T) {
	t.Parallel()

	sess1 := newFakeSession()
	c := newScriptedConnector(scriptStep{session: sess1})

	failed := make(chan struct{}, 1)
	s := lifecycle.New("test", c, lifecycle.Policy{
		Restart: lifecycle.RestartNever,
		OnFailed: func(error) {
			select {
			case failed <- struct{}{}:
			default:
			}
		},
	})

	assert.NilError(t, s.Start(t.Context()))
	sess1.fail(errors.New("crash"))

	select {
	case <-failed:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not transition to Failed")
	}

	assert.Check(t, is.Equal(s.State().State, lifecycle.StateFailed))
	assert.Check(t, is.Equal(c.Calls(), 1))
}

func TestSupervisor_RestartAndWait(t *testing.T) {
	t.Parallel()

	sess1 := newFakeSession()
	sess2 := newFakeSession()
	c := newScriptedConnector(
		scriptStep{session: sess1},
		scriptStep{session: sess2},
	)
	s := lifecycle.New("test", c, lifecycle.Policy{Backoff: fastBackoff})

	assert.NilError(t, s.Start(t.Context()))

	err := s.RestartAndWait(t.Context(), 2*time.Second)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateReady))

	assert.NilError(t, s.Stop(t.Context()))
}

func TestSupervisor_StopIdempotent(t *testing.T) {
	t.Parallel()

	sess := newFakeSession()
	c := newScriptedConnector(scriptStep{session: sess})
	s := lifecycle.New("test", c, lifecycle.Policy{})

	assert.NilError(t, s.Start(t.Context()))
	assert.NilError(t, s.Stop(t.Context()))
	assert.NilError(t, s.Stop(t.Context()))
}

func TestSupervisor_StopBeforeStart(t *testing.T) {
	t.Parallel()
	c := newScriptedConnector()
	s := lifecycle.New("test", c, lifecycle.Policy{})
	assert.NilError(t, s.Stop(t.Context()))
}

// TestSupervisor_StopWakesRestartAndWait verifies that a Stop() while
// another goroutine is blocked in RestartAndWait causes RestartAndWait
// to return promptly with ErrNotStarted instead of waiting for its
// timeout.
func TestSupervisor_StopWakesRestartAndWait(t *testing.T) {
	t.Parallel()

	sess := newFakeSession()
	c := newScriptedConnector(scriptStep{session: sess})
	s := lifecycle.New("test", c, lifecycle.Policy{Backoff: fastBackoff})
	assert.NilError(t, s.Start(t.Context()))

	done := make(chan error, 1)
	go func() { done <- s.RestartAndWait(t.Context(), 30*time.Second) }()

	// RestartAndWait force-closes the current session before parking in
	// its select; once the close is observed it is safe to Stop.
	sess.waitClosed(t)
	assert.NilError(t, s.Stop(t.Context()))

	select {
	case err := <-done:
		// We expect either ErrNotStarted or the supervisor's last error.
		assert.Check(t, err != nil)
	case <-time.After(2 * time.Second):
		t.Fatal("RestartAndWait did not return after Stop")
	}
}

// TestSupervisor_FailedWakesRestartAndWait verifies that when the
// supervisor exhausts its restart budget while RestartAndWait is
// blocked, the call returns with the last error rather than waiting
// for its timeout.
func TestSupervisor_FailedWakesRestartAndWait(t *testing.T) {
	t.Parallel()

	sess1 := newFakeSession()
	c := newScriptedConnector(
		scriptStep{session: sess1},
		scriptStep{err: errors.New("fail-1")},
		scriptStep{err: errors.New("fail-2")},
	)
	s := lifecycle.New("test", c, lifecycle.Policy{
		MaxAttempts: 2,
		Backoff:     fastBackoff,
	})
	assert.NilError(t, s.Start(t.Context()))

	done := make(chan error, 1)
	go func() { done <- s.RestartAndWait(t.Context(), 30*time.Second) }()

	// RestartAndWait force-closes the current session before parking in
	// its select; once that close lands, crash the session.
	sess1.waitClosed(t)
	sess1.fail(errors.New("crash"))

	select {
	case err := <-done:
		assert.Check(t, err != nil)
	case <-time.After(2 * time.Second):
		t.Fatal("RestartAndWait did not return after supervisor failed")
	}
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateFailed))
}

// TestSupervisor_RecoverFromFailedViaStart verifies that after the
// supervisor enters Failed (signalDone closes done), a fresh Start
// brings it back to Ready AND a subsequent RestartAndWait works
// normally rather than wedging on the stale-closed `done` channel.
func TestSupervisor_RecoverFromFailedViaStart(t *testing.T) {
	t.Parallel()

	sess1 := newFakeSession()
	sess2 := newFakeSession()
	sess3 := newFakeSession()
	c := newScriptedConnector(
		scriptStep{session: sess1},        // initial Start
		scriptStep{err: errors.New("r1")}, // tryRestart attempt 1
		scriptStep{err: errors.New("r2")}, // tryRestart attempt 2 → Failed
		scriptStep{session: sess2},        // recovery Start
		scriptStep{session: sess3},        // RestartAndWait’s reconnect
	)
	s := lifecycle.New("test", c, lifecycle.Policy{
		MaxAttempts: 2,
		Backoff:     fastBackoff,
	})

	assert.NilError(t, s.Start(t.Context()))

	// Drive into Failed.
	sess1.fail(errors.New("crash"))
	poll.WaitOn(t, func(poll.LogT) poll.Result {
		if s.State().State == lifecycle.StateFailed {
			return poll.Success()
		}
		return poll.Continue("supervisor state=%s", s.State().State)
	}, poll.WithTimeout(2*time.Second), poll.WithDelay(5*time.Millisecond))

	// Recovery: Start should refresh the done channel and bring us back
	// to Ready without RestartAndWait wedging on a stale close.
	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateReady))

	// Now exercise RestartAndWait against the fresh session.
	done := make(chan error, 1)
	go func() { done <- s.RestartAndWait(t.Context(), 2*time.Second) }()

	select {
	case err := <-done:
		assert.NilError(t, err, "RestartAndWait should reconnect, not return stale-Failed error")
	case <-time.After(3 * time.Second):
		t.Fatal("RestartAndWait did not return")
	}
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateReady))

	assert.NilError(t, s.Stop(t.Context()))
}

// TestSupervisor_PermanentErrorsDontRestart verifies that wait errors that
// are classified as Permanent (e.g. ErrAuthRequired) cause the supervisor
// to enter Failed without consuming restart attempts.
func TestSupervisor_PermanentErrorsDontRestart(t *testing.T) {
	t.Parallel()

	sess1 := newFakeSession()
	c := newScriptedConnector(scriptStep{session: sess1})

	failedCh := make(chan error, 1)
	s := lifecycle.New("test", c, lifecycle.Policy{
		Backoff: fastBackoff,
		OnFailed: func(err error) {
			select {
			case failedCh <- err:
			default:
			}
		},
	})

	assert.NilError(t, s.Start(t.Context()))
	sess1.fail(lifecycle.ErrAuthRequired)

	select {
	case got := <-failedCh:
		assert.Check(t, errors.Is(got, lifecycle.ErrAuthRequired))
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not transition to Failed")
	}
	assert.Check(t, is.Equal(c.Calls(), 1), "must not retry on permanent error")
}

// TestSupervisor_PermanentConnectErrorDoesNotRetry verifies that when
// connector.Connect returns a permanent error during a background reconnect
// attempt (e.g. ErrAuthRequired from a server-side invalid_token), the
// supervisor transitions to StateFailed immediately without burning through
// its MaxAttempts budget.
//
// This is the gap the bug exercised: the session Wait succeeded (server
// closed cleanly) but the subsequent reconnect Connect returned a permanent
// error that the old supervisor would retry N times before giving up.
func TestSupervisor_PermanentConnectErrorDoesNotRetry(t *testing.T) {
	t.Parallel()

	// sess1 is the initial successful connection; then the reconnect
	// returns a permanent auth error.
	sess1 := newFakeSession()
	c := newScriptedConnector(
		scriptStep{session: sess1},
		scriptStep{err: lifecycle.ErrAuthRequired}, // permanent: must NOT burn MaxAttempts
	)

	failedCh := make(chan error, 1)
	s := lifecycle.New("test", c, lifecycle.Policy{
		MaxAttempts: 5, // budget that must NOT be consumed
		Backoff:     fastBackoff,
		OnFailed: func(err error) {
			select {
			case failedCh <- err:
			default:
			}
		},
	})

	assert.NilError(t, s.Start(t.Context()))
	// Make the session fail non-permanently so tryRestart is entered.
	sess1.fail(errors.New("transport closed"))

	select {
	case got := <-failedCh:
		assert.Check(t, errors.Is(got, lifecycle.ErrAuthRequired))
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not call OnFailed")
	}

	assert.Check(t, is.Equal(s.State().State, lifecycle.StateFailed))
	// One initial Connect + one reconnect attempt that returned permanent error.
	assert.Check(t, is.Equal(c.Calls(), 2), "must fail-fast after one reconnect attempt on permanent error")
}

func TestSupervisor_CleanClosePolicyBoundary(t *testing.T) {
	t.Parallel()

	sess1 := newFakeSession()
	c := newScriptedConnector(scriptStep{session: sess1})

	failedCh := make(chan error, 1)
	s := lifecycle.New("test", c, lifecycle.Policy{
		Restart: lifecycle.RestartOnFailure,
		Backoff: fastBackoff,
		OnFailed: func(err error) {
			select {
			case failedCh <- err:
			default:
			}
		},
	})

	assert.NilError(t, s.Start(t.Context()))
	sess1.fail(nil)

	select {
	case err := <-failedCh:
		assert.Check(t, err == nil)
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not transition to Failed after clean close")
	}
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateFailed))
	assert.Check(t, is.Equal(c.Calls(), 1), "RestartOnFailure must not reconnect clean closes")
}

func TestSupervisor_RestartAlwaysReconnectsCleanCloseAndResetsBudget(t *testing.T) {
	t.Parallel()

	sess1 := newFakeSession()
	sess2 := newFakeSession()
	sess3 := newFakeSession()
	c := newScriptedConnector(
		scriptStep{session: sess1},
		scriptStep{session: sess2},
		scriptStep{session: sess3},
	)

	restarted := make(chan struct{}, 2)
	s := lifecycle.New("test", c, lifecycle.Policy{
		Restart:     lifecycle.RestartAlways,
		MaxAttempts: 1,
		Backoff:     fastBackoff,
		OnRestart: func(context.Context) {
			select {
			case restarted <- struct{}{}:
			default:
			}
		},
	})

	assert.NilError(t, s.Start(t.Context()))
	sess1.fail(nil)

	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not reconnect after first clean close")
	}
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateReady))
	assert.Check(t, is.Equal(c.Calls(), 2))

	sess2.fail(nil)
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not reconnect after second clean close")
	}
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateReady))
	assert.Check(t, is.Equal(c.Calls(), 3), "successful reconnect must reset the budget")

	assert.NilError(t, s.Stop(t.Context()))
}

func TestSupervisor_RestartAlwaysCleanCloseStillHonorsFailedReconnectBudget(t *testing.T) {
	t.Parallel()

	sess1 := newFakeSession()
	c := newScriptedConnector(
		scriptStep{session: sess1},
		scriptStep{err: errors.New("fail-1")},
		scriptStep{err: errors.New("fail-2")},
	)

	failedCh := make(chan error, 1)
	s := lifecycle.New("test", c, lifecycle.Policy{
		Restart:     lifecycle.RestartAlways,
		MaxAttempts: 2,
		Backoff:     fastBackoff,
		OnFailed: func(err error) {
			select {
			case failedCh <- err:
			default:
			}
		},
	})

	assert.NilError(t, s.Start(t.Context()))
	sess1.fail(nil)

	select {
	case <-failedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not give up after failed reconnect budget")
	}
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateFailed))
	assert.Check(t, is.Equal(c.Calls(), 3))
}

func TestBackoff_Defaults(t *testing.T) {
	t.Parallel()

	b := lifecycle.Backoff{}
	d0 := lifecycle.ExportedBackoffDelay(b, 0, func() float64 { return 0 })
	d1 := lifecycle.ExportedBackoffDelay(b, 1, func() float64 { return 0 })
	d2 := lifecycle.ExportedBackoffDelay(b, 2, func() float64 { return 0 })
	d6 := lifecycle.ExportedBackoffDelay(b, 6, func() float64 { return 0 })

	assert.Check(t, is.Equal(d0, time.Second))
	assert.Check(t, is.Equal(d1, 2*time.Second))
	assert.Check(t, is.Equal(d2, 4*time.Second))
	// 1<<6 = 64s, capped to default Max = 32s.
	assert.Check(t, is.Equal(d6, 32*time.Second))
}

func TestBackoff_Jitter(t *testing.T) {
	t.Parallel()

	b := lifecycle.Backoff{Initial: 100 * time.Millisecond, Jitter: 0.5}
	// random ≈ 1 → +50% offset
	d := lifecycle.ExportedBackoffDelay(b, 0, func() float64 { return 1 })
	assert.Check(t, d == 150*time.Millisecond)

	// random ≈ 0 → -50% offset
	d = lifecycle.ExportedBackoffDelay(b, 0, func() float64 { return 0 })
	assert.Check(t, d == 50*time.Millisecond)
}

func TestSupervisor_StopWaitsForWatcher(t *testing.T) {
	t.Parallel()

	sess := newFakeSession()
	c := newScriptedConnector(scriptStep{session: sess})
	s := lifecycle.New("test", c, lifecycle.Policy{})

	assert.NilError(t, s.Start(t.Context()))
	sess.waitParked(t)

	assert.NilError(t, s.Stop(t.Context()))
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateStopped))

	// Stop must not return until the watcher has observed Wait() unblock.
	assert.Check(t, sess.waitDone.Load(), "Stop returned before watcher's Wait() completed")
}

// TestSupervisor_StopConcurrent exercises the s.stopping guard: several
// goroutines call Stop concurrently while the watcher is live in
// sess.Wait(). All calls must return without error and observe a
// fully-shut-down supervisor.
func TestSupervisor_StopConcurrent(t *testing.T) {
	t.Parallel()

	sess := newFakeSession()
	c := newScriptedConnector(scriptStep{session: sess})
	s := lifecycle.New("test", c, lifecycle.Policy{})

	assert.NilError(t, s.Start(t.Context()))
	sess.waitParked(t)

	const n = 4
	errs := make(chan error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			errs <- s.Stop(t.Context())
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		assert.NilError(t, err)
	}
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateStopped))
	assert.Check(t, sess.waitDone.Load(), "a Stop returned before watcher's Wait() completed")
}

// crashErr wraps err (typically a plain "boom"-style message) in
// lifecycle.ErrServerCrashed, matching the shape lspSession.Wait produces
// for a real crashed process.
func crashErr(msg string) error {
	return fmt.Errorf("%w: %s", lifecycle.ErrServerCrashed, msg)
}

// TestSupervisor_CrashLoopStopsRestartingAndReportsOnStart verifies the
// full crash-loop lifecycle: the first two crashes (within the window) are
// ordinary restarts handled entirely by the normal restart policy: no
// pacing, state back to Ready each time. Only the third crash — reaching
// CrashLoop.Threshold — stops the supervisor from reconnecting again and
// reports ErrCrashLooping. That report is a one-shot: the Start that reads
// it does not itself reconnect, but the Start after that does.
func TestSupervisor_CrashLoopStopsRestartingAndReportsOnStart(t *testing.T) {
	t.Parallel()

	sess1, sess2, sess3, sess4 := newFakeSession(), newFakeSession(), newFakeSession(), newFakeSession()
	c := newScriptedConnector(
		scriptStep{session: sess1},
		scriptStep{session: sess2},
		scriptStep{session: sess3},
		scriptStep{session: sess4},
	)

	restarted := make(chan struct{}, 4)
	failed := make(chan error, 1)
	s := lifecycle.New("test", c, lifecycle.Policy{
		Backoff:   fastBackoff,
		CrashLoop: lifecycle.CrashLoop{Threshold: 3, Window: time.Minute},
		OnRestart: func(context.Context) {
			select {
			case restarted <- struct{}{}:
			default:
			}
		},
		OnFailed: func(err error) {
			select {
			case failed <- err:
			default:
			}
		},
	})

	assert.NilError(t, s.Start(t.Context()))

	// Crash 1: an ordinary restart, not a loop yet.
	sess1.fail(crashErr("boom"))
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not restart after the first crash")
	}
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateReady), "a single crash must not trip the loop detector")

	// Crash 2: still just an ordinary restart.
	sess2.fail(crashErr("boom again"))
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not restart after the second crash")
	}
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateReady), "two crashes must not trip the loop detector")

	// Crash 3 reaches the threshold: give up, no further reconnect attempt.
	sess3.fail(crashErr("boom a third time"))

	var loopErr error
	select {
	case loopErr = <-failed:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not report a crash loop")
	}
	assert.Check(t, errors.Is(loopErr, lifecycle.ErrCrashLooping))
	assert.Check(t, errors.Is(loopErr, lifecycle.ErrServerCrashed), "ErrCrashLooping must still match ErrServerCrashed")
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateFailed))
	assert.Check(t, is.Equal(c.Calls(), 3), "no reconnect attempt once the loop is detected")

	// The next Start reports the loop once, without connecting again.
	err := s.Start(t.Context())
	assert.Check(t, errors.Is(err, lifecycle.ErrCrashLooping))
	assert.Check(t, is.Equal(c.Calls(), 3), "the one-shot report must not itself connect")

	// The Start after that attempts a genuine reconnect.
	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(c.Calls(), 4))
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateReady))

	assert.NilError(t, s.Stop(t.Context()))
}

// TestSupervisor_CrashLoopIgnoresCleanDisconnects verifies that a clean
// exit (Wait returning nil) never counts toward the crash-loop threshold,
// even when there are more of them than the threshold and RestartAlways
// keeps reconnecting after every one.
func TestSupervisor_CrashLoopIgnoresCleanDisconnects(t *testing.T) {
	t.Parallel()

	sessions := make([]*fakeSession, 5)
	steps := make([]scriptStep, 5)
	for i := range sessions {
		sessions[i] = newFakeSession()
		steps[i] = scriptStep{session: sessions[i]}
	}
	c := newScriptedConnector(steps...)

	restarted := make(chan struct{}, 10)
	s := lifecycle.New("test", c, lifecycle.Policy{
		Restart:   lifecycle.RestartAlways,
		Backoff:   fastBackoff,
		CrashLoop: lifecycle.CrashLoop{Threshold: 3, Window: time.Minute},
		OnRestart: func(context.Context) {
			select {
			case restarted <- struct{}{}:
			default:
			}
		},
	})

	assert.NilError(t, s.Start(t.Context()))
	for i := range 4 {
		_ = sessions[i].Close(t.Context()) // clean disconnect, not a crash
		select {
		case <-restarted:
		case <-time.After(2 * time.Second):
			t.Fatalf("supervisor did not restart after clean close %d", i+1)
		}
	}

	assert.Check(t, is.Equal(s.State().State, lifecycle.StateReady),
		"clean disconnects (more of them than the crash-loop threshold) must never trip the loop detector")
	assert.Check(t, is.Equal(c.Calls(), 5))

	assert.NilError(t, s.Stop(t.Context()))
}

// TestSupervisor_CrashLoopIgnoresForcedRestart verifies that a deliberate
// RestartAndWait (e.g. /toolset-restart) never counts toward the
// crash-loop threshold, even repeated more often than the threshold.
func TestSupervisor_CrashLoopIgnoresForcedRestart(t *testing.T) {
	t.Parallel()

	sessions := make([]*fakeSession, 5)
	steps := make([]scriptStep, 5)
	for i := range sessions {
		sessions[i] = newFakeSession()
		steps[i] = scriptStep{session: sessions[i]}
	}
	c := newScriptedConnector(steps...)

	s := lifecycle.New("test", c, lifecycle.Policy{
		Backoff:   fastBackoff,
		CrashLoop: lifecycle.CrashLoop{Threshold: 3, Window: time.Minute},
	})

	assert.NilError(t, s.Start(t.Context()))
	for i := range 4 {
		sessions[i].waitParked(t)
		assert.NilError(t, s.RestartAndWait(t.Context(), 2*time.Second))
	}

	assert.Check(t, is.Equal(s.State().State, lifecycle.StateReady),
		"forced restarts (more of them than the crash-loop threshold) must never be counted as crashes")
	assert.Check(t, is.Equal(c.Calls(), 5))

	assert.NilError(t, s.Stop(t.Context()))
}

// TestSupervisor_CrashLoopStopIsClean verifies that Stop terminates cleanly
// from a crash-loop Failed state — discarding the pending crash-loop
// report and history as part of teardown — without hanging or leaving the
// watcher goroutine behind.
func TestSupervisor_CrashLoopStopIsClean(t *testing.T) {
	t.Parallel()

	sess1, sess2, sess3 := newFakeSession(), newFakeSession(), newFakeSession()
	c := newScriptedConnector(
		scriptStep{session: sess1},
		scriptStep{session: sess2},
		scriptStep{session: sess3},
	)

	failed := make(chan error, 1)
	s := lifecycle.New("test", c, lifecycle.Policy{
		Backoff:   fastBackoff,
		CrashLoop: lifecycle.CrashLoop{Threshold: 3, Window: time.Minute},
		OnFailed: func(err error) {
			select {
			case failed <- err:
			default:
			}
		},
	})

	assert.NilError(t, s.Start(t.Context()))
	sess1.fail(crashErr("boom"))
	sess2.fail(crashErr("boom again"))
	sess3.fail(crashErr("boom a third time"))

	select {
	case <-failed:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not report a crash loop")
	}
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateFailed))

	assert.NilError(t, s.Stop(t.Context()))
	assert.Check(t, is.Equal(s.State().State, lifecycle.StateStopped))
	// A permanently-stopped supervisor never restarts, crash loop or not.
	assert.Check(t, errors.Is(s.Start(t.Context()), lifecycle.ErrNotStarted))
}

// TestSupervisor_PendingCrashLoopErrorIsNonConsuming verifies that
// PendingCrashLoopError is a pure peek: repeated calls keep returning the
// same report without ever connecting, and without disturbing Start's own
// one-shot consume-then-reconnect contract.
func TestSupervisor_PendingCrashLoopErrorIsNonConsuming(t *testing.T) {
	t.Parallel()

	sess1, sess2, sess3, sess4 := newFakeSession(), newFakeSession(), newFakeSession(), newFakeSession()
	c := newScriptedConnector(
		scriptStep{session: sess1},
		scriptStep{session: sess2},
		scriptStep{session: sess3},
		scriptStep{session: sess4},
	)

	failed := make(chan error, 1)
	s := lifecycle.New("test", c, lifecycle.Policy{
		Backoff:   fastBackoff,
		CrashLoop: lifecycle.CrashLoop{Threshold: 3, Window: time.Minute},
		OnFailed: func(err error) {
			select {
			case failed <- err:
			default:
			}
		},
	})

	assert.NilError(t, s.Start(t.Context()))
	sess1.fail(crashErr("boom"))
	sess2.fail(crashErr("boom again"))
	sess3.fail(crashErr("boom a third time"))

	select {
	case <-failed:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not report a crash loop")
	}

	// Peeking repeatedly must not consume the report or trigger a connect.
	for range 3 {
		assert.Check(t, errors.Is(s.PendingCrashLoopError(), lifecycle.ErrCrashLooping))
	}
	assert.Check(t, is.Equal(c.Calls(), 3), "peeking must never itself connect")

	// Start still reports it once, then reconnects for real on the next call.
	err := s.Start(t.Context())
	assert.Check(t, errors.Is(err, lifecycle.ErrCrashLooping))
	assert.Check(t, s.PendingCrashLoopError() == nil, "Start must have consumed the report")
	assert.Check(t, is.Equal(c.Calls(), 3))

	assert.NilError(t, s.Start(t.Context()))
	assert.Check(t, is.Equal(c.Calls(), 4))

	assert.NilError(t, s.Stop(t.Context()))
}

// TestSupervisor_CrashLoopWindowPrunesOldCrashes verifies that crashes
// spread out wider than CrashLoop.Window apart never accumulate toward
// the threshold: each one ages out of the window before the next occurs,
// so the supervisor keeps restarting normally instead of giving up.
func TestSupervisor_CrashLoopWindowPrunesOldCrashes(t *testing.T) {
	t.Parallel()

	sessions := make([]*fakeSession, 5)
	steps := make([]scriptStep, 5)
	for i := range sessions {
		sessions[i] = newFakeSession()
		steps[i] = scriptStep{session: sessions[i]}
	}
	c := newScriptedConnector(steps...)

	restarted := make(chan struct{}, 10)
	s := lifecycle.New("test", c, lifecycle.Policy{
		Backoff:   fastBackoff,
		CrashLoop: lifecycle.CrashLoop{Threshold: 3, Window: 20 * time.Millisecond},
		OnRestart: func(context.Context) {
			select {
			case restarted <- struct{}{}:
			default:
			}
		},
	})

	assert.NilError(t, s.Start(t.Context()))
	for i := range 4 {
		sessions[i].fail(crashErr("boom"))
		select {
		case <-restarted:
		case <-time.After(2 * time.Second):
			t.Fatalf("supervisor did not restart after crash %d", i+1)
		}
		// Real time must actually pass beyond Window so the next crash finds
		// this one already pruned; there is nothing to synchronize on here
		// other than wall-clock time itself.
		time.Sleep(30 * time.Millisecond) //nolint:forbidigo // proving the crash-loop window ages entries out; no event to synchronize on
	}

	assert.Check(t, is.Equal(s.State().State, lifecycle.StateReady),
		"crashes spread wider than the window must never accumulate toward the threshold")
	assert.Check(t, is.Equal(c.Calls(), 5))

	assert.NilError(t, s.Stop(t.Context()))
}

// TestSupervisor_CrashLoopStopWinningRaceReportsStopped verifies that a
// Stop landing between the crash-loop branch computing crashLooping
// (unlocked) and it re-locking to record crashLoopErr leaves the
// supervisor in StateStopped, not StateFailed: Stop, once it wins,
// owns the terminal state rather than having it clobbered back to Failed.
func TestSupervisor_CrashLoopStopWinningRaceReportsStopped(t *testing.T) {
	t.Parallel()

	sess1, sess2, sess3 := newFakeSession(), newFakeSession(), newFakeSession()
	c := newScriptedConnector(
		scriptStep{session: sess1},
		scriptStep{session: sess2},
		scriptStep{session: sess3},
	)

	disconnected := make(chan struct{})
	release := make(chan struct{})
	restarted := make(chan struct{}, 4)
	var thirdCrash atomic.Bool
	s := lifecycle.New("test", c, lifecycle.Policy{
		Backoff:   fastBackoff,
		CrashLoop: lifecycle.CrashLoop{Threshold: 3, Window: time.Minute},
		OnRestart: func(context.Context) {
			select {
			case restarted <- struct{}{}:
			default:
			}
		},
		OnDisconnect: func(error) {
			// OnDisconnect runs after crashLooping is already computed
			// (true, for the third crash) but before crashLoopErr is
			// recorded: exactly the race window under test. Park the
			// watcher here so the test can land a concurrent Stop inside it.
			if thirdCrash.Load() {
				close(disconnected)
				<-release
			}
		},
	})

	assert.NilError(t, s.Start(t.Context()))

	// The first two crashes must actually land and restart (onto sess2,
	// then sess3) before the third is fired, or thirdCrash could be set
	// before the watcher has even processed the first one.
	sess1.fail(crashErr("boom"))
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not restart after the first crash")
	}

	sess2.fail(crashErr("boom again"))
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not restart after the second crash")
	}

	// sess3 is now live; its crash is the third, tripping the loop.
	thirdCrash.Store(true)
	sess3.fail(crashErr("boom a third time"))
	<-disconnected

	stopDone := make(chan error, 1)
	go func() { stopDone <- s.Stop(t.Context()) }()

	// Wait for Stop to actually win the race and record StateStopped
	// before releasing the parked crash-loop branch: Stop sets state
	// synchronously, before it ever blocks waiting for the watcher to exit.
	poll.WaitOn(t, func(poll.LogT) poll.Result {
		if s.State().State == lifecycle.StateStopped {
			return poll.Success()
		}
		return poll.Continue("supervisor state=%s", s.State().State)
	}, poll.WithTimeout(2*time.Second), poll.WithDelay(5*time.Millisecond))
	close(release)

	select {
	case err := <-stopDone:
		assert.NilError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return")
	}

	assert.Check(t, is.Equal(s.State().State, lifecycle.StateStopped),
		"Stop winning the race must leave the supervisor Stopped, not clobbered back to Failed")
}
