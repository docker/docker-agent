package defaults

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/harness"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

type commandEvaluator struct{}

func (commandEvaluator) Evaluate(context.Context, string, []string) string {
	return "custom evaluator"
}

func TestLoaderOptsPreservesGlobalFactories(t *testing.T) {
	// Global factories are restored before parallel tests run.
	t.Cleanup(func() { Opts() })
	runtime.RegisterCommandEvaluator(func([]tools.Tool) runtime.CommandEvaluator {
		return commandEvaluator{}
	})
	harnessCalls := 0
	runtime.RegisterHarness(func(*latest.HarnessConfig) (harness.Provider, error) {
		harnessCalls++
		return nil, runtime.ErrHarnessNotRegistered
	})

	ag := agent.New("root", "", agent.WithHarness(&latest.HarnessConfig{Type: "codex"}), agent.WithCommands(types.Commands{
		"test": {Instruction: "${args[0]}"},
	}))
	rt, err := runtime.New(t.Context(), team.New(team.WithAgents(ag)))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })

	LoaderOpts()
	assert.Zero(t, harnessCalls, "loader options must not invoke runtime factories")
	assert.Equal(t, "custom evaluator", runtime.ResolveCommand(t.Context(), rt, "/test hello"))

	_, err = rt.Run(t.Context(), session.New(session.WithUserMessage("hello")))
	require.Error(t, err)
	assert.Equal(t, 1, harnessCalls, "loader defaults must not replace the harness factory")
}

func TestOptsRegistersCommandEvaluator(t *testing.T) {
	t.Cleanup(func() { Opts() })
	runtime.RegisterCommandEvaluator(func([]tools.Tool) runtime.CommandEvaluator {
		return commandEvaluator{}
	})

	harnessCalls := 0
	runtime.RegisterHarness(func(*latest.HarnessConfig) (harness.Provider, error) {
		harnessCalls++
		return nil, runtime.ErrHarnessNotRegistered
	})
	ag := agent.New("root", "", agent.WithHarness(&latest.HarnessConfig{Type: "unknown"}), agent.WithCommands(types.Commands{
		"test": {Instruction: "${args[0]}"},
	}))
	rt, err := runtime.New(t.Context(), team.New(team.WithAgents(ag)))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })

	Opts()
	assert.Equal(t, "hello", runtime.ResolveCommand(t.Context(), rt, "/test hello"))
	_, err = rt.Run(t.Context(), session.New(session.WithUserMessage("hello")))
	require.ErrorContains(t, err, "unknown")
	assert.Zero(t, harnessCalls, "legacy options must reinstall the default harness factory")
}

func TestLoaderOptsLoadsDefaultCapabilities(t *testing.T) {
	t.Parallel()

	source := config.NewBytesSource("test", []byte(`agents:
  root:
    model: openai/gpt-5-mini
    instruction: A test agent
    toolsets:
      - type: think
`))
	runConfig := &config.RuntimeConfig{
		EnvProviderOverride:    environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test-key"}),
		ModelsDevStoreOverride: modelsdev.NewDatabaseStore(&modelsdev.Database{}),
	}
	loaded, err := teamloader.LoadWithConfig(t.Context(), source, runConfig, LoaderOpts()...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, loaded.Team.StopToolSets(t.Context())) })

	ag, err := loaded.Team.DefaultAgent()
	require.NoError(t, err)
	assert.Equal(t, "root", ag.Name())
	agentTools, err := ag.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, agentTools)
}

func TestLoaderOptsHistoricalVersions(t *testing.T) {
	t.Parallel()
	source := config.NewFileSource("../../config/testdata/provider_v0.yaml")
	runConfig := &config.RuntimeConfig{
		EnvProviderOverride:    environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test-key"}),
		ModelsDevStoreOverride: modelsdev.NewDatabaseStore(&modelsdev.Database{}),
	}
	loaded, err := teamloader.Load(t.Context(), source, runConfig, LoaderOpts()...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, loaded.Dispose(t.Context())) })
	root, err := loaded.DefaultAgent()
	require.NoError(t, err)
	assert.Equal(t, "root", root.Name())
}
