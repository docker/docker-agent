package dmr

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func TestExtraBodyRequests(t *testing.T) {
	t.Parallel()

	for _, engine := range []string{"", engineLlamaCpp, engineVLLM} {
		for _, override := range []bool{false, true} {
			name := engine + "/merge"
			if override {
				name = engine + "/override"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				captured := make(chan map[string]any, 2)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, "/chat/completions", r.URL.Path)
					var body map[string]any
					if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
						return
					}
					captured <- body
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
				}))
				defer server.Close()

				extraBody := map[string]any{
					"reasoning_effort": "none",
					"thinking":         map[string]any{"type": "disabled"},
				}
				if override {
					extraBody["chat_template_kwargs"] = map[string]any{"vendor_option": false}
					extraBody["thinking_token_budget"] = 123
					extraBody["max_tokens"] = 128
				}
				cfg := &latest.ModelConfig{
					Provider: "dmr", Model: "ai/qwen3", BaseURL: server.URL,
					MaxTokens: new(int64(20)), ThinkingBudget: &latest.ThinkingBudget{Effort: "none"},
					ProviderOpts: map[string]any{"extra_body": extraBody},
				}
				original, err := json.Marshal(cfg)
				require.NoError(t, err)
				client, err := NewClient(t.Context(), cfg, options.WithNoThinking(), options.WithGeneratingTitle())
				require.NoError(t, err)
				client.engine = engine

				for range 2 {
					stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
					require.NoError(t, err)
					for {
						if _, err := stream.Recv(); err != nil {
							require.ErrorIs(t, err, io.EOF)
							break
						}
					}
					stream.Close()

					var body map[string]any
					select {
					case body = <-captured:
					default:
						t.Fatal("no request received")
					}
					assert.NotContains(t, body, "extra_body")
					assert.Equal(t, "ai/qwen3", body["model"])
					assert.Equal(t, "none", body["reasoning_effort"])
					assert.Equal(t, map[string]any{"type": "disabled"}, body["thinking"])
					if override {
						assert.Equal(t, extraBody["chat_template_kwargs"], body["chat_template_kwargs"])
						assert.EqualValues(t, 123, body["thinking_token_budget"])
						assert.EqualValues(t, 128, body["max_tokens"])
					} else {
						assert.Equal(t, map[string]any{"enable_thinking": false}, body["chat_template_kwargs"])
						assert.EqualValues(t, noThinkingMinOutputTokens, body["max_tokens"])
						if engine == engineVLLM {
							assert.EqualValues(t, 0, body["thinking_token_budget"])
						} else {
							assert.NotContains(t, body, "thinking_token_budget")
						}
					}
				}
				after, err := json.Marshal(cfg)
				require.NoError(t, err)
				assert.JSONEq(t, string(original), string(after), "requests must not mutate the config")
			})
		}
	}
}

func TestInvalidExtraBody(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		Provider: "dmr", Model: "ai/qwen3", BaseURL: "http://localhost:1/v1",
		ProviderOpts: map[string]any{"extra_body": "not an object"},
	}
	client, err := NewClient(t.Context(), cfg, options.WithGeneratingTitle())
	require.NoError(t, err)
	_, err = client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
	require.EqualError(t, err, "provider_opts.extra_body must be an object")
}
