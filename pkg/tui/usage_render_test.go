package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	chattypes "github.com/docker/docker-agent/pkg/chat"
	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

func TestTokenUsageWithoutSidebarReusesRootView(t *testing.T) {
	for _, lean := range []bool{false, true} {
		for _, routed := range []bool{false, true} {
			name := map[bool]string{false: "sidebar disabled", true: "lean"}[lean]
			name += map[bool]string{false: "/direct", true: "/routed"}[routed]
			t.Run(name, func(t *testing.T) {
				root, _, _ := frozenClockRoot(t, 120, 40)
				root.leanMode = lean
				if lean {
					root.hideSidebar = false
					root.activeTab.chatPage = chat.New(root.ar, t.Context(), root.application, root.activeTab.sessionState, chat.WithLeanMode())
					root.handleWindowResize(120, 40)
				}
				root.activeTab.state = root.supervisor.GetRunner("profile").State
				page := &deferredRenderPage{Page: root.activeTab.chatPage}
				root.activeTab.chatPage = page
				_, _ = root.Update(agentruntime.StreamStarted("profile", "root"))
				before := root.View()
				require.NotNil(t, before.ProgressBar)
				views := page.views

				for i := range 1100 {
					var msg tea.Msg = agentruntime.NewTokenUsageEvent("profile", "root", &agentruntime.Usage{
						InputTokens: int64(i + 1), OutputTokens: int64(i + 2), Cost: float64(i + 1),
						LastMessage: &agentruntime.MessageUsage{Model: "test-model", Cost: 1, Usage: chattypes.Usage{InputTokens: 1}},
					})
					if routed {
						msg = messages.RoutedMsg{SessionID: "profile", Inner: msg}
					}
					_, _ = root.Update(msg)
					require.True(t, root.viewCacheValid, "accounting alone must not invalidate the frame")
					require.Equal(t, before, root.View())
				}
				require.Equal(t, views, page.views, "usage backlog must not compose frames")
				input, output := root.application.Session().Usage()
				require.Equal(t, int64(1100), input)
				require.Equal(t, int64(1101), output)
				require.Len(t, root.application.Session().MessageUsageHistorySnapshot(), 1100, "every per-message record must survive")
				cost, ok := root.activeTab.sessionState.AgentCost("root")
				require.True(t, ok)
				require.InDelta(t, 1100.0, cost, 0)

				_, _ = root.Update(agentruntime.AgentChoice("root", "profile", "FINAL-RESPONSE-AFTER-USAGE", "answer"))
				_, _ = root.Update(agentruntime.StreamStopped("profile", "root", "normal"))
				require.False(t, root.activeTab.chatPage.IsWorking())
				after := root.View()
				require.Nil(t, after.ProgressBar)
				require.NotEqual(t, before.WindowTitle, after.WindowTitle)
				require.Contains(t, ansi.Strip(after.Content), "FINAL-RESPONSE-AFTER-USAGE")
			})
		}
	}
}

func TestTokenUsageRefreshesVisibleUsage(t *testing.T) {
	for _, costDialog := range []bool{false, true} {
		t.Run(map[bool]string{false: "sidebar", true: "cost dialog"}[costDialog], func(t *testing.T) {
			root, _, _ := frozenClockRoot(t, 120, 40)
			if costDialog {
				_, _ = root.Update(dialog.OpenDialogMsg{Model: dialog.NewCostDialog(root.application.Session())})
			} else {
				root.hideSidebar = false
				root.activeTab.chatPage = chat.New(root.ar, t.Context(), root.application, root.activeTab.sessionState)
				root.handleWindowResize(120, 40)
			}
			before := root.View()
			_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.NewTokenUsageEvent("profile", "root", &agentruntime.Usage{
				InputTokens: 100, OutputTokens: 20, ContextLength: 120, ContextLimit: 1000, Cost: 0.25,
				LastMessage: &agentruntime.MessageUsage{Model: "test-model", Cost: 0.25, Usage: chattypes.Usage{InputTokens: 100, OutputTokens: 20}},
			})})
			require.False(t, root.viewCacheValid, "visible accounting must refresh")
			after := root.View().Content
			require.NotEqual(t, before.Content, after)
			require.Contains(t, ansi.Strip(after), "$0.25")
		})
	}
}

func TestTokenUsagePreservesPendingVisualChanges(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 120, 40)
	_, _ = root.Update(agentruntime.AgentChoice("root", "profile", "PENDING-CONTENT"))
	_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.NewTokenUsageEvent("profile", "root", &agentruntime.Usage{})})
	require.False(t, root.viewCacheValid)
	require.Contains(t, ansi.Strip(root.View().Content), "PENDING-CONTENT")
}
