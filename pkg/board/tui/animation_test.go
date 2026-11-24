package tui

import (
	"errors"
	"runtime"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/board"
	"github.com/docker/docker-agent/pkg/tui/animation"
)

type boardAnimationClock struct{ now time.Time }

func (c *boardAnimationClock) Now() time.Time { return c.now }
func (c *boardAnimationClock) Tick(_ time.Duration, makeMsg func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { return makeMsg(c.now) }
}

func TestBoardSpinnerSamplesOnlyWhileBusy(t *testing.T) {
	t.Parallel()
	clock := &boardAnimationClock{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(clock)
	card := &board.Card{Status: board.StatusRunning}
	m := &model{cards: map[string][]*board.Card{"column": {card}}, animationRuntime: ar, spinnerAnimation: ar.Subscribe()}
	cmd := m.syncSpinner()
	first := m.spinnerGlyph()
	advance := func(gap time.Duration) {
		t.Helper()
		clock.now = clock.now.Add(gap)
		require.NotNil(t, cmd)
		_, cmd = m.Update(cmd())
	}
	advance(80 * time.Millisecond)
	assert.Equal(t, first, m.spinnerGlyph())
	advance(175 * time.Millisecond)
	assert.Equal(t, "⠹", m.spinnerGlyph())
	card.Status = board.StatusWaiting
	assert.Nil(t, m.syncSpinner())
	assert.False(t, ar.HasActive())
	clock.now = clock.now.Add(time.Hour)
	card.Status = board.StatusRunning
	cmd = m.syncSpinner()
	advance(80 * time.Millisecond)
	assert.Equal(t, "⠹", m.spinnerGlyph())
}

func TestBoardSpinnerPausesForShell(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "success"},
		{name: "error", err: errors.New("failed to start")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := &boardAnimationClock{now: time.Unix(1, 0)}
			ar := animation.NewRuntimeWithScheduler(clock)
			card := &board.Card{Status: board.StatusRunning, Worktree: t.TempDir()}
			m := &model{
				columns:          []board.Column{{ID: "column"}},
				cards:            map[string][]*board.Card{"column": {card}},
				animationRuntime: ar,
				spinnerAnimation: ar.Subscribe(),
			}
			tick := m.syncSpinner()
			require.NotNil(t, tick)
			clock.now = clock.now.Add(80 * time.Millisecond)
			_, stale := m.Update(tick())
			require.NotNil(t, stale)

			_, shell := m.Update(keyPress("s"))
			require.NotNil(t, shell)
			clock.now = clock.now.Add(time.Hour)
			_, next := m.Update(stale())
			assert.Nil(t, next, "the pending tick cannot advance the clock while the shell owns the terminal")
			assert.Equal(t, 80*time.Millisecond, ar.Now())

			_, resumed := m.Update(shellDoneMsg{err: tc.err})
			require.NotNil(t, resumed)
			if tc.err == nil {
				assert.Empty(t, m.flash)
			} else {
				assert.Equal(t, "shell: failed to start", m.flash)
				assert.True(t, m.isErr)
				batch, ok := resumed().(tea.BatchMsg)
				require.True(t, ok)
				require.Len(t, batch, 2)
				resumed = batch[0] // the other command only expires the error flash
			}
			clock.now = clock.now.Add(40 * time.Millisecond)
			_, next = m.Update(resumed())
			assert.NotNil(t, next)
			assert.Equal(t, 120*time.Millisecond, ar.Now())
		})
	}
}

func TestBoardSpinnerAcrossTerminalSuspend(t *testing.T) {
	t.Parallel()
	clock := &boardAnimationClock{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(clock)
	card := &board.Card{Status: board.StatusRunning}
	m := &model{cards: map[string][]*board.Card{"column": {card}}, animationRuntime: ar, spinnerAnimation: ar.Subscribe()}
	tick := m.syncSpinner()
	require.NotNil(t, tick)
	clock.now = clock.now.Add(80 * time.Millisecond)
	_, pending := m.Update(tick())
	require.NotNil(t, pending)

	_, suspend := m.Update(tea.KeyPressMsg{Code: 'z', Mod: tea.ModCtrl})
	require.NotNil(t, suspend)
	msg := m.beforeTerminalSuspend(m, suspend())
	require.IsType(t, tea.SuspendMsg{}, msg)
	if runtime.GOOS == "windows" {
		_, resumed := m.Update(msg)
		assert.Nil(t, resumed)
		clock.now = clock.now.Add(40 * time.Millisecond)
		_, next := m.Update(pending())
		assert.NotNil(t, next, "a no-op suspend must not invalidate the pending tick")
		assert.Equal(t, 120*time.Millisecond, ar.Now())
		return
	}

	// Bubble Tea blocks for the suspend between the filter and Update.
	clock.now = clock.now.Add(time.Hour)
	_, resumed := m.Update(msg)
	require.NotNil(t, resumed)
	_, next := m.Update(pending())
	assert.Nil(t, next, "the tick queued before suspend cannot reuse the resumed clock")
	_, next = m.Update(tea.ResumeMsg{})
	assert.Nil(t, next, "Bubble Tea's subsequent resume notification must not start a second tick chain")
	clock.now = clock.now.Add(40 * time.Millisecond)
	_, next = m.Update(resumed())
	assert.NotNil(t, next)
	assert.Equal(t, 120*time.Millisecond, ar.Now())
}
