package anthropic

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

func TestNewClient_TokenKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		tokenKey string
		env      map[string]string
		wantKey  string
		wantErr  string
	}{
		{
			name:     "token_key wins over the native variable",
			tokenKey: "OPENCODE_API_KEY",
			env:      map[string]string{"OPENCODE_API_KEY": "opencode-key", "ANTHROPIC_API_KEY": "anthropic-key"},
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
			env:      map[string]string{"ANTHROPIC_API_KEY": "anthropic-key"},
			wantErr:  "OPENCODE_API_KEY environment variable is required",
		},
		{
			name:     "empty token_key variable does not fall back",
			tokenKey: "OPENCODE_API_KEY",
			env:      map[string]string{"OPENCODE_API_KEY": "", "ANTHROPIC_API_KEY": "anthropic-key"},
			wantErr:  "OPENCODE_API_KEY environment variable is required",
		},
		{
			name:    "no token_key keeps ANTHROPIC_API_KEY",
			env:     map[string]string{"ANTHROPIC_API_KEY": "anthropic-key"},
			wantKey: "anthropic-key",
		},
		{
			name:    "no token_key and no native variable",
			env:     map[string]string{},
			wantErr: "ANTHROPIC_API_KEY environment variable is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var seen []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen = append(seen, r.Header.Get("X-Api-Key"))
				mu.Unlock()
				writeMinimalAnthropicSSE(w)
			}))
			t.Cleanup(server.Close)

			cfg := &latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-6", BaseURL: server.URL, TokenKey: tt.tokenKey}
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
