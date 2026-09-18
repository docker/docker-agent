package gemini

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// geminiCountingTransport wraps a base RoundTripper and counts RoundTrip calls.
type geminiCountingTransport struct {
	base  http.RoundTripper
	calls atomic.Int64
}

func (c *geminiCountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return c.base.RoundTrip(req)
}

// writeGeminiSSEResponse writes a minimal valid Gemini streaming response.
func writeGeminiSSEResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)

	payload := `{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	if flusher != nil {
		flusher.Flush()
	}
}

func TestNewClient_TransportWrapperInvokedDirectPath(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeGeminiSSEResponse(w)
	}))
	defer server.Close()

	var counter geminiCountingTransport

	cfg := &latest.ModelConfig{
		Provider: "google",
		Model:    "gemini-2.0-flash",
		BaseURL:  server.URL,
	}
	env := environment.NewMapEnvProvider(map[string]string{
		"GOOGLE_API_KEY": "test-key",
	})

	client, err := NewClient(t.Context(), cfg, env,
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
	defer stream.Close()

	// Drain the stream so RoundTrip has been fully exercised.
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	assert.Positive(t, counter.calls.Load(), "transport wrapper RoundTrip should have been called at least once")
}

func TestNewClient_TransportWrapperInvokedGatewayPath(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeGeminiSSEResponse(w)
	}))
	defer server.Close()

	var counter geminiCountingTransport

	cfg := &latest.ModelConfig{
		Provider: "google",
		Model:    "gemini-2.0-flash",
	}
	// server.URL is 127.0.0.1 which IsTrustedDockerURL considers trusted,
	// so we must supply the Docker Desktop token.
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
	defer stream.Close()

	// Drain the stream so RoundTrip has been fully exercised.
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	assert.Positive(t, counter.calls.Load(), "transport wrapper RoundTrip should have been called at least once in gateway path")
}

// TestNewClient_TransportWrapperVertexAIFallsBackToGeminiAPI verifies that when
// project/location are configured (Vertex AI) but a transport wrapper is also
// set, the client automatically falls back to BackendGeminiAPI so the wrapper
// can be applied. This mirrors the WebSocket→SSE fallback in the OpenAI provider.
func TestNewClient_TransportWrapperVertexAIFallsBackToGeminiAPI(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeGeminiSSEResponse(w)
	}))
	defer server.Close()

	var counter geminiCountingTransport

	cfg := &latest.ModelConfig{
		Provider: "google",
		Model:    "gemini-2.0-flash",
		BaseURL:  server.URL,
		ProviderOpts: map[string]any{
			"project":  "test-project",
			"location": "us-central1",
		},
	}
	// GOOGLE_API_KEY is required for the GeminiAPI fallback path.
	env := environment.NewMapEnvProvider(map[string]string{
		"GOOGLE_API_KEY": "test-key",
	})

	client, err := NewClient(t.Context(), cfg, env,
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
	defer stream.Close()

	// Drain the stream so RoundTrip has been fully exercised.
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	assert.Positive(t, counter.calls.Load(), "transport wrapper RoundTrip should have been called at least once (GeminiAPI fallback)")
}

// With an explicit token source, Vertex AI stays the backend even with a
// transport wrapper: we own the HTTP client, no ADC lookup runs, and the
// token is resolved per request.
func TestNewClient_VertexAIExplicitTokenSource(t *testing.T) {
	t.Parallel()
	var got []*http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Clone(r.Context()))
		writeGeminiSSEResponse(w)
	}))
	defer server.Close()

	cfg := &latest.ModelConfig{
		Provider: "google",
		Model:    "gemini-2.0-flash",
		BaseURL:  server.URL,
		ProviderOpts: map[string]any{
			"project":  "test-project",
			"location": "us-central1",
		},
	}
	calls := 0
	var counter geminiCountingTransport
	client, err := NewClient(t.Context(), cfg, environment.NewNoEnvProvider(),
		options.WithTokenSource(func(context.Context) (string, error) {
			calls++
			return fmt.Sprintf("token-%d", calls), nil
		}),
		options.WithHTTPTransportWrapper(func(base http.RoundTripper) http.RoundTripper {
			counter.base = base
			return &counter
		}),
	)
	require.NoError(t, err)
	assert.Equal(t, apiSurfaceVertexAI, client.apiSurface)
	assert.Equal(t, 1, calls, "the token is checked once at construction")

	for range 2 {
		stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
		require.NoError(t, err)
		for {
			if _, err := stream.Recv(); err != nil {
				break
			}
		}
		stream.Close()
	}

	require.Len(t, got, 2)
	for i, req := range got {
		assert.Contains(t, req.URL.Path, "/projects/test-project/locations/us-central1/publishers/google/models/gemini-2.0-flash:")
		assert.Equal(t, fmt.Sprintf("Bearer token-%d", i+2), req.Header.Get("Authorization"))
	}
	assert.EqualValues(t, 2, counter.calls.Load(), "transport wrapper applies on the Vertex AI path")
}

func TestNewClient_VertexAIExplicitTokenSourceFailsFast(t *testing.T) {
	t.Parallel()
	cfg := &latest.ModelConfig{
		Provider:     "google",
		Model:        "gemini-2.0-flash",
		ProviderOpts: map[string]any{"project": "test-project", "location": "us-central1"},
	}
	_, err := NewClient(t.Context(), cfg, environment.NewNoEnvProvider(),
		options.WithTokenSource(func(context.Context) (string, error) { return "", errors.New("no token") }))
	require.ErrorContains(t, err, "resolving access token: no token")
}
