package teamloader

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

type mergeableSet struct {
	name string
}

func (m *mergeableSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{Name: m.name}}, nil
}
func (m *mergeableSet) MergeKey() string { return "fake" }
func (m *mergeableSet) Merge(siblings []tools.MergeSibling) tools.ToolSet {
	return &mergedSet{siblings: siblings}
}

type mergedSet struct {
	siblings []tools.MergeSibling
}

func (m *mergedSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{Name: "merged"}}, nil
}

type plainSet struct{}

func (plainSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{Name: "plain"}}, nil
}

func TestGetToolsForAgent_MergesSiblingMergeableToolsets(t *testing.T) {
	t.Parallel()

	registry := NewToolsetRegistry(map[string]ToolsetCreator{
		"fake": func(_ context.Context, ts latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			return &mergeableSet{name: ts.Command}, nil
		},
		"plain": func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
			return plainSet{}, nil
		},
	})
	runConfig := config.RuntimeConfig{}
	expander := newEnvExpander(runConfig.EnvProvider())

	t.Run("single mergeable toolset is used as-is", func(t *testing.T) {
		t.Parallel()
		a := &latest.AgentConfig{Toolsets: []latest.Toolset{{Type: "fake", Command: "a"}, {Type: "plain"}}}
		got, warnings, err := getToolsForAgent(t.Context(), a, ".", &runConfig, "cfg", &loadOptions{toolsetRegistry: registry}, expander)
		require.NoError(t, err)
		assert.Empty(t, warnings)
		require.Len(t, got, 2)
		names := allToolNames(t, got)
		assert.ElementsMatch(t, []string{"plain", "a"}, names)
	})

	t.Run("several are merged into one", func(t *testing.T) {
		t.Parallel()
		a := &latest.AgentConfig{Toolsets: []latest.Toolset{{Type: "fake", Command: "a"}, {Type: "plain"}, {Type: "fake", Command: "b"}}}
		got, _, err := getToolsForAgent(t.Context(), a, ".", &runConfig, "cfg", &loadOptions{toolsetRegistry: registry}, expander)
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, []string{"plain", "merged"}, allToolNames(t, got))
		merged, ok := got[1].(*mergedSet)
		require.True(t, ok)
		require.Len(t, merged.siblings, 2)
		assert.Equal(t, "a", merged.siblings[0].Raw.(*mergeableSet).name)
		assert.Equal(t, "b", merged.siblings[1].Raw.(*mergeableSet).name)
	})
}

func allToolNames(t *testing.T, sets []tools.ToolSet) []string {
	t.Helper()
	var names []string
	for _, s := range sets {
		names = append(names, toolNames(t, s)...)
	}
	return names
}
