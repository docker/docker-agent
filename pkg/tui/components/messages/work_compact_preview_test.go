package messages

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/workblock"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// A collapsed Work block previews the newest streamed reasoning lines and keeps
// refreshing them as reasoning arrives.
func TestCollapsedWorkPreviewFollowsStreamedReasoning(t *testing.T) {
	ar := animation.NewRuntime()
	m := NewScrollableView(ar, 100, 80, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetPosition(0, 0)
	m.SetCompactWorkLog(true)
	m.addMessage(types.User("request"))
	m.AppendReasoning("root", "one\n\ntwo\n\nthree")
	call := tools.ToolCall{ID: "preview-tool", Function: tools.FunctionCall{Name: "probe"}}
	m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "probe"}, types.ToolStatusCompleted)
	m.AppendReasoning("root", "\n\nfour")
	m.work.projectionValid = false
	m.syncCompactWork()
	work := firstWorkBlock(m)
	require.NotNil(t, work)
	require.False(t, work.Expanded())

	view := ansi.Strip(work.View())
	require.Contains(t, view, "four")
	require.NotContains(t, view, "one")

	m.AppendReasoning("root", "\n\nfive")
	view = ansi.Strip(work.View())
	require.Contains(t, view, "five", "streamed reasoning refreshes the collapsed preview")
	require.NotContains(t, view, "two")
}

// firstWorkBlock returns the only Work block of the fixture transcript.
func firstWorkBlock(m *model) *workblock.Model {
	for _, block := range m.work.blocks {
		return block
	}
	return nil
}

func TestWorkPreviewKeepsReasoningThroughToolsAndRetainsHistoryAfterProse(t *testing.T) {
	m := newCompactParityModel(t)
	m.SetCompactWorkLog(true)
	m.AddUserMessage("request")
	m.SetWorkStatus(true, 0)
	m.AppendReasoningContent("s", "one", "root", "private preview")
	m.View()
	block := firstWorkBlock(m)
	require.Equal(t, 4, block.Height(), "one live line reserves all three preview rows")
	require.Empty(t, block.Rows(), "preview text does not own hit rows")
	call := tools.ToolCall{ID: "preview-tool", Function: tools.FunctionCall{Name: "probe", Arguments: "{"}}
	m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "probe"}, types.ToolStatusPending)
	call.Function.Arguments = `{"key":"value"}`
	m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "probe"}, types.ToolStatusRunning)
	m.View()
	require.Equal(t, 4, block.Height(), "partial arguments and tool execution do not end the preview")
	m.AppendAssistantContent("s", "one", "root", "reply text")
	frame := ansi.Strip(m.View())
	require.Equal(t, 2, block.Height(), "prose settles the preview to its actual height")
	require.Contains(t, frame, "private preview")
	require.Contains(t, frame, "reply text")
	m.SetWorkStatus(false, 0)
	require.Equal(t, 2, block.Height())
	require.Contains(t, ansi.Strip(m.View()), "private preview")
}
