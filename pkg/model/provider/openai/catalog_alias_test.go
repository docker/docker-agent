package openai

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/modelsdev/embedded"
	"github.com/docker/docker-agent/pkg/tools"
)

type catalogRequestTransport struct {
	http.RoundTripper

	requests chan *http.Request
}

func (c catalogRequestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.requests <- req.Clone(req.Context())
	return c.RoundTripper.RoundTrip(req)
}

func TestChatCompletions_CatalogAliasesPreserveImages(t *testing.T) {
	t.Parallel()

	for _, provider := range []struct{ configured, catalog, model string }{
		{"fireworks", "fireworks-ai", "accounts/fireworks/models/kimi-k3"},
		{"together", "togetherai", "Qwen/Qwen3.5-397B-A17B"},
		{"moonshot", "moonshotai", "kimi-k3"},
		{"opencode-zen", "opencode", "kimi-k3"},
		{"ovhcloud", "ovhcloud", "Qwen3.5-397B-A17B"},
		{"fireworks-ai", "fireworks-ai", "accounts/fireworks/models/kimi-k3"},
		{"togetherai", "togetherai", "Qwen/Qwen3.5-397B-A17B"},
		{"moonshotai", "moonshotai", "kimi-k3"},
		{"opencode", "opencode", "kimi-k3"},
	} {
		for _, mode := range []string{"snapshot vision", "text only", "unknown", "override disables", "override enables", "direct text wins"} {
			t.Run(provider.configured+"/"+mode, func(t *testing.T) {
				t.Parallel()
				model := provider.model
				store := modelsdev.NewDatabaseStore(embedded.Snapshot())
				var override *latest.CapabilitiesConfig
				wantImages := false
				switch mode {
				case "snapshot vision":
					wantImages = true
				case "text only", "override enables":
					model = "text-only"
					store = modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
						provider.catalog: {Models: map[string]modelsdev.Model{model: {Modalities: modelsdev.Modalities{Input: []string{"text"}}}}},
					}})
					if mode == "override enables" {
						override = &latest.CapabilitiesConfig{Image: true}
						wantImages = true
					}
				case "unknown":
					model = "unknown-catalog-model"
				case "override disables":
					override = &latest.CapabilitiesConfig{Image: false}
				case "direct text wins":
					catalogModel := model
					if provider.configured == "ovhcloud" {
						catalogModel = "qwen3.5-397b-a17b"
					}
					providers := map[string]modelsdev.Provider{
						provider.catalog: {Models: map[string]modelsdev.Model{catalogModel: {Modalities: modelsdev.Modalities{Input: []string{"text", "image"}}}}},
					}
					direct := providers[provider.configured]
					if direct.Models == nil {
						direct.Models = map[string]modelsdev.Model{}
					}
					direct.Models[model] = modelsdev.Model{Modalities: modelsdev.Modalities{Input: []string{"text"}}}
					providers[provider.configured] = direct
					store = modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: providers})
				}

				server, capturedBody := captureRequestBody(t)
				requests := make(chan *http.Request, 1)
				maxTokens, wantMaxTokens := int64(32000), int64(32000)
				if metadata, err := store.GetModel(t.Context(), modelsdev.NewID(provider.configured, model)); err == nil && metadata.Limit.Output > 0 {
					maxTokens = metadata.Limit.Output
					wantMaxTokens = maxTokens
					if metadata.Limit.Context > 0 {
						wantMaxTokens = min(maxTokens, max(int64(metadata.Limit.Context)-1024, 1))
					}
				}
				cfg := &latest.ModelConfig{
					Provider: provider.configured, Model: model,
					BaseURL: server.URL + "/configured/v1", TokenKey: "CATALOG_TEST_TOKEN",
					MaxTokens:    &maxTokens,
					ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"}, Capabilities: override,
				}
				client, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{
					"CATALOG_TEST_TOKEN": "fake-configured-token", "OPENAI_API_KEY": "fake-wrong-token",
				}), options.WithModelsDevStore(store), options.WithHTTPTransportWrapper(func(base http.RoundTripper) http.RoundTripper {
					return catalogRequestTransport{RoundTripper: base, requests: requests}
				}))
				require.NoError(t, err)
				imagePart := func(name string, data byte) chat.MessagePart {
					return chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
						Name: name, MimeType: "image/png", Source: chat.DocumentSource{InlineData: []byte{data}},
					}}
				}
				stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
					{Role: chat.MessageRoleUser, MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeText, Text: "describe the attachment"}, imagePart("attachment.png", 1),
					}},
					{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "call-image", Type: "function", Function: tools.FunctionCall{Name: "read_file", Arguments: `{}`}}}},
					{Role: chat.MessageRoleTool, ToolCallID: "call-image", MultiContent: []chat.MessagePart{
						{Type: chat.MessagePartTypeText, Text: "image loaded"}, imagePart("tool.png", 2),
					}},
				}, nil)
				require.NoError(t, err)
				defer stream.Close()
				for {
					_, err := stream.Recv()
					if err != nil {
						require.ErrorIs(t, err, io.EOF)
						break
					}
				}
				var req *http.Request
				select {
				case req = <-requests:
				default:
					t.Fatal("no request captured")
				}
				assert.Equal(t, server.URL+"/configured/v1/chat/completions", req.URL.String())
				assert.Equal(t, "Bearer fake-configured-token", req.Header.Get("Authorization"))
				assert.Equal(t, *cfg, client.ModelConfig)
				var payload struct {
					Model     string `json:"model"`
					MaxTokens int64  `json:"max_tokens"`
					Messages  []struct {
						Role    string          `json:"role"`
						Content json.RawMessage `json:"content"`
					} `json:"messages"`
				}
				require.NoError(t, json.Unmarshal(capturedBody(), &payload))
				assert.Equal(t, model, payload.Model)
				assert.Equal(t, wantMaxTokens, payload.MaxTokens)
				var imageURLs, toolImageURLs []string
				for _, msg := range payload.Messages {
					if msg.Role != "user" && msg.Role != "tool" {
						continue
					}
					var parts []struct {
						Type     string `json:"type"`
						Text     string `json:"text"`
						ImageURL struct {
							URL string `json:"url"`
						} `json:"image_url"`
					}
					require.NoError(t, json.Unmarshal(msg.Content, &parts))
					for _, part := range parts {
						if msg.Role == "tool" && part.Type == "text" && strings.HasPrefix(part.Text, "data:image/") {
							toolImageURLs = append(toolImageURLs, part.Text)
						}
						if part.Type == "image_url" {
							imageURLs = append(imageURLs, part.ImageURL.URL)
						}
					}
				}
				if wantImages {
					assert.Equal(t, []string{"data:image/png;base64,AQ==", "data:image/png;base64,Ag=="}, imageURLs)
					assert.Empty(t, toolImageURLs)
				} else {
					assert.Empty(t, imageURLs)
					assert.Empty(t, toolImageURLs)
				}
			})
		}
	}
}
