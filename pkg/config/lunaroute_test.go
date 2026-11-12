package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestLunarouteRequiredCredentials(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Agents: []latest.AgentConfig{{Name: "root", Model: "lunaroute/glm-5.3"}},
		Models: map[string]latest.ModelConfig{
			"lunaroute/glm-5.3": {Provider: "lunaroute", Model: "glm-5.3"},
		},
	}
	err := CheckRequiredEnvVars(t.Context(), cfg, "", environment.NewNoEnvProvider())
	var required *environment.RequiredEnvError
	require.ErrorAs(t, err, &required)
	assert.Equal(t, []string{"LUNAROUTE_API_KEY"}, required.Missing)
	assert.True(t, required.MissingModelCredentials)
	require.NoError(t, CheckRequiredEnvVars(t.Context(), cfg, "", environment.NewMapEnvProvider(map[string]string{"LUNAROUTE_API_KEY": "lr_test"})))
}
