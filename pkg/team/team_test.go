package team

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
)

func newAgent(name string) *agent.Agent {
	return agent.New(name, "")
}

func TestDefaultAgent(t *testing.T) {
	t.Run("empty team returns error", func(t *testing.T) {
		_, err := New().DefaultAgent()
		require.Error(t, err)
	})

	t.Run("returns the agent named root when present", func(t *testing.T) {
		team := New(WithAgents(newAgent("first"), newAgent("root"), newAgent("other")))

		got, err := team.DefaultAgent()
		require.NoError(t, err)
		assert.Equal(t, "root", got.Name())
	})

	t.Run("falls back to the first agent when there is no root", func(t *testing.T) {
		team := New(WithAgents(newAgent("alice"), newAgent("bob")))

		got, err := team.DefaultAgent()
		require.NoError(t, err)
		assert.Equal(t, "alice", got.Name())
	})
}

func TestAgentOrDefault(t *testing.T) {
	t.Run("empty name resolves to the default agent", func(t *testing.T) {
		team := New(WithAgents(newAgent("alice"), newAgent("root")))

		got, err := team.AgentOrDefault("")
		require.NoError(t, err)
		assert.Equal(t, "root", got.Name())
	})

	t.Run("empty name without root falls back to the first agent", func(t *testing.T) {
		team := New(WithAgents(newAgent("alice"), newAgent("bob")))

		got, err := team.AgentOrDefault("")
		require.NoError(t, err)
		assert.Equal(t, "alice", got.Name())
	})

	t.Run("explicit name is honored even when a root exists", func(t *testing.T) {
		team := New(WithAgents(newAgent("root"), newAgent("alice")))

		got, err := team.AgentOrDefault("alice")
		require.NoError(t, err)
		assert.Equal(t, "alice", got.Name())
	})

	t.Run("unknown name returns an error listing the available agents", func(t *testing.T) {
		team := New(WithAgents(newAgent("alice"), newAgent("bob")))

		_, err := team.AgentOrDefault("missing")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing")
		assert.Contains(t, err.Error(), "alice")
		assert.Contains(t, err.Error(), "bob")
	})

	t.Run("empty team returns an error for both empty and explicit names", func(t *testing.T) {
		team := New()

		_, err := team.AgentOrDefault("")
		require.Error(t, err)

		_, err = team.AgentOrDefault("anything")
		require.Error(t, err)
	})
}
