package animation

import (
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runTransitionSynctest(t *testing.T, f func(t *testing.T)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		f(t)
	})
}

func tickTransition(t *testing.T, cmd *tea.Cmd, tr *Transition) {
	t.Helper()
	tickAnimationClock(t, cmd, tr)
	tr.Tick()
	*cmd = transitionRuntime(tr).Continue()
}

func tickAnimationClock(t *testing.T, cmd *tea.Cmd, tr *Transition) {
	t.Helper()
	require.NotNil(t, *cmd)
	tick := runTick(t, func() any { return (*cmd)() })
	_, ok := transitionRuntime(tr).Accept(tick)
	require.True(t, ok)
}

func TestTransition_BasicLifecycle(t *testing.T) {
	runTransitionSynctest(t, func(t *testing.T) {
		t.Helper()
		ar := NewRuntime()
		tr := ar.Transition()

		assert.False(t, tr.Running())
		assert.InDelta(t, 0.0, tr.Value(), 1e-9)

		cmd := tr.Start(10*TickRate, Linear)
		assert.True(t, tr.Running())
		assert.Equal(t, int32(1), ar.ActiveCount())

		for range 10 {
			tickTransition(t, &cmd, &tr)
		}

		assert.False(t, tr.Running(), "should stop after all ticks")
		assert.InDelta(t, 1.0, tr.Value(), 1e-9)
		assert.Equal(t, int32(0), ar.ActiveCount(), "should unregister on completion")
	})
}

func TestTransition_Cancel(t *testing.T) {
	runTransitionSynctest(t, func(t *testing.T) {
		t.Helper()
		ar := NewRuntime()
		tr := ar.Transition()
		cmd := tr.Start(20*TickRate, Linear)
		require.True(t, tr.Running())

		for range 5 {
			tickTransition(t, &cmd, &tr)
		}
		tr.Cancel()

		assert.False(t, tr.Running())
		assert.Equal(t, int32(0), ar.ActiveCount())
	})
}

func TestTransition_CancelWhenNotRunning(t *testing.T) {
	runTransitionSynctest(t, func(t *testing.T) {
		t.Helper()
		ar := NewRuntime()
		tr := ar.Transition()
		tr.Cancel()
		assert.Equal(t, int32(0), ar.ActiveCount())
	})
}

func TestTransition_Restart(t *testing.T) {
	runTransitionSynctest(t, func(t *testing.T) {
		t.Helper()
		ar := NewRuntime()
		tr := ar.Transition()
		cmd := tr.Start(10*TickRate, Linear)
		for range 5 {
			tickTransition(t, &cmd, &tr)
		}

		// Restart while running; this should not double-register.
		restartCmd := tr.Start(10*TickRate, Linear)
		assert.Nil(t, restartCmd)
		assert.True(t, tr.Running())
		assert.Equal(t, int32(1), ar.ActiveCount())
		assert.InDelta(t, 0.0, tr.Value(), 1e-9, "should reset to 0")
	})
}

func TestTransition_Lerp(t *testing.T) {
	runTransitionSynctest(t, func(t *testing.T) {
		t.Helper()
		ar := NewRuntime()
		tr := ar.Transition()
		cmd := tr.Start(4*TickRate, Linear)

		assert.Equal(t, 0, tr.Lerp(0, 100))

		tickTransition(t, &cmd, &tr)
		assert.Equal(t, 25, tr.Lerp(0, 100))

		tickTransition(t, &cmd, &tr)
		assert.Equal(t, 50, tr.Lerp(0, 100))

		tickTransition(t, &cmd, &tr)
		assert.Equal(t, 75, tr.Lerp(0, 100))

		tickTransition(t, &cmd, &tr)
		assert.Equal(t, 100, tr.Lerp(0, 100))
	})
}

func TestTransition_LerpReverse(t *testing.T) {
	runTransitionSynctest(t, func(t *testing.T) {
		t.Helper()
		ar := NewRuntime()
		tr := ar.Transition()
		cmd := tr.Start(2*TickRate, Linear)

		assert.Equal(t, 200, tr.Lerp(200, 0))

		tickTransition(t, &cmd, &tr)
		assert.Equal(t, 100, tr.Lerp(200, 0))

		tickTransition(t, &cmd, &tr)
		assert.Equal(t, 0, tr.Lerp(200, 0))
	})
}

func TestEaseOutCubic_BoundsAndMonotonicity(t *testing.T) {
	assert.InDelta(t, 0.0, EaseOutCubic(0), 1e-9)
	assert.InDelta(t, 1.0, EaseOutCubic(1), 1e-9)

	prev := 0.0
	for i := 1; i <= 100; i++ {
		v := EaseOutCubic(float64(i) / 100)
		assert.GreaterOrEqual(t, v, prev, "must be monotonically increasing")
		prev = v
	}
}

func TestEaseOutCubic_FastStartSlowEnd(t *testing.T) {
	firstHalf := EaseOutCubic(0.5)
	// Ease-out covers more than half the distance in the first half of time.
	assert.Greater(t, firstHalf, 0.5, "ease-out should cover >50%% in the first half")
}

