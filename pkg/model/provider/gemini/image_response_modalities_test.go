package gemini

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/tools"
)

// TestWantsImageResponseModalities exhaustively covers the predicate gating
// TEXT+IMAGE response modalities: it must be true only for supported Gemini
// surfaces, only for a model explicitly declared image-output-capable via
// output_capabilities.image, and only outside title-generation/compaction
// utility calls.
func TestWantsImageResponseModalities(t *testing.T) {
	t.Parallel()

	declaredTrue := &latest.OutputCapabilitiesConfig{Image: new(true)}
	declaredFalse := &latest.OutputCapabilitiesConfig{Image: new(false)}

	tests := []struct {
		name       string
		apiSurface string
		declared   *latest.OutputCapabilitiesConfig
		opts       []options.Opt
		want       bool
	}{
		{name: "gateway, declared true, ordinary chat: wants modalities", apiSurface: apiSurfaceGateway, declared: declaredTrue, want: true},
		{name: "direct Gemini API, declared true, ordinary chat: wants modalities", apiSurface: apiSurfaceGeminiAPI, declared: declaredTrue, want: true},
		{name: "Vertex AI, declared true, ordinary chat: wants modalities", apiSurface: apiSurfaceVertexAI, declared: declaredTrue, want: true},
		{name: "gateway, declared false: never", apiSurface: apiSurfaceGateway, declared: declaredFalse, want: false},
		{name: "direct Gemini API, declaration missing: never", apiSurface: apiSurfaceGeminiAPI, declared: nil, want: false},
		{
			name: "gateway, declared true, generating title: never", apiSurface: apiSurfaceGateway, declared: declaredTrue,
			opts: []options.Opt{options.WithGeneratingTitle()}, want: false,
		},
		{
			name: "gateway, declared true, compacting: never", apiSurface: apiSurfaceGateway, declared: declaredTrue,
			opts: []options.Opt{options.WithCompacting()}, want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{
				Config: base.Config{
					ModelConfig: latest.ModelConfig{
						Provider:           "google",
						Model:              "gemini-2.5-flash-image",
						OutputCapabilities: tt.declared,
					},
					ModelOptions: options.Apply(tt.opts...),
				},
				apiSurface: tt.apiSurface,
			}

			assert.Equal(t, tt.want, client.wantsImageResponseModalities(tt.declared != nil && tt.declared.Image != nil && *tt.declared.Image))
		})
	}
}

// capturedRequests is a mutex-guarded log of raw request bodies received by
// a [newBodyCapturingGeminiServer], letting tests assert exactly what was
// (or, cheaply, was not — an empty log) serialized onto the wire.
type capturedRequests struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (c *capturedRequests) add(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, b)
}

func (c *capturedRequests) all() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.bodies...)
}

// newBodyCapturingGeminiServer starts an httptest server that records the
// raw request body of every call it receives before responding via
// respond, so tests can decode the exact generationConfig sent on the wire.
func newBodyCapturingGeminiServer(t *testing.T, respond func(w http.ResponseWriter)) (*httptest.Server, *capturedRequests) {
	t.Helper()
	captured := &capturedRequests{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.add(body)
		respond(w)
	}))
	t.Cleanup(server.Close)
	return server, captured
}

// writeGeminiGenerateContentJSONResponse writes a minimal, non-streaming
// Gemini generateContent JSON response whose sole text part is text (used
// for Rerank, which does not use the SSE streaming endpoint).
func writeGeminiGenerateContentJSONResponse(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	payload, _ := json.Marshal(map[string]any{
		"candidates": []map[string]any{{
			"content": map[string]any{
				"role":  "model",
				"parts": []map[string]any{{"text": text}},
			},
		}},
	})
	_, _ = w.Write(payload)
}

