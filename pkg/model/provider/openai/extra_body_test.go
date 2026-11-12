package openai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/rag/types"
)

func TestExtraBodyRequests(t *testing.T) {
	t.Parallel()

	for _, api := range []string{"chat", "rerank", "responses"} {
		for _, extraBody := range []string{
			"{}",
			"{chat_template_kwargs: {enable_thinking: false}}",
			"{reasoning_effort: none}",
			"{thinking: {type: disabled}}",
			"{top_k: 10, seed: 7, temperature: 0, max_tokens: 128}",
		} {
			t.Run(api+"/"+extraBody, func(t *testing.T) {
				t.Parallel()

				captured := make(chan map[string]any, 2)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
						return
					}
					captured <- body
					switch api {
					case "chat":
						assert.Equal(t, "/chat/completions", r.URL.Path)
						writeSSEResponse(w)
					case "rerank":
						assert.Equal(t, "/chat/completions", r.URL.Path)
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"{\"scores\":[0.9]}"}}]}`)
					case "responses":
						assert.Equal(t, "/responses", r.URL.Path)
						writeResponsesSSEResponse(w)
					}
				}))
				defer server.Close()

				var cfg latest.ModelConfig
				require.NoError(t, yaml.Unmarshal([]byte(fmt.Sprintf(`
provider: openai
model: qwen3
max_tokens: 512
temperature: 0.7
provider_opts:
  api_type: openai_chatcompletions
  transport: sse
  top_k: 40.0
  min_p: 0.05
  seed: 42.0
  extra_body: %s
`, extraBody)), &cfg))
				cfg.BaseURL = server.URL
				if api == "responses" {
					cfg.ProviderOpts["api_type"] = "openai_responses"
				}
				original, err := json.Marshal(cfg)
				require.NoError(t, err)
				client, err := NewClient(t.Context(), &cfg, environment.NewMapEnvProvider(nil), options.WithNoThinking())
				require.NoError(t, err)
				defer client.Close()

				for range 2 {
					if api == "rerank" {
						scores, err := client.Rerank(t.Context(), "hello", []types.Document{{Content: "hello"}}, "")
						require.NoError(t, err)
						assert.Equal(t, []float64{0.9}, scores)
					} else {
						stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
						require.NoError(t, err)
						for {
							if _, err := stream.Recv(); err != nil {
								require.ErrorIs(t, err, io.EOF)
								break
							}
						}
						stream.Close()
					}

					var body map[string]any
					select {
					case body = <-captured:
					default:
						t.Fatal("no request received")
					}
					assert.NotContains(t, body, "extra_body")
					assert.NotContains(t, body, "api_type")
					assert.NotContains(t, body, "transport")
					assert.Equal(t, "qwen3", body["model"])
					if api == "responses" {
						for _, key := range []string{"chat_template_kwargs", "thinking", "reasoning_effort", "top_k", "seed", "max_tokens"} {
							assert.NotContains(t, body, key)
						}
					} else {
						want := map[string]any{"top_k": float64(40), "min_p": 0.05, "seed": float64(42), "temperature": 0.7}
						extraJSON, err := json.Marshal(cfg.ProviderOpts["extra_body"])
						require.NoError(t, err)
						require.NoError(t, json.Unmarshal(extraJSON, &want))
						for key, value := range want {
							assert.Equal(t, value, body[key], key)
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
		Provider: "openai", Model: "qwen3",
		ProviderOpts: map[string]any{"extra_body": "not an object", "api_type": "openai_chatcompletions"},
	}
	client, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(nil))
	require.NoError(t, err)
	defer client.Close()
	_, err = client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
	require.EqualError(t, err, "provider_opts.extra_body must be an object")
	_, err = client.Rerank(t.Context(), "hello", []types.Document{{Content: "hello"}}, "")
	require.EqualError(t, err, "provider_opts.extra_body must be an object")
}

func TestExtraBodyOverridesNoThinking(t *testing.T) {
	t.Parallel()

	server, body := captureRequestBody(t)
	cfg := &latest.ModelConfig{
		Provider: "openai", Model: "gpt-5.2", BaseURL: server.URL,
		MaxTokens: new(int64(20)),
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
			"extra_body": map[string]any{
				"reasoning_effort":      "medium",
				"max_completion_tokens": 128,
			},
		},
	}
	client, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(nil), options.WithNoThinking())
	require.NoError(t, err)
	defer client.Close()

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err != nil {
			require.ErrorIs(t, err, io.EOF)
			break
		}
	}

	var request map[string]any
	require.NoError(t, json.Unmarshal(body(), &request))
	assert.Equal(t, "medium", request["reasoning_effort"])
	assert.EqualValues(t, 128, request["max_completion_tokens"])
	assert.NotContains(t, request, "max_tokens")
}
