package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestAtmoRouterRequiredCredentials(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Agents: []latest.AgentConfig{{Name: "root", Model: "atmorouter/cc/claude-sonnet-4-6"}},
		Models: map[string]latest.ModelConfig{
			"atmorouter/cc/claude-sonnet-4-6": {Provider: "atmorouter", Model: "cc/claude-sonnet-4-6"},
		},
	}
	err := CheckRequiredEnvVars(t.Context(), cfg, "", environment.NewNoEnvProvider())
	var required *environment.RequiredEnvError
	require.ErrorAs(t, err, &required)
	assert.Equal(t, []string{"ATMOROUTER_API_KEY"}, required.Missing)
	assert.True(t, required.MissingModelCredentials)
	require.NoError(t, CheckRequiredEnvVars(t.Context(), cfg, "", environment.NewMapEnvProvider(map[string]string{"ATMOROUTER_API_KEY": "test-key"})))
}
