package hooks

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/evaluator"
)

func routeOutput(agent string) *Output {
	return &Output{HookSpecificOutput: &HookSpecificOutput{Transition: &Transition{Action: TransitionActionRoute, Agent: agent}}}
}

func controlExecutor(t *testing.T, event EventType, fn BuiltinFunc, hook Hook) *Executor {
	t.Helper()
	registry := NewRegistry()
	require.NoError(t, registry.RegisterBuiltin("select", fn))
	hook.Type, hook.Command = HookTypeBuiltin, "select"
	return NewExecutorWithRegistry(configWithFlatHook(event, hook), "", nil, registry)
}

func TestControlEventProtocol(t *testing.T) {
	t.Parallel()
	for _, event := range []EventType{EventBeforeAgentRun, EventAfterAgentComplete} {
		tests := []struct {
			name       string
			out        *Output
			err        error
			allowed    bool
			transition *Transition
			failed     bool
		}{
			{name: "empty result is a no-op", allowed: true},
			{name: "valid route", out: routeOutput("reviewer"), allowed: true, transition: &Transition{Action: "route", Agent: "reviewer"}},
			{name: "unknown action", out: &Output{HookSpecificOutput: &HookSpecificOutput{Transition: &Transition{Action: "retry", Agent: "x"}}}, failed: true},
			{name: "missing target", out: &Output{HookSpecificOutput: &HookSpecificOutput{Transition: &Transition{Action: "route"}}}, failed: true},
			{name: "event name mismatch", out: &Output{HookSpecificOutput: &HookSpecificOutput{HookEventName: EventStop, Transition: &Transition{Action: "route", Agent: "x"}}}, failed: true},
			{name: "crash fails closed", err: errors.New("boom"), failed: true},
			{name: "block beats route", out: &Output{Decision: DecisionBlockValue, Reason: "no", HookSpecificOutput: &HookSpecificOutput{Transition: &Transition{Action: "route", Agent: "x"}}}},
			{name: "continue true is not a request for another turn", out: &Output{Continue: new(true)}, allowed: true},
		}
		for _, tt := range tests {
			t.Run(string(event)+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				exec := controlExecutor(t, event, func(context.Context, *Input, []string) (*Output, error) { return tt.out, tt.err }, Hook{})
				result, err := exec.Dispatch(t.Context(), event, &Input{})
				require.NoError(t, err)
				assert.Equal(t, tt.allowed, result.Allowed)
				assert.Equal(t, tt.transition, result.Transition)
				assert.Equal(t, tt.failed, len(result.FailedHooks) > 0)
			})
		}
	}
}

func TestControlOutputIsStrictEvenWhenHookIsPermissive(t *testing.T) {
	t.Parallel()
	for _, stdout := range []string{
		`not json`,
		`{"hook_specific_output":{"transition":{"action":"route","agent":"x"}},"surprise":1}`,
		`{"hook_specific_output":{"transition":{"action":"route","agent":"x"}}} trailing`,
	} {
		_, err := parseStdoutJSON(stdout, EventContract(EventBeforeAgentRun).Control)
		require.Error(t, err, stdout)
	}
	out, err := parseStdoutJSON(`{"hook_specific_output":{"transition":{"action":"route","agent":"x"}}}`, true)
	require.NoError(t, err)
	assert.Equal(t, &Transition{Action: "route", Agent: "x"}, out.HookSpecificOutput.Transition)
}

func TestTransitionRejectedOnObservationalEvents(t *testing.T) {
	t.Parallel()
	for _, event := range []EventType{EventStop, EventOnAgentSwitch, EventSubagentStop, EventBeforeLLMCall, EventToolGuard} {
		for _, strict := range []bool{false, true} {
			assert.Error(t, validateOutput(event, routeOutput("x"), strict), "%s strict=%v", event, strict)
		}
	}
}

func TestConflictingTransitionsBlock(t *testing.T) {
	t.Parallel()
	results := []hookResult{
		{HandlerResult: HandlerResult{Output: routeOutput("a")}, hook: Hook{Command: "one"}},
		{HandlerResult: HandlerResult{Output: routeOutput("b")}, hook: Hook{Command: "two"}},
	}
	final := aggregate(results, EventBeforeAgentRun)
	assert.False(t, final.Allowed)
	assert.Nil(t, final.Transition)
	assert.Contains(t, final.Message, "conflicting route transitions")
}

func TestRoutingMetadataSurvivesAggregation(t *testing.T) {
	t.Parallel()
	out := routeOutput("a")
	out.HookSpecificOutput.Metadata = map[string]string{"evaluator": "judge"}
	final := aggregate([]hookResult{{HandlerResult: HandlerResult{Output: out}, hook: Hook{Command: "one"}}}, EventAfterAgentComplete)
	assert.Equal(t, map[string]string{"evaluator": "judge"}, final.Metadata)
}

type fixedEvaluator struct {
	result *evaluator.Result
	err    error
	state  any
}

