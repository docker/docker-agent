package lifecycle_test

import (
	"errors"
	"fmt"
	"testing"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"

	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

func TestState_String(t *testing.T) {
	t.Parallel()

	cases := []struct {
		state lifecycle.State
		want  string
	}{
		{lifecycle.StateStopped, "stopped"},
		{lifecycle.StateStarting, "starting"},
		{lifecycle.StateReady, "ready"},
		{lifecycle.StateDegraded, "degraded"},
		{lifecycle.StateRestarting, "restarting"},
		{lifecycle.StateFailed, "failed"},
		{lifecycle.State(99), "state(99)"},
		{lifecycle.State(-1), "state(-1)"}, // must not panic on negative
	}
	for _, tc := range cases {
		assert.Check(t, is.Equal(tc.state.String(), tc.want))
	}
}

func TestState_IsTerminal(t *testing.T) {
	t.Parallel()

	assert.Check(t, lifecycle.StateStopped.IsTerminal())
	assert.Check(t, lifecycle.StateFailed.IsTerminal())
	assert.Check(t, !lifecycle.StateStarting.IsTerminal())
	assert.Check(t, !lifecycle.StateReady.IsTerminal())
	assert.Check(t, !lifecycle.StateDegraded.IsTerminal())
	assert.Check(t, !lifecycle.StateRestarting.IsTerminal())
}

func TestState_IsUsable(t *testing.T) {
	t.Parallel()

	assert.Check(t, lifecycle.StateReady.IsUsable())
	assert.Check(t, lifecycle.StateDegraded.IsUsable())
	assert.Check(t, !lifecycle.StateStopped.IsUsable())
	assert.Check(t, !lifecycle.StateStarting.IsUsable())
	assert.Check(t, !lifecycle.StateRestarting.IsUsable())
	assert.Check(t, !lifecycle.StateFailed.IsUsable())
}

func TestTracker_NewIsStopped(t *testing.T) {
	t.Parallel()

	tr := lifecycle.NewTracker()
	snap := tr.Snapshot()
	assert.Check(t, is.Equal(snap.State, lifecycle.StateStopped))
	assert.Check(t, is.Nil(snap.LastError))
	assert.Check(t, is.Equal(snap.RestartCount, 0))
	assert.Check(t, !snap.Since.IsZero())
}

func TestTracker_ZeroValueIsStopped(t *testing.T) {
	t.Parallel()

	var tr lifecycle.Tracker
	assert.Check(t, is.Equal(tr.State(), lifecycle.StateStopped))
	assert.Check(t, is.Nil(tr.LastError()))
}

func TestTracker_SetClearsError(t *testing.T) {
	t.Parallel()

	tr := lifecycle.NewTracker()
	tr.Fail(lifecycle.StateRestarting, errors.New("boom"))
	assert.Check(t, tr.LastError() != nil)

	tr.Set(lifecycle.StateReady)
	assert.Check(t, is.Equal(tr.State(), lifecycle.StateReady))
	assert.Check(t, is.Nil(tr.LastError()))
}

func TestTracker_SetSameStateIsNoop(t *testing.T) {
	t.Parallel()

	tr := lifecycle.NewTracker()
	tr.Set(lifecycle.StateReady)
	first := tr.Snapshot()

	// Setting the same state must not bump the timestamp.
	tr.Set(lifecycle.StateReady)
	second := tr.Snapshot()
	assert.Check(t, is.Equal(first.Since, second.Since))
}

func TestTracker_FailRecordsError(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("transport reset")
	tr := lifecycle.NewTracker()
	tr.Fail(lifecycle.StateRestarting, errBoom)

	snap := tr.Snapshot()
	assert.Check(t, is.Equal(snap.State, lifecycle.StateRestarting))
	assert.Check(t, is.Equal(snap.LastError, errBoom))
}

func TestTracker_RestartCounter(t *testing.T) {
	t.Parallel()

	tr := lifecycle.NewTracker()
	assert.Check(t, is.Equal(tr.IncRestarts(), 1))
	assert.Check(t, is.Equal(tr.IncRestarts(), 2))
	assert.Check(t, is.Equal(tr.IncRestarts(), 3))

	tr.ResetRestarts()
	assert.Check(t, is.Equal(tr.Snapshot().RestartCount, 0))
}

// TestTracker_Concurrent ensures the tracker is safe for concurrent use
// (smoke test under the race detector). It does not verify ordering.
func TestTracker_Concurrent(t *testing.T) {
	t.Parallel()

	tr := lifecycle.NewTracker()
	const n = 100
	done := make(chan struct{})
	for range n {
		go func() {
			tr.Set(lifecycle.StateStarting)
			tr.Set(lifecycle.StateReady)
			tr.Fail(lifecycle.StateRestarting, errors.New("boom"))
			tr.IncRestarts()
			_ = tr.Snapshot()
			done <- struct{}{}
		}()
	}
	for range n {
		<-done
	}
}

// TestErrors_AreDistinct ensures sentinels do not alias each other,
// guarding against accidental var x = y refactors.
func TestErrors_AreDistinct(t *testing.T) {
	t.Parallel()

	all := []error{
		lifecycle.ErrTransport,
		lifecycle.ErrServerUnavailable,
		lifecycle.ErrServerCrashed,
		lifecycle.ErrInitTimeout,
		lifecycle.ErrInitNotification,
		lifecycle.ErrCapabilityMissing,
		lifecycle.ErrAuthRequired,
		lifecycle.ErrSessionMissing,
		lifecycle.ErrNotStarted,
	}
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			assert.Check(t, !errors.Is(a, b),
				"sentinel %d (%v) must not be %d (%v)", i, a, j, b)
		}
	}
}

// TestErrors_WrapWorks ensures errors.Is works across wrapping, which is
// the contract callers will rely on.
func TestErrors_WrapWorks(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("connect: %w", lifecycle.ErrTransport)
	assert.Check(t, errors.Is(wrapped, lifecycle.ErrTransport))
	assert.Check(t, !errors.Is(wrapped, lifecycle.ErrAuthRequired))
}
