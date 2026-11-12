package openai

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/rag/types"
)

func TestServiceTier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts map[string]any
		want string
	}{
		{name: "unset"},
		{name: "empty options", opts: map[string]any{}},
		{name: "null", opts: map[string]any{"service_tier": nil}},
		{name: "empty", opts: map[string]any{"service_tier": ""}},
		{name: "number ignored", opts: map[string]any{"service_tier": 1}},
		{name: "boolean ignored", opts: map[string]any{"service_tier": true}},
	}
	for _, tier := range []string{"auto", "default", "flex", "scale", "priority", "fast", "ultrafast", "future-tier"} {
		tests = append(tests, struct {
			name string
			opts map[string]any
			want string
		}{name: tier, opts: map[string]any{"service_tier": tier}, want: tier})
	}

	for _, api := range []string{"chat", "responses", "websocket", "rerank"} {
		t.Run(api, func(t *testing.T) {
			t.Parallel()
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()

					captured := make(chan map[string]any, 1)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var payload map[string]any
						if api == "websocket" {
							assert.Equal(t, "/responses", r.URL.Path)
							upgrader := websocket.Upgrader{}
							conn, err := upgrader.Upgrade(w, r, nil)
							if !assert.NoError(t, err) {
								return
							}
							defer conn.Close()
							if !assert.NoError(t, conn.ReadJSON(&payload)) {
								return
							}
							captured <- payload
							assert.Equal(t, "response.create", payload["type"])
							assert.NoError(t, conn.WriteJSON(completedEvent("resp_service_tier")))
							return
						}

						if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload)) {
							return
						}
						captured <- payload
						switch api {
						case "responses":
							assert.Equal(t, "/responses", r.URL.Path)
							writeResponsesSSEResponse(w)
						case "chat":
							assert.Equal(t, "/chat/completions", r.URL.Path)
							writeSSEResponse(w)
						case "rerank":
							assert.Equal(t, "/chat/completions", r.URL.Path)
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"{\"scores\":[0.9]}"}}]}`)
						}
					}))
					defer server.Close()

					opts := map[string]any{}
					maps.Copy(opts, tt.opts)
					if api == "chat" {
						opts["api_type"] = "openai_chatcompletions"
						// Extra sampling fields must not overwrite the native service tier.
						opts["top_k"] = 40
					}
					if api == "websocket" {
						opts["transport"] = "websocket"
					}
					cfg := &latest.ModelConfig{
						Provider:     "openai",
						Model:        "gpt-4.1",
						BaseURL:      server.URL,
						TokenKey:     "MY_TOKEN",
						ProviderOpts: opts,
					}
					env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})
					client, err := NewClient(t.Context(), cfg, env)
					require.NoError(t, err)
					defer client.Close()

					if api == "rerank" {
						scores, err := client.Rerank(t.Context(), "hello", []types.Document{{Content: "hello"}}, "")
						require.NoError(t, err)
						assert.Equal(t, []float64{0.9}, scores)
					} else {
						stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
							{Role: chat.MessageRoleUser, Content: "hello"},
						}, nil)
						require.NoError(t, err)
						defer stream.Close()
						for {
							if _, err := stream.Recv(); err != nil {
								require.ErrorIs(t, err, io.EOF)
								break
							}
						}
					}

					select {
					case payload := <-captured:
						if tt.want == "" {
							assert.NotContains(t, payload, "service_tier")
						} else {
							assert.Equal(t, tt.want, payload["service_tier"])
						}
						if api == "chat" {
							assert.InDelta(t, 40, payload["top_k"], 0)
						}
					default:
						t.Fatal("no request received")
					}
				})
			}
		})
	}
}

func TestServiceTierUsageTransports(t *testing.T) {
	t.Parallel()

	for _, api := range []string{"chat", "responses", "websocket"} {
		for _, tc := range []struct {
			requested, actual string
		}{
			{requested: "fast", actual: "priority"},
			{requested: "fast", actual: "fast"},
			{requested: "ultrafast", actual: "ultrafast"},
			{requested: "fast", actual: "default"},
			{requested: "ultrafast", actual: "default"},
			{requested: "ultrafast", actual: "fast"},
			{requested: "fast", actual: "ultrafast"},
			{actual: "ultrafast"},
			{actual: "fast"},
			{requested: "fast"},
		} {
			t.Run(api+"/"+tc.requested+"/"+tc.actual, func(t *testing.T) {
				t.Parallel()
				completed := map[string]any{
					"type": "response.completed",
					"response": map[string]any{
						"id": "resp_tier", "service_tier": tc.actual,
						"usage": map[string]any{
							"input_tokens": 100, "output_tokens": 7, "total_tokens": 107,
							"input_tokens_details":  map[string]any{"cached_tokens": 20, "cache_write_tokens": 30},
							"output_tokens_details": map[string]any{"reasoning_tokens": 3},
						},
					},
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var payload map[string]any
					if api == "websocket" {
						conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
						if !assert.NoError(t, err) {
							return
						}
						defer conn.Close()
						if !assert.NoError(t, conn.ReadJSON(&payload)) {
							return
						}
						if tc.requested == "" {
							assert.NotContains(t, payload, "service_tier")
						} else {
							assert.Equal(t, tc.requested, payload["service_tier"])
						}
						assert.NoError(t, conn.WriteJSON(completed))
						return
					}
					if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload)) {
						return
					}
					if tc.requested == "" {
						assert.NotContains(t, payload, "service_tier")
					} else {
						assert.Equal(t, tc.requested, payload["service_tier"])
					}
					w.Header().Set("Content-Type", "text/event-stream")
					var event any = completed
					if api == "chat" {
						event = map[string]any{
							"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "gpt-6-astra",
							"service_tier": tc.actual, "choices": []any{},
							"usage": map[string]any{
								"prompt_tokens": 100, "completion_tokens": 7, "total_tokens": 107,
								"prompt_tokens_details":     map[string]any{"cached_tokens": 20, "cache_write_tokens": 30},
								"completion_tokens_details": map[string]any{"reasoning_tokens": 3},
							},
						}
					}
					data, err := json.Marshal(event)
					if !assert.NoError(t, err) {
						return
					}
					_, _ = io.WriteString(w, "data: "+string(data)+"\n\ndata: [DONE]\n\n")
				}))
				defer server.Close()
				opts := map[string]any{}
				if tc.requested != "" {
					opts["service_tier"] = tc.requested
				}
				if api == "chat" {
					opts["api_type"] = "openai_chatcompletions"
				}
				if api == "websocket" {
					opts["transport"] = "websocket"
				}
				client, err := NewClient(t.Context(), &latest.ModelConfig{
					Provider: "openai", Model: "gpt-6-astra", BaseURL: server.URL, TokenKey: "MY_TOKEN", ProviderOpts: opts,
				}, environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"}))
				require.NoError(t, err)
				defer client.Close()
				stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
				require.NoError(t, err)
				defer stream.Close()
				var usage *chat.Usage
				for {
					resp, err := stream.Recv()
					if err != nil {
						require.ErrorIs(t, err, io.EOF)
						break
					}
					if resp.Usage != nil {
						usage = resp.Usage
					}
				}
				require.NotNil(t, usage)
				assert.Equal(t, tc.actual, usage.ServiceTier)
				assert.Equal(t, int64(50), usage.InputTokens)
				assert.Equal(t, int64(100), usage.PromptTokens())
				assert.Equal(t, int64(7), usage.OutputTokens)
				assert.Equal(t, int64(3), usage.ReasoningTokens)
			})
		}
	}
}

func TestNewClientPricingEndpoint(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "https://example.com/v1")
	env := environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "secret"})
	client, err := NewClient(t.Context(), &latest.ModelConfig{Provider: "openai", Model: "gpt-6-astra"}, env)
	require.NoError(t, err)
	defer client.Close()
	assert.Equal(t, "https://example.com/v1", client.BaseConfig().BaseURL)

	// Accounting uses the endpoint captured at construction, not the current environment.
	t.Setenv("OPENAI_BASE_URL", "https://other.example/v1")
	assert.Equal(t, "https://example.com/v1", client.BaseConfig().BaseURL)

	direct, err := NewClient(t.Context(), &latest.ModelConfig{Provider: "openai", Model: "gpt-6-astra", BaseURL: "https://explicit.example/v1"}, env)
	require.NoError(t, err)
	defer direct.Close()
	assert.Equal(t, "https://explicit.example/v1", direct.BaseConfig().BaseURL)

	gateway, err := NewClient(t.Context(), &latest.ModelConfig{Provider: "openai", Model: "gpt-6-astra"}, env, options.WithGateway("https://gateway.example"))
	require.NoError(t, err)
	defer gateway.Close()
	assert.Equal(t, "https://api.openai.com/v1", gateway.BaseConfig().BaseURL)
}

func TestNewClientPricingEndpointTransports(t *testing.T) {
	for _, tc := range []struct {
		name, api string
		websocket bool
		want      []string
	}{
		{name: "chat", api: "openai_chatcompletions", want: []string{"chat"}},
		{name: "websocket", api: "openai_responses", websocket: true, want: []string{"websocket"}},
		{name: "websocket to SSE", api: "openai_responses", want: []string{"websocket", "sse"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan string, 3)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
				if r.Header.Get("Upgrade") != "" {
					requests <- "websocket"
					assert.Equal(t, "/v1/responses", r.URL.Path)
					if !tc.websocket {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if !assert.NoError(t, err) {
						return
					}
					defer conn.Close()
					var payload map[string]any
					if !assert.NoError(t, conn.ReadJSON(&payload)) {
						return
					}
					assert.Equal(t, "response.create", payload["type"])
					assert.Equal(t, "ultrafast", payload["service_tier"])
					event := completedEvent("resp_endpoint")
					event["response"].(map[string]any)["service_tier"] = "ultrafast"
					assert.NoError(t, conn.WriteJSON(event))
					return
				}
				if tc.api == "openai_chatcompletions" {
					requests <- "chat"
					assert.Equal(t, "/v1/chat/completions", r.URL.Path)
					writeSSEResponse(w)
				} else {
					requests <- "sse"
					assert.Equal(t, "/v1/responses", r.URL.Path)
					w.Header().Set("Content-Type", "text/event-stream")
					event := completedEvent("resp_endpoint")
					event["response"].(map[string]any)["service_tier"] = "ultrafast"
					data, err := json.Marshal(event)
					if !assert.NoError(t, err) {
						return
					}
					_, _ = io.WriteString(w, "data: "+string(data)+"\n\ndata: [DONE]\n\n")
				}
			}))
			defer server.Close()
			baseURL := server.URL + "/v1"
			t.Setenv("OPENAI_BASE_URL", baseURL)
			client, err := NewClient(t.Context(), &latest.ModelConfig{
				Provider: "openai", Model: "gpt-6-astra", TokenKey: "OPENAI_API_KEY", ProviderOpts: map[string]any{"transport": "websocket", "api_type": tc.api, "service_tier": "ultrafast"},
			}, environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "secret"}))
			require.NoError(t, err)
			defer client.Close()
			assert.Equal(t, baseURL, client.BaseConfig().BaseURL)
			require.NotNil(t, client.wsPool)
			assert.Equal(t, httpToWSURL(baseURL), client.wsPool.wsURL)
			// Endpoint selection is fixed at construction for both transports.
			t.Setenv("OPENAI_BASE_URL", "https://changed.example/v1")
			stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
			require.NoError(t, err)
			defer stream.Close()
			var usage *chat.Usage
			for {
				resp, err := stream.Recv()
				if err != nil {
					require.ErrorIs(t, err, io.EOF)
					break
				}
				if resp.Usage != nil {
					usage = resp.Usage
				}
			}
			require.NotNil(t, usage)
			if tc.api == "openai_responses" {
				assert.Equal(t, "ultrafast", usage.ServiceTier)
			}
			var got []string
			for {
				select {
				case request := <-requests:
					got = append(got, request)
				default:
					assert.Equal(t, tc.want, got)
					assert.Equal(t, baseURL, client.BaseConfig().BaseURL)
					return
				}
			}
		})
	}
}
