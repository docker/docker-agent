package animation

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type pauseScheduler struct{ now time.Time }

func (s *pauseScheduler) Now() time.Time { return s.now }
func (s *pauseScheduler) Tick(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
	delivered := s.now.Add(d)
	return func() tea.Msg { return f(delivered) }
}

func TestRuntimePausePreservesSubscriptionsAndRejectsQueuedTicks(t *testing.T) {
	t.Parallel()
	ar := NewRuntimeWithScheduler(&pauseScheduler{now: time.Unix(1, 0)})
	sub := ar.Subscribe()
	queued := sub.Start()
	ar.Pause()
	ar.Pause()
	require.Equal(t, int32(1), ar.ActiveCount())
	_, ok := ar.Accept(queued().(TickMsg))
	assert.False(t, ok)
	assert.Zero(t, ar.Now())
	assert.Nil(t, ar.Continue())
	assert.Nil(t, ar.EnsureRunning())

	next := ar.Subscribe()
	assert.Nil(t, next.Start(), "new work cannot schedule while paused")
	assert.Equal(t, int32(2), ar.ActiveCount())
	require.NotNil(t, ar.Resume())
	assert.Nil(t, ar.Resume(), "duplicate focus events cannot fork the chain")
	assert.Nil(t, ar.Continue(), "resume owns the only outstanding tick")
	sub.Stop()
	next.Stop()
}

func TestRuntimePauseWhileIdlePreventsNewTickChains(t *testing.T) {
	t.Parallel()
	ar := NewRuntime()
	ar.Pause()
	sub := ar.Subscribe()
	assert.Nil(t, sub.Start())
	sub.Stop()
	assert.Nil(t, sub.Start(), "zero-to-one registration must also respect pause")
	sub.Stop()
	assert.Nil(t, ar.Resume(), "completed hidden work needs no tick on focus")
	require.NotNil(t, sub.Start(), "visible new work starts normally")
	sub.Stop()
}

func TestRuntimeResumeExcludesHiddenTime(t *testing.T) {
	t.Parallel()
	clock := &pauseScheduler{now: time.Unix(1, 0)}
	ar := NewRuntimeWithScheduler(clock)
	sub := ar.Subscribe()
	_, ok := ar.Accept(sub.Start()().(TickMsg))
	require.True(t, ok)
	require.Equal(t, TickRate, ar.Now())
	stale := ar.Continue()
	ar.Pause()
	clock.now = clock.now.Add(time.Hour)
	resumed := ar.Resume()
	_, ok = ar.Accept(stale().(TickMsg))
	require.False(t, ok, "a pre-blur tick must remain stale after resume")
	_, ok = ar.Accept(resumed().(TickMsg))
	require.True(t, ok)
	assert.Equal(t, 2*TickRate, ar.Now(), "fades must not jump by the time spent hidden")
	sub.Stop()
}
