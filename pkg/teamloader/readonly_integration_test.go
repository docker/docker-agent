package teamloader

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/tools"
)

// toolNames returns the names of the tools exposed by ts.
func toolNames(t *testing.T, ts tools.ToolSet) []string {
	t.Helper()
	all, err := ts.Tools(t.Context())
	require.NoError(t, err)
	var names []string
	for _, tool := range all {
		names = append(names, tool.Name)
	}
	return names
}

func TestGetToolsForAgent_ToolsetReadOnly(t *testing.T) {
	t.Parallel()

	a := &latest.AgentConfig{
		Instruction: "test",
		Toolsets: []latest.Toolset{
			{Type: "background_jobs", ReadOnly: true},
		},
	}

	runConfig := config.RuntimeConfig{
		Config:              config.Config{WorkingDir: t.TempDir()},
		EnvProviderForTests: &noEnvProvider{},
	}
	expander := js.NewJsExpander(runConfig.EnvProvider())

	got, warnings, err := getToolsForAgent(t.Context(), a, ".", &runConfig, "test-config", &loadOptions{toolsetRegistry: testToolsetRegistry()}, expander)
	require.NoError(t, err)
	require.Empty(t, warnings)
	require.Len(t, got, 1)

	names := toolNames(t, got[0])
	assert.ElementsMatch(t, []string{"list_background_jobs", "view_background_job", "wait_background_job"}, names)
}

func TestGetToolsForAgent_AgentReadOnly(t *testing.T) {
	t.Parallel()

	// The agent-level flag applies the read-only filter to every toolset,
	// even though the toolset itself does not set readonly.
	a := &latest.AgentConfig{
		Instruction: "test",
		ReadOnly:    true,
		Toolsets: []latest.Toolset{
			{Type: "background_jobs"},
		},
	}

	runConfig := config.RuntimeConfig{
		Config:              config.Config{WorkingDir: t.TempDir()},
		EnvProviderForTests: &noEnvProvider{},
	}
	expander := js.NewJsExpander(runConfig.EnvProvider())

	got, warnings, err := getToolsForAgent(t.Context(), a, ".", &runConfig, "test-config", &loadOptions{toolsetRegistry: testToolsetRegistry()}, expander)
	require.NoError(t, err)
	require.Empty(t, warnings)
	require.Len(t, got, 1)

	names := toolNames(t, got[0])
	assert.ElementsMatch(t, []string{"list_background_jobs", "view_background_job", "wait_background_job"}, names)
}

func TestGetToolsForAgent_NoReadOnlyKeepsAllTools(t *testing.T) {
	t.Parallel()

	a := &latest.AgentConfig{
		Instruction: "test",
		Toolsets: []latest.Toolset{
			{Type: "shell"},
			{Type: "background_jobs"},
		},
	}

	runConfig := config.RuntimeConfig{
		Config:              config.Config{WorkingDir: t.TempDir()},
		EnvProviderForTests: &noEnvProvider{},
	}
	expander := js.NewJsExpander(runConfig.EnvProvider())

	got, warnings, err := getToolsForAgent(t.Context(), a, ".", &runConfig, "test-config", &loadOptions{toolsetRegistry: testToolsetRegistry()}, expander)
	require.NoError(t, err)
	require.Empty(t, warnings)
	require.Len(t, got, 2)

	shellNames := toolNames(t, got[0])
	assert.ElementsMatch(t, []string{"shell"}, shellNames)

	backgroundNames := toolNames(t, got[1])
	assert.ElementsMatch(t, []string{
		"run_background_job", "list_background_jobs",
		"view_background_job", "stop_background_job", "wait_background_job",
	}, backgroundNames)
}