// responseModalitiesInBody decodes body's generationConfig.responseModalities
// (see genai's generateContentConfigToMldev, which nests the serialized
// GenerateContentConfig under a top-level "generationConfig" key for the
// Gemini Developer API), returning nil when either key is absent.
func responseModalitiesInBody(t *testing.T, body []byte) []string {
	t.Helper()

	var req map[string]any
	require.NoError(t, json.Unmarshal(body, &req))

	genCfg, ok := req["generationConfig"].(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := genCfg["responseModalities"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, len(raw))
	for i, v := range raw {
		out[i], _ = v.(string)
	}
	return out
}

func drainStream(t *testing.T, stream chat.MessageStream) {
	t.Helper()
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
}

// TestCreateChatCompletionStream_ImageResponseModalities_PositiveRoutes pins
// that supported Gemini surfaces request TEXT+IMAGE output in that order.
func TestCreateChatCompletionStream_ImageResponseModalities_PositiveRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     func(serverURL string) *latest.ModelConfig
		env     map[string]string
		gateway bool
	}{
		{
			name: "gateway",
			cfg: func(string) *latest.ModelConfig {
				return &latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash-image", OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)}}
			},
			env:     map[string]string{environment.DockerDesktopTokenEnv: "test-dd-token"},
			gateway: true,
		},
		{
			name: "direct Gemini API",
			cfg: func(serverURL string) *latest.ModelConfig {
				return &latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash-image", BaseURL: serverURL, OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)}}
			},
			env: map[string]string{"GOOGLE_API_KEY": "test-key"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server, captured := newBodyCapturingGeminiServer(t, writeGeminiSSEResponse)
			var opts []options.Opt
			if tt.gateway {
				opts = append(opts, options.WithGateway(server.URL))
			}
			client, err := NewClient(t.Context(), tt.cfg(server.URL), environment.NewMapEnvProvider(tt.env), opts...)
			require.NoError(t, err)

			stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "generate an image of a red panda"}}, nil)
			require.NoError(t, err)
			drainStream(t, stream)

			bodies := captured.all()
			require.Len(t, bodies, 1)
			assert.Equal(t, []string{"TEXT", "IMAGE"}, responseModalitiesInBody(t, bodies[0]))
		})
	}
}

// TestCreateChatCompletionStream_ImageResponseModalities_AbsentOnOtherRoutes
// pins that undeclared image output and utility calls send no response
// modalities.
func TestCreateChatCompletionStream_ImageResponseModalities_AbsentOnOtherRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     func(serverURL string) *latest.ModelConfig
		env     map[string]string
		gateway bool
		opts    []options.Opt
	}{
		{
			name: "direct Gemini API, declaration missing: absent",
			cfg: func(serverURL string) *latest.ModelConfig {
				return &latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash-image", BaseURL: serverURL}
			},
			env: map[string]string{"GOOGLE_API_KEY": "test-key"},
		},
		{
			name: "gateway, declared false: absent",
			cfg: func(string) *latest.ModelConfig {
				return &latest.ModelConfig{
					Provider: "google", Model: "gemini-2.5-flash-image",
					OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(false)},
				}
			},
			env:     map[string]string{environment.DockerDesktopTokenEnv: "test-dd-token"},
			gateway: true,
		},
		{
			name: "gateway, declaration missing: absent",
			cfg: func(string) *latest.ModelConfig {
				return &latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash-image"}
			},
			env:     map[string]string{environment.DockerDesktopTokenEnv: "test-dd-token"},
			gateway: true,
		},
		{
			name: "gateway, declared true, generating title: absent",
			cfg: func(string) *latest.ModelConfig {
				return &latest.ModelConfig{
					Provider: "google", Model: "gemini-2.5-flash-image",
					OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)},
				}
			},
			env:     map[string]string{environment.DockerDesktopTokenEnv: "test-dd-token"},
			gateway: true,
			opts:    []options.Opt{options.WithGeneratingTitle()},
		},
		{
			name: "gateway, declared true, compacting: absent",
			cfg: func(string) *latest.ModelConfig {
				return &latest.ModelConfig{
					Provider: "google", Model: "gemini-2.5-flash-image",
					OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)},
				}
			},
			env:     map[string]string{environment.DockerDesktopTokenEnv: "test-dd-token"},
			gateway: true,
			opts:    []options.Opt{options.WithCompacting()},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server, captured := newBodyCapturingGeminiServer(t, writeGeminiSSEResponse)

			cfg := tt.cfg(server.URL)
			env := environment.NewMapEnvProvider(tt.env)
			opts := tt.opts
			if tt.gateway {
				opts = append(opts, options.WithGateway(server.URL))
			}
			client, err := NewClient(t.Context(), cfg, env, opts...)
			require.NoError(t, err)

			stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
				{Role: chat.MessageRoleUser, Content: "hello"},
			}, nil)
			require.NoError(t, err)
			drainStream(t, stream)

			bodies := captured.all()
			require.Len(t, bodies, 1)
			assert.Nil(t, responseModalitiesInBody(t, bodies[0]), "response modalities must be absent on this route")
		})
	}
}

