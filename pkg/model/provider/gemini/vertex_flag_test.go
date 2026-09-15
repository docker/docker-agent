package gemini

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestVertexAIEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value string
		want  bool
	}{
		{"", false},
		{"false", false},
		{"FALSE", false},
		{"False", false},
		{"0", false},
		{"f", false},
		{"no", false},
		{"off", false},
		{"banana", false},
		{"true", true},
		{"True", true},
		{"TRUE", true},
		{"1", true},
		{"t", true},
		{"T", true},
		{"  true  ", true},
	}

	for _, tt := range tests {
		t.Run("value="+tt.value, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, vertexAIEnabled(tt.value))
		})
	}
}

// TestNewClient_VertexFlagFalseUsesDirectAPI is the regression for issue
// #4292: GOOGLE_GENAI_USE_VERTEXAI=false must not route a Google-typed custom
// provider with an explicit base_url and token_key to the Vertex/ADC path,
// where token_key is ignored. With the flag disabled the client must use the
// direct Gemini API path and send the token_key value as the API key.
func TestNewClient_VertexFlagFalseUsesDirectAPI(t *testing.T) {
	t.Parallel()

	for _, flag := range []string{"false", "FALSE", "0", ""} {
		t.Run("flag="+flag, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var seen []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen = append(seen, r.Header.Get("X-Goog-Api-Key"))
				mu.Unlock()
				writeGeminiSSEResponse(w)
			}))
			t.Cleanup(server.Close)

			cfg := &latest.ModelConfig{
				Provider: "google",
				Model:    "gemini-3.5-flash",
				BaseURL:  server.URL,
				TokenKey: "CUSTOM_GEMINI_KEY",
			}
			env := environment.NewMapEnvProvider(map[string]string{
				"GOOGLE_GENAI_USE_VERTEXAI": flag,
				"CUSTOM_GEMINI_KEY":         "custom-key",
			})

			client, err := NewClient(t.Context(), cfg, env)
			require.NoError(t, err)

			stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
			require.NoError(t, err)
			defer stream.Close()
			for {
				if _, err := stream.Recv(); err != nil {
					break
				}
			}

			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, []string{"custom-key"}, seen, "request must reach the custom base_url with the token_key API key")
		})
	}
}

// TestNewClient_VertexFlagRouting verifies the two sides of the flag: a
// truthy value keeps the Vertex/ADC path (no API key required), while a
// falsy value falls back to the direct path (missing token_key is reported).
func TestNewClient_VertexFlagRouting(t *testing.T) {
	t.Parallel()

	t.Run("truthy flag ignores missing token_key", func(t *testing.T) {
		t.Parallel()
		for _, flag := range []string{"true", "1"} {
			cfg := &latest.ModelConfig{
				Provider: "google",
				Model:    "gemini-3.5-flash",
				BaseURL:  "https://example.invalid",
				TokenKey: "MISSING_KEY",
			}
			env := environment.NewMapEnvProvider(map[string]string{
				"GOOGLE_GENAI_USE_VERTEXAI": flag,
			})
			_, err := NewClient(t.Context(), cfg, env)
			assert.NoError(t, err, "flag %q must take the Vertex path, which ignores token_key", flag)
		}
	})

	t.Run("falsy flag reports missing token_key", func(t *testing.T) {
		t.Parallel()
		for _, flag := range []string{"false", "0", ""} {
			cfg := &latest.ModelConfig{
				Provider: "google",
				Model:    "gemini-3.5-flash",
				BaseURL:  "https://example.invalid",
				TokenKey: "MISSING_KEY",
			}
			env := environment.NewMapEnvProvider(map[string]string{
				"GOOGLE_GENAI_USE_VERTEXAI": flag,
			})
			_, err := NewClient(t.Context(), cfg, env)
			require.EqualError(t, err, "MISSING_KEY environment variable is required", "flag %q must take the direct path", flag)
		}
	})
}
