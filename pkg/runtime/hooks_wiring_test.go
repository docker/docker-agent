package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/team"
)

// TestHooksExecWiresAgentFlagsToBuiltins verifies the wiring performed
// by [LocalRuntime.buildHooksExecutors] (and the underlying
// [builtins.ApplyAgentDefaults]): the always-on large result limiter must
// be present, and agent.AddDate / AddEnvironmentInfo / AddPromptFiles flags
// must translate into builtin hook entries on the right event:
//
//   - AddDate           -> turn_start (re-evaluated every turn)
//   - AddPromptFiles    -> turn_start (file may be edited mid-session)
//   - AddEnvironmentInfo -> session_start (wd/OS/arch don't change)
//
// The behavior of each builtin (what it puts in AdditionalContext) is
// covered by pkg/hooks/builtins; this test only asserts the wiring, plus a
// smoke Dispatch of the builtins wired by builtins.ApplyAgentDefaults.
// These events don't block, so an unresolved builtin name would only show
// up in the result's FailedHooks/SystemMessage, which the test doesn't
// check.
func TestHooksExecWiresAgentFlagsToBuiltins(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}

	cases := []struct {
		name            string
		opts            []agent.Opt
		wantOnlyLimiter bool
		wantTurnStart   bool
		wantSessStart   bool
	}{
		{
			name:            "no flags: only large-result limiter",
			opts:            []agent.Opt{agent.WithModel(prov)},
			wantOnlyLimiter: true,
		},
		{
			name:          "AddDate wires turn_start",
			opts:          []agent.Opt{agent.WithModel(prov), agent.WithAddDate(true)},
			wantTurnStart: true,
		},
		{
			name:          "AddPromptFiles wires turn_start",
			opts:          []agent.Opt{agent.WithModel(prov), agent.WithAddPromptFiles([]string{"PROMPT.md"})},
			wantTurnStart: true,
		},
		{
			name:          "AddEnvironmentInfo wires session_start",
			opts:          []agent.Opt{agent.WithModel(prov), agent.WithAddEnvironmentInfo(true)},
			wantSessStart: true,
		},
		{
			name: "all flags route to their respective events",
			opts: []agent.Opt{
				agent.WithModel(prov),
				agent.WithAddDate(true),
				agent.WithAddPromptFiles([]string{"PROMPT.md"}),
				agent.WithAddEnvironmentInfo(true),
			},
			wantTurnStart: true,
			wantSessStart: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := agent.New("root", "instructions", tc.opts...)
			tm := team.New(team.WithAgents(a))
			r, err := NewLocalRuntime(t.Context(), tm, WithModelStore(mockModelStore{}))
			require.NoError(t, err)

			exec := r.hooksExec(a)
			require.NotNil(t, exec)

			// hooksExec is read-only after [LocalRuntime.buildHooksExecutors],
			// so calling it twice returns the same pointer.
			assert.Same(t, exec, r.hooksExec(a), "hooksExec must be stable across calls")

			assert.True(t, exec.Has(hooks.EventToolResponseTransform),
				"large-result limiter must be wired for every agent")
			if tc.wantOnlyLimiter {
				assert.False(t, exec.Has(hooks.EventTurnStart))
				assert.False(t, exec.Has(hooks.EventSessionStart))
				return
			}

			assert.Equal(t, tc.wantTurnStart, exec.Has(hooks.EventTurnStart),
				"turn_start activation must match flags")
			assert.Equal(t, tc.wantSessStart, exec.Has(hooks.EventSessionStart),
				"session_start activation must match flags")

			// Smoke Dispatch of the builtins wired by
			// builtins.ApplyAgentDefaults on the runtime's private registry
			// (populated by builtins.Register).
			if tc.wantTurnStart {
				res, err := exec.Dispatch(t.Context(), hooks.EventTurnStart, &hooks.Input{
					SessionID: "test-session",
					Cwd:       t.TempDir(),
				})
				require.NoError(t, err)
				assert.True(t, res.Allowed, "turn_start dispatch must succeed")
			}
			if tc.wantSessStart {
				res, err := exec.Dispatch(t.Context(), hooks.EventSessionStart, &hooks.Input{
					SessionID: "test-session",
					Cwd:       t.TempDir(),
					Source:    "startup",
				})
				require.NoError(t, err)
				assert.True(t, res.Allowed, "session_start dispatch must succeed")
			}
		})
	}
}

// TestModelHookFactoryIsRegistered pins the wiring of the model hook:
// NewLocalRuntime must register a [hooks.HookTypeModel] factory so
// agents can reference `{type: model, model: ..., prompt: ...}` in
// YAML without any additional setup. We assert at the registry level
// (rather than dispatching a hook) because the real ModelClient would
// otherwise need network and credentials.
func TestModelHookFactoryIsRegistered(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	a := agent.New("root", "instructions", agent.WithModel(prov))
	tm := team.New(team.WithAgents(a))
	r, err := NewLocalRuntime(t.Context(), tm, WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	factory, ok := r.hooksRegistry.Lookup(hooks.HookTypeModel)
	assert.True(t, ok, "model hook factory must be registered by NewLocalRuntime")
	assert.NotNil(t, factory)
}
