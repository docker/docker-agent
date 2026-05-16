package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// preToolUseHooksConfig builds a hooks.Config that wires `name` as a
// pre_tool_use builtin matched against every tool. Use with
// agent.WithHooks(...).
func preToolUseHooksConfig(name string) *latest.HooksConfig {
	return &latest.HooksConfig{
		PreToolUse: []latest.HookMatcherConfig{{
			Matcher: ".*",
			Hooks: []latest.HookDefinition{{
				Type:    hooks.HookTypeBuiltin,
				Command: name,
				Timeout: 5,
			}},
		}},
	}
}

// recordingTool is a tools.Tool that flips its `executed` field when
// invoked. Centralising the boilerplate keeps the per-test bodies
// short and focused on the assertion that matters.
func recordingTool(name string, executed *bool) []tools.Tool {
	return []tools.Tool{{
		Name:       name,
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolCallResult, error) {
			*executed = true
			return tools.ResultSuccess("ok"), nil
		},
	}}
}

// collectClosedEvents reads every event already buffered on ch and
// returns the slice. It assumes the caller closed ch (or that no
// further events are produced); used after processToolCalls returns.
// Distinct from drainEvents in loop_steps_test.go, which is
// non-blocking and intended for still-open channels.
func collectClosedEvents(ch chan Event) []Event {
	var out []Event
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

// hasEventOfType reports whether evs contains an event of the given
// concrete type. Used to assert that a ToolCallConfirmationEvent was
// (or was not) emitted without caring about its other fields.
func hasEventOfType[E Event](evs []Event) bool {
	for _, ev := range evs {
		if _, ok := ev.(E); ok {
			return true
		}
	}
	return false
}

// makeJudgedRuntime wires a verdict-returning builtin onto a fresh
// runtime, plus a recording tool, and returns the pieces needed by the
// per-decision tests below. Permissions can be supplied to test the
// interaction between deterministic rules and the hook verdict.
//
// We construct the runtime first (so we have access to its private
// registry), register the verdict builtin on it, and only then exercise
// the agent — the hooks Executor is built lazily on first use, so by
// the time the matcher resolves "command" against the registry, our
// builtin is there.
func makeJudgedRuntime(
	t *testing.T,
	verdict hooks.Decision,
	reason string,
	perm *permissions.Checker,
) (rt *LocalRuntime, sess *session.Session, executed *bool, agentTools []tools.Tool) {
	t.Helper()

	executed = new(bool)
	agentTools = recordingTool("the_tool", executed)

	hookName := "test_judge_" + t.Name()
	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "instr",
		agent.WithModel(prov),
		agent.WithToolSets(newStubToolSet(nil, agentTools, nil)),
		agent.WithHooks(preToolUseHooksConfig(hookName)),
	)

	teamOpts := []team.Opt{team.WithAgents(root)}
	if perm != nil {
		teamOpts = append(teamOpts, team.WithPermissions(perm))
	}
	tm := team.New(teamOpts...)

	var err error
	rt, err = NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	require.NoError(t, rt.hooksRegistry.RegisterBuiltin(hookName, func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
		if in == nil || in.HookEventName != hooks.EventPreToolUse {
			return nil, nil
		}
		return &hooks.Output{
			HookSpecificOutput: &hooks.HookSpecificOutput{
				HookEventName:            hooks.EventPreToolUse,
				PermissionDecision:       verdict,
				PermissionDecisionReason: reason,
			},
		}, nil
	}))

	sess = session.New(session.WithUserMessage("test"))
	require.False(t, sess.ToolsApproved, "fixture must not use --yolo")
	return rt, sess, executed, agentTools
}

// runJudgedToolCall is the assertion-friendly wrapper around
// processToolCalls used by the per-decision tests. It returns whether
// the tool ran and the captured events so callers can also check that
// the user prompt did or did not fire.
func runJudgedToolCall(t *testing.T, rt *LocalRuntime, sess *session.Session, agentTools []tools.Tool) []Event {
	t.Helper()
	calls := []tools.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "the_tool", Arguments: "{}"},
	}}
	events := make(chan Event, 16)
	rt.processToolCalls(t.Context(), sess, calls, agentTools, NewChannelSink(events))
	close(events)
	return collectClosedEvents(events)
}

