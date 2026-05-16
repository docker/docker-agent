package hooks_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
)

// TestPostToolUseBlockProducesDenyResult pins the contract widening
// for the post_tool_use event: a hook returning decision="block"
// must produce Result.Allowed=false. The runtime relies on this to
// drive its hook-driven shutdown path (loop_detector et al).
//
// The same test would have passed before the widening at the
// executor layer (the aggregate function has always set Allowed=false
// uniformly across events) — pinning it here documents the behavior
// as part of the public contract so a future refactor can't silently
// regress it.
func TestPostToolUseBlockProducesDenyResult(t *testing.T) {
	t.Parallel()

	r := hooks.NewRegistry()
	require.NoError(t, r.RegisterBuiltin("blocker", func(_ context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
		return &hooks.Output{
			Decision: hooks.DecisionBlockValue,
			Reason:   "stop",
		}, nil
	}))

	exec := hooks.NewExecutorWithRegistry(&hooks.Config{
		PostToolUse: []hooks.MatcherConfig{{
			Matcher: "*",
			Hooks: []hooks.Hook{{
				Type:    hooks.HookTypeBuiltin,
				Command: "blocker",
			}},
		}},
	}, t.TempDir(), nil, r)

	res, err := exec.Dispatch(t.Context(), hooks.EventPostToolUse, &hooks.Input{
		SessionID: "s",
		ToolName:  "any",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.Allowed,
		"post_tool_use block must produce Allowed=false")
	assert.Contains(t, res.Message, "stop",
		"reason must be propagated as the Result message")
}

// TestBeforeLLMCallBlockProducesDenyResult is the symmetric pin for
// before_llm_call: max_iterations and other budget-style builtins
// rely on a block decision producing Allowed=false to stop the run
// before the model is invoked.
func TestBeforeLLMCallBlockProducesDenyResult(t *testing.T) {
	t.Parallel()

	r := hooks.NewRegistry()
	require.NoError(t, r.RegisterBuiltin("blocker", func(_ context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
		return &hooks.Output{
			Decision: hooks.DecisionBlockValue,
			Reason:   "budget exhausted",
		}, nil
	}))

	exec := hooks.NewExecutorWithRegistry(&hooks.Config{
		BeforeLLMCall: []hooks.Hook{{
			Type:    hooks.HookTypeBuiltin,
			Command: "blocker",
		}},
	}, t.TempDir(), nil, r)

	res, err := exec.Dispatch(t.Context(), hooks.EventBeforeLLMCall, &hooks.Input{
		SessionID: "s",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.Allowed,
		"before_llm_call block must produce Allowed=false")
	assert.Contains(t, res.Message, "budget exhausted")
}

// TestPostToolUseContinueFalseProducesDenyResult documents that the
// continue=false form of the deny verdict produces the same
// Allowed=false outcome as decision="block". This is what allows
// shell-based hooks (which can't emit a structured Decision field)
// to participate in the run-termination contract.
func TestPostToolUseContinueFalseProducesDenyResult(t *testing.T) {
	t.Parallel()

	r := hooks.NewRegistry()
	require.NoError(t, r.RegisterBuiltin("stopper", func(_ context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
		stop := false
		return &hooks.Output{
			Continue:   &stop,
			StopReason: "budget exhausted",
		}, nil
	}))

	exec := hooks.NewExecutorWithRegistry(&hooks.Config{
		PostToolUse: []hooks.MatcherConfig{{
			Matcher: "*",
			Hooks: []hooks.Hook{{
				Type:    hooks.HookTypeBuiltin,
				Command: "stopper",
			}},
		}},
	}, t.TempDir(), nil, r)

	res, err := exec.Dispatch(t.Context(), hooks.EventPostToolUse, &hooks.Input{
		SessionID: "s",
		ToolName:  "any",
	})
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Contains(t, res.Message, "budget exhausted")
}

// TestUserPromptSubmitBlockProducesDenyResult pins the contract for
// the user_prompt_submit event: a hook returning decision="block" must
// produce Result.Allowed=false so the runtime can short-circuit the
// turn before the model is invoked.
func TestUserPromptSubmitBlockProducesDenyResult(t *testing.T) {
	t.Parallel()

	r := hooks.NewRegistry()
	require.NoError(t, r.RegisterBuiltin("redactor", func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
		return &hooks.Output{
			Decision: hooks.DecisionBlockValue,
			Reason:   "prompt rejected: " + in.Prompt,
		}, nil
	}))

	exec := hooks.NewExecutorWithRegistry(&hooks.Config{
		UserPromptSubmit: []hooks.Hook{{
			Type:    hooks.HookTypeBuiltin,
			Command: "redactor",
		}},
	}, t.TempDir(), nil, r)

	res, err := exec.Dispatch(t.Context(), hooks.EventUserPromptSubmit, &hooks.Input{
		SessionID: "s",
		Prompt:    "do the dangerous thing",
	})
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Contains(t, res.Message, "do the dangerous thing",
		"hook must see the user prompt via Input.Prompt")
}

// TestPreCompactBlockProducesDenyResult pins the contract for the
// pre_compact event: a hook returning decision="block" must produce
// Result.Allowed=false so summarizeWithSource skips compaction.
func TestPreCompactBlockProducesDenyResult(t *testing.T) {
	t.Parallel()

	r := hooks.NewRegistry()
	require.NoError(t, r.RegisterBuiltin("veto", func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
		return &hooks.Output{
			Decision: hooks.DecisionBlockValue,
			Reason:   "vetoed compaction (source=" + in.Source + ")",
		}, nil
	}))

	exec := hooks.NewExecutorWithRegistry(&hooks.Config{
		PreCompact: []hooks.Hook{{
			Type:    hooks.HookTypeBuiltin,
			Command: "veto",
		}},
	}, t.TempDir(), nil, r)

	res, err := exec.Dispatch(t.Context(), hooks.EventPreCompact, &hooks.Input{
		SessionID: "s",
		Source:    "manual",
	})
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Contains(t, res.Message, "source=manual",
		"hook must see the compaction trigger via Input.Source")
}

