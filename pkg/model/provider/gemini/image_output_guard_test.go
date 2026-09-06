package gemini

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

// TestCheckImageOutputRequestCompatibility_GateConditions exhaustively
// covers when the guard does and does not apply: it must reject on each
// supported Google surface, only for a model with image output enabled,
// and only when the request also carries custom function tools, a built-in
// tool, or structured output.
func TestCheckImageOutputRequestCompatibility_GateConditions(t *testing.T) {
	t.Parallel()

	declaredTrue := &latest.OutputCapabilitiesConfig{Image: new(true)}
	declaredFalse := &latest.OutputCapabilitiesConfig{Image: new(false)}

	tests := []struct {
		name         string
		apiSurface   string
		declared     *latest.OutputCapabilitiesConfig
		builtInTools []*genai.Tool
		requestTools int
		structured   bool
		wantReject   []imageOutputIncompatibility
	}{
		{name: "gateway declared true, no extras: allowed", apiSurface: apiSurfaceGateway, declared: declaredTrue},
		{name: "gateway declared false: never rejects even with tools", apiSurface: apiSurfaceGateway, declared: declaredFalse, requestTools: 1},
		{name: "gateway undeclared: never rejects even with tools", apiSurface: apiSurfaceGateway, declared: nil, requestTools: 1},
		{name: "direct Gemini API declared true + tools: rejected", apiSurface: apiSurfaceGeminiAPI, declared: declaredTrue, requestTools: 1, wantReject: []imageOutputIncompatibility{imageOutputIncompatibleTools}},
		{name: "Vertex AI declared true + tools: rejected", apiSurface: apiSurfaceVertexAI, declared: declaredTrue, requestTools: 1, wantReject: []imageOutputIncompatibility{imageOutputIncompatibleTools}},
		{
			name:       "gateway declared true + custom function tools: rejected",
			apiSurface: apiSurfaceGateway, declared: declaredTrue, requestTools: 2,
			wantReject: []imageOutputIncompatibility{imageOutputIncompatibleTools},
		},
		{
			name:       "gateway declared true + built-in tool: rejected",
			apiSurface: apiSurfaceGateway, declared: declaredTrue, builtInTools: []*genai.Tool{{GoogleSearch: &genai.GoogleSearch{}}},
			wantReject: []imageOutputIncompatibility{imageOutputIncompatibleBuiltInTools},
		},
		{
			name:       "gateway declared true + structured output: rejected",
			apiSurface: apiSurfaceGateway, declared: declaredTrue, structured: true,
			wantReject: []imageOutputIncompatibility{imageOutputIncompatibleStructuredOutput},
		},
		{
			name:       "gateway declared true + tools and structured output: both reported",
			apiSurface: apiSurfaceGateway, declared: declaredTrue, requestTools: 1, structured: true,
			wantReject: []imageOutputIncompatibility{imageOutputIncompatibleTools, imageOutputIncompatibleStructuredOutput},
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
				},
				apiSurface: tt.apiSurface,
			}
			config := &genai.GenerateContentConfig{}
			if tt.structured {
				config.ResponseMIMEType = "application/json"
			}

			err := client.checkImageOutputRequestCompatibility(t.Context(), tt.declared != nil && tt.declared.Image != nil && *tt.declared.Image, config, tt.builtInTools, tt.requestTools)

			if len(tt.wantReject) == 0 {
				assert.NoError(t, err)
				return
			}
			var incompatible *ImageOutputRequestIncompatibleError
			require.ErrorAs(t, err, &incompatible, "expected an *ImageOutputRequestIncompatibleError, got %v", err)
			assert.Equal(t, tt.wantReject, incompatible.Incompatibilities)
		})
	}
}

