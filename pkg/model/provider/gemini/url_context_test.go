package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestURLContext(t *testing.T) {
	t.Parallel()
	for _, surface := range []string{apiSurfaceGeminiAPI, apiSurfaceGateway, apiSurfaceVertexAI} {
		for _, tc := range []struct {
			name       string
			opts       map[string]any
			custom     bool
			wantURL    bool
			wantSearch bool
		}{
			{name: "unset"},
			{name: "disabled", opts: map[string]any{"url_context": false}},
			{name: "string ignored", opts: map[string]any{"url_context": "true"}},
			{name: "null ignored", opts: map[string]any{"url_context": nil}},
			{name: "enabled", opts: map[string]any{"url_context": true}, wantURL: true},
			{name: "search and custom tools", opts: map[string]any{"url_context": true, "google_search": true}, custom: true, wantURL: true, wantSearch: true},
		} {
			t.Run(fmt.Sprintf("%s/%s", surface, tc.name), func(t *testing.T) {
				t.Parallel()
				server, captured := newBodyCapturingGeminiServer(t, func(w http.ResponseWriter) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"summary\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}]}\n\n")
				})
				cfg := &latest.ModelConfig{Provider: "google", Model: "gemini-3.8-flash", BaseURL: server.URL, ProviderOpts: tc.opts}
				envMap := map[string]string{"GOOGLE_API_KEY": "test-key", environment.DockerDesktopTokenEnv: "test-dd-token"}
				var opts []options.Opt
				switch surface {
				case apiSurfaceGateway:
					cfg.BaseURL = ""
					opts = append(opts, options.WithGateway(server.URL))
				case apiSurfaceVertexAI:
					envMap["GOOGLE_GENAI_USE_VERTEXAI"] = "1"
					opts = append(opts, options.WithTokenSource(func(context.Context) (string, error) { return "test-token", nil }))
				}
				client, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(envMap), opts...)
				require.NoError(t, err)
				assert.Equal(t, surface, client.apiSurface)
				var requestTools []tools.Tool
				if tc.custom {
					requestTools = []tools.Tool{{Name: "read_file", Description: "Read a local file", Parameters: map[string]any{"type": "object"}}}
				}
				stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Summarize https://docs.docker.com/"}}, requestTools)
				require.NoError(t, err)
				defer stream.Close()
				if surface == apiSurfaceVertexAI && tc.custom {
					_, err = stream.Recv()
					require.ErrorContains(t, err, "includeServerSideToolInvocations parameter is only supported")
					assert.Empty(t, captured.all())
					return
				}
				for {
					_, err = stream.Recv()
					if err != nil {
						require.ErrorIs(t, err, io.EOF)
						break
					}
				}
				bodies := captured.all()
				require.Len(t, bodies, 1)
				var body struct {
					Tools      []map[string]json.RawMessage `json:"tools"`
					ToolConfig struct {
						Include bool `json:"includeServerSideToolInvocations"`
					} `json:"toolConfig"`
				}
				require.NoError(t, json.Unmarshal(bodies[0], &body))
				var urlCount, searchCount, functionCount int
				for _, tool := range body.Tools {
					if raw, ok := tool["urlContext"]; ok {
						urlCount++
						assert.JSONEq(t, `{}`, string(raw))
					}
					if _, ok := tool["googleSearch"]; ok {
						searchCount++
					}
					if _, ok := tool["functionDeclarations"]; ok {
						functionCount++
					}
				}
				assert.Equal(t, tc.wantURL, urlCount == 1)
				assert.Equal(t, tc.wantSearch, searchCount == 1)
				assert.Equal(t, tc.custom, functionCount == 1)
				assert.Equal(t, tc.custom && tc.wantURL, body.ToolConfig.Include)
				if tc.wantURL {
					assert.Contains(t, builtInToolKinds(client.builtInTools()), "url_context")
				} else {
					assert.NotContains(t, builtInToolKinds(client.builtInTools()), "url_context")
				}
			})
		}
	}
}

func TestURLContextRejection(t *testing.T) {
	t.Parallel()
	for _, message := range []string{"url_context is not supported", "Unsupported urlContext tool"} {
		category, matched := classifyByMessage(message)
		assert.True(t, matched)
		assert.Equal(t, RejectionIncompatibleFunctionOrBuiltinTools, category)
	}
}
