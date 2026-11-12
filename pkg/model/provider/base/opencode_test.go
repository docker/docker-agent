package base

import (
	"context"
	"net/http"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/httpclient"
)

// Alias configs as the clients receive them, base URL already resolved.
var (
	opencodeGoCfg  = &latest.ModelConfig{Provider: "opencode-go", BaseURL: "https://opencode.ai/zen/go/v1"}
	opencodeZenCfg = &latest.ModelConfig{Provider: "opencode-zen", BaseURL: "https://opencode.ai/zen/v1"}
)

func TestIsOpenCodeProvider(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  *latest.ModelConfig
		want bool
	}{
		{name: "nil config", cfg: nil, want: false},
		{name: "opencode-go alias", cfg: opencodeGoCfg, want: true},
		{name: "opencode-zen alias", cfg: opencodeZenCfg, want: true},
		{
			name: "alias pointed at another host",
			cfg:  &latest.ModelConfig{Provider: "opencode-go", BaseURL: "https://proxy.example/v1"},
			want: false,
		},
		{
			name: "alias name alone is not enough",
			cfg:  &latest.ModelConfig{Provider: "opencode-go"},
			want: false,
		},
		{
			name: "custom provider on opencode.ai",
			cfg:  &latest.ModelConfig{Provider: "custom", BaseURL: "https://opencode.ai/zen/go/v1"},
			want: true,
		},
		{
			name: "custom anthropic provider on opencode.ai",
			cfg:  &latest.ModelConfig{Provider: "anthropic", BaseURL: "https://opencode.ai/zen"},
			want: true,
		},
		{
			name: "custom google provider on opencode.ai",
			cfg:  &latest.ModelConfig{Provider: "google", BaseURL: "https://opencode.ai/zen"},
			want: true,
		},
		{
			name: "custom provider on opencode.ai subdomain",
			cfg:  &latest.ModelConfig{Provider: "custom", BaseURL: "https://eu.opencode.ai/zen/v1"},
			want: true,
		},
		{
			name: "host case and port are ignored",
			cfg:  &latest.ModelConfig{Provider: "custom", BaseURL: "https://OpenCode.AI:443/zen/v1"},
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
		{name: "anthropic", cfg: &latest.ModelConfig{Provider: "anthropic"}, want: false},
		{name: "github-copilot", cfg: &latest.ModelConfig{Provider: "github-copilot", BaseURL: "https://api.githubcopilot.com"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, IsOpenCodeProvider(tt.cfg))
		})
	}
}

func TestOpenCodeSessionIDIsStableAndOpaque(t *testing.T) {
	t.Parallel()
	a := opencodeSessionID("conversation-a")
	b := opencodeSessionID("conversation-b")

	// Pinned: a namespace change would rotate every resumed session's ID.
	assert.Equal(t, "58407f8e-c204-56ce-8fcf-8ae43bcc4a8c", a)
	assert.NotEqual(t, a, b, "different sessions must map to different header values")
	assert.NotEqual(t, "conversation-a", a, "raw session ID must not leak")
	_, err := uuid.Parse(b)
	require.NoError(t, err, "header value must be a UUID")
}

type headerRecorder struct {
	seen []string
}

func (r *headerRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.seen = append(r.seen, req.Header.Get(OpenCodeSessionHeader))
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

func TestWrapOpenCodeSessionOnlyWrapsOpenCodeClients(t *testing.T) {
	t.Parallel()
	rec := &headerRecorder{}

	other := &http.Client{Transport: rec}
	WrapOpenCodeSession(&latest.ModelConfig{Provider: "anthropic", BaseURL: "https://api.anthropic.com"}, other)
	assert.Same(t, rec, other.Transport, "non-OpenCode clients must keep their transport")

	opencode := &http.Client{Transport: rec}
	WrapOpenCodeSession(&latest.ModelConfig{Provider: "anthropic", BaseURL: "https://opencode.ai/zen"}, opencode)
	require.IsType(t, &opencodeSessionTransport{}, opencode.Transport)
	assert.Same(t, rec, opencode.Transport.(*opencodeSessionTransport).base)

	bare := &http.Client{}
	WrapOpenCodeSession(opencodeGoCfg, bare)
	require.IsType(t, &opencodeSessionTransport{}, bare.Transport)
	assert.Same(t, http.DefaultTransport, bare.Transport.(*opencodeSessionTransport).base, "a nil transport means the default one")

	assert.NotPanics(t, func() { WrapOpenCodeSession(opencodeGoCfg, nil) })
}

func TestOpenCodeSessionTransportSetsHeaderPerSession(t *testing.T) {
	t.Parallel()
	rec := &headerRecorder{}
	client := &http.Client{Transport: rec}
	WrapOpenCodeSession(opencodeGoCfg, client)

	do := func(ctx context.Context) *http.Request {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://opencode.ai/zen/go/v1/chat/completions", http.NoBody)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		return req
	}

	ctxA := httpclient.ContextWithSessionID(t.Context(), "conversation-a")
	ctxB := httpclient.ContextWithSessionID(t.Context(), "conversation-b")
	original := do(ctxA)
	do(ctxA)
	do(ctxB)
	do(t.Context())
	do(t.Context())

	require.Len(t, rec.seen, 5)
	assert.Equal(t, opencodeSessionID("conversation-a"), rec.seen[0])
	assert.Equal(t, rec.seen[0], rec.seen[1], "one conversation must keep one stable ID across requests")
	assert.Equal(t, opencodeSessionID("conversation-b"), rec.seen[2])
	assert.NotEmpty(t, rec.seen[3], "header must still be sent when no session is on the context")
	assert.Equal(t, rec.seen[3], rec.seen[4], "fallback ID must be stable for the client's lifetime")
	assert.NotEqual(t, rec.seen[0], rec.seen[3], "fallback must not collide with a derived ID")
	fallback, err := uuid.Parse(rec.seen[3])
	require.NoError(t, err)
	assert.Equal(t, byte(4), fallback[6]>>4, "fallback must remain a v4 UUID")
	assert.Empty(t, original.Header.Get(OpenCodeSessionHeader), "the caller's request must not be modified")
}

func TestOpenCodeSessionTransportKeepsPinnedHeader(t *testing.T) {
	t.Parallel()
	rec := &headerRecorder{}
	client := &http.Client{Transport: rec}
	WrapOpenCodeSession(opencodeZenCfg, client)

	ctx := httpclient.ContextWithSessionID(t.Context(), "conversation-a")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://opencode.ai/zen/v1/responses", http.NoBody)
	require.NoError(t, err)
	req.Header.Set(OpenCodeSessionHeader, "pinned-by-user")
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Len(t, rec.seen, 1)
	assert.Equal(t, "pinned-by-user", rec.seen[0])
}