func TestImageOutputRequestIncompatibleError_ToolMessages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		incompatibilities []imageOutputIncompatibility
		toolCallSupport   modelinfo.ToolCallSupport
		want              string
	}{
		{
			name:              "model does not support tools",
			incompatibilities: []imageOutputIncompatibility{imageOutputIncompatibleTools},
			toolCallSupport:   modelinfo.ToolCallUnsupported,
			want:              "this model does not support tool calls; use a tool-capable model or remove tools from the request",
		},
		{
			name:              "tool-capable image model",
			incompatibilities: []imageOutputIncompatibility{imageOutputIncompatibleTools},
			toolCallSupport:   modelinfo.ToolCallSupported,
			want:              "this model supports tool calls, but not while image output is enabled; use a separate model or request for that combination",
		},
		{
			name:              "unknown support keeps conservative message",
			incompatibilities: []imageOutputIncompatibility{imageOutputIncompatibleTools},
			toolCallSupport:   modelinfo.ToolCallSupportUnknown,
			want:              "this model is configured for image output (output_capabilities.image) and does not support tools in the same request; use a separate model or request for that combination",
		},
		{
			name:              "structured output ignores tool support",
			incompatibilities: []imageOutputIncompatibility{imageOutputIncompatibleStructuredOutput},
			toolCallSupport:   modelinfo.ToolCallUnsupported,
			want:              "this model is configured for image output (output_capabilities.image) and does not support structured output in the same request; use a separate model or request for that combination",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := (&ImageOutputRequestIncompatibleError{
				Incompatibilities: tt.incompatibilities,
				ToolCallSupport:   tt.toolCallSupport,
			}).Error()
			assert.Equal(t, tt.want, err)
		})
	}
}

func TestCheckImageOutputRequestCompatibility_ToolCallSupport(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"google": {Models: map[string]modelsdev.Model{
			"tool-model":    {ToolCall: true},
			"no-tool-model": {ToolCall: false},
		}},
	}})

	tests := []struct {
		name  string
		model string
		want  modelinfo.ToolCallSupport
	}{
		{name: "catalogue reports no tool calls", model: "no-tool-model", want: modelinfo.ToolCallUnsupported},
		{name: "catalogue reports tool calls", model: "tool-model", want: modelinfo.ToolCallSupported},
		{name: "missing catalogue model stays unknown", model: "missing", want: modelinfo.ToolCallSupportUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{Config: base.Config{
				ModelConfig:  latest.ModelConfig{Provider: "google", Model: tt.model},
				ModelOptions: options.Apply(options.WithModelsDevStore(store)),
			}}
			err := client.checkImageOutputRequestCompatibility(t.Context(), true, &genai.GenerateContentConfig{}, nil, 1)

			var incompatible *ImageOutputRequestIncompatibleError
			require.ErrorAs(t, err, &incompatible)
			assert.Equal(t, tt.want, incompatible.ToolCallSupport)
		})
	}
}

func TestImageOutputRequestIncompatibleError_MessageNamesCategoriesOnly(t *testing.T) {
	t.Parallel()

	err := &ImageOutputRequestIncompatibleError{Incompatibilities: []imageOutputIncompatibility{
		imageOutputIncompatibleTools, imageOutputIncompatibleStructuredOutput,
	}}
	msg := err.Error()
	assert.Contains(t, msg, "output_capabilities.image")
	assert.Contains(t, msg, "tools")
	assert.Contains(t, msg, "structured output")
}

// TestImageOutputRequestIncompatibleError_RoutesThroughExistingErrorSeam
// drives the guard's error through the same modelerrors.FormatError call the
// runtime loop uses to build ErrorEvent.Error (pkg/runtime/loop_steps.go),
// which the TUI renders verbatim (pkg/tui/page/chat/runtime_events.go). No
// new plumbing is needed: the guard's error is a plain error, not an
// overflow/truncation-shaped one, so FormatError must pass it through
// unchanged, and ClassifyModelError must not mark it retryable (retrying
// this exact request would just reject again).
func TestImageOutputRequestIncompatibleError_RoutesThroughExistingErrorSeam(t *testing.T) {
	t.Parallel()

	err := &ImageOutputRequestIncompatibleError{Incompatibilities: []imageOutputIncompatibility{imageOutputIncompatibleBuiltInTools}}

	visible := modelerrors.FormatError(err)
	assert.Equal(t, err.Error(), visible, "a plain incompatibility error must pass through FormatError unchanged")
	assert.Contains(t, visible, "output_capabilities.image")
	assert.Contains(t, visible, "built-in tools")

	retryable, rateLimited, _ := modelerrors.ClassifyModelError(err)
	assert.False(t, retryable, "a deterministic local rejection must not be retried")
	assert.False(t, rateLimited)
}

