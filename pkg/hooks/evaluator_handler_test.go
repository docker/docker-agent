package hooks

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/evaluator"
)

type evaluatorFunc func(context.Context, any) (*evaluator.Result, error)

func (f evaluatorFunc) Evaluate(ctx context.Context, state any) (*evaluator.Result, error) {
	return f(ctx, state)
}

func evaluatorTestHook() Hook {
	return Hook{
		Type:      HookTypeEvaluator,
		Evaluator: "guard",
		EvaluatorPolicy: &latest.EvaluatorPolicy{
			Decisions:      map[string]string{"true": "allow", "false": "deny", "safe": "allow", "unsafe": "deny"},
			MinProbability: 0.8,
			Fallback:       "ask",
		},
	}
}

func evaluatorTestHandler(t *testing.T, client evaluator.Evaluator, hook Hook) Handler {
	t.Helper()
	factory := NewEvaluatorFactory(func(_, name string) (evaluator.Evaluator, bool) {
		assert.Equal(t, "guard", name)
		return client, name == "guard"
	})
	handler, err := factory(HandlerEnv{}, hook)
	require.NoError(t, err)
	return handler
}

func TestNewEvaluatorFactoryRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		change func(*Hook)
		want   string
	}{
		{"missing reference", func(h *Hook) { h.Evaluator = "" }, "non-empty evaluator reference"},
		{"blank reference", func(h *Hook) { h.Evaluator = " \t" }, "non-empty evaluator reference"},
		{"missing policy", func(h *Hook) { h.EvaluatorPolicy = nil }, "evaluator_policy is required"},
		{"empty decisions", func(h *Hook) { h.EvaluatorPolicy.Decisions = nil }, "decisions must not be empty"},
		{"invalid decision", func(h *Hook) { h.EvaluatorPolicy.Decisions["true"] = "block" }, "decisions must be"},
		{"zero threshold", func(h *Hook) { h.EvaluatorPolicy.MinProbability = 0 }, "min_probability"},
		{"negative threshold", func(h *Hook) { h.EvaluatorPolicy.MinProbability = -0.1 }, "min_probability"},
		{"threshold above one", func(h *Hook) { h.EvaluatorPolicy.MinProbability = 1.1 }, "min_probability"},
		{"nan threshold", func(h *Hook) { h.EvaluatorPolicy.MinProbability = math.NaN() }, "min_probability"},
		{"infinite threshold", func(h *Hook) { h.EvaluatorPolicy.MinProbability = math.Inf(1) }, "min_probability"},
		{"missing fallback", func(h *Hook) { h.EvaluatorPolicy.Fallback = "" }, "fallback must be"},
		{"allow fallback", func(h *Hook) { h.EvaluatorPolicy.Fallback = "allow" }, "fallback must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hook := evaluatorTestHook()
			tc.change(&hook)
			factory := NewEvaluatorFactory(func(string, string) (evaluator.Evaluator, bool) {
				t.Error("invalid config must be rejected before lookup")
				return nil, false
			})
			handler, err := factory(HandlerEnv{}, hook)
			require.ErrorContains(t, err, tc.want)
			assert.Nil(t, handler)
		})
	}
}

func TestNewEvaluatorFactoryRejectsMissingLookup(t *testing.T) {
	t.Parallel()

	handler, err := NewEvaluatorFactory(nil)(HandlerEnv{}, evaluatorTestHook())
	require.ErrorContains(t, err, "no evaluator lookup configured")
	assert.Nil(t, handler)
}

func TestEvaluatorHandlerRejectsMissingEvaluator(t *testing.T) {
	t.Parallel()

	for _, found := range []bool{false, true} {
		handler, err := NewEvaluatorFactory(func(agentName, name string) (evaluator.Evaluator, bool) {
			assert.Equal(t, "child", agentName)
			assert.Equal(t, "guard", name)
			return nil, found
		})(HandlerEnv{}, evaluatorTestHook())
		require.NoError(t, err)
		result, err := handler.Run(t.Context(), []byte(`{"hook_event_name":"tool_guard","agent_name":"child"}`))
		require.ErrorContains(t, err, `unknown evaluator "guard" for agent "child"`)
		assert.Equal(t, -1, result.ExitCode)
		assert.Nil(t, result.Output)
	}
}

