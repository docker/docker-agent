package runtime

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// TestBeforeLLMCallHookFiresOncePerLoopIteration is a regression test
// for a duplicate dispatch in [LocalRuntime.RunStream] that fired
// [LocalRuntime.executeBeforeLLMCallHooks] twice per iteration. The
// bug would silently break stateful before_llm_call hooks (the
// max_iterations builtin would have tripped at half its configured
// limit). A single-turn session must observe exactly one fire.
//
// It also pins the [hooks.Input.Iteration] contract: the runtime
// surfaces a 1-based loop counter on every dispatch so stateless
// guards (like the max_iterations builtin) can compare it to a
// configured budget without keeping their own per-session counter.
func TestBeforeLLMCallHookFiresOncePerLoopIteration(t *testing.T) {
	t.Parallel()

	const counterName = "test-before-llm-counter"
	var calls atomic.Int32
	var lastIteration atomic.Int32

	stream := newStreamBuilder().
		AddContent("Hello").
		AddStopWithUsage(3, 2).
		Build()
	prov := &mockProvider{id: "test/mock-model", stream: stream}

	root := agent.New("root", "test agent",
		agent.WithModel(prov),
		agent.WithHooks(&latest.HooksConfig{
			BeforeLLMCall: []latest.HookDefinition{
				{Type: "builtin", Command: counterName},
			},
		}),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	// Builtin lookup happens at dispatch time, not at executor build,
	// so registering after NewLocalRuntime is sufficient.
	require.NoError(t, rt.hooksRegistry.RegisterBuiltin(
		counterName,
		func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
			calls.Add(1)
			lastIteration.Store(int32(in.Iteration))
			return nil, nil
		},
	))

	sess := session.New(session.WithUserMessage("hi"))
	sess.Title = "Unit Test"

	for range rt.RunStream(t.Context(), sess) {
	}

	assert.Equal(t, int32(1), calls.Load(),
		"before_llm_call must fire exactly once per loop iteration; "+
			"a duplicate dispatch would silently break stateful hooks like max_iterations")
	assert.Equal(t, int32(1), lastIteration.Load(),
		"first model call of a RunStream must carry Iteration=1 "+
			"so the max_iterations builtin can compare it to its configured budget")
}

// TestMaxIterationsBuiltin_TripsAfterConfiguredLimit is the real e2e
// test for the max_iterations builtin: it stands up an agent whose
// model issues a tool call on every iteration (so the loop never
// terminates on its own), wires the builtin in via
// before_llm_call, and asserts that the loop is hard-stopped at
// exactly the configured budget — not under it (nothing was lost
// by going stateless), not over it (the gate held).
//
// This is the regression test that pins issue #2698: the runtime
// surfaces [hooks.Input.Iteration] on every dispatch so the builtin
// can compare it to its limit without keeping a per-session counter
// or relying on session_end cleanup.
func TestMaxIterationsBuiltin_TripsAfterConfiguredLimit(t *testing.T) {
	t.Parallel()

	const limit = 3

	// A tool that always succeeds. Every iteration the model issues a
	// call to it, which means the loop would run forever if not for the
	// max_iterations gate.
	loopTool := tools.Tool{
		Name:       "do_thing",
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("ok"), nil
		},
	}

	// Build a fresh tool-call stream per iteration. We queue more
	// streams than `limit` so a regression that lets the loop run an
	// extra time still has a stream to consume — the test then catches
	// the over-run via prov.callIdx.
	newToolCallStream := func(callID string) *mockStream {
		return newStreamBuilder().
			AddToolCallName(callID, loopTool.Name).
			AddToolCallArguments(callID, `{}`).
			AddToolCallStopWithUsage(2, 2).
			Build()
	}
	prov := &queueProvider{
		id: "test/mock-model",
		streams: []chat.MessageStream{
			newToolCallStream("call_1"),
			newToolCallStream("call_2"),
			newToolCallStream("call_3"),
			// Extra streams so an off-by-one regression can be detected
			// rather than masked by an empty queue defaulting to a stop.
			newToolCallStream("call_4"),
			newToolCallStream("call_5"),
		},
	}

	root := agent.New("root", "test agent",
		agent.WithModel(prov),
		agent.WithToolSets(newStubToolSet(nil, []tools.Tool{loopTool}, nil)),
		agent.WithHooks(&latest.HooksConfig{
			BeforeLLMCall: []latest.HookDefinition{
				{Type: "builtin", Command: builtins.MaxIterations, Args: []string{"3"}},
			},
		}),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)
	rt.registerDefaultTools()

	sess := session.New(
		session.WithUserMessage("loop forever"),
		session.WithToolsApproved(true),
	)
	sess.Title = "max_iterations e2e"

	var events []Event
	for ev := range rt.RunStream(t.Context(), sess) {
		events = append(events, ev)
	}

	// The model MUST have been called exactly `limit` times. The 4th
	// dispatch is where the builtin trips, before any model call.
	prov.mu.Lock()
	// queueProvider doesn't track callIdx, but it pops from streams as
	// it runs — the residual length tells us how many calls happened.
	callsMade := 5 - len(prov.streams)
	prov.mu.Unlock()
	assert.Equal(t, limit, callsMade,
		"max_iterations(%d) must allow exactly %d model calls, got %d",
		limit, limit, callsMade)

	// The runtime must surface the builtin's block reason as an
	// ErrorEvent so the user sees why the run stopped — not just
	// silently exit.
	var errEvt *ErrorEvent
	for _, ev := range events {
		if e, ok := ev.(*ErrorEvent); ok {
			errEvt = e
			break
		}
	}
	require.NotNil(t, errEvt, "max_iterations trip must surface as an ErrorEvent")
	assert.Contains(t, strings.ToLower(errEvt.Error), "max_iterations",
		"error must mention the tripping builtin so users can identify the cause")
	assert.Contains(t, errEvt.Error, "3",
		"error must include the configured limit so users can adjust their YAML")
}

// TestMaxIterationsBuiltin_NoOpOnInvalidLimit asserts the lenient-args
// contract of the builtin from end-to-end: when the YAML configures an
// invalid limit (zero, negative, non-numeric, missing), the runtime
// must NOT trip prematurely. A misconfigured limit becomes a no-op
// rather than an instant kill switch — a safer default for users.
func TestMaxIterationsBuiltin_NoOpOnInvalidLimit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{"missing", nil},
		{"empty", []string{}},
		{"zero", []string{"0"}},
		{"negative", []string{"-5"}},
		{"non_numeric", []string{"three"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stream := newStreamBuilder().
				AddContent("hello").
				AddStopWithUsage(2, 2).
				Build()
			prov := &mockProvider{id: "test/mock-model", stream: stream}

			root := agent.New("root", "test agent",
				agent.WithModel(prov),
				agent.WithHooks(&latest.HooksConfig{
					BeforeLLMCall: []latest.HookDefinition{
						{Type: "builtin", Command: builtins.MaxIterations, Args: tc.args},
					},
				}),
			)
			tm := team.New(team.WithAgents(root))

			rt, err := NewLocalRuntime(tm,
				WithSessionCompaction(false),
				WithModelStore(mockModelStore{}),
			)
			require.NoError(t, err)

			sess := session.New(session.WithUserMessage("hi"))
			sess.Title = "Unit Test"

			var events []Event
			for ev := range rt.RunStream(t.Context(), sess) {
				events = append(events, ev)
			}

			for _, ev := range events {
				if e, ok := ev.(*ErrorEvent); ok {
					t.Fatalf("invalid limit %v must be a no-op, but the run was terminated: %s", tc.args, e.Error)
				}
			}
		})
	}
}
