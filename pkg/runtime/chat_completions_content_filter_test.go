package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// TestRunStream_ChatCompletionsContentFilter drives a real *openai.Client on
// the Chat Completions API through LocalRuntime. A content_filter finish
// (Azure OpenAI, OpenAI moderation) must end the turn as a refusal, as the
// Responses API path already does: the tool call streamed before it must not
// run, and the usage chunk that follows must still be recorded.
func TestRunStream_ChatCompletionsContentFilter(t *testing.T) {
	t.Parallel()

	const (
		toolCall  = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":null}]}` + "\n\n"
		text      = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}` + "\n\n"
		filtered  = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}` + "\n\n"
		usageOnly = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}` + "\n\n"
		done      = "data: [DONE]\n\n"
	)

	tests := []struct {
		name       string
		sse        string
		wantOutput int64
	}{
		{"after a tool call, with usage chunk", toolCall + filtered + usageOnly + done, 3},
		{"after text, with usage chunk", text + filtered + usageOnly + done, 3},
		{"after a tool call, no usage chunk", toolCall + filtered + done, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var requests atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(tt.sse))
			}))
			t.Cleanup(srv.Close)

			cfg := latest.ModelConfig{
				Provider:     "openai",
				Model:        "gpt-4o",
				BaseURL:      srv.URL,
				ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"},
			}
			client, err := openai.NewClient(t.Context(), &cfg,
				environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test-key"}))
			require.NoError(t, err)

			var toolRuns atomic.Int64
			lookup := tools.Tool{
				Name:       "lookup",
				Parameters: map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
				Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
					toolRuns.Add(1)
					return tools.ResultSuccess("ok"), nil
				},
			}
			root := agent.New("root", "test", agent.WithModel(client), agent.WithToolSets(newStubToolSet(nil, []tools.Tool{lookup}, nil)))
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
			require.NoError(t, err)

			sess := session.New(session.WithUserMessage("hi"), session.WithToolsApproved(true))
			sess.NonInteractive = true

			var stopped *StreamStoppedEvent
			var last *MessageUsage
			for ev := range rt.RunStream(t.Context(), sess) {
				switch e := ev.(type) {
				case *ErrorEvent:
					t.Fatalf("unexpected error event: %s", e.Error)
				case *StreamStoppedEvent:
					stopped = e
				case *TokenUsageEvent:
					if e.Usage != nil && e.Usage.LastMessage != nil {
						last = e.Usage.LastMessage
					}
				}
			}

			require.NotNil(t, stopped)
			assert.Equal(t, chat.FinishReasonRefusal, stopped.FinishReason)
			assert.Zero(t, toolRuns.Load(), "a filtered turn must not run its tool calls")
			assert.Equal(t, int64(1), requests.Load(), "a refusal ends the run")
			if tt.wantOutput > 0 {
				require.NotNil(t, last)
				assert.Equal(t, tt.wantOutput, last.OutputTokens)
			}
		})
	}
}
