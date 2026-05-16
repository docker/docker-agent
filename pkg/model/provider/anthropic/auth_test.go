package anthropic

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func TestNewClient_RequiresAPIKeyWhenNoAuthConfigured(t *testing.T) {
	cfg := &latest.ModelConfig{Provider: "anthropic", Model: "claude-x"}
	_, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ANTHROPIC_API_KEY environment variable is required")
}

func TestNewClient_WIFAuth_BypassesAPIKey(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("jwt"), 0o600))

	cfg := &latest.ModelConfig{
		Provider: "anthropic",
		Model:    "claude-x",
		Auth: &latest.AuthConfig{
			Type: latest.AuthTypeWorkloadIdentityFederation,
			Federation: &latest.FederationAuthConfig{
				FederationRuleID: "fdrl_abc",
				OrganizationID:   "org",
				IdentityToken: &latest.IdentityTokenSourceConfig{
					File: tokenPath,
				},
			},
		},
	}

	client, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(nil))
	require.NoError(t, err, "WIF auth must not require ANTHROPIC_API_KEY")
	require.NotNil(t, client)
}

func TestNewClient_WIFAuth_RejectsBrokenConfig(t *testing.T) {
	cfg := &latest.ModelConfig{
		Provider: "anthropic",
		Model:    "claude-x",
		Auth: &latest.AuthConfig{
			Type: latest.AuthTypeWorkloadIdentityFederation,
			Federation: &latest.FederationAuthConfig{
				FederationRuleID: "fdrl_abc",
				OrganizationID:   "org",
				IdentityToken:    &latest.IdentityTokenSourceConfig{}, // no source set
			},
		},
	}

	_, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "anthropic workload identity federation")
}

func TestNewClient_WIFAuth_RejectsUnknownType(t *testing.T) {
	cfg := &latest.ModelConfig{
		Provider: "anthropic",
		Model:    "claude-x",
		Auth:     &latest.AuthConfig{Type: "oauth"},
	}
	_, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported auth.type")
}

func TestNewClient_AuthAndGatewayMutuallyExclusive(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("jwt"), 0o600))

	cfg := &latest.ModelConfig{
		Provider: "anthropic",
		Model:    "claude-x",
		Auth: &latest.AuthConfig{
			Type: latest.AuthTypeWorkloadIdentityFederation,
			Federation: &latest.FederationAuthConfig{
				FederationRuleID: "fdrl_abc",
				OrganizationID:   "org",
				IdentityToken: &latest.IdentityTokenSourceConfig{
					File: tokenPath,
				},
			},
		},
	}

	// With a Docker Desktop token present, gateway mode is normally accepted.
	env := environment.NewMapEnvProvider(map[string]string{
		environment.DockerDesktopTokenEnv: "ddtok",
	})
	_, err := NewClient(t.Context(), cfg, env, options.WithGateway("https://gateway.example.com"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth and Docker AI Gateway are mutually exclusive")
}