func TestEvaluatorFactoryResolvesAgentScopeAtRun(t *testing.T) {
	t.Parallel()

	var lookups []string
	factory := NewEvaluatorFactory(func(agentName, name string) (evaluator.Evaluator, bool) {
		lookups = append(lookups, agentName)
		assert.Equal(t, "guard", name)
		return evaluatorFunc(func(context.Context, any) (*evaluator.Result, error) {
			probability := 0.0
			if agentName == "root" {
				probability = 1
			}
			return &evaluator.Result{Type: "boolean", Probability: &probability}, nil
		}), true
	})
	for _, tc := range []struct {
		agent    string
		decision Decision
	}{
		{"root", DecisionAllow},
		{"child", DecisionDeny},
	} {
		before := len(lookups)
		handler, err := factory(HandlerEnv{}, evaluatorTestHook())
		require.NoError(t, err)
		require.Len(t, lookups, before, "construction must not resolve an agent scope")
		input, err := (&Input{HookEventName: EventToolGuard, AgentName: tc.agent}).ToJSON()
		require.NoError(t, err)
		result, err := handler.Run(t.Context(), input)
		require.NoError(t, err)
		require.Len(t, lookups, before+1, "lookup must happen during Run, not construction")
		assert.Equal(t, tc.decision, result.Output.HookSpecificOutput.PermissionDecision)
	}
	assert.Equal(t, []string{"root", "child"}, lookups)
}

func TestEvaluatorHandlerPolicy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		result      evaluator.Result
		threshold   float64
		fallback    string
		decision    Decision
		choice      string
		probability string
		matched     bool
	}{
		{
			name: "boolean true at threshold", result: evaluator.Result{Type: "boolean", Probability: new(0.8)},
			decision: DecisionAllow, choice: "true", probability: "0.8", matched: true,
		},
		{
			name: "boolean false complements probability", result: evaluator.Result{Type: "boolean", Probability: new(0.2)},
			decision: DecisionDeny, choice: "false", probability: "0.8", matched: true,
		},
		{
			name: "boolean zero is a valid answer", result: evaluator.Result{Type: "boolean", Probability: new(0.0)}, threshold: 1,
			decision: DecisionDeny, choice: "false", probability: "1", matched: true,
		},
		{
			name: "boolean one", result: evaluator.Result{Type: "boolean", Probability: new(1.0)}, threshold: 1,
			decision: DecisionAllow, choice: "true", probability: "1", matched: true,
		},
		{
			name: "boolean tie selects true", result: evaluator.Result{Type: "boolean", Probability: new(0.5)}, threshold: 0.5,
			decision: DecisionAllow, choice: "true", probability: "0.5", matched: true,
		},
		{
			name: "boolean below threshold", result: evaluator.Result{Type: "boolean", Probability: new(0.79)},
			decision: DecisionAsk, choice: "true", probability: "0.79",
		},
		{
			name: "boolean deny fallback", result: evaluator.Result{Type: "boolean", Probability: new(0.5)}, fallback: "deny",
			decision: DecisionDeny, choice: "true", probability: "0.5",
		},
		{
			name:     "choice ignores low confidence",
			result:   evaluator.Result{Type: "choice", Choice: "safe", Probabilities: map[string]float64{"safe": 0.8, "unsafe": 0.2}, Confidence: new(0.1)},
			decision: DecisionAllow, choice: "safe", probability: "0.8", matched: true,
		},
		{
			name:     "choice ignores high confidence",
			result:   evaluator.Result{Type: "choice", Choice: "safe", Probabilities: map[string]float64{"safe": 0.79, "unsafe": 0.21}, Confidence: new(1.0)},
			decision: DecisionAsk, choice: "safe", probability: "0.79",
		},
		{
			name:     "choice deny",
			result:   evaluator.Result{Type: "choice", Choice: "unsafe", Probabilities: map[string]float64{"safe": 0.1, "unsafe": 0.9}},
			decision: DecisionDeny, choice: "unsafe", probability: "0.9", matched: true,
		},
		{
			name:     "unmapped outcome uses fallback",
			result:   evaluator.Result{Type: "choice", Choice: "unknown", Probabilities: map[string]float64{"safe": 0, "unknown": 1}},
			decision: DecisionAsk, choice: "unknown", probability: "1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hook := evaluatorTestHook()
			if tc.threshold != 0 {
				hook.EvaluatorPolicy.MinProbability = tc.threshold
			}
			if tc.fallback != "" {
				hook.EvaluatorPolicy.Fallback = tc.fallback
			}
			tc.result.Model = "test-model"
			handler := evaluatorTestHandler(t, evaluatorFunc(func(context.Context, any) (*evaluator.Result, error) {
				return &tc.result, nil
			}), hook)
			res, err := handler.Run(t.Context(), []byte(`{"hook_event_name":"tool_guard","tool_input":{"secret":"do not echo"}}`))
			require.NoError(t, err)
			assert.Zero(t, res.ExitCode)
			require.NotNil(t, res.Output)
			out := res.Output.HookSpecificOutput
			require.NotNil(t, out)
			assert.Equal(t, EventToolGuard, out.HookEventName)
			assert.Equal(t, tc.decision, out.PermissionDecision)
			assert.Equal(t, map[string]string{
				"evaluator": "guard", "evaluator_type": tc.result.Type, "evaluator_choice": tc.choice,
				"evaluator_probability": tc.probability, "evaluator_model": "test-model",
			}, out.Metadata)
			if tc.matched {
				assert.Equal(t, "Evaluator policy matched the selected outcome.", out.PermissionDecisionReason)
			} else {
				assert.Equal(t, "Evaluator policy fallback applied.", out.PermissionDecisionReason)
			}
		})
	}
}

