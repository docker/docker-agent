package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func startOpenCodeCapture(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get(base.OpenCodeSessionHeader))
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

// redirectTo lets a client configured for opencode.ai hit a local server and
// records the header the wrapper itself saw.
func redirectTo(t *testing.T, target string) (options.Opt, func() []string) {
	t.Helper()
	u, err := url.Parse(target)
	require.NoError(t, err)
	r := &redirectTransport{to: u}
	opt := options.WithHTTPTransportWrapper(func(next http.RoundTripper) http.RoundTripper {
		r.next = next
		return r
	})
	return opt, func() []string {
		r.mu.Lock()
		defer r.mu.Unlock()
		return append([]string(nil), r.seen...)
	}
}

type redirectTransport struct {
	next http.RoundTripper
	to   *url.URL
	mu   sync.Mutex
	seen []string
}

func (r *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.seen = append(r.seen, req.Header.Get(base.OpenCodeSessionHeader))
	r.mu.Unlock()
	req = req.Clone(req.Context())
	req.URL.Scheme = r.to.Scheme
	req.URL.Host = r.to.Host
	req.Host = r.to.Host
	return r.next.RoundTrip(req)
}

func newOpenCodeTestClient(t *testing.T, cfg *latest.ModelConfig, opts ...options.Opt) *Client {
	t.Helper()
	env := environment.NewMapEnvProvider(map[string]string{"OPENCODE_API_KEY": "test-key"})
	client, err := NewClient(t.Context(), cfg, env, opts...)
	require.NoError(t, err)
	return client
}

func streamOnce(t *testing.T, ctx context.Context, client *Client) {
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
	t.Parallel()
	server, seen := startOpenCodeCapture(t)
	redirect, _ := redirectTo(t, server.URL)
	client := newOpenCodeTestClient(t, &latest.ModelConfig{
		Provider: "opencode-go",
		Model:    "deepseek-v4-flash",
		BaseURL:  "https://opencode.ai/zen/go/v1",
		TokenKey: "OPENCODE_API_KEY",
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
		},
	}, redirect)

	ctxA := httpclient.ContextWithSessionID(t.Context(), "conversation-a")
	ctxB := httpclient.ContextWithSessionID(t.Context(), "conversation-b")
	streamOnce(t, ctxA, client)
	streamOnce(t, ctxA, client)
	streamOnce(t, ctxB, client)

	got := seen()
	require.Len(t, got, 3)
	_, err := uuid.Parse(got[0])
	require.NoError(t, err, "header value must be a UUID")
	assert.NotEqual(t, "conversation-a", got[0], "raw session ID must not leak")
	assert.Equal(t, got[0], got[1], "one conversation must keep one stable ID across requests")
	assert.NotEqual(t, got[0], got[2], "distinct conversations on a shared client must not share an ID")
}

func TestOpenCodeSessionHeaderOnCustomProvider(t *testing.T) {
	t.Parallel()
	server, seen := startOpenCodeCapture(t)
	redirect, wrapperSaw := redirectTo(t, server.URL)
	client := newOpenCodeTestClient(t, &latest.ModelConfig{
		Provider: "openai",
		Model:    "kimi-k2.6",
		BaseURL:  "https://opencode.ai/zen/v1",
		TokenKey: "OPENCODE_API_KEY",
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
		},
	}, redirect)

	ctxA := httpclient.ContextWithSessionID(t.Context(), "conversation-a")
	streamOnce(t, ctxA, client)
	streamOnce(t, httpclient.ContextWithSessionID(t.Context(), "conversation-b"), client)

	got := seen()
	require.Len(t, got, 2)
	_, err := uuid.Parse(got[0])
	require.NoError(t, err, "header value must be a UUID")
	assert.NotEqual(t, got[0], got[1], "distinct conversations on a shared client must not share an ID")
	assert.Equal(t, got, wrapperSaw(), "a registered transport wrapper must see the header on every request")
}