func TestEaseOutQuint_BoundsAndMonotonicity(t *testing.T) {
	assert.InDelta(t, 0.0, EaseOutQuint(0), 1e-9)
	assert.InDelta(t, 1.0, EaseOutQuint(1), 1e-9)

	prev := 0.0
	for i := 1; i <= 100; i++ {
		v := EaseOutQuint(float64(i) / 100)
		assert.GreaterOrEqual(t, v, prev)
		prev = v
	}
}

func TestEaseInOutCubic_BoundsAndSymmetry(t *testing.T) {
	assert.InDelta(t, 0.0, EaseInOutCubic(0), 1e-9)
	assert.InDelta(t, 0.5, EaseInOutCubic(0.5), 1e-9)
	assert.InDelta(t, 1.0, EaseInOutCubic(1), 1e-9)
}

func TestTransition_EaseOutCubic_Integration(t *testing.T) {
	runTransitionSynctest(t, func(t *testing.T) {
		t.Helper()
		ar := NewRuntime()
		tr := ar.Transition()
		cmd := tr.Start(10*TickRate, EaseOutCubic)

		// Values should increase monotonically and ease out.
		prev := 0.0
		for range 10 {
			tickTransition(t, &cmd, &tr)
			v := tr.Value()
			assert.GreaterOrEqual(t, v, prev)
			prev = v
		}
		assert.InDelta(t, 1.0, prev, 1e-9)
	})
}

func TestTransition_TickWhenNotRunning(t *testing.T) {
	runTransitionSynctest(t, func(t *testing.T) {
		t.Helper()
		ar := NewRuntime()
		tr := ar.Transition()
		tr.Tick()
		assert.Equal(t, int32(0), ar.ActiveCount())
	})
}

func TestTransition_ZeroTicks(t *testing.T) {
	runTransitionSynctest(t, func(t *testing.T) {
		t.Helper()
		ar := NewRuntime()
		tr := ar.Transition()
		cmd := tr.Start(0*TickRate, Linear)
		assert.True(t, tr.Running())

		tickTransition(t, &cmd, &tr)
		assert.False(t, tr.Running())
		assert.InDelta(t, 1.0, tr.Value(), 1e-9)
	})
}

func transitionRuntime(tr *Transition) *Runtime { return tr.ar }

type manualScheduler struct{ now time.Time }

func (s *manualScheduler) Now() time.Time { return s.now }
func (s *manualScheduler) Tick(_ time.Duration, makeMsg func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { return makeMsg(s.now) }
}

func TestTransitionAndSpinnerSampleElapsedTime(t *testing.T) {
	t.Parallel()
	const duration = 500 * time.Millisecond
	for _, samples := range [][]time.Duration{
		{80 * time.Millisecond, 170 * time.Millisecond},
		{250 * time.Millisecond},
	} {
		clock := &manualScheduler{now: time.Unix(1, 0)}
		ar := NewRuntimeWithScheduler(clock)
		tr := ar.Transition()
		cmd := tr.Start(duration, EaseOutCubic)
		advance := func(gap time.Duration) {
			t.Helper()
			clock.now = clock.now.Add(gap)
			_, ok := ar.Accept(cmd().(TickMsg))
			require.True(t, ok)
			tr.Tick()
			cmd = ar.Continue()
		}
		for _, d := range samples {
			advance(d)
		}
		assert.Equal(t, 88, tr.Lerp(0, 100))
		assert.Equal(t, 2, Chat.FrameIndexAt(ar.Now()))

		ar.Pause()
		clock.now = clock.now.Add(time.Hour)
		assert.Equal(t, 88, tr.Lerp(0, 100))
		cmd = ar.Resume()
		advance(50 * time.Millisecond)
		assert.Equal(t, 3, Chat.FrameIndexAt(ar.Now()), "hidden time must not advance the phase")
		advance(time.Hour)
		assert.Equal(t, 100, tr.Lerp(0, 100), "a long stall clamps a completed transition")
		assert.Equal(t, 3, Chat.FrameIndexAt(ar.Now()), "a long stall samples the elapsed phase directly")
		assert.Nil(t, cmd)
	}
}

func TestRuntimeExcludesIdleTimeBetweenSubscriptions(t *testing.T) {
	t.Parallel()
	clock := &manualScheduler{now: time.Unix(1, 0)}
	ar := NewRuntimeWithScheduler(clock)
	sub := ar.Subscribe()
	cmd := sub.Start()
	clock.now = clock.now.Add(TickRate)
	_, ok := ar.Accept(cmd().(TickMsg))
	require.True(t, ok)
	sub.Stop()
	clock.now = clock.now.Add(time.Hour)
	cmd = sub.Start()
	clock.now = clock.now.Add(TickRate)
	_, ok = ar.Accept(cmd().(TickMsg))
	require.True(t, ok)
	assert.Equal(t, 2*TickRate, ar.Now())
}
