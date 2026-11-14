package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestTrustedRouterRequiredCredentials(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Agents: []latest.AgentConfig{{Name: "root", Model: "trustedrouter/trustedrouter/auto"}},
		Models: map[string]latest.ModelConfig{
			"trustedrouter/trustedrouter/auto": {Provider: "trustedrouter", Model: "trustedrouter/auto"},
		},
	}
	err := CheckRequiredEnvVars(t.Context(), cfg, "", environment.NewNoEnvProvider())
	var required *environment.RequiredEnvError
	require.ErrorAs(t, err, &required)
	assert.Equal(t, []string{"TRUSTEDROUTER_API_KEY"}, required.Missing)
	assert.True(t, required.MissingModelCredentials)
	require.NoError(t, CheckRequiredEnvVars(t.Context(), cfg, "", environment.NewMapEnvProvider(map[string]string{"TRUSTEDROUTER_API_KEY": "test-key"})))
}
