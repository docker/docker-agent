package runtime_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/jscommands"
	"github.com/docker/docker-agent/pkg/team"
)

type runtimeDecorator struct {
	runtime.Runtime

	factory func() runtime.CommandEvaluatorFactory
}

func (r runtimeDecorator) CommandEvaluatorFactory() runtime.CommandEvaluatorFactory {
	return r.factory()
}

func TestCommandEvaluatorThroughDecorator(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
			t.Parallel()
			root := agent.New("root", "", agent.WithHarness(&latest.HarnessConfig{Type: "codex"}),
				agent.WithCommands(types.Commands{"test": {Instruction: "${args[0]}"}}))
			var factory runtime.CommandEvaluatorFactory
			expected := "${args[0]}"
			if enabled {
				factory = jscommands.Factory
				expected = "hello"
			}
			r, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), runtime.WithCommandEvaluatorFactory(factory))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, r.Close()) })
			wrapped := runtimeDecorator{Runtime: r, factory: r.CommandEvaluatorFactory}
			assert.Equal(t, expected, runtime.ResolveCommand(t.Context(), wrapped, "/test hello"))
		})
	}
}