func (e *fixedEvaluator) Evaluate(_ context.Context, state any) (*evaluator.Result, error) {
	e.state = state
	return e.result, e.err
}

func routingHandler(client evaluator.Evaluator) Handler {
	factory := NewEvaluatorFactory(
		func(string, string) (evaluator.Evaluator, bool) { return client, client != nil },
		WithRoutingDefaults(func(string) string { return "fallback" }),
	)
	handler, err := factory(HandlerEnv{}, Hook{
		Type: HookTypeEvaluator, Evaluator: "judge",
		RoutingPolicy: &latest.RoutingPolicy{Routes: map[string]string{"a": "agent_a", "b": "agent_b", "c": "agent_c"}, MinProbability: 0.5},
	})
	if err != nil {
		panic(err)
	}
	return handler
}

func runRouting(t *testing.T, handler Handler, in Input) (*Output, error) {
	t.Helper()
	in.HookEventName = EventBeforeAgentRun
	raw, err := in.ToJSON()
	require.NoError(t, err)
	result, err := handler.Run(t.Context(), raw)
	if err != nil {
		return nil, err
	}
	return result.Output, nil
}

func choice(selected string, p map[string]float64) *evaluator.Result {
	return &evaluator.Result{Type: "choice", Model: "judge-1", Choice: selected, Probabilities: p}
}

func TestRoutingEvaluatorDecision(t *testing.T) {
	t.Parallel()
	nan, inf := math.NaN(), math.Inf(1)
	tests := []struct {
		name     string
		result   *evaluator.Result
		err      error
		agent    string
		fallback string
	}{
		{"above threshold", choice("a", map[string]float64{"a": 0.9, "b": 0.05, "c": 0.05}), nil, "agent_a", ""},
		{"exactly at threshold", choice("b", map[string]float64{"a": 0.25, "b": 0.5, "c": 0.25}), nil, "agent_b", ""},
		{"just below threshold", choice("b", map[string]float64{"a": 0.26, "b": 0.49, "c": 0.25}), nil, "fallback", FallbackBelowThreshold},
		{"tie for highest", choice("a", map[string]float64{"a": 0.5, "b": 0.5, "c": 0}), nil, "fallback", FallbackTie},
		{"unknown choice", choice("z", map[string]float64{"a": 0.1, "b": 0.1, "c": 0.8}), nil, "fallback", FallbackUnknownChoice},
		{"missing probability", choice("a", map[string]float64{"a": 0.9, "b": 0.1}), nil, "fallback", FallbackInvalidResult},
		{"extra probability", choice("a", map[string]float64{"a": 0.9, "b": 0.05, "c": 0.03, "d": 0.02}), nil, "fallback", FallbackInvalidResult},
		{"renamed probability", choice("a", map[string]float64{"a": 0.9, "b": 0.05, "x": 0.05}), nil, "fallback", FallbackInvalidResult},
		{"NaN", choice("a", map[string]float64{"a": nan, "b": 0.5, "c": 0.5}), nil, "fallback", FallbackInvalidResult},
		{"Inf", choice("a", map[string]float64{"a": inf, "b": 0, "c": 0}), nil, "fallback", FallbackInvalidResult},
		{"negative", choice("a", map[string]float64{"a": 1.1, "b": -0.1, "c": 0}), nil, "fallback", FallbackInvalidResult},
		{"sum too small", choice("a", map[string]float64{"a": 0.5, "b": 0.1, "c": 0.1}), nil, "fallback", FallbackInvalidResult},
		{"sum too large", choice("a", map[string]float64{"a": 0.9, "b": 0.5, "c": 0.5}), nil, "fallback", FallbackInvalidResult},
		{"boolean result", &evaluator.Result{Type: "boolean"}, nil, "fallback", FallbackInvalidResult},
		{"nil result", nil, nil, "fallback", FallbackInvalidResult},
		{"provider error", nil, errors.New("503"), "fallback", FallbackEvaluatorFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, err := runRouting(t, routingHandler(&fixedEvaluator{result: tt.result, err: tt.err}), Input{AgentName: "root", TaskInput: "hello"})
			require.NoError(t, err)
			hso := out.HookSpecificOutput
			assert.Equal(t, &Transition{Action: TransitionActionRoute, Agent: tt.agent}, hso.Transition)
			assert.Equal(t, EventBeforeAgentRun, hso.HookEventName)
			assert.Equal(t, tt.fallback, hso.Metadata["fallback_reason"])
			assert.Equal(t, "judge", hso.Metadata["evaluator"])
			assert.NoError(t, validateOutput(EventBeforeAgentRun, out, true), "the handler must satisfy strict control validation")
		})
	}
}

