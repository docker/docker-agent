package teamloader

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestStreamIdleTimeoutImportedAgentAndWorkspace(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	parent := `runtime:
  stream_idle_timeout: 9s
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    sub_agents: [child:example/imported]
`
	child := `runtime:
  stream_idle_timeout: 22s
agents:
  root:
    model: openai/gpt-4o
    instruction: test
`
	loaded, err := LoadWithConfig(t.Context(), config.NewBytesSource("parent.yaml", []byte(parent)), &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}}, withTestProviderRegistry(WithSourceResolver(func(string, environment.Provider) (config.Source, error) {
		return config.NewBytesSource("child.yaml", []byte(child)), nil
	}))...)
	require.NoError(t, err)
	for _, name := range []string{"root", "child"} {
		a, err := loaded.Team.Agent(name)
		require.NoError(t, err)
		want := 9 * time.Second
		if name == "child" {
			want = 22 * time.Second
		}
		require.NotNil(t, a.StreamIdleTimeout())
		assert.Equal(t, want, a.StreamIdleTimeout().Duration)
		workspace, err := a.ForWorkingDir(t.Context(), t.TempDir())
		require.NoError(t, err)
		require.NotNil(t, workspace.StreamIdleTimeout())
		assert.Equal(t, want, workspace.StreamIdleTimeout().Duration)
	}
}
