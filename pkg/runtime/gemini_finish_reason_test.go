package runtime

import (
	"context"
	"io"
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
	"github.com/docker/docker-agent/pkg/model/provider/gemini"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/testutil/geminiintegration"
	"github.com/docker/docker-agent/pkg/tools"
)

// TestRunStream_GeminiTerminalFinishReasons drives a real Gemini client
// against a scripted GenerateContent stream through LocalRuntime: MAX_TOKENS
// must end the turn as length, and a SAFETY stop after a function call must
// end it as refusal without running the tool.
func TestRunStream_GeminiTerminalFinishReasons(t *testing.T) {
	geminiintegration.Require(t)
	t.Parallel()

	const (
		maxTokens = `data: {"candidates":[{"content":{"parts":[{"text":"partial"}],"role":"model"},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}` + "\n\n"
		callThen  = `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}],"role":"model"}}]}` + "\n\n" +
			`data: {"candidates":[{"finishReason":"SAFETY"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}` + "\n\n"
	)

	tests := []struct {
		name string
		sse  string
		want chat.FinishReason
	}{
		{"max tokens", maxTokens, chat.FinishReasonLength},
		{"safety after a function call", callThen, chat.FinishReasonRefusal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tt.sse)
			}))
			t.Cleanup(srv.Close)

			cfg := &latest.ModelConfig{Provider: "google", Model: "gemini-3.8-flash", BaseURL: srv.URL}
			client, err := gemini.NewClient(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"GOOGLE_API_KEY": "test-key"}))
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
			for ev := range rt.RunStream(t.Context(), sess) {
				switch e := ev.(type) {
				case *ErrorEvent:
					t.Fatalf("unexpected error event: %s", e.Error)
				case *StreamStoppedEvent:
					stopped = e
				}
			}
			require.NotNil(t, stopped)
			assert.Equal(t, tt.want, stopped.FinishReason)
			assert.Zero(t, toolRuns.Load(), "a truncated or refused turn must not run tools")
		})
	}
}
