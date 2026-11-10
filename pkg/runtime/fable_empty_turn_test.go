package runtime

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic/anthropictest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestFableEmptyFinalTurnAfterTools(t *testing.T) {
	t.Parallel()

	const toolReply = `event: message_start
data: {"type":"message_start","message":{"id":"msg_tool","type":"message","role":"assistant","model":"claude-fable-5-2","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"record","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`
	for _, tc := range []struct {
		name, thinking, text, warning string
	}{
		{name: "empty", warning: "stopped without a final reply after using tools"},
		{name: "whitespace", text: " \n\t", warning: "stopped without a final reply after using tools"},
		{name: "reasoning only", thinking: "I should continue investigating.", warning: "only reasoning and no reply"},
		{name: "visible reply", text: "All done."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			finalReply := anthropictest.TextReply("msg_final", tc.text, 10)
			if tc.thinking != "" {
				finalReply = anthropictest.ThinkingTextReply("msg_final", tc.thinking, "sig-final", tc.text, 10)
			}
			server := anthropictest.NewServer(t, toolReply, finalReply)
			var calls atomic.Int32
			record := agent.WithTools(tools.Tool{
				Name: "record", Parameters: map[string]any{"type": "object"},
				Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
					calls.Add(1)
					return tools.ResultSuccess("recorded"), nil
				},
			})
			rt := anthropicRuntime(t, server, "claude-fable-5-2", nil, nil, mockModelStore{}, record)
			sess := session.New(session.WithUserMessage("Investigate and report back"),
				session.WithToolsApproved(true), session.WithTitle("Fable empty turn"))
			sess.NonInteractive = true
			events := drainRunStream(t, rt, sess)
			requireNoErrorEvents(t, events)

			var warnings []string
			var stopped *StreamStoppedEvent
			for _, event := range events {
				switch e := event.(type) {
				case *WarningEvent:
					warnings = append(warnings, e.Message)
				case *StreamStoppedEvent:
					stopped = e
				}
			}
			if tc.warning == "" {
				assert.Empty(t, warnings)
			} else {
				require.Len(t, warnings, 1)
				assert.Contains(t, warnings[0], tc.warning)
				assert.Contains(t, warnings[0], "anthropic/claude-fable-5-2")
				assert.Contains(t, warnings[0], "stop reason: stop")
				assert.Contains(t, warnings[0], "Send a follow-up to continue")
				assert.NotContains(t, warnings[0], "rate-limit")
			}
			require.NotNil(t, stopped)
			assert.Equal(t, chat.FinishReasonStop, stopped.FinishReason)
			assert.Len(t, server.Requests(), 2, "do not silently retry completed tool work")
			assert.EqualValues(t, 1, calls.Load())

			messages := sess.GetAllMessages()
			require.NotEmpty(t, messages)
			last := messages[len(messages)-1].Message
			assert.Equal(t, chat.MessageRoleAssistant, last.Role)
			assert.Equal(t, tc.thinking, last.ReasoningContent)
			if tc.warning != "" {
				assert.Empty(t, last.Content)
			} else {
				assert.Equal(t, tc.text, last.Content)
			}
			require.NotNil(t, last.Usage, "empty replies still count towards usage")
			assert.EqualValues(t, 7, last.Usage.OutputTokens)
		})
	}
}