func TestEvaluatorHandlerRejectsInvalidResults(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		result *evaluator.Result
		want   string
	}{
		{"nil result", nil, "missing evaluator result"},
		{"missing answer", &evaluator.Result{}, "boolean or choice"},
		{"unknown type", &evaluator.Result{Type: "other"}, "boolean or choice"},
		{"score rejected", &evaluator.Result{Type: "score", Score: new(1.0)}, "boolean or choice"},
		{"boolean missing probability", &evaluator.Result{Type: "boolean"}, "missing boolean probability"},
		{"boolean negative", &evaluator.Result{Type: "boolean", Probability: new(-0.1)}, "invalid evaluator probability"},
		{"boolean above one", &evaluator.Result{Type: "boolean", Probability: new(1.1)}, "invalid evaluator probability"},
		{"boolean nan", &evaluator.Result{Type: "boolean", Probability: new(math.NaN())}, "invalid evaluator probability"},
		{"boolean infinite", &evaluator.Result{Type: "boolean", Probability: new(math.Inf(1))}, "invalid evaluator probability"},
		{"boolean negative infinite", &evaluator.Result{Type: "boolean", Probability: new(math.Inf(-1))}, "invalid evaluator probability"},
		{"missing choice", &evaluator.Result{Type: "choice", Probabilities: map[string]float64{"": 1}}, "missing evaluator choice"},
		{"blank choice", &evaluator.Result{Type: "choice", Choice: " ", Probabilities: map[string]float64{" ": 1}}, "missing evaluator choice"},
		{"missing probability map", &evaluator.Result{Type: "choice", Choice: "safe"}, "missing selected choice probability"},
		{"missing choice probability", &evaluator.Result{Type: "choice", Choice: "safe", Probabilities: map[string]float64{"unsafe": 1}}, "missing selected choice probability"},
		{"choice negative", &evaluator.Result{Type: "choice", Choice: "safe", Probabilities: map[string]float64{"safe": -0.1}}, "invalid evaluator probability"},
		{"choice above one", &evaluator.Result{Type: "choice", Choice: "safe", Probabilities: map[string]float64{"safe": 1.1}}, "invalid evaluator probability"},
		{"choice nan", &evaluator.Result{Type: "choice", Choice: "safe", Probabilities: map[string]float64{"safe": math.NaN()}}, "invalid evaluator probability"},
		{"choice infinite", &evaluator.Result{Type: "choice", Choice: "safe", Probabilities: map[string]float64{"safe": math.Inf(1)}}, "invalid evaluator probability"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handler := evaluatorTestHandler(t, evaluatorFunc(func(context.Context, any) (*evaluator.Result, error) {
				return tc.result, nil
			}), evaluatorTestHook())
			res, err := handler.Run(t.Context(), []byte(`{"hook_event_name":"tool_guard"}`))
			require.ErrorContains(t, err, tc.want)
			assert.Nil(t, res.Output, "malformed results must never use the policy fallback")
			assert.Equal(t, -1, res.ExitCode)
		})
	}
}

func TestEvaluatorHandlerForwardsOnlyToolStateAndContext(t *testing.T) {
	t.Parallel()

	type contextKey struct{}
	ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), contextKey{}, "context-value"), time.Minute)
	defer cancel()
	in := &Input{
		HookEventName: EventToolGuard,
		ToolName:      "shell", ToolCategory: "filesystem", ToolInput: map[string]any{"cmd": "ls", "nested": map[string]any{"value": true}},
		SessionID: "private-session", ParentSessionID: "private-parent", Cwd: "private-cwd", ToolUseID: "private-call",
		AgentName: "private-agent", LastUserMessage: "private-message", Prompt: "private-prompt",
		Messages: []chat.Message{{Role: chat.MessageRoleUser, Content: "private-history"}},
	}
	calls := 0
	handler := evaluatorTestHandler(t, evaluatorFunc(func(gotCtx context.Context, state any) (*evaluator.Result, error) {
		calls++
		assert.Equal(t, ctx, gotCtx)
		assert.Equal(t, map[string]any{
			"tool_name": "shell", "tool_category": "filesystem", "tool_input": in.ToolInput,
		}, state)
		return &evaluator.Result{Type: "boolean", Probability: new(1.0)}, nil
	}), evaluatorTestHook())
	assert.Zero(t, calls, "construction must not evaluate")
	body, err := in.ToJSON()
	require.NoError(t, err)
	res, err := handler.Run(ctx, body)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.NotContains(t, res.Output.HookSpecificOutput.Metadata, "evaluator_model")
}

