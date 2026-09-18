package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type commandEvaluatorFunc func(context.Context, string, []string) string

func (f commandEvaluatorFunc) Evaluate(ctx context.Context, instruction string, args []string) string {
	return f(ctx, instruction, args)
}

func commandRuntime(t *testing.T, opts ...Opt) *LocalRuntime {
	t.Helper()
	root := agent.New("root", "", agent.WithModel(&mockProvider{id: "test/mock-model"}), agent.WithCommands(types.Commands{
		"test": {Instruction: "${args[0]}"},
	}))
	opts = append([]Opt{WithModelStore(mockModelStore{})}, opts...)
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), opts...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	return r
}

func TestWithCommandEvaluatorFactory(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			r := commandRuntime(t, WithCommandEvaluatorFactory(func([]tools.Tool) CommandEvaluator {
				calls++
				return commandEvaluatorFunc(func(_ context.Context, instruction string, args []string) string {
					assert.Equal(t, "${args[0]}", instruction)
					assert.Equal(t, []string{"hello"}, args)
					return name
				})
			}))
			require.Zero(t, calls, "factory must remain lazy")
			assert.Equal(t, name, ResolveCommand(t.Context(), r, "/test hello"))
			assert.Equal(t, 1, calls)
		})
	}
}

func TestCommandEvaluatorOverridesGlobal(t *testing.T) {
	original := commandEvaluatorFactory.Load()
	t.Cleanup(func() { commandEvaluatorFactory.Store(original) })

	r := commandRuntime(t)
	calls := 0
	RegisterCommandEvaluator(func([]tools.Tool) CommandEvaluator {
		calls++
		return commandEvaluatorFunc(func(context.Context, string, []string) string { return "global" })
	})
	assert.Equal(t, "global", ResolveCommand(t.Context(), r, "/test"))
	assert.Equal(t, 1, calls, "registration after construction remains supported")

	disabled := commandRuntime(t, WithCommandEvaluatorFactory(nil))
	assert.Equal(t, "${args[0]}", ResolveCommand(t.Context(), disabled, "/test"))
	assert.Equal(t, 1, calls)

	local := commandRuntime(t, WithCommandEvaluatorFactory(func([]tools.Tool) CommandEvaluator {
		return commandEvaluatorFunc(func(context.Context, string, []string) string { return "local" })
	}))
	assert.Equal(t, "local", ResolveCommand(t.Context(), local, "/test"))
	assert.Equal(t, 1, calls)

	// Third-party Runtime implementations keep the global fallback.
	mock := &mockRuntime{commands: types.Commands{"test": {Instruction: "${args[0]}"}}}
	assert.Equal(t, "global", ResolveCommand(t.Context(), mock, "/test"))
	assert.Equal(t, 2, calls)
}