// TestSubagentStopReceivesAgentName pins the contract for the
// subagent_stop event: agent_name and stop_response must reach the
// hook so handlers can identify which child finished and inspect its
// final assistant message.
func TestSubagentStopReceivesAgentName(t *testing.T) {
	t.Parallel()

	var gotName, gotResponse, gotParent string
	r := hooks.NewRegistry()
	require.NoError(t, r.RegisterBuiltin("capture", func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
		gotName = in.AgentName
		gotResponse = in.StopResponse
		gotParent = in.ParentSessionID
		return nil, nil
	}))

	exec := hooks.NewExecutorWithRegistry(&hooks.Config{
		SubagentStop: []hooks.Hook{{
			Type:    hooks.HookTypeBuiltin,
			Command: "capture",
		}},
	}, t.TempDir(), nil, r)

	_, err := exec.Dispatch(t.Context(), hooks.EventSubagentStop, &hooks.Input{
		SessionID:       "child-id",
		ParentSessionID: "parent-id",
		AgentName:       "researcher",
		StopResponse:    "done",
	})
	require.NoError(t, err)
	assert.Equal(t, "researcher", gotName)
	assert.Equal(t, "done", gotResponse)
	assert.Equal(t, "parent-id", gotParent)
}

// TestPermissionRequestAllowProducesPermissionAllowed pins the
// contract for the permission_request event: a hook returning
// permission_decision="allow" must produce
// Result.PermissionAllowed=true so the runtime skips the interactive
// confirmation. A bare deny (decision="block") must continue to
// produce Allowed=false, consistent with pre_tool_use.
func TestPermissionRequestAllowProducesPermissionAllowed(t *testing.T) {
	t.Parallel()

	r := hooks.NewRegistry()
	require.NoError(t, r.RegisterBuiltin("approver", func(_ context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
		return &hooks.Output{
			HookSpecificOutput: &hooks.HookSpecificOutput{
				HookEventName:            hooks.EventPermissionRequest,
				PermissionDecision:       hooks.DecisionAllow,
				PermissionDecisionReason: "safe",
			},
		}, nil
	}))

	exec := hooks.NewExecutorWithRegistry(&hooks.Config{
		PermissionRequest: []hooks.MatcherConfig{{
			Matcher: "*",
			Hooks: []hooks.Hook{{
				Type:    hooks.HookTypeBuiltin,
				Command: "approver",
			}},
		}},
	}, t.TempDir(), nil, r)

	res, err := exec.Dispatch(t.Context(), hooks.EventPermissionRequest, &hooks.Input{
		SessionID: "s",
		ToolName:  "shell",
	})
	require.NoError(t, err)
	assert.True(t, res.Allowed,
		"allow decision must NOT flip Result.Allowed")
	assert.True(t, res.PermissionAllowed,
		"permission_decision=allow must produce PermissionAllowed=true")
}

// TestPermissionRequestDenyProducesDenyResult is the symmetric pin:
// permission_decision="deny" must flip Allowed=false and propagate
// the reason via Result.Message, mirroring pre_tool_use behaviour.
func TestPermissionRequestDenyProducesDenyResult(t *testing.T) {
	t.Parallel()

	r := hooks.NewRegistry()
	require.NoError(t, r.RegisterBuiltin("denier", func(_ context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
		return &hooks.Output{
			HookSpecificOutput: &hooks.HookSpecificOutput{
				HookEventName:            hooks.EventPermissionRequest,
				PermissionDecision:       hooks.DecisionDeny,
				PermissionDecisionReason: "policy violation",
			},
		}, nil
	}))

	exec := hooks.NewExecutorWithRegistry(&hooks.Config{
		PermissionRequest: []hooks.MatcherConfig{{
			Matcher: "*",
			Hooks: []hooks.Hook{{
				Type:    hooks.HookTypeBuiltin,
				Command: "denier",
			}},
		}},
	}, t.TempDir(), nil, r)

	res, err := exec.Dispatch(t.Context(), hooks.EventPermissionRequest, &hooks.Input{
		SessionID: "s",
		ToolName:  "shell",
	})
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.False(t, res.PermissionAllowed)
	assert.Contains(t, res.Message, "policy violation")
}
