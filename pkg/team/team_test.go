package team

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

func newAgent(name string) *agent.Agent {
	return agent.New(name, "")
}

// stopRecordingToolSet is a Startable ToolSet whose Stop records calls and
// returns stopErr.
type stopRecordingToolSet struct {
	stopErr error
	stops   atomic.Int32
}

func (s *stopRecordingToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (s *stopRecordingToolSet) Start(context.Context) error                 { return nil }
func (s *stopRecordingToolSet) Stop(context.Context) error {
	s.stops.Add(1)
	return s.stopErr
}

// TestStopToolSetsContinuesAcrossAgents pins the team-level aggregation
// contract: an agent whose toolsets fail to stop must not abandon stopping
// the later agents' toolsets, and every failure surfaces in the joined
// error together with the owning agent's name.
func TestStopToolSetsContinuesAcrossAgents(t *testing.T) {
	t.Parallel()

	errFirst := errors.New("first boom")
	errSecond := errors.New("second boom")
	tsFirst := &stopRecordingToolSet{stopErr: errFirst}
	tsSecond := &stopRecordingToolSet{stopErr: errSecond}
	tsHealthy := &stopRecordingToolSet{}

	first := agent.New("first", "", agent.WithToolSets(tsFirst))
	second := agent.New("second", "", agent.WithToolSets(tsSecond))
	third := agent.New("third", "", agent.WithToolSets(tsHealthy))
	tm := New(WithAgents(first, second, third))

	// Start every toolset so StopToolSets has something to stop.
	for _, a := range []*agent.Agent{first, second, third} {
		for _, ts := range a.ToolSets() {
			startable, ok := ts.(*tools.StartableToolSet)
			require.True(t, ok)
			require.NoError(t, startable.Start(t.Context()))
		}
	}

	err := tm.StopToolSets(t.Context())
	require.ErrorIs(t, err, errFirst)
	require.ErrorIs(t, err, errSecond)
	assert.Contains(t, err.Error(), "first")
	assert.Contains(t, err.Error(), "second")
	assert.EqualValues(t, 1, tsFirst.stops.Load())
	assert.EqualValues(t, 1, tsSecond.stops.Load(), "an earlier failing agent must not abandon the next agent's toolsets")
	assert.EqualValues(t, 1, tsHealthy.stops.Load(), "a failing earlier agent must not abandon stopping later agents' toolsets")
}

// RuntimeSafety returns the config-wide default set via WithRuntimeSafety,
// empty when unset.
func TestRuntimeSafety(t *testing.T) {
	t.Parallel()
	assert.Equal(t, latest.SafetyMode(""), New().RuntimeSafety())
	assert.Equal(t, latest.SafetyModeBalanced, New(WithRuntimeSafety(latest.SafetyModeBalanced)).RuntimeSafety())
}

func TestDefaultAgent(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// TestAgentConfig verifies the raw per-agent config retained via
// WithAgentConfigs is returned by name, and that callers can distinguish a
// team built without configs (remote runtime) from one built with them: both
// the unknown-agent and no-configs cases report false so the inspector omits
// config-derived sections.
func TestAgentConfig(t *testing.T) {
	t.Parallel()

	configs := map[string]latest.AgentConfig{
		"root": {Name: "root", Model: "openai/gpt-5", MaxIterations: 42},
	}

	t.Run("returns retained config by name", func(t *testing.T) {
		t.Parallel()
		tm := New(WithAgents(newAgent("root")), WithAgentConfigs(configs))

		cfg, ok := tm.AgentConfig("root")
		require.True(t, ok)
		assert.Equal(t, "openai/gpt-5", cfg.Model)
		assert.Equal(t, 42, cfg.MaxIterations)
	})

	t.Run("unknown agent returns false", func(t *testing.T) {
		t.Parallel()
		tm := New(WithAgents(newAgent("root")), WithAgentConfigs(configs))

		_, ok := tm.AgentConfig("missing")
		assert.False(t, ok)
	})

	t.Run("team built without configs returns false", func(t *testing.T) {
		t.Parallel()
		tm := New(WithAgents(newAgent("root")))

		_, ok := tm.AgentConfig("root")
		assert.False(t, ok)
	})
}
