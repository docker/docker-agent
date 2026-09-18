package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/rag/types"
)

func TestBuildConfig_ServiceTier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts map[string]any
		want genai.ServiceTier
	}{
		{name: "omitted"},
		{name: "empty", opts: map[string]any{"service_tier": ""}},
		{name: "flex", opts: map[string]any{"service_tier": "flex"}, want: genai.ServiceTierFlex},
		{name: "standard", opts: map[string]any{"service_tier": "standard"}, want: genai.ServiceTierStandard},
		{name: "priority", opts: map[string]any{"service_tier": "priority"}, want: genai.ServiceTierPriority},
		{name: "future tier passes through", opts: map[string]any{"service_tier": "turbo"}, want: "turbo"},
		{name: "int ignored", opts: map[string]any{"service_tier": 1}},
		{name: "bool ignored", opts: map[string]any{"service_tier": true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{Config: base.Config{
				ModelConfig: latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash", ProviderOpts: tt.opts},
			}}

			assert.Equal(t, tt.want, client.buildConfig().ServiceTier)
		})
	}
}

// Title generation returns early from buildConfig; the tier must already be set.
func TestBuildConfig_ServiceTier_NoThinkingTitle(t *testing.T) {
	t.Parallel()

	client := &Client{Config: base.Config{
		ModelConfig:  latest.ModelConfig{Provider: "google", Model: "gemini-3-flash", ProviderOpts: map[string]any{"service_tier": "flex"}},
		ModelOptions: options.Apply(options.WithGeneratingTitle(), options.WithNoThinking()),
	}}

	config := client.buildConfig()
	assert.Nil(t, config.ThinkingConfig)
	assert.Equal(t, genai.ServiceTierFlex, config.ServiceTier)
}

// serviceTierInBody returns the top-level serviceTier of a generateContent
// request body. Both the Gemini API and Vertex AI converters place it there,
// outside generationConfig.
func serviceTierInBody(t *testing.T, body []byte) (string, bool) {
	t.Helper()

	var req map[string]any
	require.NoError(t, json.Unmarshal(body, &req))
	_, inGenCfg := req["generationConfig"].(map[string]any)["serviceTier"]
	assert.False(t, inGenCfg, "serviceTier must not be nested under generationConfig")
	tier, ok := req["serviceTier"].(string)
	return tier, ok
}

// geminiSurface describes one Google API surface pointed at a test server.
// Each surface uses a distinct SDK request converter, so the wire shape is
// pinned on all three. An explicit Vertex AI token source avoids ADC discovery,
// including on WASM.
type geminiSurface struct {
	name       string
	apiSurface string
	env        map[string]string
	gateway    bool
}

func (s geminiSurface) newClient(t *testing.T, serverURL string, providerOpts map[string]any) *Client {
	t.Helper()
	cfg := &latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash", ProviderOpts: providerOpts}
	var opts []options.Opt
	if s.gateway {
		opts = append(opts, options.WithGateway(serverURL))
	} else {
		cfg.BaseURL = serverURL
	}
	if s.apiSurface == apiSurfaceVertexAI {
		opts = append(opts, options.WithTokenSource(func(context.Context) (string, error) { return "test-token", nil }))
	}
	client, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(s.env), opts...)
	require.NoError(t, err)
	require.Equal(t, s.apiSurface, client.apiSurface)
	return client
}

var geminiSurfaces = []geminiSurface{
	{name: "direct Gemini API", apiSurface: apiSurfaceGeminiAPI, env: map[string]string{"GOOGLE_API_KEY": "test-key"}},
	{name: "gateway", apiSurface: apiSurfaceGateway, env: map[string]string{environment.DockerDesktopTokenEnv: "test-dd-token"}, gateway: true},
	{name: "Vertex AI", apiSurface: apiSurfaceVertexAI, env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "1"}},
}

func TestCreateChatCompletionStream_ServiceTierOnWire(t *testing.T) {
	t.Parallel()

	tiers := []struct {
		name string
		opts map[string]any
		want string
	}{
		{name: "omitted"},
		{name: "empty", opts: map[string]any{"service_tier": ""}},
		{name: "flex", opts: map[string]any{"service_tier": "flex"}, want: "flex"},
		{name: "priority", opts: map[string]any{"service_tier": "priority"}, want: "priority"},
		{name: "future tier", opts: map[string]any{"service_tier": "turbo"}, want: "turbo"},
		{name: "malformed", opts: map[string]any{"service_tier": 42}},
	}

	for _, surface := range geminiSurfaces {
		for _, tier := range tiers {
			t.Run(surface.name+"/"+tier.name, func(t *testing.T) {
				t.Parallel()
				server, captured := newBodyCapturingGeminiServer(t, writeGeminiSSEResponse)
				client := surface.newClient(t, server.URL, tier.opts)

				stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
				require.NoError(t, err)
				drainStream(t, stream)

				bodies := captured.all()
				require.Len(t, bodies, 1)
				got, present := serviceTierInBody(t, bodies[0])
				assert.Equal(t, tier.want != "", present, "serviceTier presence")
				assert.Equal(t, tier.want, got)
			})
		}
	}
}

func TestRerank_ServiceTierOnWire(t *testing.T) {
	t.Parallel()

	for _, surface := range geminiSurfaces {
		t.Run(surface.name, func(t *testing.T) {
			t.Parallel()
			server, captured := newBodyCapturingGeminiServer(t, func(w http.ResponseWriter) {
				writeGeminiGenerateContentJSONResponse(w, `{"scores":[1]}`)
			})
			client := surface.newClient(t, server.URL, map[string]any{"service_tier": "flex"})

			scores, err := client.Rerank(t.Context(), "query", []types.Document{{Content: "doc1"}}, "")
			require.NoError(t, err)
			require.Len(t, scores, 1)

			bodies := captured.all()
			require.Len(t, bodies, 1)
			got, _ := serviceTierInBody(t, bodies[0])
			assert.Equal(t, "flex", got)
		})
	}
}