func TestEvaluatorHandlerRejectsNonGuardInput(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		`{`, `null`, `{}`, `{"hook_event_name":"pre_tool_use"}`, `{"hook_event_name":"permission_request"}`, `{"hook_event_name":"turn_start"}`,
	} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			handler := evaluatorTestHandler(t, evaluatorFunc(func(context.Context, any) (*evaluator.Result, error) {
				t.Error("invalid hook input must not reach the evaluator")
				return nil, nil
			}), evaluatorTestHook())
			res, err := handler.Run(t.Context(), []byte(input))
			require.Error(t, err)
			assert.Equal(t, -1, res.ExitCode)
			assert.Nil(t, res.Output)
		})
	}
}

func TestEvaluatorHandlerPropagatesCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	handler := evaluatorTestHandler(t, evaluatorFunc(func(ctx context.Context, _ any) (*evaluator.Result, error) {
		return nil, ctx.Err()
	}), evaluatorTestHook())
	res, err := handler.Run(ctx, []byte(`{"hook_event_name":"tool_guard"}`))
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, -1, res.ExitCode)
	assert.Nil(t, res.Output)
}

func TestEvaluatorFactorySnapshotsPolicy(t *testing.T) {
	t.Parallel()

	hook := evaluatorTestHook()
	handler := evaluatorTestHandler(t, evaluatorFunc(func(context.Context, any) (*evaluator.Result, error) {
		return &evaluator.Result{Type: "boolean", Probability: new(0.9)}, nil
	}), hook)
	hook.EvaluatorPolicy.Decisions["true"] = "deny"
	hook.EvaluatorPolicy.MinProbability = 1
	hook.EvaluatorPolicy.Fallback = "deny"
	res, err := handler.Run(t.Context(), []byte(`{"hook_event_name":"tool_guard"}`))
	require.NoError(t, err)
	assert.Equal(t, DecisionAllow, res.Output.HookSpecificOutput.PermissionDecision)
}

func TestEvaluatorHookExecutorFailsClosed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		result *evaluator.Result
		err    error
	}{
		{"provider error", nil, errors.New("provider unavailable")},
		{"partial answer and error", &evaluator.Result{Type: "boolean", Probability: new(1.0)}, errors.New("incomplete response")},
		{"nil result", nil, nil},
		{"missing probability", &evaluator.Result{Type: "boolean"}, nil},
		{"timeout", nil, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			registry := NewRegistry()
			registry.Register(HookTypeEvaluator, NewEvaluatorFactory(func(string, string) (evaluator.Evaluator, bool) {
				return evaluatorFunc(func(context.Context, any) (*evaluator.Result, error) {
					return tc.result, tc.err
				}), true
			}))
			hook := evaluatorTestHook()
			hook.OnError = "ignore"
			exec := NewExecutorWithRegistry(&Config{ToolGuard: []MatcherConfig{{Hooks: []Hook{hook}}}}, "", nil, registry)
			res, err := exec.Dispatch(t.Context(), EventToolGuard, &Input{ToolName: "shell"})
			require.NoError(t, err)
			assert.False(t, res.Allowed)
			assert.False(t, res.PermissionAllowed)
			assert.Equal(t, -1, res.ExitCode)
			assert.Empty(t, res.Decision, "provider failure must not become an ask fallback")
			assert.Contains(t, res.Message, "tool_guard hook failed to execute")
		})
	}
}

func TestEvaluatorHookExecutorAllowIsAdvisory(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	registry.Register(HookTypeEvaluator, NewEvaluatorFactory(func(string, string) (evaluator.Evaluator, bool) {
		return evaluatorFunc(func(context.Context, any) (*evaluator.Result, error) {
			return &evaluator.Result{Type: "boolean", Probability: new(1.0)}, nil
		}), true
	}))
	exec := NewExecutorWithRegistry(&Config{ToolGuard: []MatcherConfig{{Hooks: []Hook{evaluatorTestHook()}}}}, "", nil, registry)
	res, err := exec.Dispatch(t.Context(), EventToolGuard, &Input{ToolName: "shell"})
	require.NoError(t, err)
	assert.True(t, res.Allowed)
	assert.Equal(t, DecisionAllow, res.Decision)
	assert.False(t, res.PermissionAllowed)
	assert.Nil(t, res.ModifiedInput)
	assert.Equal(t, "guard", res.Metadata["evaluator"])
}
