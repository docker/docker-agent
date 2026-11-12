package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestViewsRequestFocusWithoutKeyboardEventTypes(t *testing.T) {
	t.Parallel()
	for _, lean := range []bool{false, true} {
		view := toFullscreenView("content", "title", true, lean)
		assert.True(t, view.ReportFocus)
		assert.False(t, view.KeyboardEnhancements.ReportEventTypes, "do not revive the VS Code AZERTY regression")
	}
}

func TestRootBlurKeepsRenderingAndAnimating(t *testing.T) {
	root, _ := newTestModel(t)
	root.ar = animation.NewRuntimeWithScheduler(&rootImmediateScheduler{})
	t.Cleanup(root.ar.Stop)
	page := &countingChatPage{text: "before", dirtyTick: true}
	root.activeTab.chatPage = page
	root.activeTab.sessionState = &service.SessionState{}
	root.ready, root.leanMode = true, true

	before := root.View()
	require.Contains(t, before.Content, "before")

	sub := root.ar.Subscribe()
	queued := sub.Start()
	t.Cleanup(sub.Stop)
	require.NotNil(t, queued)

	_, _ = root.Update(tea.BlurMsg{})
	page.text = "after"
	_, _ = root.Update(agentruntime.AgentChoice("root", "profile", "stream update"))
	after := root.View()
	assert.Contains(t, after.Content, "after", "blurred windows must continue redrawing")

	_, cmd := root.Update(queued())
	assert.NotNil(t, cmd, "blurred windows must continue scheduling animation ticks")
	assert.Positive(t, root.ar.Now())
}
