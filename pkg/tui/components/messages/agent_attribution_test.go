package messages

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	handoff "github.com/docker/docker-agent/pkg/tools/builtin/handoff/types"
	transfertask "github.com/docker/docker-agent/pkg/tools/builtin/transfertask/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/components/tool/defaults"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestTranscriptAttributesHiddenAgentBoundary(t *testing.T) {
	t.Parallel()

	const (
		facts         = "These are the extracted facts."
		summary       = "This is the final summary."
		callerThought = "Extracting facts."
		childThought  = "Preparing the summary."
	)
	for _, tc := range []struct {
		name string
		load func(*model)
	}{
		{"streamed", func(m *model) {
			m.AddUserMessage("Summarize this.")
			m.AppendAssistantContent("session", "facts", "root", facts)
			m.AppendReasoningContent("session", "facts", "root", callerThought)
			assert.NotContains(t, ansi.Strip(m.View()), "root", "a single agent's reply has no label")
			m.AddOrUpdateToolCall("background", tools.ToolCall{ID: "job", Function: tools.FunctionCall{Name: "check_job"}}, tools.Tool{}, types.ToolStatusCompleted)
			m.AppendReasoningContent("session", "summary", "summarizer", childThought)
			out := ansi.Strip(m.View())
			assertInOrder(t, out, callerThought, "→", childThought)
			assert.NotContains(t, out, summary)
			m.AppendAssistantContent("session", "summary", "summarizer", "This is ")
			m.View()
			m.AppendAssistantContent("session", "summary", "summarizer", "the final summary.")
		}},
		{"reloaded", func(m *model) {
			m.LoadFromSession(&session.Session{ID: "session", Messages: []session.Item{
				session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "Summarize this."}}),
				session.NewMessageItem(&session.Message{AgentName: "root", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: facts, ReasoningContent: callerThought}}),
				session.NewMessageItem(session.ImplicitUserMessage("The conversation was automatically handed off to you.")),
				session.NewMessageItem(&session.Message{AgentName: "summarizer", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: summary, ReasoningContent: childThought}}),
			}}, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
			tc.load(m)

			out := ansi.Strip(m.View())
			assert.NotContains(t, out, "automatically handed off")
			assertInOrder(t, out, facts, "→")
			assert.Regexp(t, `root\s*→\s*summarizer`, out)
			assert.Equal(t, 1, strings.Count(out, "root"))
			assert.Equal(t, 1, strings.Count(out, "summarizer"))
			assertInOrder(t, out, callerThought, "→", childThought, summary)
			assert.Equal(t, 2, strings.Count(out, "Thinking"))
		})
	}
}

func TestTranscriptDoesNotDuplicateVisibleHandoff(t *testing.T) {
	t.Parallel()

	tc := tools.ToolCall{ID: "handoff", Function: tools.FunctionCall{Name: handoff.ToolNameHandoff, Arguments: `{"agent":"summarizer"}`}}
	for _, path := range []string{"streamed", "reloaded"} {
		t.Run(path, func(t *testing.T) {
			m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
			m.SetToolRenderers(defaults.NewRegistry())
			if path == "streamed" {
				m.AppendAssistantContent("session", "facts", "root", "These are the extracted facts.")
				m.AddOrUpdateToolCall("root", tc, tools.Tool{Name: handoff.ToolNameHandoff}, types.ToolStatusCompleted)
				m.AppendAssistantContent("session", "summary", "summarizer", "This is the final summary.")
			} else {
				m.LoadFromSession(&session.Session{ID: "session", Messages: []session.Item{
					session.NewMessageItem(&session.Message{AgentName: "root", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "These are the extracted facts.", ToolCalls: []tools.ToolCall{tc}}}),
					session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: tc.ID, Content: "Handed off."}}),
					session.NewMessageItem(&session.Message{AgentName: "summarizer", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "This is the final summary."}}),
				}}, nil)
			}

			out := ansi.Strip(m.View())
			assert.Contains(t, out, "─►")
			assert.Equal(t, 1, strings.Count(out, "root"))
			assert.Equal(t, 1, strings.Count(out, "summarizer"))
		})
	}
}

