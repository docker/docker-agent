package resizegrip

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type scheduler struct{ now time.Time }

func (s *scheduler) Now() time.Time { return s.now }
func (s *scheduler) Tick(d time.Duration, makeMsg func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { s.now = s.now.Add(d); return makeMsg(s.now) }
}

func TestIndependentHoverFades(t *testing.T) {
	t.Parallel()
	ar := animation.NewRuntimeWithScheduler(&scheduler{now: time.Unix(1, 0)})
	r := NewReveal(ar)
	rest := r.Render("│", 1)
	require.Equal(t, " ", rest)
	require.Equal(t, "        ", r.Render("─", 8))
	advance := func(cmd tea.Cmd) tea.Cmd {
		t.Helper()
		require.NotNil(t, cmd)
		tick, ok := ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		r.Tick(tick)
		assert.True(t, tick.Dirty())
		return ar.Continue()
	}
	settle := func(cmd tea.Cmd) {
		t.Helper()
		for i := 0; cmd != nil; i++ {
			require.Less(t, i, 3, "fade must settle in three ticks")
			assert.EqualValues(t, 1, ar.ActiveCount())
			cmd = advance(cmd)
		}
		assert.False(t, ar.HasActive())
	}

	settle(r.Set(true, false, false))
	assert.InDelta(t, 1.0, r.edge, 0)
	assert.Zero(t, r.handle)
	edge := r.Render("│", 1)
	assert.NotEqual(t, rest, edge)

	cmd := advance(r.Set(true, true, false))
	assert.Positive(t, r.handle)
	assert.Less(t, r.handle, 1.0)
	assert.InDelta(t, 1.0, r.edge, 0)
	assert.Nil(t, r.Set(true, false, false), "reversing does not start a second tick chain")
	settle(cmd)
	assert.Equal(t, edge, r.Render("│", 1))

	settle(r.Set(true, true, false))
	assert.Equal(t, styles.ResizeHandleHoverStyle.Render("│"), r.Render("│", 1))
	cmd = advance(r.Set(false, false, false))
	assert.Positive(t, r.edge, "the grip fades out rather than vanishing on leave")
	assert.Less(t, r.edge, 1.0)
	settle(cmd)
	assert.Equal(t, rest, r.Render("│", 1))
	assert.Equal(t, "        ", r.Render("─", 8))
}

func TestDragPinsActiveStyleWithoutTicking(t *testing.T) {
	t.Parallel()
	ar := animation.NewRuntimeWithScheduler(&scheduler{now: time.Unix(1, 0)})
	r := NewReveal(ar)
	pending := r.Set(true, true, false)
	require.NotNil(t, pending)
	assert.Nil(t, r.Set(false, false, true))
	assert.Equal(t, darkenedStyle().Render("│"), r.Render("│", 1))
	assert.Zero(t, ar.ActiveCount())
	_, ok := ar.Accept(pending().(animation.TickMsg))
	assert.False(t, ok, "capture stops a pending reveal")
	release := r.Set(false, false, false)
	require.NotNil(t, release)
	assert.Positive(t, r.edge, "release off the edge starts a fade from the revealed color")
	r.Reset()
	assert.Zero(t, ar.ActiveCount())
	assert.Zero(t, r.edge)
}
