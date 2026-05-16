package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/team"
)

// TestWithAgentModel covers the LocalRuntime.WithAgentModel helper. The
// helper is the public entry point for "apply a temporary model override
// for a scope, then CAS-restore it"; it composes resolution (SetAgentModel)
// with the agent-level snapshot/restore primitives.
func TestWithAgentModel(t *testing.T) {
	t.Parallel()

	t.Run("agent not found returns no-op restore and error", func(t *testing.T) {
		t.Parallel()
		tm := team.New(team.WithAgents(agent.New("root", "test")))
		r := &LocalRuntime{
			team:             tm,
			modelSwitcherCfg: &ModelSwitcherConfig{},
		}

		restore, err := r.WithAgentModel(t.Context(), "missing", "openai/gpt-4o")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "agent not found")
		require.NotNil(t, restore, "restore must always be non-nil")
		assert.NotPanics(t, restore, "restore on error must be a safe no-op")
	})

	t.Run("nil modelSwitcherCfg returns no-op restore and error", func(t *testing.T) {
		t.Parallel()
		root := agent.New("root", "test")
		tm := team.New(team.WithAgents(root))
		r := &LocalRuntime{team: tm} // modelSwitcherCfg is nil

		restore, err := r.WithAgentModel(t.Context(), "root", "openai/gpt-4o")
		require.Error(t, err)
		require.NotNil(t, restore)
		assert.NotPanics(t, restore)
		assert.False(t, root.HasModelOverride(), "agent state must not be touched on error")
	})

	t.Run("invalid model ref returns no-op restore and error", func(t *testing.T) {
		t.Parallel()
		root := agent.New("root", "test")
		tm := team.New(team.WithAgents(root))
		r := &LocalRuntime{
			team:             tm,
			modelSwitcherCfg: &ModelSwitcherConfig{},
		}

		// "invalid" has no slash → not an inline spec, and no named config
		// matches → SetAgentModel returns an error.
		restore, err := r.WithAgentModel(t.Context(), "root", "invalid")
		require.Error(t, err)
		require.NotNil(t, restore)
		assert.NotPanics(t, restore)
		assert.False(t, root.HasModelOverride(), "agent state must not be touched on error")
	})

	t.Run("apply clears existing override; restore puts it back", func(t *testing.T) {
		t.Parallel()
		// Pre-existing override (e.g. set by the user via the model picker
		// before the skill ran).
		userPick := &mockProvider{id: "user/pick"}
		root := agent.New("root", "test", agent.WithModel(&mockProvider{id: "default/model"}))
		root.SetModelOverride(userPick)
		require.Equal(t, "user/pick", root.Model(t.Context()).ID().String())

		tm := team.New(team.WithAgents(root))
		r := &LocalRuntime{
			team:             tm,
			modelSwitcherCfg: &ModelSwitcherConfig{},
		}

		// Empty modelRef clears the override (handled inside SetAgentModel
		// without requiring any provider resolution).
		restore, err := r.WithAgentModel(t.Context(), "root", "")
		require.NoError(t, err)
		require.NotNil(t, restore)

		// Inside the scope: override is cleared.
		assert.False(t, root.HasModelOverride())
		assert.Equal(t, "default/model", root.Model(t.Context()).ID().String())

		// After restore: user's pick is back.
		restore()
		assert.True(t, root.HasModelOverride())
		assert.Equal(t, "user/pick", root.Model(t.Context()).ID().String())
	})

	t.Run("restore is idempotent", func(t *testing.T) {
		t.Parallel()
		root := agent.New("root", "test", agent.WithModel(&mockProvider{id: "default/model"}))
		userPick := &mockProvider{id: "user/pick"}
		root.SetModelOverride(userPick)

		tm := team.New(team.WithAgents(root))
		r := &LocalRuntime{
			team:             tm,
			modelSwitcherCfg: &ModelSwitcherConfig{},
		}

		restore, err := r.WithAgentModel(t.Context(), "root", "")
		require.NoError(t, err)

		restore()
		assert.Equal(t, "user/pick", root.Model(t.Context()).ID().String())
		// Second call is a CAS no-op (the state is already restored).
		assert.NotPanics(t, restore)
		assert.Equal(t, "user/pick", root.Model(t.Context()).ID().String())
	})

	t.Run("concurrent change is preserved by restore", func(t *testing.T) {
		t.Parallel()
		// This is the TUI-while-skill-runs scenario at the runtime layer:
		// after the skill applies its override, another caller (e.g. the
		// model picker) sets a different override before the deferred
		// restore runs. The restore must NOT clobber that change.
		root := agent.New("root", "test", agent.WithModel(&mockProvider{id: "default/model"}))
		tm := team.New(team.WithAgents(root))
		r := &LocalRuntime{
			team:             tm,
			modelSwitcherCfg: &ModelSwitcherConfig{},
		}

		// Apply: clears any override (none was set).
		restore, err := r.WithAgentModel(t.Context(), "root", "")
		require.NoError(t, err)

		// Concurrent caller wins between apply and restore.
		userPick := &mockProvider{id: "user/pick"}
		root.SetModelOverride(userPick)

		// Restore must be a no-op because the override changed.
		restore()
		require.True(t, root.HasModelOverride(), "concurrent change must be preserved")
		assert.Equal(t, "user/pick", root.Model(t.Context()).ID().String())
	})
}