// TestRerank_NeverSetsResponseModalities pins that Rerank — which shares
// buildConfig with CreateChatCompletionStream but must retain its own
// structured-output-only config — never gains response modalities, even on
// a model explicitly declared image-output-capable.
func TestRerank_NeverSetsResponseModalities(t *testing.T) {
	t.Parallel()

	server, captured := newBodyCapturingGeminiServer(t, func(w http.ResponseWriter) {
		writeGeminiGenerateContentJSONResponse(w, `{"scores":[1]}`)
	})

	cfg := &latest.ModelConfig{
		Provider:           "google",
		Model:              "gemini-2.5-flash-image",
		OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)},
	}
	env := environment.NewMapEnvProvider(map[string]string{
		environment.DockerDesktopTokenEnv: "test-dd-token",
	})
	client, err := NewClient(t.Context(), cfg, env, options.WithGateway(server.URL))
	require.NoError(t, err)

	scores, err := client.Rerank(t.Context(), "query", []types.Document{{Content: "doc1"}}, "")
	require.NoError(t, err)
	require.Len(t, scores, 1)

	bodies := captured.all()
	require.Len(t, bodies, 1)
	assert.Nil(t, responseModalitiesInBody(t, bodies[0]), "Rerank must never request response modalities")
}

// TestCreateChatCompletionStream_ImageResponseModalities_GuardRejectedRoutesNeverDispatch
// preserves guard precedence: on every declared-image route, a request shape
// the guard rejects (custom function tools, a built-in tool, or structured
// output) must still make zero provider calls, so nothing — including response
// modalities — is ever serialized onto the wire.
func TestCreateChatCompletionStream_ImageResponseModalities_GuardRejectedRoutesNeverDispatch(t *testing.T) {
	t.Parallel()

	newRejectedClient := func(t *testing.T, serverURL string, extraOpts ...options.Opt) *Client {
		t.Helper()
		cfg := &latest.ModelConfig{
			Provider:           "google",
			Model:              "gemini-2.5-flash-image",
			OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)},
		}
		env := environment.NewMapEnvProvider(map[string]string{
			environment.DockerDesktopTokenEnv: "test-dd-token",
		})
		opts := append([]options.Opt{options.WithGateway(serverURL)}, extraOpts...)
		client, err := NewClient(t.Context(), cfg, env, opts...)
		require.NoError(t, err)
		return client
	}

	assertRejectedWithNoDispatch := func(t *testing.T, err error, stream chat.MessageStream, captured *capturedRequests) {
		t.Helper()
		require.Nil(t, stream)
		var incompatible *base.OutputRequestIncompatibleError
		require.ErrorAs(t, err, &incompatible)
		assert.Empty(t, captured.all(), "guard rejection must dispatch nothing, so no modalities are ever serialized")
	}

	t.Run("custom function tools rejected", func(t *testing.T) {
		t.Parallel()
		server, captured := newBodyCapturingGeminiServer(t, writeGeminiSSEResponse)
		client := newRejectedClient(t, server.URL)

		stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
			{Role: chat.MessageRoleUser, Content: "hello"},
		}, []tools.Tool{{Name: "read_file", Description: "reads a file", Parameters: map[string]any{"type": "object"}}})

		assertRejectedWithNoDispatch(t, err, stream, captured)
	})

	t.Run("built-in tool rejected", func(t *testing.T) {
		t.Parallel()
		server, captured := newBodyCapturingGeminiServer(t, writeGeminiSSEResponse)
		client := newRejectedClient(t, server.URL)
		client.ModelConfig.ProviderOpts = map[string]any{"google_search": true}

		stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
			{Role: chat.MessageRoleUser, Content: "hello"},
		}, nil)

		assertRejectedWithNoDispatch(t, err, stream, captured)
	})

	t.Run("structured output rejected", func(t *testing.T) {
		t.Parallel()
		server, captured := newBodyCapturingGeminiServer(t, writeGeminiSSEResponse)
		client := newRejectedClient(t, server.URL, options.WithStructuredOutput(&latest.StructuredOutput{Schema: map[string]any{"type": "object"}}))

		stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
			{Role: chat.MessageRoleUser, Content: "hello"},
		}, nil)

		assertRejectedWithNoDispatch(t, err, stream, captured)
	})
}
