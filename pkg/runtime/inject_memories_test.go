package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
)

// TestInjectMemoriesBuiltin_ReturnsNilWhenNoopStub verifies that the scaffold
// implementation is a no-op: it returns (nil, nil) so the hook pipeline
// treats it as contributing no additional context.
func TestInjectMemoriesBuiltin_ReturnsNilWhenNoopStub(t *testing.T) {
	t.Parallel()

	rt := &LocalRuntime{}
	out, err := rt.injectMemoriesBuiltin(context.Background(), &hooks.Input{AgentName: "a"}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

// TestApplyInjectMemoriesDefault verifies that the turn_start hook entry is
// appended only when inject_memories is enabled on the agent.
func TestApplyInjectMemoriesDefault(t *testing.T) {
	t.Parallel()

	t.Run("disabled leaves cfg unchanged", func(t *testing.T) {
		t.Parallel()
		a := agent.New("a", "")
		got := applyInjectMemoriesDefault(nil, a)
		assert.Nil(t, got)
	})

	t.Run("enabled appends hook to nil cfg", func(t *testing.T) {
		t.Parallel()
		a := agent.New("a", "", agent.WithInjectMemories(true, 5))
		got := applyInjectMemoriesDefault(nil, a)
		require.NotNil(t, got)
		require.Len(t, got.TurnStart, 1)
		assert.Equal(t, hooks.HookTypeBuiltin, got.TurnStart[0].Type)
		assert.Equal(t, BuiltinInjectMemories, got.TurnStart[0].Command)
	})

	t.Run("enabled appends hook to existing cfg", func(t *testing.T) {
		t.Parallel()
		a := agent.New("a", "", agent.WithInjectMemories(true, 0))
		existing := &hooks.Config{
			TurnStart: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: "add_date"}},
		}
		got := applyInjectMemoriesDefault(existing, a)
		require.Len(t, got.TurnStart, 2)
		assert.Equal(t, BuiltinInjectMemories, got.TurnStart[1].Command)
	})
}

// TestEffectiveMaxInjectMemories verifies the fallback to DefaultMaxInjectMemories.
func TestEffectiveMaxInjectMemories(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		max  int
		want int
	}{
		{name: "zero uses default", max: 0, want: latest.DefaultMaxInjectMemories},
		{name: "positive value used as-is", max: 3, want: 3},
		{name: "large value used as-is", max: 100, want: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := agent.New("a", "", agent.WithInjectMemories(true, tt.max))
			assert.Equal(t, tt.want, effectiveMaxInjectMemories(a))
		})
	}
}
