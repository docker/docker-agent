package gemini

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
		writeGeminiSSEResponse(w)
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

func TestOpenCodeSessionHeaderOnCustomProvider(t *testing.T) {
	t.Parallel()
	server, seen := startOpenCodeCapture(t)
	redirect, wrapperSaw := redirectTo(t, server.URL)
	cfg := &latest.ModelConfig{
		Provider: "google",
		Model:    "gemini-3.5-flash",
		BaseURL:  "https://opencode.ai/zen",
	}
	env := environment.NewMapEnvProvider(map[string]string{"GOOGLE_API_KEY": "test-key"})
	client, err := NewClient(t.Context(), cfg, env, redirect)
	require.NoError(t, err)

	ctxA := httpclient.ContextWithSessionID(t.Context(), "conversation-a")
	ctxB := httpclient.ContextWithSessionID(t.Context(), "conversation-b")
	streamOnce(t, ctxA, client)
	streamOnce(t, ctxA, client)
	streamOnce(t, ctxB, client)

	got := seen()
	require.Len(t, got, 3)
	_, err = uuid.Parse(got[0])
	require.NoError(t, err, "header value must be a UUID")
	assert.NotEqual(t, "conversation-a", got[0], "raw session ID must not leak")
	assert.Equal(t, got[0], got[1], "one conversation must keep one stable ID across requests")
	assert.NotEqual(t, got[0], got[2], "distinct conversations on a shared client must not share an ID")
	assert.Equal(t, got, wrapperSaw(), "a registered transport wrapper must see the header on every request")
}

func TestOpenCodeSessionHeaderNotSentToGemini(t *testing.T) {
	t.Parallel()
	server, seen := startOpenCodeCapture(t)
	cfg := &latest.ModelConfig{
		Provider: "google",
		Model:    "gemini-3.5-flash",
		BaseURL:  server.URL,
	}
	env := environment.NewMapEnvProvider(map[string]string{"GOOGLE_API_KEY": "test-key"})
	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	streamOnce(t, httpclient.ContextWithSessionID(t.Context(), "conversation-a"), client)

	got := seen()
	require.Len(t, got, 1)
	assert.Empty(t, got[0], "session identifiers must not leak to unrelated providers")
}