func TestRoutingEvaluatorTerminalErrorsDoNotFallBack(t *testing.T) {
	t.Parallel()
	_, err := runRouting(t, routingHandler(&fixedEvaluator{err: &evaluator.TerminalError{Err: errors.New("budget exceeded")}}), Input{AgentName: "root"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "budget exceeded")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	raw, jsonErr := (&Input{HookEventName: EventAfterAgentComplete, AgentName: "root"}).ToJSON()
	require.NoError(t, jsonErr)
	_, err = routingHandler(&fixedEvaluator{err: context.Canceled}).Run(ctx, raw)
	assert.Error(t, err, "a cancelled run must not route to the default")
}

func TestRoutingEvaluatorProjectionAndMetadata(t *testing.T) {
	t.Parallel()
	client := &fixedEvaluator{result: choice("a", map[string]float64{"a": 0.9, "b": 0.05, "c": 0.05})}
	out, err := runRouting(t, routingHandler(client), Input{
		SessionID: "session-1", InvocationID: "inv-1", StepID: "step-2", AgentName: "root", Cwd: "/secret",
		TaskInput: "request", PreviousOutput: "previous", Output: "current",
		Conversation: []ConversationMessage{{Role: "user", Content: "earlier"}},
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"input": "request", "previous_output": "previous", "output": "current",
		"conversation": []ConversationMessage{{Role: "user", Content: "earlier"}},
	}, client.state, "only task data reaches the evaluator: no IDs, cwd, or agent names")
	assert.Equal(t, "a", out.HookSpecificOutput.Metadata["evaluator_choice"])
	assert.Equal(t, "0.9", out.HookSpecificOutput.Metadata["evaluator_probability"])
	assert.Equal(t, "judge-1", out.HookSpecificOutput.Metadata["evaluator_model"])
	assert.Equal(t, "choice", out.HookSpecificOutput.Metadata["evaluator_type"])
}

func TestRoutingEvaluatorHandlerConfiguration(t *testing.T) {
	t.Parallel()
	lookup := func(string, string) (evaluator.Evaluator, bool) { return nil, false }
	factory := NewEvaluatorFactory(lookup, WithRoutingDefaults(func(string) string { return "fallback" }))
	policy := &latest.RoutingPolicy{Routes: map[string]string{"a": "x"}, MinProbability: 0.5}

	_, err := factory(HandlerEnv{}, Hook{
		Type: HookTypeEvaluator, Evaluator: "judge", RoutingPolicy: policy,
		EvaluatorPolicy: &latest.EvaluatorPolicy{},
	})
	require.Error(t, err, "policies are mutually exclusive")
	_, err = factory(HandlerEnv{}, Hook{
		Type: HookTypeEvaluator, Evaluator: "judge",
		RoutingPolicy: &latest.RoutingPolicy{Routes: map[string]string{"a": "x"}, MinProbability: 2},
	})
	require.Error(t, err)

	handler, err := factory(HandlerEnv{}, Hook{Type: HookTypeEvaluator, Evaluator: "judge", RoutingPolicy: policy})
	require.NoError(t, err)
	_, err = runRouting(t, handler, Input{AgentName: "root"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown evaluator", "an unknown evaluator is a configuration failure, not a fallback")

	raw, jsonErr := (&Input{HookEventName: EventToolGuard}).ToJSON()
	require.NoError(t, jsonErr)
	_, err = handler.Run(t.Context(), raw)
	assert.Error(t, err, "routing policies only run on control events")
}

func TestToolGuardEvaluatorBehaviorUnchangedByRoutingSupport(t *testing.T) {
	t.Parallel()
	client := &fixedEvaluator{err: errors.New("provider down")}
	factory := NewEvaluatorFactory(func(string, string) (evaluator.Evaluator, bool) { return client, true })
	handler, err := factory(HandlerEnv{}, Hook{Type: HookTypeEvaluator, Evaluator: "safety", EvaluatorPolicy: &latest.EvaluatorPolicy{
		Decisions: map[string]string{"true": "allow", "false": "deny"}, MinProbability: 0.9, Fallback: "ask",
	}})
	require.NoError(t, err)
	raw, jsonErr := (&Input{HookEventName: EventToolGuard, ToolName: "shell"}).ToJSON()
	require.NoError(t, jsonErr)
	result, err := handler.Run(t.Context(), raw)
	require.Error(t, err, "tool guards must keep failing closed on evaluator failures")
	assert.Equal(t, -1, result.ExitCode)
}

func TestIdenticalRoutingHooksDeduplicate(t *testing.T) {
	t.Parallel()
	policy := func(p float64) *latest.RoutingPolicy {
		return &latest.RoutingPolicy{Routes: map[string]string{"a": "x"}, MinProbability: p}
	}
	assert.True(t, sameHook(Hook{Type: "evaluator", RoutingPolicy: policy(0.5)}, Hook{Type: "evaluator", RoutingPolicy: policy(0.5)}))
	assert.False(t, sameHook(Hook{Type: "evaluator", RoutingPolicy: policy(0.5)}, Hook{Type: "evaluator", RoutingPolicy: policy(0.6)}))
	assert.False(t, sameHook(Hook{Type: "evaluator", RoutingPolicy: policy(0.5)}, Hook{Type: "evaluator"}))
}
