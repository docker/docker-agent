package teamloader

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/cagent/pkg/config"
	"github.com/docker/cagent/pkg/config/latest"
	"github.com/docker/cagent/pkg/environment"
)

func TestCreateShellTool(t *testing.T) {
	toolset := latest.Toolset{
		Type: "shell",
	}

	registry := NewDefaultToolsetRegistry()

	runConfig := &config.RuntimeConfig{
		Config:              config.Config{WorkingDir: t.TempDir()},
		EnvProviderForTests: environment.NewOsEnvProvider(),
	}

	tool, err := registry.CreateTool(t.Context(), toolset, ".", runConfig, "test-agent")
	require.NoError(t, err)
	require.NotNil(t, tool)
}
