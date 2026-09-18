//go:build !js

package bedrock

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

// Without a bearer token the native client falls back to the AWS credential
// chain, which is resolved lazily so construction succeeds without credentials.
func TestNewClient_ValidConfig(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		Provider: "amazon-bedrock",
		Model:    "anthropic.claude-v2",
		ProviderOpts: map[string]any{
			"region": "us-east-1",
		},
	}

	client, err := NewClient(t.Context(), cfg, environment.NewNoEnvProvider())
	require.NoError(t, err)
	require.NotNil(t, client)

	assert.Equal(t, "anthropic.claude-v2", client.ModelConfig.Model)
	assert.Equal(t, "amazon-bedrock", client.ModelConfig.Provider)
}