// TestCreateChatCompletionStream_ImageOutputGuard_RejectsBeforeDispatch drives
// the guard through the real CreateChatCompletionStream path against an
// httptest server, and asserts zero provider calls on rejection.
func TestCreateChatCompletionStream_ImageOutputGuard_RejectsBeforeDispatch(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeGeminiSSEResponse(w)
	}))
	defer server.Close()

	newClient := func(t *testing.T, counter *geminiCountingTransport) *Client {
		t.Helper()
		cfg := &latest.ModelConfig{
			Provider:           "google",
			Model:              "gemini-2.5-flash-image",
			OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)},
		}
		env := environment.NewMapEnvProvider(map[string]string{
			environment.DockerDesktopTokenEnv: "test-dd-token",
		})
		client, err := NewClient(t.Context(), cfg, env,
			options.WithGateway(server.URL),
			options.WithHTTPTransportWrapper(func(base http.RoundTripper) http.RoundTripper {
				counter.base = base
				return counter
			}),
		)
		require.NoError(t, err)
		return client
	}

	t.Run("custom function tools rejected with zero provider calls", func(t *testing.T) {
		t.Parallel()
		var counter geminiCountingTransport
		client := newClient(t, &counter)

		stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
			{Role: chat.MessageRoleUser, Content: "hello"},
		}, []tools.Tool{{Name: "read_file", Description: "reads a file", Parameters: map[string]any{"type": "object"}}})

		require.Nil(t, stream)
		var incompatible *ImageOutputRequestIncompatibleError
		require.ErrorAs(t, err, &incompatible)
		assert.Equal(t, []imageOutputIncompatibility{imageOutputIncompatibleTools}, incompatible.Incompatibilities)
		assert.Zero(t, counter.calls.Load(), "guard must reject before any provider dispatch")
	})

	t.Run("built-in tool rejected with zero provider calls", func(t *testing.T) {
		t.Parallel()
		var counter geminiCountingTransport
		client := newClient(t, &counter)
		client.ModelConfig.ProviderOpts = map[string]any{"google_search": true}

		stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
			{Role: chat.MessageRoleUser, Content: "hello"},
		}, nil)

		require.Nil(t, stream)
		var incompatible *ImageOutputRequestIncompatibleError
		require.ErrorAs(t, err, &incompatible)
		assert.Equal(t, []imageOutputIncompatibility{imageOutputIncompatibleBuiltInTools}, incompatible.Incompatibilities)
		assert.Zero(t, counter.calls.Load(), "guard must reject before any provider dispatch")
	})

	t.Run("structured output rejected with zero provider calls", func(t *testing.T) {
		t.Parallel()
		var counter geminiCountingTransport
		cfg := &latest.ModelConfig{
			Provider:           "google",
			Model:              "gemini-2.5-flash-image",
			OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)},
		}
		env := environment.NewMapEnvProvider(map[string]string{
			environment.DockerDesktopTokenEnv: "test-dd-token",
		})
		client, err := NewClient(t.Context(), cfg, env,
			options.WithGateway(server.URL),
			options.WithStructuredOutput(&latest.StructuredOutput{Schema: map[string]any{"type": "object"}}),
			options.WithHTTPTransportWrapper(func(base http.RoundTripper) http.RoundTripper {
				counter.base = base
				return &counter
			}),
		)
		require.NoError(t, err)

		stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
			{Role: chat.MessageRoleUser, Content: "hello"},
		}, nil)

		require.Nil(t, stream)
		var incompatible *ImageOutputRequestIncompatibleError
		require.ErrorAs(t, err, &incompatible)
		assert.Equal(t, []imageOutputIncompatibility{imageOutputIncompatibleStructuredOutput}, incompatible.Incompatibilities)
		assert.Zero(t, counter.calls.Load(), "guard must reject before any provider dispatch")
	})
}

