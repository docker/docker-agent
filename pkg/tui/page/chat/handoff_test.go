package chat

import (
	"strings"
	"testing"
	"testing/synctest"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	chatapi "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
)

func TestRoutedHandoffLiveAndRestored(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, phase, action string }{
		{"forced", "force_handoff", "force_handoff"},
		{"forwarded child", "force_handoff", "force_handoff"},
		{"completion hook", string(hooks.EventAfterAgentComplete), hooks.TransitionActionRoute},
		{"entry hook", string(hooks.EventBeforeAgentRun), hooks.TransitionActionRoute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sess := newContentTestPage(t)
			send := func(event runtime.Event) { _, _ = p.handleRuntimeEvent(event) }
			frame := func() string { return strings.Join(strings.Fields(ansi.Strip(p.messages.View())), " ") }
			if tc.name == "forwarded child" {
				send(runtime.StreamStarted(sess.ID, "parent"))
				sess = session.New(session.WithParentID(sess.ID))
			}
			send(runtime.StreamStarted(sess.ID, "writer"))
			call := tools.ToolCall{ID: "explicit", Function: tools.FunctionCall{Name: "handoff", Arguments: `{"agent":"editor"}`}}
			def := tools.Tool{Name: "handoff"}
			send(runtime.ToolCall(call, def, "writer"))
			send(runtime.ToolCallResponse(call.ID, def, tools.ResultSuccess("done"), "done", "writer"))
			sess.AddMessage(session.NewAgentMessage("writer", &chatapi.Message{Role: chatapi.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}}))
			sess.AddMessage(session.NewAgentMessage("writer", &chatapi.Message{Role: chatapi.MessageRoleTool, ToolCallID: call.ID, Content: "done"}))

			d := &session.RoutingDecision{ID: "routed", Phase: tc.phase, Action: tc.action, FromAgent: "editor", ToAgent: "reviewer"}
			sess.AddRoutingDecision(d)
			send(&runtime.RoutingDecisionEvent{SessionID: sess.ID, Decision: d})
			send(&runtime.AgentRouteEvent{SessionID: sess.ID, Phase: d.Phase, FromAgent: d.FromAgent, ToAgent: d.ToAgent})
			const explicit, routed, reply = "writer ─► editor", "editor ─► reviewer", "Final answer."
			require.Contains(t, frame(), routed, "the route is visible before any target output")
			send(runtime.AgentChoice("reviewer", sess.ID, reply))
			sess.AddMessage(session.NewAgentMessage("reviewer", &chatapi.Message{Role: chatapi.MessageRoleAssistant, Content: reply}))
			check := func() {
				t.Helper()
				out := frame()
				require.Equal(t, 2, strings.Count(out, "─►"), out)
				require.Contains(t, out, explicit)
				require.Contains(t, out, reply)
				require.Less(t, strings.Index(out, explicit), strings.Index(out, routed))
				require.Less(t, strings.Index(out, routed), strings.Index(out, reply))
			}
			check()
			p.messages.LoadFromSession(sess, nil)
			check()
		})
	}
}

func TestEntryHandoffAfterCancelAndResubmit(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p, sess := newContentTestPage(t)
		p.SetInterruptMode(msgtypes.InterruptModeNone)
		route := func(to string) {
			_, _ = p.Update(&runtime.RoutingDecisionEvent{SessionID: sess.ID, Decision: &session.RoutingDecision{
				ID: to, Phase: string(hooks.EventBeforeAgentRun), Action: hooks.TransitionActionRoute, FromAgent: "root", ToAgent: to,
			}})
		}
		_, _ = p.Update(msgtypes.SendMsg{Content: "first"})
		synctest.Wait()
		_, _ = p.Update(runtime.StreamStarted(sess.ID, "root"))
		_, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
		route("old-target")
		require.NotContains(t, ansi.Strip(p.messages.View()), "old-target")

		_, _ = p.Update(msgtypes.SendMsg{Content: "second"})
		_, _ = p.Update(runtime.AgentChoice("root", sess.ID, "cancelled reply"))
		route("new-target")
		frame := strings.Join(strings.Fields(ansi.Strip(p.messages.View())), " ")
		require.Contains(t, frame, "root ─► new-target", "entry routing precedes StreamStarted")
		require.NotContains(t, frame, "cancelled reply")
	})
}
