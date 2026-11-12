package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/codemode"
)

func TestCodeModeToolCallEvents(t *testing.T) {
	t.Parallel()

	for _, outcome := range []string{"success", "error", "error_result", "nil"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			inner := tools.Tool{
				Name:       "echo",
				Category:   "test",
				Parameters: map[string]any{"type": "object"},
				Handler: func(ctx context.Context, tc tools.ToolCall, rt tools.Runtime) (*tools.ToolCallResult, error) {
					rt.EmitOutput(ctx, "working")
					switch outcome {
					case "error":
						return nil, errors.New("failed")
					case "error_result":
						return tools.ResultError("failed"), nil
					case "nil":
						return nil, nil
					default:
						return tools.ResultSuccess(tc.Function.Arguments), nil
					}
				},
			}
			wrapped := codemode.Wrap(newStubToolSet(nil, []tools.Tool{inner}, nil))
			root := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/codemode", stream: &mockStream{}}), agent.WithToolSets(wrapped))
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithCurrentAgent("root"), WithModelStore(mockModelStore{}))
			require.NoError(t, err)
			available, err := wrapped.Tools(t.Context())
			require.NoError(t, err)
			sess := session.New()
			sess.ToolsApproved = true
			args, err := json.Marshal(codemode.RunToolsWithJavascriptArgs{
				Script: `return JSON.stringify(await Promise.allSettled([Echo({value: "a", omitted: null}), echo({value: "b"})]));`,
			})
			require.NoError(t, err)
			events := make(chan Event, 32)
			rt.processToolCalls(t.Context(), sess, root, []tools.ToolCall{{
				ID: "outer", Type: "function",
				Function: tools.FunctionCall{Name: "run_tools_with_javascript", Arguments: string(args)},
			}}, available, NewChannelSink(events))
			close(events)

			calls := make(map[string]*ToolCallEvent)
			outputs := make(map[string]*ToolCallOutputEvent)
			responses := make(map[string]*ToolCallResponseEvent)
			for event := range events {
				switch ev := event.(type) {
				case *ToolCallEvent:
					assert.NotEmpty(t, ev.ToolCall.ID)
					assert.NotContains(t, calls, ev.ToolCall.ID)
					calls[ev.ToolCall.ID] = ev
				case *ToolCallOutputEvent:
					assert.Contains(t, calls, ev.ToolCallID)
					outputs[ev.ToolCallID] = ev
				case *ToolCallResponseEvent:
					assert.Contains(t, calls, ev.ToolCallID)
					responses[ev.ToolCallID] = ev
				}
			}
			require.Len(t, calls, 3)
			require.Len(t, outputs, 2)
			require.Len(t, responses, 3)
			for id, call := range calls {
				if id == "outer" {
					assert.False(t, responses[id].Result.IsError)
					if outcome == "error" {
						assert.Contains(t, responses[id].Response, "rejected")
					} else {
						assert.Contains(t, responses[id].Response, "fulfilled")
					}
					continue
				}
				assert.Equal(t, "function", string(call.ToolCall.Type))
				assert.Equal(t, "echo", call.ToolCall.Function.Name)
				assert.Equal(t, "test", call.ToolDefinition.Category)
				assert.NotContains(t, call.ToolCall.Function.Arguments, "omitted")
				require.Contains(t, outputs, id)
				assert.Equal(t, "echo", outputs[id].ToolDefinition.Name)
				assert.Equal(t, "working", outputs[id].Output)
				require.Contains(t, responses, id)
				assert.Equal(t, "echo", responses[id].ToolDefinition.Name)
				assert.Equal(t, outcome == "error" || outcome == "error_result", responses[id].Result.IsError)
				switch outcome {
				case "error", "error_result":
					assert.Equal(t, "failed", responses[id].Response)
				case "nil":
					assert.Empty(t, responses[id].Response)
				default:
					assert.JSONEq(t, call.ToolCall.Function.Arguments, responses[id].Response)
				}
			}
			// Nested calls are UI-only; only the outer response belongs in model history.
			require.Len(t, sess.GetAllMessages(), 1)
		})
	}
}
