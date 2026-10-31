package runtime

import (
	"net/http"
	"net/http/httptest"
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
)

// TestRunStream_ChatCompletionsFinishReason drives a real *openai.Client on
// the Chat Completions API (the path DMR and OpenAI-compatible providers
// use) through LocalRuntime, with track_usage at its default. A provider
// "length" must reach stream_stopped and the turn's usage as length, so
// truncation is reported, and a stream with no usage chunk must still carry
// the provider's reason instead of an inferred one.
func TestRunStream_ChatCompletionsFinishReason(t *testing.T) {
	t.Parallel()

	const (
		content   = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"partial answer"},"finish_reason":null}]}` + "\n\n"
		usageOnly = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}` + "\n\n"
		done      = "data: [DONE]\n\n"
	)
	finish := func(reason string) string {
		return `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"` + reason + `"}]}` + "\n\n"
	}

	tests := []struct {
		name       string
		sse        string
		want       chat.FinishReason
		wantOutput int64
	}{
		{"length with usage chunk", content + finish("length") + usageOnly + done, chat.FinishReasonLength, 3},
		{"length without usage chunk", content + finish("length") + done, chat.FinishReasonLength, 0},
		{"stop with usage chunk", content + finish("stop") + usageOnly + done, chat.FinishReasonStop, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

			root := agent.New("root", "test", agent.WithModel(client))
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
			require.NoError(t, err)

			sess := session.New(session.WithUserMessage("hi"))
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
			assert.Equal(t, tt.want, stopped.FinishReason)
			if tt.wantOutput > 0 {
				require.NotNil(t, last)
				assert.Equal(t, tt.want, last.FinishReason)
				assert.Equal(t, tt.wantOutput, last.OutputTokens)
			}

			msgs := sess.GetAllMessages()
			require.NotEmpty(t, msgs)
			assistant := msgs[len(msgs)-1].Message
			assert.Equal(t, chat.MessageRoleAssistant, assistant.Role)
			assert.Equal(t, "partial answer", assistant.Content)
			assert.Equal(t, tt.want, assistant.FinishReason)
		})
	}
}
