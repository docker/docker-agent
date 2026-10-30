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

// TestRunStream_OpenAIRefusal drives a real *openai.Client through
// LocalRuntime on both OpenAI APIs. A model refusal arrives in a dedicated
// refusal field, not as content; it must reach the user as the answer and end
// the turn as a refusal rather than an empty stop.
func TestRunStream_OpenAIRefusal(t *testing.T) {
	t.Parallel()

	const refusal = "I can't help with that."
	chat1 := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","refusal":"I can't "},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"refusal":"help with that."},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}` + "\n\n" +
		"data: [DONE]\n\n"
	responses := `event: response.created` + "\n" + `data: {"type":"response.created","sequence_number":0,"response":{"id":"r1","object":"response","status":"in_progress","output":[]}}` + "\n\n" +
		`event: response.output_item.added` + "\n" + `data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[]}}` + "\n\n" +
		`event: response.content_part.added` + "\n" + `data: {"type":"response.content_part.added","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}` + "\n\n" +
		`event: response.refusal.delta` + "\n" + `data: {"type":"response.refusal.delta","sequence_number":3,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"I can't "}` + "\n\n" +
		`event: response.refusal.delta` + "\n" + `data: {"type":"response.refusal.delta","sequence_number":4,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"help with that."}` + "\n\n" +
		`event: response.refusal.done` + "\n" + `data: {"type":"response.refusal.done","sequence_number":5,"item_id":"msg_1","output_index":0,"content_index":0,"refusal":"I can't help with that."}` + "\n\n" +
		`event: response.output_item.done` + "\n" + `data: {"type":"response.output_item.done","sequence_number":6,"output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"I can't help with that."}]}}` + "\n\n" +
		`event: response.completed` + "\n" + `data: {"type":"response.completed","sequence_number":7,"response":{"id":"r1","object":"response","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"I can't help with that."}]}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}` + "\n\n"

	tests := []struct {
		name    string
		apiType string
		sse     string
	}{
		{"chat completions", "openai_chatcompletions", chat1},
		{"responses", "openai_responses", responses},
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
				ProviderOpts: map[string]any{"api_type": tt.apiType},
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
			var shown string
			for ev := range rt.RunStream(t.Context(), sess) {
				switch e := ev.(type) {
				case *ErrorEvent:
					t.Fatalf("unexpected error event: %s", e.Error)
				case *StreamStoppedEvent:
					stopped = e
				case *AgentChoiceEvent:
					shown += e.Content
				}
			}

			require.NotNil(t, stopped)
			assert.Equal(t, chat.FinishReasonRefusal, stopped.FinishReason)
			assert.Equal(t, refusal, shown, "the refusal text must reach the user")
			msgs := sess.GetAllMessages()
			require.NotEmpty(t, msgs)
			assert.Equal(t, refusal, msgs[len(msgs)-1].Message.Content)
		})
	}
}