func TestTranscriptAttributesAfterVisibleReturnWithoutChildReply(t *testing.T) {
	t.Parallel()

	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
	m.SetToolRenderers(defaults.NewRegistry())
	call := tools.ToolCall{ID: "transfer", Function: tools.FunctionCall{Name: transfertask.ToolNameTransferTask, Arguments: `{"agent":"summarizer","task":"Check the facts."}`}}
	m.AppendAssistantContent("session", "facts", "root", "Here are the facts.")
	m.AddOrUpdateToolCall("root", call, tools.Tool{}, types.ToolStatusRunning)
	m.AddAgentReturn("summarizer", "root")
	m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: call.ID, Result: tools.ResultSuccess("")}, types.ToolStatusCompleted)
	m.AppendAssistantContent("session", "summary", "summarizer", "Here is my summary.")

	out := ansi.Strip(m.View())
	assertInOrder(t, out, "Here are the facts.", "calls", types.AgentReturnLabel, "→", "Here is my summary.")
	assert.Regexp(t, `root\s*→\s*summarizer`, out)
	assert.Equal(t, 1, strings.Count(out, "→"))
}

func TestTranscriptFollowsConnectedToolOnlyHandoffs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, secondCaller, inferred string
	}{
		{"connected", "router", ""},
		{"unrelated", "other", `router\s*→\s*summarizer`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
			m.SetToolRenderers(defaults.NewRegistry())
			m.AppendAssistantContent("session", "facts", "root", "Here are the facts.")
			first := tools.ToolCall{ID: "first", Function: tools.FunctionCall{Name: handoff.ToolNameHandoff, Arguments: `{"agent":"router"}`}}
			second := tools.ToolCall{ID: "second", Function: tools.FunctionCall{Name: handoff.ToolNameHandoff, Arguments: `{"agent":"summarizer"}`}}
			m.AddOrUpdateToolCall("root", first, tools.Tool{}, types.ToolStatusCompleted)
			m.AddOrUpdateToolCall(tc.secondCaller, second, tools.Tool{}, types.ToolStatusCompleted)
			m.AppendAssistantContent("session", "summary", "summarizer", "Here is my summary.")

			out := ansi.Strip(m.View())
			assertInOrder(t, out, "Here are the facts.", "─►", "─►", "Here is my summary.")
			assert.Equal(t, 2, strings.Count(out, "─►"))
			if tc.inferred == "" {
				assert.NotContains(t, out, "→")
			} else {
				assert.Regexp(t, tc.inferred, out)
				assert.Equal(t, 1, strings.Count(out, "→"))
			}
		})
	}
}

func TestTranscriptAttributesFirstTurnAfterToolOnlyHandoff(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"streamed", "reloaded"} {
		t.Run(path, func(t *testing.T) {
			m := NewScrollableView(animation.NewRuntime(), 80, 35, &service.SessionState{}).(*model)
			m.SetToolRenderers(defaults.NewRegistry())
			call := tools.ToolCall{ID: "handoff", Function: tools.FunctionCall{Name: handoff.ToolNameHandoff, Arguments: `{"agent":"router"}`}}
			result := tools.ResultSuccess("Handed off to router.")
			media := []types.AssistantMedia{{Fallback: "Generated summary image."}}
			if path == "streamed" {
				m.AddUserMessage("Summarize this.")
				m.AddOrUpdateToolCall("root", call, tools.Tool{}, types.ToolStatusRunning)
				m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: call.ID, Result: result}, types.ToolStatusCompleted)
				m.AppendReasoningContent("session", "summary", "summarizer", "Preparing the summary.")
				m.AppendAssistantMediaContent("session", "summary", "summarizer", media)
				m.AddUserMessage("New turn.")
				m.AppendAssistantContent("session", "next", "root", "Starting over.")
			} else {
				m.LoadFromSession(&session.Session{ID: "session", Messages: []session.Item{
					session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "Summarize this."}}),
					session.NewMessageItem(&session.Message{AgentName: "root", Message: chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}}}),
					session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: call.ID, Content: result.Output}}),
					session.NewMessageItem(session.ImplicitUserMessage("The conversation was automatically handed off to you.")),
					session.NewMessageItem(&session.Message{AgentName: "summarizer", Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "Preparing the summary."}}),
					session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "New turn."}}),
					session.NewMessageItem(&session.Message{AgentName: "root", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "Starting over."}}),
				}}, map[int][]types.AssistantMedia{4: media})
			}

			out := ansi.Strip(m.View())
			assert.Regexp(t, `router\s*→\s*summarizer`, out)
			assert.Equal(t, 1, strings.Count(out, "→"))
			assert.Equal(t, 1, strings.Count(out, "─►"))
			assert.NotContains(t, out, "automatically handed off")
			assertInOrder(t, out, "Summarize this.", "─►", "→", "Preparing the summary.", "Generated summary image.", "New turn.", "Starting over.")
		})
	}
}