// TestCreateChatCompletionStream_ImageOutputGuard_PreservesNormalBehavior
// proves the guard is a no-op (request reaches the provider) for every route
// it must not touch: no extras on the declared route and tools/structured
// output when the declaration is false or missing.
func TestCreateChatCompletionStream_ImageOutputGuard_PreservesNormalBehavior(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeGeminiSSEResponse(w)
	}))
	defer server.Close()

	drain := func(t *testing.T, stream chat.MessageStream) {
		t.Helper()
		defer stream.Close()
		for {
			if _, err := stream.Recv(); err != nil {
				break
			}
		}
	}

	t.Run("gateway declared true, no tools/structured output: reaches provider", func(t *testing.T) {
		t.Parallel()
		var counter geminiCountingTransport
		cfg := &latest.ModelConfig{
			Provider:           "google",
			Model:              "gemini-2.5-flash-image",
			OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)},
		}
		env := environment.NewMapEnvProvider(map[string]string{
			environment.DockerDesktopTokenEnv: "test-dd-token",
		})
		client, err := NewClient(t.Context(), cfg, env,
			options.WithGateway(server.URL),
			options.WithHTTPTransportWrapper(func(base http.RoundTripper) http.RoundTripper {
				counter.base = base
				return &counter
			}),
		)
		require.NoError(t, err)

		stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
			{Role: chat.MessageRoleUser, Content: "hello"},
		}, nil)
		require.NoError(t, err)
		drain(t, stream)
		assert.Positive(t, counter.calls.Load())
	})

	t.Run("gateway with tools, declaration false: reaches provider", func(t *testing.T) {
		t.Parallel()
		var counter geminiCountingTransport
		cfg := &latest.ModelConfig{
			Provider:           "google",
			Model:              "gemini-2.5-flash",
			OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(false)},
		}
		env := environment.NewMapEnvProvider(map[string]string{
			environment.DockerDesktopTokenEnv: "test-dd-token",
		})
		client, err := NewClient(t.Context(), cfg, env,
			options.WithGateway(server.URL),
			options.WithHTTPTransportWrapper(func(base http.RoundTripper) http.RoundTripper {
				counter.base = base
				return &counter
			}),
		)
		require.NoError(t, err)

		stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
			{Role: chat.MessageRoleUser, Content: "hello"},
		}, []tools.Tool{{Name: "read_file", Description: "reads a file", Parameters: map[string]any{"type": "object"}}})
		require.NoError(t, err)
		drain(t, stream)
		assert.Positive(t, counter.calls.Load())
	})

	t.Run("gateway with tools, declaration missing: reaches provider", func(t *testing.T) {
		t.Parallel()
		var counter geminiCountingTransport
		cfg := &latest.ModelConfig{
			Provider: "google",
			Model:    "gemini-2.5-flash",
		}
		env := environment.NewMapEnvProvider(map[string]string{
			environment.DockerDesktopTokenEnv: "test-dd-token",
		})
		client, err := NewClient(t.Context(), cfg, env,
			options.WithGateway(server.URL),
			options.WithHTTPTransportWrapper(func(base http.RoundTripper) http.RoundTripper {
				counter.base = base
				return &counter
			}),
		)
		require.NoError(t, err)

		stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
			{Role: chat.MessageRoleUser, Content: "hello"},
		}, []tools.Tool{{Name: "read_file", Description: "reads a file", Parameters: map[string]any{"type": "object"}}})
		require.NoError(t, err)
		drain(t, stream)
		assert.Positive(t, counter.calls.Load())
	})

	t.Run("direct Gemini call with tools, resolved image output: rejected before dispatch", func(t *testing.T) {
		t.Parallel()
		var counter geminiCountingTransport
		cfg := &latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash-image", BaseURL: server.URL, OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)}}
		env := environment.NewMapEnvProvider(map[string]string{"GOOGLE_API_KEY": "test-key"})
		client, err := NewClient(t.Context(), cfg, env, options.WithHTTPTransportWrapper(func(base http.RoundTripper) http.RoundTripper { counter.base = base; return &counter }))
		require.NoError(t, err)
		stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, []tools.Tool{{Name: "read_file", Description: "reads a file", Parameters: map[string]any{"type": "object"}}})
		require.Nil(t, stream)
		require.Error(t, err)
		assert.Zero(t, counter.calls.Load())
	})
}