// TestPreToolUseHook_AllowAutoApproves verifies the headline new
// behavior: a pre_tool_use hook returning Allow auto-approves the
// tool call, the user is NOT prompted, and the tool runs. This is the
// LLM-as-a-judge happy path.
func TestPreToolUseHook_AllowAutoApproves(t *testing.T) {
	t.Parallel()

	rt, sess, executed, agentTools := makeJudgedRuntime(t, hooks.DecisionAllow, "judged safe", nil)
	evs := runJudgedToolCall(t, rt, sess, agentTools)

	assert.True(t, *executed, "Allow verdict must auto-approve and run the tool")
	assert.False(t, hasEventOfType[*ToolCallConfirmationEvent](evs),
		"Allow must skip the user prompt: no ToolCallConfirmationEvent expected")
}

// TestPreToolUseHook_DenyBlocksWithoutPrompting verifies that a Deny
// verdict blocks the tool BEFORE the user is asked (the previous
// implementation prompted the user and only then denied — wasting a
// click and confusing the UX).
func TestPreToolUseHook_DenyBlocksWithoutPrompting(t *testing.T) {
	t.Parallel()

	rt, sess, executed, agentTools := makeJudgedRuntime(t, hooks.DecisionDeny, "destructive", nil)
	evs := runJudgedToolCall(t, rt, sess, agentTools)

	assert.False(t, *executed, "Deny verdict must block the tool")
	assert.False(t, hasEventOfType[*ToolCallConfirmationEvent](evs),
		"Deny must NOT prompt the user before blocking")
	assert.True(t, hasEventOfType[*HookBlockedEvent](evs),
		"Deny must emit a HookBlockedEvent so the UI can surface the reason")
}

// TestPreToolUseHook_AskEscalatesToUser verifies that an Ask verdict
// falls through to the user-confirmation prompt instead of either
// auto-approving (Allow) or blocking (Deny). The tool does not run
// here because no resume signal is delivered.
func TestPreToolUseHook_AskEscalatesToUser(t *testing.T) {
	t.Parallel()

	rt, sess, executed, agentTools := makeJudgedRuntime(t, hooks.DecisionAsk, "unclear", nil)

	// processToolCalls blocks waiting for resume on Ask. Run it in a
	// goroutine and cancel via context to unstick once we've observed
	// the prompt event.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	calls := []tools.ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: tools.FunctionCall{Name: "the_tool", Arguments: "{}"},
	}}
	events := make(chan Event, 16)
	done := make(chan struct{})
	go func() {
		rt.processToolCalls(ctx, sess, calls, agentTools, NewChannelSink(events))
		close(done)
	}()

	// Wait for the prompt event, then cancel.
	gotPrompt := false
DRAIN:
	for {
		select {
		case ev := <-events:
			if _, ok := ev.(*ToolCallConfirmationEvent); ok {
				gotPrompt = true
				cancel()
				break DRAIN
			}
		case <-t.Context().Done():
			break DRAIN
		}
	}
	<-done
	close(events)

	assert.True(t, gotPrompt, "Ask verdict must escalate to a ToolCallConfirmationEvent")
	assert.False(t, *executed, "tool must not run when user has not approved")
}

// TestPreToolUseHook_PermissionsDenyWinsOverHookAllow pins the
// security-critical ordering: deterministic permissions DENY rules
// must win even if the LLM judge says Allow. This protects against
// prompt-injection attacks that try to flip the verdict.
func TestPreToolUseHook_PermissionsDenyWinsOverHookAllow(t *testing.T) {
	t.Parallel()

	checker := permissions.NewChecker(&latest.PermissionsConfig{
		Deny: []string{"the_tool"},
	})
	rt, sess, executed, agentTools := makeJudgedRuntime(t, hooks.DecisionAllow, "judged safe", checker)
	_ = runJudgedToolCall(t, rt, sess, agentTools)

	assert.False(t, *executed,
		"deterministic Deny must beat hook Allow — protects against jailbroken judges")
}

// TestPreToolUseHook_PermissionsAllowShortCircuitsHook documents the
// cost-saving ordering: deterministic permissions ALLOW rules run the
// tool without consulting the hook at all. This keeps the LLM judge
// off the hot path for obviously-safe tool calls.
func TestPreToolUseHook_PermissionsAllowShortCircuitsHook(t *testing.T) {
	t.Parallel()

	checker := permissions.NewChecker(&latest.PermissionsConfig{
		Allow: []string{"the_tool"},
	})
	// The hook would Deny if asked. Deterministic Allow must win and
	// the hook must NOT be consulted (the tool runs).
	rt, sess, executed, agentTools := makeJudgedRuntime(t, hooks.DecisionDeny, "would-deny", checker)
	_ = runJudgedToolCall(t, rt, sess, agentTools)

	assert.True(t, *executed,
		"deterministic Allow must short-circuit the hook (cost / latency win)")
}
