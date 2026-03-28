package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestValidateAndFillDefaults(t *testing.T) {
	t.Parallel()

	t.Run("fills missing endpoints from authServerURL", func(t *testing.T) {
		t.Parallel()

		metadata := &AuthorizationServerMetadata{}
		result := validateAndFillDefaults(metadata, "https://auth.example.com")

		assert.Equal(t, "https://auth.example.com", result.Issuer)
		assert.Equal(t, "https://auth.example.com/authorize", result.AuthorizationEndpoint)
		assert.Equal(t, "https://auth.example.com/token", result.TokenEndpoint)
	})

	t.Run("preserves existing endpoints", func(t *testing.T) {
		t.Parallel()

		metadata := &AuthorizationServerMetadata{
			Issuer:                "https://issuer.example.com",
			AuthorizationEndpoint: "https://auth.example.com/oauth/authorize",
			TokenEndpoint:         "https://auth.example.com/oauth/token",
		}
		result := validateAndFillDefaults(metadata, "https://auth.example.com")

		assert.Equal(t, "https://issuer.example.com", result.Issuer)
		assert.Equal(t, "https://auth.example.com/oauth/authorize", result.AuthorizationEndpoint)
		assert.Equal(t, "https://auth.example.com/oauth/token", result.TokenEndpoint)
	})

	t.Run("preserves registration endpoint when server advertises it", func(t *testing.T) {
		t.Parallel()

		metadata := &AuthorizationServerMetadata{
			RegistrationEndpoint: "https://auth.example.com/register",
		}
		result := validateAndFillDefaults(metadata, "https://auth.example.com")

		assert.Equal(t, "https://auth.example.com/register", result.RegistrationEndpoint)
	})

	t.Run("does not fabricate registration endpoint when server omits it", func(t *testing.T) {
		t.Parallel()

		// Servers like Slack do not advertise a registration_endpoint
		// because they do not support Dynamic Client Registration (RFC 7591).
		// validateAndFillDefaults must not invent one — doing so causes a
		// guaranteed 404/302 and misleads the caller into attempting registration.
		metadata := &AuthorizationServerMetadata{}
		result := validateAndFillDefaults(metadata, "https://mcp.slack.com")

		assert.Empty(t, result.RegistrationEndpoint)
	})

	t.Run("fills default response types when empty", func(t *testing.T) {
		t.Parallel()

		metadata := &AuthorizationServerMetadata{}
		result := validateAndFillDefaults(metadata, "https://auth.example.com")

		assert.Equal(t, []string{"code"}, result.ResponseTypesSupported)
	})

	t.Run("preserves existing response types", func(t *testing.T) {
		t.Parallel()

		metadata := &AuthorizationServerMetadata{
			ResponseTypesSupported: []string{"code", "token"},
		}
		result := validateAndFillDefaults(metadata, "https://auth.example.com")

		assert.Equal(t, []string{"code", "token"}, result.ResponseTypesSupported)
	})
}

// TestOAuthTransport_UserDeclined verifies that once a user declines the OAuth
// prompt, all subsequent requests are rejected immediately without re-prompting.
func TestOAuthTransport_UserDeclined(t *testing.T) {
	t.Parallel()

	var promptCount atomic.Int32

	// Minimal server that:
	//  - returns 401 on the MCP endpoint to trigger the OAuth flow
	//  - returns 404 for /.well-known/oauth-protected-resource (acceptable; flow continues)
	//  - returns minimal auth server metadata for /.well-known/oauth-authorization-server
	//    so the managed flow reaches the elicitation prompt
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource":
			w.WriteHeader(http.StatusNotFound)
		case "/.well-known/oauth-authorization-server":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q}`,
				"http://"+r.Host, "http://"+r.Host+"/authorize", "http://"+r.Host+"/token")
		default:
			w.Header().Set("WWW-Authenticate", `Bearer error="unauthorized"`)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer server.Close()

	client := newRemoteClient(server.URL, "streamable", nil, NewInMemoryTokenStore(), &latest.RemoteOAuthConfig{ClientID: "test-client-id"})
	client.SetElicitationHandler(func(_ context.Context, _ *gomcp.ElicitParams) (tools.ElicitationResult, error) {
		promptCount.Add(1)
		return tools.ElicitationResult{Action: tools.ElicitationActionDecline}, nil
	})

	httpClient := client.createHTTPClient()

	// First request — OAuth flow fires, user declines once.
	req1, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	require.NoError(t, err)
	resp1, err := httpClient.Do(req1)
	if resp1 != nil {
		resp1.Body.Close()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declined")
	assert.Equal(t, int32(1), promptCount.Load(), "elicitation should fire exactly once")

	// Second request — UserDeclined sentinel must short-circuit without re-prompting.
	req2, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	require.NoError(t, err)
	resp2, err := httpClient.Do(req2)
	if resp2 != nil {
		resp2.Body.Close()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declined")
	assert.Equal(t, int32(1), promptCount.Load(), "elicitation must not fire again after user declined")

	// Third request — same guarantee holds for any number of follow-up calls.
	req3, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	require.NoError(t, err)
	resp3, err := httpClient.Do(req3)
	if resp3 != nil {
		resp3.Body.Close()
	}
	require.Error(t, err)
	assert.Equal(t, int32(1), promptCount.Load(), "elicitation count must stay at 1 regardless of further requests")
}

// TestOAuthUserDeclinedSentinel groups unit-level checks on the UserDeclined
// sentinel: that it is written to the token store correctly, that it does not
// bleed across server URLs, and that IsExpired does not misclassify it.
func TestOAuthUserDeclinedSentinel(t *testing.T) {
	t.Parallel()

	t.Run("sentinel written to store after decline", func(t *testing.T) {
		t.Parallel()

		store := NewInMemoryTokenStore()
		require.NoError(t, store.StoreToken("https://mcp.example.com", &OAuthToken{UserDeclined: true}))

		token, err := store.GetToken("https://mcp.example.com")
		require.NoError(t, err)
		assert.True(t, token.UserDeclined)
		assert.Empty(t, token.AccessToken, "declined sentinel must not carry an access token")
		assert.False(t, token.IsExpired(), "sentinel with zero ExpiresAt must not be considered expired")
	})

	t.Run("decline is scoped to the declined server URL only", func(t *testing.T) {
		t.Parallel()

		store := NewInMemoryTokenStore()
		require.NoError(t, store.StoreToken("https://mcp.slack.com", &OAuthToken{UserDeclined: true}))

		declined, err := store.GetToken("https://mcp.slack.com")
		require.NoError(t, err)
		assert.True(t, declined.UserDeclined)

		_, err = store.GetToken("https://mcp.github.com")
		assert.Error(t, err, "unrelated server must have no token in the store")
	})

	t.Run("IsExpired returns false for sentinel with zero ExpiresAt", func(t *testing.T) {
		t.Parallel()

		sentinel := &OAuthToken{UserDeclined: true}
		assert.False(t, sentinel.IsExpired())
		assert.Empty(t, sentinel.AccessToken)
	})
}
