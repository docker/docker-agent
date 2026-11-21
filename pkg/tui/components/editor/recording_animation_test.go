package editor

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

type recordingClock struct{ now time.Time }

func (c *recordingClock) Now() time.Time { return c.now }
func (c *recordingClock) Tick(_ time.Duration, makeMsg func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { return makeMsg(c.now) }
}

func TestRecordingDotsFollowElapsedTime(t *testing.T) {
	t.Parallel()
	clock := &recordingClock{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(clock)
	ed := New(nil, WithAnimationRuntime(ar)).(*editor)
	cmd := ed.SetRecording(true)
	assert.Equal(t, "🎤 Listening", ed.textarea.Placeholder)
	advance := func(gap time.Duration) bool {
		t.Helper()
		clock.now = clock.now.Add(gap)
		tick, ok := ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		ed.Update(tick)
		cmd = ar.Continue()
		return tick.Dirty()
	}
	assert.False(t, advance(399*time.Millisecond))
	assert.True(t, advance(time.Millisecond))
	assert.Equal(t, "🎤 Listening·", ed.textarea.Placeholder)
	ar.Pause()
	clock.now = clock.now.Add(time.Hour)
	cmd = ar.Resume()
	assert.True(t, advance(2*animation.RecordingDotsStepDuration))
	assert.Equal(t, "🎤 Listening···", ed.textarea.Placeholder)
	ed.SetRecording(false)
	assert.Equal(t, defaultPlaceholder, ed.textarea.Placeholder)
	assert.False(t, ar.HasActive())
}