func TestTranscriptAttributesToolOutcomes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, tool, replyAgent, wantCue string
		status                          types.ToolStatus
	}{
		{"rejected handoff", handoff.ToolNameHandoff, "root", "", types.ToolStatusError},
		{"errored transfer", transfertask.ToolNameTransferTask, "root", "", types.ToolStatusError},
		{"hidden switch after rejection", handoff.ToolNameHandoff, "summarizer", `root\s*→\s*summarizer`, types.ToolStatusError},
		{"completed handoff", handoff.ToolNameHandoff, "reviewer", "", types.ToolStatusCompleted},
		{"completed transfer without child transcript", transfertask.ToolNameTransferTask, "root", "", types.ToolStatusCompleted},
		{"unconfirmed handoff", handoff.ToolNameHandoff, "root", "", types.ToolStatusPending},
		{"saved or pending transfer", transfertask.ToolNameTransferTask, "root", "", types.ToolStatusPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{"streamed", "reloaded"} {
				t.Run(path, func(t *testing.T) {
					m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
					t.Cleanup(m.StopAnimations)
					m.SetToolRenderers(defaults.NewRegistry())
					call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: tc.tool, Arguments: `{"agent":"reviewer","task":"Check the facts."}`}}
					result := &tools.ToolCallResult{Output: "Work finished."}
					if tc.status == types.ToolStatusError {
						result.Output, result.IsError = "Tool call failed.", true
					}
					if path == "streamed" {
						m.AppendAssistantContent("session", "caller", "root", "Here are the facts.")
						m.AddOrUpdateToolCall("root", call, tools.Tool{}, types.ToolStatusPending)
						if tc.status != types.ToolStatusPending {
							m.AddOrUpdateToolCall("root", call, tools.Tool{}, types.ToolStatusConfirmation)
							m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: call.ID, Response: result.Output, Result: result}, tc.status)
						}
						m.AppendAssistantContent("session", "next", tc.replyAgent, "Continuing the work.")
					} else {
						sess := &session.Session{ID: "session", Messages: []session.Item{
							session.NewMessageItem(&session.Message{AgentName: "root", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "Here are the facts.", ToolCalls: []tools.ToolCall{call}}}),
						}}
						if tc.status != types.ToolStatusPending {
							sess.Messages = append(sess.Messages, session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: call.ID, Content: result.Output, IsError: result.IsError}}))
						}
						sess.Messages = append(sess.Messages, session.NewMessageItem(&session.Message{AgentName: tc.replyAgent, Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "Continuing the work."}}))
						m.LoadFromSession(sess, nil)
					}

					out := ansi.Strip(m.View())
					assertInOrder(t, out, "Here are the facts.", "reviewer", "Continuing the work.")
					if tc.wantCue == "" {
						assert.NotContains(t, out, "→")
					} else {
						assert.Regexp(t, tc.wantCue, out)
						assert.Equal(t, 1, strings.Count(out, "→"))
					}
					if tc.status == types.ToolStatusError {
						assert.Contains(t, out, "✗")
						assert.NotContains(t, out, "─►")
					}
				})
			}
		})
	}
}

