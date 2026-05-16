package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/team"
)

// runtimeWithRecordedSessionResume mirrors runtimeWithRecordedAgentSwitch
// for the on_session_resume event. Same pattern: register a recording
// builtin on the runtime's private registry post-construction so the
// test can assert on dispatched input without exposing a runtime
// option that production callers shouldn't reach for.
func runtimeWithRecordedSessionResume(t *testing.T) (*LocalRuntime, *recordingBuiltin) {
	t.Helper()

	rb := &recordingBuiltin{}
	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	a := agent.New("root", "instructions",
		agent.WithModel(prov),
		agent.WithHooks(&hooks.Config{
			OnSessionResume: []hooks.Hook{{
				Type:    hooks.HookTypeBuiltin,
				Command: "test_record_session_resume",
			}},
		}),
	)
	tm := team.New(team.WithAgents(a))

	r, err := NewLocalRuntime(tm, WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	require.NoError(t, r.hooksRegistry.RegisterBuiltin("test_record_session_resume", rb.hook))
	r.buildHooksExecutors()

	return r, rb
}

// TestExecuteOnSessionResumeHooks_ForwardsLimits pins the contract:
// PreviousMaxIterations and NewMaxIterations both reach the hook
// verbatim. Audit pipelines compute the granted-runtime delta from
// these directly without rebuilding it from the iteration counter.
func TestExecuteOnSessionResumeHooks_ForwardsLimits(t *testing.T) {
	t.Parallel()

	r, rb := runtimeWithRecordedSessionResume(t)
	a := r.CurrentAgent()
	require.NotNil(t, a)

	r.executeOnSessionResumeHooks(t.Context(), a, "session-y", 5, 15)

	got := rb.snapshot()
	require.Len(t, got, 1)
	in := got[0]
	assert.Equal(t, "session-y", in.SessionID)
	assert.Equal(t, 5, in.PreviousMaxIterations)
	assert.Equal(t, 15, in.NewMaxIterations)
}

// TestExecuteOnSessionResumeHooks_NoopWhenNoHookRegistered keeps the
// cheap-when-unused property symmetric with on_agent_switch: no
// dispatch, no panic, no error when no hook is configured.
func TestExecuteOnSessionResumeHooks_NoopWhenNoHookRegistered(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	a := agent.New("root", "instructions", agent.WithModel(prov))
	tm := team.New(team.WithAgents(a))

	r, err := NewLocalRuntime(tm, WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	r.executeOnSessionResumeHooks(t.Context(), a, "s", 5, 15)
}
