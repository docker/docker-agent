package messages

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestReasoningResetPreservesToolsTextAndOtherMessages(t *testing.T) {
	t.Parallel()
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 100, 40, &service.SessionState{}).(*model)
	m.AppendReasoningContent("other-session", "answer", "root", "OTHER-SESSION")
	m.AppendReasoningContent("s", "other-message", "root", "OTHER-MESSAGE")
	m.AppendReasoningContent("s", "answer", "root", "DECLINED-IN-BLOCK")
	call := tools.ToolCall{ID: "kept-tool", Function: tools.FunctionCall{Name: "shell"}}
	m.AddOrUpdateToolCall("root", call, tools.Tool{}, types.ToolStatusPending)
	block := m.views[2].(*reasoningblock.Model)
	block.SetExpanded(true)
	m.AppendAssistantContent("s", "answer", "root", "KEPT-TEXT")
	m.AppendReasoningContent("s", "answer", "root", "DECLINED-SEGMENT")
	m.BreakMessageGroup()
	m.AppendAssistantContent("s", "next", "root", "NEXT")
	require.Equal(t, 5, m.groupStart)
	m.ResetReasoningContent("s", "answer", "root")
	require.Equal(t, 4, m.groupStart)
	require.Len(t, m.messages, 5)
	require.Len(t, m.views, 5)
	require.Empty(t, m.messages[2].Content, "copySelectedMessageToClipboard reads this backing field")
	m.selectedMessageIndex = 2
	require.Nil(t, m.copySelectedMessageToClipboard(), "no declined text is copied from a tool-only block")
	require.Empty(t, block.Reasoning())
	require.True(t, block.HasToolCall("kept-tool"))
	require.Equal(t, 1, block.ToolCount())
	require.NotContains(t, block.View(), "DECLINED")
	require.Equal(t, "OTHER-SESSION", m.messages[0].Content)
	require.Equal(t, "OTHER-MESSAGE", m.messages[1].Content)
	require.Equal(t, "KEPT-TEXT", m.messages[3].Content)
	m.AppendToLastMessage("root", "-CONTINUED")
	require.Equal(t, "NEXT-CONTINUED", m.messages[4].Content)
	m.AppendReasoningContent("s", "answer", "root", "FALLBACK")
	require.Equal(t, "FALLBACK", m.lastMessage().Content)
	require.NotContains(t, ansi.Strip(m.View()), "DECLINED")
}

func TestReasoningResetRemovesPureBlocksAndAcceptsNewDeltas(t *testing.T) {
	t.Parallel()
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 100, 40, &service.SessionState{}).(*model)
	m.AppendReasoningContent("s", "answer", "root", "DECLINED")
	m.ResetReasoningContent("s", "answer", "root")
	require.Empty(t, m.messages)
	require.Empty(t, m.views)
	m.ResetReasoningContent("s", "answer", "root")
	m.AppendReasoningContent("s", "answer", "root", "FALLBACK")
	require.Equal(t, "FALLBACK", m.messages[0].Content)
	require.Equal(t, "FALLBACK", m.views[0].(*reasoningblock.Model).Reasoning())
}
