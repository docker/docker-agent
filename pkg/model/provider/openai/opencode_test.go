package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
)

func TestIsOpenCodeProvider(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  *latest.ModelConfig
		want bool
	}{
		{name: "nil config", cfg: nil, want: false},
		{name: "opencode-go alias", cfg: &latest.ModelConfig{Provider: "opencode-go"}, want: true},
		{name: "opencode-zen alias", cfg: &latest.ModelConfig{Provider: "opencode-zen"}, want: true},
		{
			name: "custom provider on opencode.ai",
			cfg:  &latest.ModelConfig{Provider: "custom", BaseURL: "https://opencode.ai/zen/go/v1"},
			want: true,
		},
		{
			name: "custom provider on opencode.ai subdomain",
			cfg:  &latest.ModelConfig{Provider: "custom", BaseURL: "https://eu.opencode.ai/zen/v1"},
			want: true,
		},
		{
			name: "lookalike host is not opencode",
			cfg:  &latest.ModelConfig{Provider: "custom", BaseURL: "https://notopencode.ai/v1"},
			want: false,
		},
		{
			name: "opencode.ai in path only",
			cfg:  &latest.ModelConfig{Provider: "custom", BaseURL: "https://evil.example/opencode.ai/v1"},
			want: false,
		},
		{name: "openai", cfg: &latest.ModelConfig{Provider: "openai"}, want: false},
		{name: "github-copilot", cfg: &latest.ModelConfig{Provider: "github-copilot", BaseURL: "https://api.githubcopilot.com"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isOpenCodeProvider(tt.cfg))
		})
	}
}

func TestOpenCodeSessionIDIsStableAndOpaque(t *testing.T) {
	t.Parallel()
	a := opencodeSessionID("session-a")
	b := opencodeSessionID("session-b")

	assert.Equal(t, a, opencodeSessionID("session-a"), "same session must map to the same header value")
	assert.NotEqual(t, a, b, "different sessions must map to different header values")
	assert.NotEqual(t, "session-a", a, "raw session ID must not leak")
	_, err := uuid.Parse(a)
	require.NoError(t, err, "header value must be a UUID")
}

// startOpenCodeCapture returns a fake OpenAI-compatible endpoint that records
// the x-opencode-session header of every request.
func startOpenCodeCapture(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get(opencodeSessionHeader))
		mu.Unlock()
		writeSSEResponse(w)
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func newOpenCodeTestClient(t *testing.T, cfg *latest.ModelConfig) *Client {
	t.Helper()
	env := environment.NewMapEnvProvider(map[string]string{"OPENCODE_API_KEY": "test-key"})
	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)
	return client
}

func streamOnce(t *testing.T, client *Client, ctx context.Context) {
	t.Helper()
	stream, err := client.CreateChatCompletionStream(ctx, []chat.Message{
		{Role: chat.MessageRoleUser, Content: "hello"},
	}, nil)
	require.NoError(t, err)
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
}

func TestOpenCodeSessionHeaderDerivedFromContext(t *testing.T) {
	server, seen := startOpenCodeCapture(t)
	client := newOpenCodeTestClient(t, &latest.ModelConfig{
		Provider: "opencode-go",
		Model:    "deepseek-v4-flash",
		BaseURL:  server.URL,
		TokenKey: "OPENCODE_API_KEY",
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
		},
	})

	ctxA := httpclient.ContextWithSessionID(t.Context(), "conversation-a")
	ctxB := httpclient.ContextWithSessionID(t.Context(), "conversation-b")
	streamOnce(t, client, ctxA)
	streamOnce(t, client, ctxA)
	streamOnce(t, client, ctxB)

	got := seen()
	require.Len(t, got, 3)
	assert.Equal(t, opencodeSessionID("conversation-a"), got[0])
	assert.Equal(t, got[0], got[1], "one conversation must keep one stable ID across requests")
	assert.Equal(t, opencodeSessionID("conversation-b"), got[2])
	assert.NotEqual(t, got[0], got[2], "distinct conversations on a shared client must not share an ID")
}

func TestOpenCodeSessionHeaderFallsBackWithoutSession(t *testing.T) {
	server, seen := startOpenCodeCapture(t)
	client := newOpenCodeTestClient(t, &latest.ModelConfig{
		Provider: "opencode-zen",
		Model:    "gpt-5",
		BaseURL:  server.URL,
		TokenKey: "OPENCODE_API_KEY",
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
		},
	})

	streamOnce(t, client, t.Context())
	streamOnce(t, client, t.Context())

	got := seen()
	require.Len(t, got, 2)
	require.NotEmpty(t, got[0], "header must still be sent when no session is on the context")
	_, err := uuid.Parse(got[0])
	require.NoError(t, err)
	assert.Equal(t, got[0], got[1], "fallback ID must be stable for the client's lifetime")
}

func TestOpenCodeSessionHeaderUserOverrideWins(t *testing.T) {
	server, seen := startOpenCodeCapture(t)
	client := newOpenCodeTestClient(t, &latest.ModelConfig{
		Provider: "opencode-go",
		Model:    "deepseek-v4-flash",
		BaseURL:  server.URL,
		TokenKey: "OPENCODE_API_KEY",
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
			"http_headers": map[string]any{
				"X-OpenCode-Session": "pinned-by-user",
			},
		},
	})

	streamOnce(t, client, httpclient.ContextWithSessionID(t.Context(), "conversation-a"))

	got := seen()
	require.Len(t, got, 1)
	assert.Equal(t, "pinned-by-user", got[0])
}

func TestOpenCodeSessionHeaderNotSentToOtherProviders(t *testing.T) {
	server, seen := startOpenCodeCapture(t)
	env := environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test-key"})
	client, err := NewClient(t.Context(), &latest.ModelConfig{
		Provider: "openai",
		Model:    "gpt-4o",
		BaseURL:  server.URL,
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
		},
	}, env)
	require.NoError(t, err)

	streamOnce(t, client, httpclient.ContextWithSessionID(t.Context(), "conversation-a"))

	got := seen()
	require.Len(t, got, 1)
	assert.Empty(t, got[0], "session identifiers must not leak to unrelated providers")
}
