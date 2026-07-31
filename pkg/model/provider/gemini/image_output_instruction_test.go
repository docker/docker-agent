package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// systemInstructionTextsInBody decodes body's systemInstruction part texts
// (genai serializes GenerateContentConfig.SystemInstruction under the
// top-level "systemInstruction" key for the Gemini Developer API), returning
// nil when the key is absent.
func systemInstructionTextsInBody(t *testing.T, body []byte) []string {
	t.Helper()

	var req map[string]any
	require.NoError(t, json.Unmarshal(body, &req))

	si, ok := req["systemInstruction"].(map[string]any)
	if !ok {
		return nil
	}
	parts, ok := si["parts"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, p := range parts {
		partMap, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := partMap["text"].(string); ok {
			out = append(out, text)
		}
	}
	return out
}

// TestCreateChatCompletionStream_MediaFileInstruction_PositiveRoutes pins
// that ordinary image-output chat requests carry the media-file marker
// instruction exactly once on every supported Google surface.
func TestCreateChatCompletionStream_MediaFileInstruction_PositiveRoutes(t *testing.T) {
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
			opts := []options.Opt(nil)
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
			texts := systemInstructionTextsInBody(t, bodies[0])
			require.Len(t, texts, 1, "the instruction must be sent exactly once")
			assert.Equal(t, imageOutputMediaFileInstruction, texts[0])
			assert.Equal(t, 1, strings.Count(texts[0], "[media-file: "), "the instruction must show the marker format exactly once")
		})
	}
}

// TestCreateChatCompletionStream_MediaFileInstruction_AbsentOnOtherRoutes
// pins that non-image-output and internal text-only requests send no marker
// instruction: gateway calls without image output enabled, plus gateway
// title-generation and compaction calls.
func TestCreateChatCompletionStream_MediaFileInstruction_AbsentOnOtherRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     func(serverURL string) *latest.ModelConfig
		env     map[string]string
		gateway bool
		opts    []options.Opt
	}{
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
			assert.Nil(t, systemInstructionTextsInBody(t, bodies[0]), "the marker instruction must be absent on this route")
		})
	}
}
