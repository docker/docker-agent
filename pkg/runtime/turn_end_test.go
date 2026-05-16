package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// turnEndRecorder is a thread-safe recorder for turn_end fires; tests
// inspect the captured reasons after RunStream drains. We capture the
// reason via the input's Reason field — the runtime sets it to one of
// the turnEndReason* constants depending on which exit path the loop
// took.
type turnEndRecorder struct {
	mu      sync.Mutex
	reasons []string
}

func (r *turnEndRecorder) record(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reasons = append(r.reasons, reason)
}

func (r *turnEndRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.reasons))
	copy(out, r.reasons)
	return out
}

// installTurnEndRecorder registers a builtin turn_end hook on rt that
// captures the reason on every fire. It returns the recorder so the
// test can inspect the captured fires after running.
func installTurnEndRecorder(t *testing.T, rt *LocalRuntime, name string) *turnEndRecorder {
	t.Helper()
	rec := &turnEndRecorder{}
	require.NoError(t, rt.hooksRegistry.RegisterBuiltin(
		name,
		func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
			rec.record(in.Reason)
			return nil, nil
		},
	))
	return rec
}

// TestTurnEndFiresOnNormalStop pins the contract that turn_end fires
// once when the model stops cleanly without any tool calls or
// follow-up — the most common path through the loop. The reason
// reported is one of the turnEndReason* constants; "normal" is the
// canonical exit, but "continue" is also acceptable on this branch
// because both indicate a clean fall-through.
func TestTurnEndFiresOnNormalStop(t *testing.T) {
	t.Parallel()

	stream := newStreamBuilder().
		AddContent("Hello").
		AddStopWithUsage(3, 2).
		Build()
	prov := &mockProvider{id: "test/mock-model", stream: stream}

	root := agent.New("root", "test agent",
		agent.WithModel(prov),
		agent.WithHooks(&latest.HooksConfig{
			TurnEnd: []latest.HookDefinition{
				{Type: "builtin", Command: "test-turn-end-normal"},
			},
		}),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	rec := installTurnEndRecorder(t, rt, "test-turn-end-normal")

	sess := session.New(session.WithUserMessage("hi"))
	for range rt.RunStream(t.Context(), sess) {
	}

	reasons := rec.snapshot()
	require.Len(t, reasons, 1, "turn_end must fire exactly once for a single-turn clean stop")
	assert.Equal(t, "normal", reasons[0],
		"a clean res.Stopped with no follow-up must report 'normal'")
}

// TestTurnEndFiresOnHookBlocked pins the contract that turn_end fires
// with reason="hook_blocked" when before_llm_call signals run
// termination via a deny verdict. The runtime emits its hook-driven
// shutdown stanzas and turn_end runs after — observability over the
// actual exit reason.
func TestTurnEndFiresOnHookBlocked(t *testing.T) {
	t.Parallel()

	stream := newStreamBuilder().
		AddContent("Hello").
		AddStopWithUsage(3, 2).
		Build()
	prov := &mockProvider{id: "test/mock-model", stream: stream}

	root := agent.New("root", "test agent",
		agent.WithModel(prov),
		agent.WithHooks(&latest.HooksConfig{
			BeforeLLMCall: []latest.HookDefinition{
				{Type: "builtin", Command: "test-blocker"},
			},
			TurnEnd: []latest.HookDefinition{
				{Type: "builtin", Command: "test-turn-end-blocked"},
			},
		}),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	// before_llm_call returns a deny verdict, terminating the run.
	require.NoError(t, rt.hooksRegistry.RegisterBuiltin(
		"test-blocker",
		func(_ context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
			return &hooks.Output{
				Decision: hooks.DecisionBlockValue,
				Reason:   "test-block",
			}, nil
		},
	))

	rec := installTurnEndRecorder(t, rt, "test-turn-end-blocked")

	sess := session.New(session.WithUserMessage("hi"))
	for range rt.RunStream(t.Context(), sess) {
	}

	reasons := rec.snapshot()
	require.Len(t, reasons, 1, "turn_end must fire even when the run was blocked")
	assert.Equal(t, "hook_blocked", reasons[0],
		"turn_end must report hook_blocked when before_llm_call denied the call")
}

// TestTurnEndFiresOnStreamError pins the contract that turn_end fires
// with reason="error" when fallback.execute returns an error and the
// runtime exits the loop via handleStreamError. The deferred dispatch
// is what makes this contract robust: explicit dispatch calls would
// have to be sprinkled at every error-path return, and any miss would
// silently break observability.
func TestTurnEndFiresOnStreamError(t *testing.T) {
	t.Parallel()

	prov := &mockProviderWithError{id: "test/error-model"}

	root := agent.New("root", "test agent",
		agent.WithModel(prov),
		agent.WithHooks(&latest.HooksConfig{
			TurnEnd: []latest.HookDefinition{
				{Type: "builtin", Command: "test-turn-end-error"},
			},
		}),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	rec := installTurnEndRecorder(t, rt, "test-turn-end-error")

	sess := session.New(session.WithUserMessage("hi"))
	for range rt.RunStream(t.Context(), sess) {
	}

	reasons := rec.snapshot()
	require.Len(t, reasons, 1, "turn_end must fire on stream error")
	assert.Equal(t, "error", reasons[0],
		"turn_end must report 'error' when handleStreamError exits the loop")
}

// blockingProvider returns a stream whose Recv blocks until the
// CreateChatCompletionStream-supplied context is cancelled, modelling
// a slow upstream that receives a cancel mid-stream. The cancellation
// signal is plumbed via the per-call done channel captured below —
// stashing the context on the stream itself would trip the
// containedctx linter, since context belongs in function arguments,
// not struct fields.
type blockingProvider struct {
	id string
}

func (p *blockingProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero(p.id) }

func (p *blockingProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	// Snapshot ctx.Done() at stream-construction time — the runtime
	// passes a per-call streamCtx that is cancelled when the parent
	// (RunStream's) context is cancelled, so capturing the channel
	// here is equivalent to capturing the context for our purposes
	// and avoids holding a context.Context in struct state.
	return &blockingStream{
		done: ctx.Done(),
		err:  func() error { return ctx.Err() },
	}, nil
}

func (p *blockingProvider) BaseConfig() base.Config { return base.Config{} }
func (p *blockingProvider) MaxTokens() int          { return 0 }

type blockingStream struct {
	done <-chan struct{}
	err  func() error
}

func (s *blockingStream) Recv() (chat.MessageStreamResponse, error) {
	// Block until the parent context is cancelled. On cancellation
	// we surface ctx.Err() so the runtime's fallback.execute treats
	// it as a stream error and proceeds to handleStreamError, which
	// exits the loop and triggers the deferred turn_end dispatch.
	<-s.done
	return chat.MessageStreamResponse{}, s.err()
}

func (s *blockingStream) Close() {}

// TestTurnEndFiresOnContextCancellation pins the critical contract
// that turn_end fires even when the parent context is cancelled
// mid-turn. The runtime uses context.WithoutCancel internally for the
// turn_end dispatch so handlers run to completion on Ctrl+C — without
// this, a session_end-only observability strategy would silently miss
// the per-turn lifecycle data.
//
// We synchronise the cancellation against turn_start to avoid the
// pre-turn-start race where the loop's ctx.Err() guard exits before
// turn_start has fired (correctly skipping turn_end). A turn_start
// hook signals that the loop has entered the turn body; the test
// then cancels and asserts turn_end fired with a cancellation-class
// reason.
func TestTurnEndFiresOnContextCancellation(t *testing.T) {
	t.Parallel()

	prov := &blockingProvider{id: "test/blocking-model"}

	root := agent.New("root", "test agent",
		agent.WithModel(prov),
		agent.WithHooks(&latest.HooksConfig{
			TurnStart: []latest.HookDefinition{
				{Type: "builtin", Command: "test-turn-start-signal"},
			},
			TurnEnd: []latest.HookDefinition{
				{Type: "builtin", Command: "test-turn-end-cancel"},
			},
		}),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	// turn_start fires AFTER the loop's ctx.Err() guard, so by the
	// time this hook runs the loop is committed to the turn and
	// turn_end is on the deferred path. Closing turnStartFired
	// unblocks the test goroutine which then cancels the context.
	turnStartFired := make(chan struct{})
	var once sync.Once
	require.NoError(t, rt.hooksRegistry.RegisterBuiltin(
		"test-turn-start-signal",
		func(_ context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
			once.Do(func() { close(turnStartFired) })
			return nil, nil
		},
	))

	rec := installTurnEndRecorder(t, rt, "test-turn-end-cancel")

	ctx, cancel := context.WithCancel(t.Context())

	sess := session.New(session.WithUserMessage("hi"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range rt.RunStream(ctx, sess) {
		}
	}()

	<-turnStartFired // model call is in flight; turn_end is on the defer path
	cancel()
	<-done

	reasons := rec.snapshot()
	require.Len(t, reasons, 1, "turn_end must fire exactly once on cancellation")
	// Either "canceled" (deferred guard caught ctx.Err) or "error"
	// (handleStreamError ran first and set 'error') is acceptable —
	// both legitimately indicate the cancellation path. The
	// invariant under test is "turn_end fires", not which specific
	// reason wins this micro-race.
	assert.Contains(t, []string{"canceled", "error"}, reasons[0],
		"turn_end must report a cancellation-class reason")
}

// TestTurnEndFiresEveryIteration pins the symmetric contract with
// turn_start: when a tool call provokes a follow-on iteration,
// turn_end fires once per iteration. A single missed dispatch would
// let stateful turn_end handlers (e.g. a metrics span that closes
// per turn) silently leak.
func TestTurnEndFiresEveryIteration(t *testing.T) {
	t.Parallel()

	// Two iterations: the first emits a tool call (stop reason =
	// tool_calls so the loop re-enters), the second stops cleanly.
	turn1 := newStreamBuilder().
		AddToolCallName("call_1", "noop").
		AddToolCallArguments("call_1", `{}`).
		AddToolCallStopWithUsage(3, 2).
		Build()
	turn2 := newStreamBuilder().
		AddContent("done").
		AddStopWithUsage(3, 2).
		Build()

	prov := &queueProvider{
		id:      "test/mock-model",
		streams: []chat.MessageStream{turn1, turn2},
	}

	noopTool := tools.Tool{
		Name:       "noop",
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("ok"), nil
		},
	}

	root := agent.New("root", "test agent",
		agent.WithModel(prov),
		agent.WithToolSets(newStubToolSet(nil, []tools.Tool{noopTool}, nil)),
		agent.WithHooks(&latest.HooksConfig{
			TurnEnd: []latest.HookDefinition{
				{Type: "builtin", Command: "test-turn-end-iter"},
			},
		}),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	rec := installTurnEndRecorder(t, rt, "test-turn-end-iter")

	sess := session.New(
		session.WithUserMessage("hi"),
		session.WithToolsApproved(true),
	)
	for range rt.RunStream(t.Context(), sess) {
	}

	reasons := rec.snapshot()
	require.Len(t, reasons, 2,
		"turn_end must fire once per loop iteration; one missing dispatch leaks state")
	assert.Equal(t, "continue", reasons[0],
		"intermediate iteration (after tool calls) must report 'continue'")
	assert.Equal(t, "normal", reasons[1],
		"final iteration must report 'normal'")
}