func TestTranscriptShowsTransitionToolsOutsideThinking(t *testing.T) {
	t.Parallel()

	const (
		callerThought = "Planning this work."
		childThought  = "Preparing the summary."
		reply         = "This is the final summary."
	)
	for _, tt := range []struct {
		name, arguments, attempt, completed string
	}{
		{handoff.ToolNameHandoff, `{"agent":"summarizer"}`, "handoff to", "─►"},
		{transfertask.ToolNameTransferTask, `{"agent":"summarizer","task":"Summarize facts."}`, "calls", "calls"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			call := tools.ToolCall{ID: "transition", Function: tools.FunctionCall{Name: tt.name, Arguments: tt.arguments}}
			ordinary := tools.ToolCall{ID: "ordinary", Function: tools.FunctionCall{Name: "check_facts"}}
			ordinaryResult := &tools.ToolCallResult{Output: "Facts checked."}
			result := &tools.ToolCallResult{Output: "Work returned."}
			newModel := func(ar *animation.Runtime) *model {
				m := NewScrollableView(ar, 80, 40, &service.SessionState{}).(*model)
				m.SetToolRenderers(defaults.NewRegistry())
				return m
			}
			assertRow := func(t *testing.T, m *model, connector string) string {
				t.Helper()
				out := ansi.Strip(m.View())
				assert.Equal(t, 1, strings.Count(out, connector))
				assert.NotContains(t, out, "→")
				assert.Equal(t, 1, m.MessageTypeCount(types.MessageTypeToolCall))
				assertInOrder(t, out, callerThought, connector)
				return out
			}
			assertDisclosure := func(t *testing.T, m *model, connector string) {
				t.Helper()
				block := m.views[0].(*reasoningblock.Model)
				require.False(t, block.IsExpanded())
				assert.NotContains(t, assertRow(t, m, connector), "check_facts")
				m.handleMouseClick(tea.MouseClickMsg{X: 4, Y: m.lineOffsets[0], Button: tea.MouseLeft})
				require.True(t, block.IsExpanded())
				assertInOrder(t, assertRow(t, m, connector), "check_facts", connector)
				m.handleMouseClick(tea.MouseClickMsg{X: 4, Y: m.lineOffsets[0], Button: tea.MouseLeft})
				require.False(t, block.IsExpanded())
				assert.NotContains(t, assertRow(t, m, connector), "check_facts")
			}
			t.Run("streamed", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ar := animation.NewRuntime()
					defer ar.Stop()
					m := newModel(ar)
					m.AppendReasoningContent("session", "facts", "root", callerThought)
					partial := call
					partial.Function.Arguments = ""
					tickCmd := m.AddOrUpdateToolCall("root", partial, tools.Tool{}, types.ToolStatusPending)
					require.NotNil(t, tickCmd)
					assertInOrder(t, ansi.Strip(m.View()), callerThought, tt.name)
					assert.Equal(t, 1, strings.Count(ansi.Strip(m.View()), tt.name))
					m.AddOrUpdateToolCall("root", call, tools.Tool{}, types.ToolStatusPending)
					m.AddOrUpdateToolCall("root", call, tools.Tool{}, types.ToolStatusRunning)
					m.AddOrUpdateToolCall("root", ordinary, tools.Tool{Name: ordinary.Function.Name}, types.ToolStatusRunning)
					m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: ordinary.ID, Result: ordinaryResult}, types.ToolStatusCompleted)
					connector := tt.attempt
					out := assertRow(t, m, connector)
					assert.NotContains(t, out, reply)
					assert.NotContains(t, out, "✓ Summarize facts.")
					assert.Contains(t, out, "check_facts")

					if tt.name == handoff.ToolNameHandoff {
						m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: call.ID, Result: result}, types.ToolStatusCompleted)
						connector = tt.completed
					}
					//nolint:forbidigo // Advance the synctest clock past the ordinary tool's fade.
					time.Sleep(3 * time.Second)
					tick, ok := ar.Accept(tickCmd().(animation.TickMsg))
					require.True(t, ok)
					m.Update(tick)
					assertDisclosure(t, m, connector)

					m.AppendReasoningContent("session", "summary", "summarizer", childThought)
					out = assertRow(t, m, connector)
					assertInOrder(t, out, callerThought, connector, childThought)
					assert.NotContains(t, out, reply)
					m.AppendAssistantContent("session", "summary", "summarizer", reply)
					out = assertRow(t, m, connector)
					assertInOrder(t, out, callerThought, connector, childThought, reply)
					assert.Equal(t, 2, strings.Count(out, "Thinking"))
					if tt.name == transfertask.ToolNameTransferTask {
						m.AddAgentReturn("summarizer", "root")
						m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: call.ID, Result: result}, types.ToolStatusCompleted)
						assert.Contains(t, assertRow(t, m, connector), "✓ Summarize facts.")
					}
				})
			})
			t.Run("reloaded", func(t *testing.T) {
				m := newModel(animation.NewRuntime())
				sess := &session.Session{ID: "session", Messages: []session.Item{
					session.NewMessageItem(&session.Message{AgentName: "root", Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: callerThought, ToolCalls: []tools.ToolCall{call, ordinary}, ToolDefinitions: []tools.Tool{{Name: tt.name}, {Name: ordinary.Function.Name}}}}),
					session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: ordinary.ID, Content: ordinaryResult.Output}}),
				}}
				m.LoadFromSession(sess, nil)
				out := assertRow(t, m, tt.attempt)
				assert.NotContains(t, out, reply)
				assert.False(t, m.ar.HasActive(), "a restored call with no result is not running")
				assert.Contains(t, out, "●")
				assertDisclosure(t, m, tt.attempt)

				toolResult := &session.Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: call.ID, Content: "Tool failed.", IsError: true}}
				sess.Messages = append(sess.Messages, session.NewMessageItem(toolResult))
				m.LoadFromSession(sess, nil)
				assert.Contains(t, assertRow(t, m, tt.attempt), "✗")

				toolResult.Message.Content, toolResult.Message.IsError = result.Output, false
				replyAgent := "summarizer"
				if tt.name == transfertask.ToolNameTransferTask {
					// The child's transcript is in its own saved session.
					replyAgent = "root"
				}
				sess.Messages = append(sess.Messages,
					session.NewMessageItem(&session.Message{AgentName: replyAgent, Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: childThought, Content: reply}}),
				)
				m.LoadFromSession(sess, nil)
				out = assertRow(t, m, tt.completed)
				assertInOrder(t, out, callerThought, tt.completed, childThought, reply)
				assert.Equal(t, 2, strings.Count(out, "Thinking"))
				if tt.name == transfertask.ToolNameTransferTask {
					assert.Contains(t, out, "✓ Summarize facts.")
				}
			})
		})
	}
}

func assertInOrder(t *testing.T, out string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		_, rest, found := strings.Cut(out, part)
		require.Truef(t, found, "%q missing or out of order in %q", part, out)
		out = rest
	}
}

func TestTranscriptDoesNotDuplicateVisibleAgentReturn(t *testing.T) {
	t.Parallel()

	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
	m.AppendAssistantContent("child", "facts", "researcher", "These are the extracted facts.")
	m.AddAgentReturn("researcher", "root")
	m.AppendAssistantContent("session", "summary", "root", "This is the final summary.")

	out := ansi.Strip(m.View())
	assert.Contains(t, out, types.AgentReturnLabel)
	assert.Equal(t, 1, strings.Count(out, "researcher"))
	assert.Equal(t, 1, strings.Count(out, "root"))
}
