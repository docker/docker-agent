package messages

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// shrinkingView is a layout.Model whose rendered height can be changed
// between renders, simulating content shrinkage (e.g. tools fading out
// of a reasoning block).
type shrinkingView struct {
	height int
}

func (s *shrinkingView) Init() tea.Cmd                          { return nil }
func (s *shrinkingView) Update(tea.Msg) (layout.Model, tea.Cmd) { return s, nil }
func (s *shrinkingView) View() string {
	if s.height <= 0 {
		return ""
	}
	return strings.Repeat("x\n", s.height-1) + "x"
}
func (s *shrinkingView) SetSize(_, _ int) tea.Cmd { return nil }

// addShrinkingView adds a shrinkingView with a placeholder message. It is
// intentionally not cached (MessageTypeAssistantReasoningBlock is not cached)
// so that height changes propagate through ensureAllItemsRendered.
func addShrinkingView(m *model, height int) *shrinkingView {
	view := &shrinkingView{height: height}
	msg := &types.Message{Type: types.MessageTypeAssistantReasoningBlock, Sender: "root"}
	m.messages = append(m.messages, msg)
	m.views = append(m.views, view)
	m.renderDirty = true
	return view
}

func TestBottomSlackCappedOnLargeShrinkage(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	view := addShrinkingView(m, 30)

	// Initial render: content fits the auto-scroll path; no slack.
	m.View()
	require.Equal(t, 30, m.totalHeight)
	require.Equal(t, 0, m.bottomSlack)

	// Simulate a large shrinkage (e.g. multiple tools fading out at once)
	// and re-render. Without the cap, slack would absorb all 25 lines and
	// leave the viewport mostly empty.
	view.height = 5
	m.invalidateAllItems()
	m.View()

	require.Equal(t, 5, m.totalHeight)
	maxSlack := m.maxBottomSlack()
	assert.LessOrEqual(t, m.bottomSlack, maxSlack,
		"slack must be capped to keep the viewport from being mostly empty")
}

func TestBottomSlackDecaysOnAnimationTick(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	addShrinkingView(m, 10)
	m.View()

	// Pretend a previous shrinkage left some slack behind.
	m.bottomSlack = 3

	m.Update(animation.TickMsg{Frame: 1})
	assert.Equal(t, 2, m.bottomSlack, "tick should decay slack by one line")

	m.Update(animation.TickMsg{Frame: 2})
	assert.Equal(t, 1, m.bottomSlack)

	m.Update(animation.TickMsg{Frame: 3})
	assert.Equal(t, 0, m.bottomSlack, "slack should reach zero after enough ticks")

	// Further ticks must not produce negative slack.
	m.Update(animation.TickMsg{Frame: 4})
	assert.Equal(t, 0, m.bottomSlack)
}

func TestBottomSlackAnimationSubscribesWhileDecaying(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	addShrinkingView(m, 10)
	m.View()

	require.False(t, m.slackAnimationSub.IsActive(),
		"no slack means no animation subscription")

	// Slack > 0: a tick should subscribe so further ticks keep firing
	// even after fade animations finish. We can only assert on the local
	// subscription state — the global tick command from Subscription.Start
	// is non-nil only for the first global subscriber, which is racy when
	// tests touching the animation coordinator run in parallel.
	m.bottomSlack = 2
	m.Update(animation.TickMsg{Frame: 1})
	assert.True(t, m.slackAnimationSub.IsActive(),
		"subscription should be active while slack > 0")

	// Once slack hits zero, the subscription must release the global tick.
	m.Update(animation.TickMsg{Frame: 2})
	m.Update(animation.TickMsg{Frame: 3})
	assert.Equal(t, 0, m.bottomSlack)
	assert.False(t, m.slackAnimationSub.IsActive(),
		"subscription should be released once slack reaches zero")
}

func TestBottomSlackDoesNotLeaveEmptyViewportAfterShrinkage(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 10, sessionState).(*model)
	m.SetSize(80, 10)

	// Start with content that fills the viewport while auto-scrolled.
	view := addShrinkingView(m, 30)
	m.View()
	require.Equal(t, 30, m.totalHeight)

	// Shrink the content drastically (simulates several tools fading out
	// at the same time).
	view.height = 2
	m.invalidateAllItems()
	out := m.View()

	// The visible viewport must still contain real content; not be filled
	// with empty slack lines.
	contentLines := 0
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) != "" {
			contentLines++
		}
	}
	assert.Positive(t, contentLines,
		"viewport should not be entirely empty after content shrinks")
}

func TestMaxBottomSlackScalesWithViewportHeight(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}

	cases := []struct {
		height int
		want   int
	}{
		{height: 3, want: 1},   // tiny viewport → at least 1
		{height: 12, want: 4},  // height/3
		{height: 24, want: 5},  // capped at 5
		{height: 100, want: 5}, // still capped at 5
	}
	for _, c := range cases {
		m := NewScrollableView(80, c.height, sessionState).(*model)
		m.SetSize(80, c.height)
		assert.Equal(t, c.want, m.maxBottomSlack(),
			"maxBottomSlack(height=%d)", c.height)
	}
}

func TestBottomSlackIsZeroWhenUserHasScrolledAway(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	view := addShrinkingView(m, 30)
	m.View()

	// User scrolls away from the bottom, then content shrinks. We don't
	// want slack to be added in that case, since the user already chose
	// their scroll position.
	m.userHasScrolled = true
	view.height = 5
	m.invalidateAllItems()
	m.View()

	assert.Equal(t, 0, m.bottomSlack,
		"slack must remain zero when the user scrolled away from the bottom")
}

func TestBottomSlackDecayPausesWhenUserScrollsAway(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	addShrinkingView(m, 10)
	m.View()

	// Pretend a previous shrinkage left some slack while auto-following.
	m.bottomSlack = 3

	// First tick decays one line as expected.
	m.Update(animation.TickMsg{Frame: 1})
	require.Equal(t, 2, m.bottomSlack)

	// User scrolls away mid-decay. updateScrollState resets slack to zero
	// for the userHasScrolled path; the next tick should leave it there
	// and not produce a negative value.
	m.userHasScrolled = true
	m.Update(animation.TickMsg{Frame: 2})
	assert.Equal(t, 0, m.bottomSlack,
		"slack must drop to zero (not be decayed below it) once the user scrolls away")
}

func TestAdjustBottomSlackRespectsCapAndFloor(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	maxSlack := m.maxBottomSlack()

	// Adding more than the cap must clamp to the cap.
	m.AdjustBottomSlack(100)
	assert.Equal(t, maxSlack, m.bottomSlack,
		"AdjustBottomSlack must clamp positive deltas to maxBottomSlack")

	// Subtracting below zero must clamp to zero.
	m.AdjustBottomSlack(-100)
	assert.Equal(t, 0, m.bottomSlack,
		"AdjustBottomSlack must clamp negative deltas at zero")

	// Zero delta is a no-op.
	m.bottomSlack = 2
	m.AdjustBottomSlack(0)
	assert.Equal(t, 2, m.bottomSlack,
		"AdjustBottomSlack(0) must leave slack unchanged")
}
