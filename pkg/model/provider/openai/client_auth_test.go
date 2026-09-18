package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func TestNewClient_RefreshesConfiguredTokenSource(t *testing.T) {
	t.Parallel()

	var gotAuth []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		writeSSEResponse(w)
	}))
	defer server.Close()

	calls := 0
	source := func(context.Context) (string, error) {
		calls++
		return fmt.Sprintf("token-%d", calls), nil
	}
	client, err := NewClient(t.Context(), &latest.ModelConfig{
		Provider: "openai",
		Model:    "gpt-4o",
		BaseURL:  server.URL,
	}, environment.NewNoEnvProvider(), options.WithTokenSource(source))
	require.NoError(t, err)

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

	assert.Equal(t, []string{"Bearer token-2", "Bearer token-3"}, gotAuth)
}

// TestNewClient_NoTokenKeyUsesEnvProvider verifies that when no token_key is
// configured, the API key is resolved through the environment provider chain
// rather than the SDK's os.Getenv fallback. This matters when the OS env holds
// a secret reference (e.g. "op://vault/item/field") that the provider chain
// resolves: the SDK fallback would send the raw reference as the bearer token.
func TestNewClient_NoTokenKeyUsesEnvProvider(t *testing.T) {
	// Simulate an unresolved 1Password reference in the process environment.
	t.Setenv("OPENAI_API_KEY", "op://vault/item/field")

	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeSSEResponse(w)
	}))
	defer server.Close()

	cfg := &latest.ModelConfig{
		Provider: "openai",
		Model:    "gpt-4o",
		BaseURL:  server.URL,
	}
	// The provider chain resolves the reference to the actual secret.
	env := environment.NewMapEnvProvider(map[string]string{
		"OPENAI_API_KEY": "resolved-secret",
	})

	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
		{Role: chat.MessageRoleUser, Content: "hello"},
	}, nil)
	require.NoError(t, err)
	defer stream.Close()

	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	assert.Equal(t, "Bearer resolved-secret", gotAuth,
		"API key must come from the environment provider chain, not os.Getenv")
}

// TestNewClient_NoTokenKeyFallsBackToOSEnv verifies that the SDK's default
// OPENAI_API_KEY behavior is preserved when the environment provider chain
// has no value for it.
func TestNewClient_NoTokenKeyFallsBackToOSEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "os-env-key")

	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeSSEResponse(w)
	}))
	defer server.Close()

	cfg := &latest.ModelConfig{
		Provider: "openai",
		Model:    "gpt-4o",
		BaseURL:  server.URL,
	}
	env := environment.NewMapEnvProvider(map[string]string{})

	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
		{Role: chat.MessageRoleUser, Content: "hello"},
	}, nil)
	require.NoError(t, err)
	defer stream.Close()

	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	assert.Equal(t, "Bearer os-env-key", gotAuth)
}

func TestNewClient_GitHubCopilotFallsBackToGHToken(t *testing.T) {
	t.Parallel()

	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeSSEResponse(w)
	}))
	defer server.Close()

	cfg := &latest.ModelConfig{
		Provider: "github-copilot",
		Model:    "gpt-4.1",
		BaseURL:  server.URL,
		TokenKey: "GITHUB_TOKEN",
	}
	env := environment.NewMapEnvProvider(map[string]string{
		"GH_TOKEN": "gh-token",
	})

	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
		{Role: chat.MessageRoleUser, Content: "hello"},
	}, nil)
	require.NoError(t, err)
	defer stream.Close()

	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	assert.Equal(t, "Bearer gh-token", gotAuth)
}
