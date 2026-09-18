package vertex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// recordingWrapper is an HTTP transport wrapper that records the requests
// reaching the wire and answers each with an empty message stream.
func recordingWrapper(got *[]*http.Request) options.Opt {
	return options.WithHTTPTransportWrapper(func(http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			*got = append(*got, req)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")),
				Request:    req,
			}, nil
		})
	})
}

func drain(t *testing.T, stream chat.MessageStream, err error) {
	t.Helper()
	require.NoError(t, err)
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
	stream.Close()
}

// With an explicit token source, no ADC lookup happens: the token is resolved
// on every request, from the request context, and reaches the Vertex endpoint
// through our own transport without the SDK's OAuth wrapper overriding it.
func TestNewClientWithTokenSource(t *testing.T) {
	t.Parallel()

	var got []*http.Request
	var sessions []string
	source := func(ctx context.Context) (string, error) {
		sessions = append(sessions, httpclient.SessionIDFromContext(ctx))
		return fmt.Sprintf("token-%d", len(sessions)), nil
	}
	cfg := &latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-6"}
	env := environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "must-not-leak"})
	client, err := NewClientWithTokenSource(t.Context(), cfg, env, "test-project", "us-east5", source, recordingWrapper(&got))
	require.NoError(t, err)
	assert.Equal(t, []string{""}, sessions, "the token is checked once at construction")

	messages := []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}
	for _, session := range []string{"session-a", "session-b"} {
		ctx := httpclient.ContextWithSessionID(t.Context(), session)
		stream, err := client.CreateChatCompletionStream(ctx, messages, nil)
		drain(t, stream, err)
	}

	assert.Equal(t, []string{"", "session-a", "session-b"}, sessions)
	require.Len(t, got, 2)
	for i, req := range got {
		assert.Equal(t, "us-east5-aiplatform.googleapis.com", req.URL.Host)
		assert.Equal(t, "/v1/projects/test-project/locations/us-east5/publishers/anthropic/models/claude-sonnet-4-6:streamRawPredict", req.URL.Path)
		assert.Equal(t, fmt.Sprintf("Bearer token-%d", i+2), req.Header.Get("Authorization"))
		assert.Empty(t, req.Header.Get("X-Api-Key"))
	}
}

// A cancelled request context reaches the token source, and its error fails
// the request before anything is sent.
func TestNewClientWithTokenSource_RequestContext(t *testing.T) {
	t.Parallel()

	var got []*http.Request
	var sourceErr error
	source := func(ctx context.Context) (string, error) {
		sourceErr = ctx.Err()
		if sourceErr != nil {
			return "", sourceErr
		}
		return "token", nil
	}
	cfg := &latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-6"}
	client, err := NewClientWithTokenSource(t.Context(), cfg, environment.NewMapEnvProvider(nil), "test-project", "us-east5", source, recordingWrapper(&got))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stream, err := client.CreateChatCompletionStream(ctx, []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
	if err == nil {
		_, err = stream.Recv()
		stream.Close()
	}
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, sourceErr, context.Canceled, "the source saw the cancelled request context")
	assert.Empty(t, got)
}

// Options are applied exactly once even though the token-source path needs
// them inside the SDK client factory.
func TestNewClientWithTokenSource_AppliesOptionsOnce(t *testing.T) {
	t.Parallel()

	applied := 0
	cfg := &latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-6"}
	source := func(context.Context) (string, error) { return "token", nil }
	client, err := NewClientWithTokenSource(t.Context(), cfg, environment.NewMapEnvProvider(nil), "test-project", "us-east5", source,
		func(*options.ModelOptions) { applied++ }, options.WithMaxTokens(42))
	require.NoError(t, err)
	assert.Equal(t, 1, applied)
	assert.Equal(t, int64(42), client.ModelOptions.MaxTokens())
}

func TestNewClientWithTokenSource_FailsFast(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-6"}
	env := environment.NewMapEnvProvider(nil)

	_, err := NewClientWithTokenSource(t.Context(), cfg, env, "test-project", "us-east5", nil)
	require.ErrorContains(t, err, "token source is required")

	_, err = NewClientWithTokenSource(t.Context(), cfg, env, "test-project", "us-east5",
		func(context.Context) (string, error) { return "", errors.New("no token") })
	require.ErrorContains(t, err, "resolving access token: no token")
}

func TestNewClient_RejectsFullThinkingDisplayOnUnsupportedModel(t *testing.T) {
	t.Parallel()
	cfg := &latest.ModelConfig{
		Provider:     "anthropic",
		Model:        "claude-sonnet-5",
		ProviderOpts: map[string]any{"thinking_display": "display"},
	}
	// Validation runs before GCP credential discovery, so no credentials needed.
	_, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(nil), "project", "location")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support thinking_display")
}

func TestNewClient_RequiresProjectAndLocation(t *testing.T) {
	t.Parallel()
	cfg := &latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-6"}
	env := environment.NewMapEnvProvider(nil)

	_, err := NewClient(t.Context(), cfg, env, "", "location")
	require.ErrorContains(t, err, "requires a GCP project")

	_, err = NewClient(t.Context(), cfg, env, "project", "")
	require.ErrorContains(t, err, "requires a GCP location")
}

func TestNewClient_RequiresConfigAndEnv(t *testing.T) {
	t.Parallel()

	_, err := NewClient(t.Context(), nil, environment.NewMapEnvProvider(nil), "project", "location")
	require.ErrorContains(t, err, "model configuration is required")

	_, err = NewClient(t.Context(), &latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-6"}, nil, "project", "location")
	require.ErrorContains(t, err, "environment provider is required")
}

func TestRejectClaudeAPIOnlyFeatures(t *testing.T) {
	t.Parallel()
	for _, feature := range []string{"native_compaction", "cache_diagnostics"} {
		t.Run(feature, func(t *testing.T) {
			cfg := &latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-5", ProviderOpts: map[string]any{feature: true}}
			_, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(nil), "project", "location")
			require.ErrorContains(t, err, "requires the Claude API")
		})
	}
}
