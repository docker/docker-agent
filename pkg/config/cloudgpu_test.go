package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestCloudGPURequiredCredentials(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Agents: []latest.AgentConfig{{Name: "root", Model: "cloudgpu/deepseek-v4.1-flash"}},
		Models: map[string]latest.ModelConfig{
			"cloudgpu/deepseek-v4.1-flash": {Provider: "cloudgpu", Model: "deepseek-v4.1-flash"},
		},
	}
	err := CheckRequiredEnvVars(t.Context(), cfg, "", environment.NewNoEnvProvider())
	var required *environment.RequiredEnvError
	require.ErrorAs(t, err, &required)
	assert.Equal(t, []string{"CLOUDGPU_API_KEY"}, required.Missing)
	assert.True(t, required.MissingModelCredentials)
	require.NoError(t, CheckRequiredEnvVars(t.Context(), cfg, "", environment.NewMapEnvProvider(map[string]string{"CLOUDGPU_API_KEY": "test-key"})))
}
