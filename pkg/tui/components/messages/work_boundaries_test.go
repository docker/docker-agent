package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	handoff "github.com/docker/docker-agent/pkg/tools/builtin/handoff/types"
	transfertask "github.com/docker/docker-agent/pkg/tools/builtin/transfertask/types"
	"github.com/docker/docker-agent/pkg/tui/components/tool/defaults"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func boundaryCall(name string) tools.ToolCall {
	return tools.ToolCall{ID: name, Function: tools.FunctionCall{Name: name, Arguments: `{"agent":"child","task":"child task"}`}}
}

func appendBoundaryTool(m *model, name string) *runtime.ToolCallResponseEvent {
	call := boundaryCall(name)
	def := tools.Tool{Name: name}
	m.AddOrUpdateToolCall("root", call, def, types.ToolStatusRunning)
	return &runtime.ToolCallResponseEvent{ToolCallID: call.ID, ToolDefinition: def, Result: tools.ResultSuccess("tool result")}
}

func saveBoundaryTool(sess *session.Session, name string, failed bool) {
	call := boundaryCall(name)
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role: chat.MessageRoleAssistant, ReasoningContent: "parent work",
		ToolCalls: []tools.ToolCall{call}, ToolDefinitions: []tools.Tool{{Name: name}},
	}))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role: chat.MessageRoleTool, ToolCallID: call.ID, IsError: failed, Content: "tool result",
	}))
}

func TestCompactWorkHandoffLiveAndReload(t *testing.T) {
	for _, name := range []string{handoff.ToolNameHandoff, "force_handoff"} {
		for _, replay := range []bool{false, true} {
			mode := "live"
			if replay {
				mode = "reload"
			}
			t.Run(name+"/"+mode, func(t *testing.T) {
				m := newCompactParityModel(t)
				m.SetToolRenderers(defaults.NewRegistry())
				m.SetCompactWorkLog(true)
				m.SetVisibleReplies(messages.VisibleRepliesLatest)
				if replay {
					sess := session.New()
					sess.AddMessage(session.UserMessage("request"))
					sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "parent reply"}))
					if name == handoff.ToolNameHandoff {
						saveBoundaryTool(sess, name, false)
					} else {
						sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "parent work"}))
					}
					sess.AddMessage(session.NewAgentMessage("child", &chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "child work", Content: "child reply"}))
					m.LoadFromSession(sess, nil)
				} else {
					m.AddUserMessage("request")
					m.SetWorkStatus(true, 0)
					m.AppendToLastMessage("root", "parent reply")
					m.AppendReasoning("root", "parent work")
					if name == handoff.ToolNameHandoff {
						response := appendBoundaryTool(m, name)
						require.NotContains(t, ansi.Strip(m.View()), "─►", "a running handoff has not changed control")
						m.AddToolResult(response, types.ToolStatusCompleted)
						m.AddToolResult(response, types.ToolStatusCompleted)
						requireWorkBefore(t, m, "Work", "─►")
						require.NotContains(t, ansi.Strip(m.View()), "Working", "the target has not done any work")
					}
					m.AppendReasoning("child", "child work")
					m.AppendToLastMessage("child", "child reply")
					m.SetWorkStatus(false, 0)
				}
				requireWorkBefore(t, m, "Work", "─►", "child reply")
				require.NotContains(t, ansi.Strip(m.View()), "parent reply", "quota stays per human exchange")
				require.Len(t, m.work.ranges, 2)
				first, second := m.work.ranges[0].block, m.work.ranges[1].block
				clickAt(m, m.work.ranges[0].start)
				require.True(t, first.Expanded())
				require.False(t, second.Expanded())
				first.SetExpandedImmediately(true)
				m.invalidateAllItems()
				requireWorkBefore(t, m, "Work", "parent reply", "parent work", "─►", "child reply")
				clickAt(m, m.work.ranges[1].start)
				require.True(t, second.Expanded())
				require.True(t, first.Expanded(), "segments expand independently")
				second.SetExpandedImmediately(true)
				m.invalidateAllItems()
				requireWorkBefore(t, m, "Work", "parent work", "─►", "child work", "child reply")
			})
		}
	}
}

