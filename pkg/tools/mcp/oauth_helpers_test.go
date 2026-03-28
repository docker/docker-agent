package mcp

import (
	"crypto/tls"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelfSignedTLSConfig(t *testing.T) {
	t.Parallel()

	cfg, err := selfSignedTLSConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Len(t, cfg.Certificates, 1)
	assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion)
}

func TestBuildAuthorizationURL(t *testing.T) {
	rawURL := BuildAuthorizationURL(
		"https://auth.example.com/authorize",
		"my-client-id",
		"http://127.0.0.1:3118/callback",
		"random-state-value",
		"code-challenge-value",
		"https://mcp.example.com/mcp",
		[]string{"search:read", "chat:write"},
	)

	parsed, err := url.Parse(rawURL)
	require.NoError(t, err)

	q := parsed.Query()
	assert.Equal(t, "https://auth.example.com/authorize", parsed.Scheme+"://"+parsed.Host+parsed.Path)
	assert.Equal(t, "code", q.Get("response_type"))
	assert.Equal(t, "my-client-id", q.Get("client_id"))
	assert.Equal(t, "http://127.0.0.1:3118/callback", q.Get("redirect_uri"))
	assert.Equal(t, "random-state-value", q.Get("state"))
	assert.Equal(t, "code-challenge-value", q.Get("code_challenge"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.Equal(t, "https://mcp.example.com/mcp", q.Get("resource"))
	assert.Equal(t, "search:read chat:write", q.Get("scope"))
}
