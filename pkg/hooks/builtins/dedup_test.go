package builtins_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
)

func TestAgentDefaultsDeduplicateIdenticalHooks(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		hook hooks.Hook
		want int
	}{
		{name: "identical", hook: hooks.Hook{Type: hooks.HookTypeBuiltin, Command: builtins.AddDate}, want: 1},
		{name: "empty collections", hook: hooks.Hook{Type: hooks.HookTypeBuiltin, Command: builtins.AddDate, Args: []string{}, Env: map[string]string{}}, want: 1},
		{name: "named", hook: hooks.Hook{Name: "my date", Type: hooks.HookTypeBuiltin, Command: builtins.AddDate}, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := builtins.ApplyAgentDefaults(&hooks.Config{TurnStart: []hooks.Hook{tc.hook}}, builtins.AgentDefaults{AddDate: true})
			registry := hooks.NewRegistry()
			require.NoError(t, builtins.Register(registry))
			exec := hooks.NewExecutorWithRegistry(cfg, "", nil, registry)
			for range 2 {
				result, err := exec.Dispatch(t.Context(), hooks.EventTurnStart, &hooks.Input{})
				require.NoError(t, err)
				assert.Len(t, result.InstructionContext, tc.want)
			}
		})
	}
}
