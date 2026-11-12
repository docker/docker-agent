//go:build js

package bedrock

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func bedrockConfig(opts map[string]any) *latest.ModelConfig {
	return &latest.ModelConfig{Provider: "amazon-bedrock", Model: "anthropic.claude-v2", ProviderOpts: opts}
}

func TestNewClient_JSRequiresBearerToken(t *testing.T) {
	t.Parallel()

	_, err := NewClient(t.Context(), bedrockConfig(nil), environment.NewNoEnvProvider())
	require.ErrorContains(t, err, "requires a bearer token")

	cfg := bedrockConfig(nil)
	cfg.TokenKey = "MY_TOKEN"
	_, err = NewClient(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "ignored"}))
	require.ErrorContains(t, err, "requires a bearer token", "token_key is authoritative when set")
}

func TestNewClient_JSRejectsCredentialChainOptions(t *testing.T) {
	t.Parallel()

	env := environment.NewMapEnvProvider(map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "token"})
	for _, key := range []string{"profile", "role_arn"} {
		_, err := NewClient(t.Context(), bedrockConfig(map[string]any{key: "x"}), env)
		require.ErrorContains(t, err, "provider_opts."+key+" is not supported in the browser")
	}
}
