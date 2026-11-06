package gemini

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/testutil/geminiintegration"
)

func TestDirectAPIKeyCatalogAlternatives(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{Provider: "google", TokenKey: "GOOGLE_API_KEY"}
	env := environment.NewMapEnvProvider(map[string]string{
		"GEMINI_API_KEY":            "alternative-key",
		"GOOGLE_GENAI_USE_VERTEXAI": "true",
	})
	key, err := directAPIKey(t.Context(), cfg, env)
	require.NoError(t, err)
	assert.Equal(t, "alternative-key", key)

	cfg.TokenKey = "CUSTOM_KEY"
	_, err = directAPIKey(t.Context(), cfg, env)
	require.ErrorContains(t, err, "CUSTOM_KEY")

	cfg.TokenKey = "GOOGLE_API_KEY"
	_, err = directAPIKey(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{
		"GOOGLE_GENAI_USE_VERTEXAI": "true",
	}))
	require.Error(t, err)
}

func TestNewClient_TokenKey(t *testing.T) {
	t.Parallel()
	type testCase struct {
		name     string
		tokenKey string
		env      map[string]string
		wantKey  string
		wantErr  string
	}
	tests := []testCase{
		{
			name:     "token_key wins over the native variable",
			tokenKey: "OPENCODE_API_KEY",
			env:      map[string]string{"OPENCODE_API_KEY": "opencode-key", "GOOGLE_API_KEY": "google-key"},
			wantKey:  "opencode-key",
		},
		{
			name:     "documented OpenCode config with only its own variable",
			tokenKey: "OPENCODE_API_KEY",
			env:      map[string]string{"OPENCODE_API_KEY": "opencode-key"},
			wantKey:  "opencode-key",
		},
		{
			name:     "missing token_key variable names it",
			tokenKey: "OPENCODE_API_KEY",
			env:      map[string]string{"GOOGLE_API_KEY": "google-key"},
			wantErr:  "OPENCODE_API_KEY environment variable is required",
		},
		{
			name:     "empty token_key variable does not fall back",
			tokenKey: "OPENCODE_API_KEY",
			env:      map[string]string{"OPENCODE_API_KEY": "", "GOOGLE_API_KEY": "google-key"},
			wantErr:  "OPENCODE_API_KEY environment variable is required",
		},
		{
			name:    "no token_key keeps GOOGLE_API_KEY over GEMINI_API_KEY",
			env:     map[string]string{"GEMINI_API_KEY": "gemini-key", "GOOGLE_API_KEY": "google-key"},
			wantKey: "google-key",
		},
		{
			name:    "no token_key accepts GEMINI_API_KEY alone",
			env:     map[string]string{"GEMINI_API_KEY": "gemini-key"},
			wantKey: "gemini-key",
		},
		{
			name:    "no token_key and no native variable",
			env:     map[string]string{},
			wantErr: "GOOGLE_API_KEY or GEMINI_API_KEY environment variable is required",
		},
	}
	for _, value := range []string{"false", "False", "FALSE", "0", "", "invalid"} {
		tests = append(tests, testCase{
			name:     "disabled Vertex flag " + value,
			tokenKey: "OPENCODE_API_KEY",
			env:      map[string]string{"OPENCODE_API_KEY": "opencode-key", "GOOGLE_GENAI_USE_VERTEXAI": value},
			wantKey:  "opencode-key",
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantErr == "" {
				geminiintegration.Require(t)
			}
			t.Parallel()
			var mu sync.Mutex
			var seen []string
			cfg := &latest.ModelConfig{Provider: "google", Model: "gemini-3.5-flash", TokenKey: tt.tokenKey}
			if tt.wantErr == "" {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					seen = append(seen, r.Header.Get("X-Goog-Api-Key"))
					mu.Unlock()
					writeGeminiSSEResponse(w)
				}))
				t.Cleanup(server.Close)
				cfg.BaseURL = server.URL
			}
			client, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(tt.env))
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
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
			assert.Equal(t, []string{tt.wantKey}, seen)
		})
	}
}

func TestNewClient_VertexAIFlag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		flag string
		opts map[string]any
	}{
		{name: "true", flag: "true"},
		{name: "uppercase true", flag: "TRUE"},
		{name: "one", flag: "1"},
		{name: "explicit options override false", flag: "false", opts: map[string]any{"project": "test-project", "location": "us-central1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash", ProviderOpts: tt.opts}
			env := environment.NewMapEnvProvider(map[string]string{
				"GOOGLE_GENAI_USE_VERTEXAI": tt.flag,
				"GOOGLE_CLOUD_PROJECT":      "test-project",
				"GOOGLE_CLOUD_LOCATION":     "us-central1",
			})
			client, err := NewClient(t.Context(), cfg, env,
				options.WithTokenSource(func(context.Context) (string, error) { return "test-token", nil }))
			require.NoError(t, err)
			assert.Equal(t, apiSurfaceVertexAI, client.apiSurface)
		})
	}
}
