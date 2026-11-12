package chat

import (
	"context"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestBackgroundAgentStatusPreservesForegroundState(t *testing.T) {
	t.Parallel()

	for _, working := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "working"}[working], func(t *testing.T) {
			t.Parallel()
			sess := session.New()
			ss := service.NewSessionState(sess)
			ss.SetCurrentAgentName("root")
			p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), queueTestRuntime{}, sess), ss).(*chatPage)
			p.SetSize(140, 40)
			_, cancel := context.WithCancel(t.Context())
			defer cancel()
			p.msgCancel = cancel
			p.working = working
			p.messageQueue = []queuedMessage{{content: "queued"}}
			for _, running := range []bool{true, false} {
				handled, _ := p.handleRuntimeEvent(runtime.BackgroundAgentStatus("child", "worker", running))
				require.True(t, handled)
				assert.Equal(t, working, p.working)
				assert.NotNil(t, p.msgCancel)
				assert.Zero(t, p.streamDepth)
				assert.Len(t, p.messageQueue, 1)
				assert.Equal(t, "root", ss.CurrentAgentName())
				if running {
					assert.Contains(t, ansi.Strip(p.sidebar.View()), "Background: 1 running")
				} else {
					assert.NotContains(t, p.sidebar.View(), "Background:")
				}
			}
		})
	}
}

func TestBackgroundAgentCountRelayoutsSidebarBand(t *testing.T) {
	t.Parallel()

	for _, position := range []msgtypes.SidebarPosition{msgtypes.SidebarTop, msgtypes.SidebarBottom} {
		t.Run(string(position), func(t *testing.T) {
			t.Parallel()
			p := newLayoutTestPage(t, position)
			p.SetLayoutSettings(msgtypes.LayoutSettings{
				SidebarPosition: position, HideUsage: true, HideTools: true, HideTodos: true,
			})
			p.SetSize(40, 40)
			before := p.computeSidebarLayout()
			p.Update(runtime.BackgroundAgentStatus("child", "worker", true))
			during := p.computeSidebarLayout()
			assert.NotEqual(t, before, during, "the count needs more space in a narrow band")
			assert.Equal(t, during, p.appliedLayout)
			assert.Contains(t, ansi.Strip(p.sidebar.View()), "Background: 1 running")
			p.Update(runtime.BackgroundAgentStatus("child", "worker", false))
			assert.Equal(t, before, p.computeSidebarLayout())
			assert.Equal(t, before, p.appliedLayout)
		})
	}
}

func TestBackgroundAgentCountBandHeightMatchesRendering(t *testing.T) {
	t.Parallel()

	for _, position := range []msgtypes.SidebarPosition{msgtypes.SidebarTop, msgtypes.SidebarBottom} {
		t.Run(string(position), func(t *testing.T) {
			t.Parallel()
			p := newLayoutTestPage(t, position)
			p.sessionState.SetCurrentAgentName("root")
			p.sidebar.SetAgentInfo("root", "anthropic/claude-sonnet-4-5", "", 0, "", 0)
			p.sidebar.SetTeamInfo([]runtime.AgentDetails{{Name: "root"}, {Name: "worker"}})
			p.SetLayoutSettings(msgtypes.LayoutSettings{
				SidebarPosition: position, HideSessionPath: true, HideUsage: true, HideTools: true, HideTodos: true,
			})
			p.SetSize(37, 40)
			for _, running := range []bool{false, true, false} {
				p.Update(runtime.BackgroundAgentStatus("child", "worker", running))
				sl := p.computeSidebarLayout()
				assert.Equal(t, sl.sidebarHeight, lipgloss.Height(p.renderCollapsedSidebar(sl)), "running=%v", running)
				assert.Equal(t, p.height, lipgloss.Height(p.View()), "running=%v", running)
			}
		})
	}
}