func TestOpenCodeSessionHeaderFallsBackWithoutSession(t *testing.T) {
	t.Parallel()
	server, seen := startOpenCodeCapture(t)
	redirect, _ := redirectTo(t, server.URL)
	client := newOpenCodeTestClient(t, &latest.ModelConfig{
		Provider: "opencode-zen",
		Model:    "gpt-5",
		BaseURL:  "https://opencode.ai/zen/v1",
		TokenKey: "OPENCODE_API_KEY",
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
		},
	}, redirect)

	streamOnce(t, t.Context(), client)
	streamOnce(t, t.Context(), client)

	got := seen()
	require.Len(t, got, 2)
	require.NotEmpty(t, got[0], "header must still be sent when no session is on the context")
	_, err := uuid.Parse(got[0])
	require.NoError(t, err)
	assert.Equal(t, got[0], got[1], "fallback ID must be stable for the client's lifetime")
}

func TestOpenCodeSessionHeaderUserOverrideWins(t *testing.T) {
	t.Parallel()
	server, seen := startOpenCodeCapture(t)
	redirect, _ := redirectTo(t, server.URL)
	client := newOpenCodeTestClient(t, &latest.ModelConfig{
		Provider: "opencode-go",
		Model:    "deepseek-v4-flash",
		BaseURL:  "https://opencode.ai/zen/go/v1",
		TokenKey: "OPENCODE_API_KEY",
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
			"http_headers": map[string]any{
				"X-OpenCode-Session": "pinned-by-user",
			},
		},
	}, redirect)

	streamOnce(t, httpclient.ContextWithSessionID(t.Context(), "conversation-a"), client)

	got := seen()
	require.Len(t, got, 1)
	assert.Equal(t, "pinned-by-user", got[0])
}

func TestOpenCodeSessionHeaderNotSentToOtherProviders(t *testing.T) {
	t.Parallel()
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

	streamOnce(t, httpclient.ContextWithSessionID(t.Context(), "conversation-a"), client)

	got := seen()
	require.Len(t, got, 1)
	assert.Empty(t, got[0], "session identifiers must not leak to unrelated providers")
}

func TestOpenCodeSessionHeaderNotSentThroughGateway(t *testing.T) {
	t.Parallel()
	server, seen := startOpenCodeCapture(t)
	// 127.0.0.1 counts as a trusted Docker URL, so a Desktop token is required.
	env := environment.NewMapEnvProvider(map[string]string{
		environment.DockerDesktopTokenEnv: "test-dd-token",
	})
	client, err := NewClient(t.Context(), &latest.ModelConfig{
		Provider: "opencode-go",
		Model:    "deepseek-v4-flash",
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
		},
	}, env, options.WithGateway(server.URL))
	require.NoError(t, err)

	streamOnce(t, httpclient.ContextWithSessionID(t.Context(), "conversation-a"), client)

	got := seen()
	require.Len(t, got, 1)
	assert.Empty(t, got[0], "gateway requests carry the gateway's own session header instead")
}

func TestOpenCodeSessionHeaderNotSentToAliasWithOtherBaseURL(t *testing.T) {
	t.Parallel()
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

	streamOnce(t, httpclient.ContextWithSessionID(t.Context(), "conversation-a"), client)

	got := seen()
	require.Len(t, got, 1)
	assert.Empty(t, got[0], "an alias pointed at another host must not receive the session header")
}

func TestOpenCodeWebSocketFallsBackToSSE(t *testing.T) {
	t.Parallel()
	client := newOpenCodeTestClient(t, &latest.ModelConfig{
		Provider: "opencode-zen",
		Model:    "gpt-5",
		BaseURL:  "https://opencode.ai/zen/v1",
		TokenKey: "OPENCODE_API_KEY",
		ProviderOpts: map[string]any{
			"api_type":  "openai_responses",
			"transport": "websocket",
		},
	})

	assert.Nil(t, client.wsPool, "transport=websocket must be ignored for OpenCode")
}
