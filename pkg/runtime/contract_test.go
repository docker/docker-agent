package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// runRuntimeContract exercises every non-streaming method on the Runtime
// interface. It runs against any factory so that LocalRuntime and (in a
// future commit) RemoteRuntime can be checked against the same expectations.
//
// Streaming methods (RunStream, Run, Summarize) are exercised by the
// type-specific test files because they need real models or providers.
func runRuntimeContract(t *testing.T, newRT func(t *testing.T) Runtime) {
	t.Helper()

	t.Run("CurrentAgentName not empty", func(t *testing.T) {
		rt := newRT(t)
		t.Cleanup(func() { _ = rt.Close() })
		assert.NotEmpty(t, rt.CurrentAgentName())
	})

	t.Run("CurrentAgentInfo carries the current name", func(t *testing.T) {
		rt := newRT(t)
		t.Cleanup(func() { _ = rt.Close() })
		info := rt.CurrentAgentInfo(t.Context())
		assert.Equal(t, rt.CurrentAgentName(), info.Name)
	})

	t.Run("SetCurrentAgent rejects unknown agents", func(t *testing.T) {
		rt := newRT(t)
		t.Cleanup(func() { _ = rt.Close() })
		err := rt.SetCurrentAgent("nonexistent-agent")
		assert.Error(t, err)
	})

	t.Run("CurrentAgentTools returns slice or ErrUnsupported", func(t *testing.T) {
		rt := newRT(t)
		t.Cleanup(func() { _ = rt.Close() })
		_, err := rt.CurrentAgentTools(t.Context())
		if err != nil {
			assert.ErrorIs(t, err, ErrUnsupported, "unexpected error type: %v", err)
		}
	})

	t.Run("CurrentAgentToolsetStatuses does not panic", func(t *testing.T) {
		rt := newRT(t)
		t.Cleanup(func() { _ = rt.Close() })
		_ = rt.CurrentAgentToolsetStatuses()
	})

	t.Run("RestartToolset on unknown name returns error", func(t *testing.T) {
		rt := newRT(t)
		t.Cleanup(func() { _ = rt.Close() })
		err := rt.RestartToolset(t.Context(), "nonexistent-toolset")
		assert.Error(t, err)
	})

	t.Run("CurrentMCPPrompts returns a non-nil map", func(t *testing.T) {
		rt := newRT(t)
		t.Cleanup(func() { _ = rt.Close() })
		prompts := rt.CurrentMCPPrompts(t.Context())
		assert.NotNil(t, prompts)
	})

	t.Run("ExecuteMCPPrompt for unknown prompt returns error", func(t *testing.T) {
		rt := newRT(t)
		t.Cleanup(func() { _ = rt.Close() })
		_, err := rt.ExecuteMCPPrompt(t.Context(), "nonexistent-prompt", nil)
		assert.Error(t, err)
	})

	t.Run("Steer accepts a message", func(t *testing.T) {
		rt := newRT(t)
		t.Cleanup(func() { _ = rt.Close() })
		err := rt.Steer(QueuedMessage{Content: "hello"})
		if err != nil {
			assert.ErrorIs(t, err, ErrUnsupported, "unexpected error type: %v", err)
		}
	})

	t.Run("FollowUp accepts a message", func(t *testing.T) {
		rt := newRT(t)
		t.Cleanup(func() { _ = rt.Close() })
		err := rt.FollowUp(QueuedMessage{Content: "hello"})
		if err != nil {
			assert.ErrorIs(t, err, ErrUnsupported, "unexpected error type: %v", err)
		}
	})

	t.Run("ResetStartupInfo is idempotent", func(t *testing.T) {
		rt := newRT(t)
		t.Cleanup(func() { _ = rt.Close() })
		rt.ResetStartupInfo()
		rt.ResetStartupInfo()
	})

	t.Run("Close is idempotent", func(t *testing.T) {
		rt := newRT(t)
		require.NoError(t, rt.Close())
		// A second Close may error or not; both are acceptable as long as it
		// doesn't panic.
		_ = rt.Close()
	})

	t.Run("Resume on a fresh runtime is non-blocking", func(t *testing.T) {
		rt := newRT(t)
		t.Cleanup(func() { _ = rt.Close() })
		// No tool call is in flight; Resume must not deadlock.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		rt.Resume(ctx, ResumeApprove())
	})

	t.Run("ResumeElicitation outside an elicitation does not panic", func(t *testing.T) {
		rt := newRT(t)
		t.Cleanup(func() { _ = rt.Close() })
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_ = rt.ResumeElicitation(ctx, tools.ElicitationActionDecline, nil)
	})
}

func TestLocalRuntime_Contract(t *testing.T) {
	runRuntimeContract(t, func(t *testing.T) Runtime {
		t.Helper()
		prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
		root := agent.New("root", "You are a test agent", agent.WithModel(prov))
		tm := team.New(team.WithAgents(root))

		rt, err := New(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)
		return rt
	})
}
