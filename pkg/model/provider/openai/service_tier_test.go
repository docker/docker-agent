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