func TestCompactWorkTransferReplayDoesNotInventChildOrReturn(t *testing.T) {
	sess := session.New()
	sess.AddMessage(session.UserMessage("request"))
	saveBoundaryTool(sess, transfertask.ToolNameTransferTask, false)
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "resumed work"}))
	m := newCompactParityModel(t)
	m.SetToolRenderers(defaults.NewRegistry())
	m.LoadFromSession(sess, nil)
	ordinary := ansi.Strip(m.View())
	require.Equal(t, 1, strings.Count(ordinary, "Thinking"), "same-agent reasoning is one ordinary card")
	require.Equal(t, 1, m.MessageTypeCount(types.MessageTypeAssistantReasoningBlock))
	m.SetCompactWorkLog(true)
	requireWorkBefore(t, m, "Work", "calls", "resumed work")
	require.Len(t, m.work.ranges, 2)
	for _, row := range m.work.ranges {
		row.block.SetExpandedImmediately(true)
	}
	m.invalidateAllItems()
	requireWorkBefore(t, m, "Work", "parent work", "child task", "calls", "resumed work")
	frame := ansi.Strip(m.View())
	require.NotContains(t, frame, types.AgentReturnLabel)
	require.NotContains(t, frame, "─►")
	require.NotContains(t, frame, "tool result", "the tool result is not a restored child reply")
	m.SetCompactWorkLog(false)
	require.Equal(t, ordinary, ansi.Strip(m.View()), "disabling Compact restores the unmodified ordinary card")
}

func TestCompactWorkSwitchToolFailureDoesNotChangeControl(t *testing.T) {
	for _, name := range []string{handoff.ToolNameHandoff, transfertask.ToolNameTransferTask} {
		t.Run(name, func(t *testing.T) {
			m := newCompactParityModel(t)
			m.SetToolRenderers(defaults.NewRegistry())
			m.SetCompactWorkLog(true)
			m.AddUserMessage("request")
			m.AppendReasoning("root", "parent work")
			response := appendBoundaryTool(m, name)
			response.Result = tools.ResultError("tool failed")
			response.Response = "tool failed"
			m.AddToolResult(response, types.ToolStatusError)
			m.AppendReasoning("root", "resumed work")
			m.View()
			require.Len(t, m.work.ranges, 1)
			expandWork(t, m)
			frame := ansi.Strip(m.View())
			require.Contains(t, frame, "tool failed")
			require.NotContains(t, frame, "─►")
			require.NotContains(t, frame, "root calls")

			sess := session.New()
			sess.AddMessage(session.UserMessage("request"))
			saveBoundaryTool(sess, name, true)
			sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "resumed work"}))
			m.LoadFromSession(sess, nil)
			expandWork(t, m)
			frame = ansi.Strip(m.View())
			require.Len(t, m.work.ranges, 1)
			require.NotContains(t, frame, "─►")
			require.NotContains(t, frame, "root calls")
		})
	}
}

func TestCompactWorkReplyOnlyAfterSwitchHasNoBlock(t *testing.T) {
	m := newCompactParityModel(t)
	m.AddUserMessage("request")
	m.AppendReasoning("root", "parent work")
	m.AppendToLastMessage("child", "child reply")
	ordinary := ansi.Strip(m.View())
	require.NotContains(t, ordinary, "─►")
	m.SetCompactWorkLog(true)
	m.SetWorkStatus(true, 0)
	requireWorkBefore(t, m, "Work", "─►", "child reply")
	require.Len(t, m.work.ranges, 1)
	require.NotContains(t, ansi.Strip(m.View()), "Working")
	m.SetCompactWorkLog(false)
	require.Equal(t, ordinary, ansi.Strip(m.View()))
}
