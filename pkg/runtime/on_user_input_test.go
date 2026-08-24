package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
)

// runtimeWithRecordedUserInput mirrors runtimeWithRecordedSessionResume
// for the on_user_input event: a recording builtin is registered on the
// runtime's private registry post-construction and wired onto the root
// agent. extraAgents become sub-agents of root so delegation paths
// (run_background_agent) can be exercised; only root gets the hook wired
// here, but an extra agent constructed with its own on_user_input config
// shares the same recording builtin.
func runtimeWithRecordedUserInput(t *testing.T, extraAgents ...*agent.Agent) (*LocalRuntime, *recordingBuiltin) {
	t.Helper()

	rb := &recordingBuiltin{}
	prov := &mockProvider{
		id:     "test/mock-model",
		stream: newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build(),
	}
	root := agent.New("root", "instructions",
		agent.WithModel(prov),
		agent.WithHooks(&hooks.Config{
			OnUserInput: []hooks.Hook{{
				Type:    hooks.HookTypeBuiltin,
				Command: "test_record_on_user_input",
			}},
		}),
	)
	if len(extraAgents) > 0 {
		agent.WithSubAgents(extraAgents...)(root)
	}
	tm := team.New(team.WithAgents(append([]*agent.Agent{root}, extraAgents...)...))

	r, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	require.NoError(t, r.hooksRegistry.RegisterBuiltin("test_record_on_user_input", rb.hook))
	r.buildHooksExecutors()

	return r, rb
}

// TestOnUserInputHooks_RootInteractiveStreamEnd_FiresOnce pins the
// PR #1847 semantics: when a root interactive stream finishes, the
// agent is waiting for the user's next message, so on_user_input fires
// exactly once with the session id.
func TestOnUserInputHooks_RootInteractiveStreamEnd_FiresOnce(t *testing.T) {
	t.Parallel()

	r, rb := runtimeWithRecordedUserInput(t)
	sess := session.New(session.WithUserMessage("hi"))

	for range r.RunStream(t.Context(), sess) {
	}

	got := rb.snapshot()
	require.Len(t, got, 1, "root interactive teardown must fire on_user_input exactly once")
	assert.Equal(t, sess.ID, got[0].SessionID)
}

// TestOnUserInputHooks_NonInteractiveStreamEnd_DoesNotFire is the #4004
// regression guard: a non-interactive session (MCP serve, A2A, evals)
// has no user to wait for, so its teardown must not fire on_user_input.
func TestOnUserInputHooks_NonInteractiveStreamEnd_DoesNotFire(t *testing.T) {
	t.Parallel()

	r, rb := runtimeWithRecordedUserInput(t)
	sess := session.New(session.WithUserMessage("hi"), session.WithNonInteractive(true))

	for range r.RunStream(t.Context(), sess) {
	}

	assert.Empty(t, rb.snapshot(), "non-interactive teardown must not fire on_user_input")
}

// TestOnUserInputHooks_SubSessionStreamEnd_DoesNotFire covers the
// interactive sub-session teardown (transfer_task): the child stream
// ending hands control back to the parent loop, not to the user, so
// on_user_input must not fire (#4004).
func TestOnUserInputHooks_SubSessionStreamEnd_DoesNotFire(t *testing.T) {
	t.Parallel()

	r, rb := runtimeWithRecordedUserInput(t)
	sub := session.New(session.WithUserMessage("hi"), session.WithParentID("parent-session"))

	for range r.RunStream(t.Context(), sub) {
	}

	assert.Empty(t, rb.snapshot(), "sub-session teardown must not fire on_user_input")
}

// TestOnUserInputHooks_BackgroundAgentTeardown_DoesNotFire reproduces
// the #4004 report: run_background_agent (runCollecting) tears down its
// child stream on normal completion, which used to reach the current
// (root) agent's on_user_input hook and signal a bogus "needs input".
func TestOnUserInputHooks_BackgroundAgentTeardown_DoesNotFire(t *testing.T) {
	t.Parallel()

	worker := agent.New("worker", "worker instructions",
		agent.WithModel(&mockProvider{
			id:     "test/mock-model",
			stream: newStreamBuilder().AddContent("worker done").AddStopWithUsage(1, 1).Build(),
		}))
	r, rb := runtimeWithRecordedUserInput(t, worker)

	parent := session.New(session.WithUserMessage("dispatch"), session.WithToolsApproved(true))
	res := r.RunAgent(t.Context(), agenttool.RunParams{
		AgentName:     "worker",
		Task:          "background work",
		ParentSession: parent,
	})
	require.Empty(t, res.ErrMsg)

	assert.Empty(t, rb.snapshot(), "background sub-session teardown must not fire on_user_input")
}

// TestEnforceMaxIterations_Interactive_FiresOnUserInput pins the one
// max-iterations path that genuinely waits for the user: the hook fires
// before blocking on the resume decision.
func TestEnforceMaxIterations_Interactive_FiresOnUserInput(t *testing.T) {
	t.Parallel()

	r, rb := runtimeWithRecordedUserInput(t)
	a := r.CurrentAgent()
	require.NotNil(t, a)
	sess := session.New()
	events := make(chan Event, 8)

	go func() { r.resumeChan <- ResumeReject("stop") }()
	_, decision := r.enforceMaxIterations(t.Context(), sess, a, 10, 10, NewChannelSink(events))

	assert.Equal(t, iterationStop, decision)
	got := rb.snapshot()
	require.Len(t, got, 1, "interactive max-iterations wait must fire on_user_input once")
	assert.Equal(t, sess.ID, got[0].SessionID)
}

// TestEnforceMaxIterations_NonInteractive_DoesNotFireOnUserInput is the
// #4004 counterpart: the non-interactive auto-stop never waits for the
// user, so on_user_input must not fire.
func TestEnforceMaxIterations_NonInteractive_DoesNotFireOnUserInput(t *testing.T) {
	t.Parallel()

	r, rb := runtimeWithRecordedUserInput(t)
	a := r.CurrentAgent()
	require.NotNil(t, a)
	sess := session.New()
	sess.NonInteractive = true
	events := make(chan Event, 8)

	_, decision := r.enforceMaxIterations(t.Context(), sess, a, 10, 10, NewChannelSink(events))

	assert.Equal(t, iterationStop, decision)
	assert.Empty(t, rb.snapshot(), "non-interactive auto-stop must not fire on_user_input")
}

// TestEnforceMaxIterations_ContextCancelled_DoesNotFireOnUserInput pins
// the cancelled-run guard: a run whose context is already done can never
// deliver a resume decision, so the runtime must stop without signalling
// a bogus "waiting for user input" (#4004). The limit event itself still
// fires — only the input wait is skipped.
func TestEnforceMaxIterations_ContextCancelled_DoesNotFireOnUserInput(t *testing.T) {
	t.Parallel()

	r, rb := runtimeWithRecordedUserInput(t)
	a := r.CurrentAgent()
	require.NotNil(t, a)
	sess := session.New()
	events := make(chan Event, 8)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, decision := r.enforceMaxIterations(ctx, sess, a, 10, 10, NewChannelSink(events))

	assert.Equal(t, iterationStop, decision)
	assert.Empty(t, rb.snapshot(), "a cancelled context must not fire on_user_input")

	select {
	case ev := <-events:
		assert.IsType(t, &MaxIterationsReachedEvent{}, ev,
			"MaxIterationsReached must still be emitted before the cancelled-context stop")
	default:
		t.Fatal("expected MaxIterationsReached to be emitted")
	}
}

// TestEnforceMaxIterations_PinnedSessionAgent_AttributesHookToThatAgent
// pins the attribution fix: the hook fires on the agent the callsite
// resolved for the session (here a pinned worker), not on whatever
// CurrentAgent happens to be.
func TestEnforceMaxIterations_PinnedSessionAgent_AttributesHookToThatAgent(t *testing.T) {
	t.Parallel()

	worker := agent.New("worker", "worker instructions",
		agent.WithModel(&mockProvider{
			id:     "test/mock-model",
			stream: newStreamBuilder().AddContent("worker done").AddStopWithUsage(1, 1).Build(),
		}),
		agent.WithHooks(&hooks.Config{
			OnUserInput: []hooks.Hook{{
				Type:    hooks.HookTypeBuiltin,
				Command: "test_record_on_user_input",
			}},
		}),
	)
	r, rb := runtimeWithRecordedUserInput(t, worker)
	sess := session.New()
	events := make(chan Event, 8)

	go func() { r.resumeChan <- ResumeReject("stop") }()
	_, decision := r.enforceMaxIterations(t.Context(), sess, worker, 10, 10, NewChannelSink(events))

	assert.Equal(t, iterationStop, decision)
	got := rb.snapshot()
	require.Len(t, got, 1, "the pinned agent's on_user_input hook must fire once")
	assert.Equal(t, "worker", got[0].AgentName,
		"the hook must be attributed to the session's agent, not CurrentAgent")
}

// TestOnUserInputHooks_Elicitation_CarriesConversationID verifies the
// elicitation wait fires on_user_input with the conversation id seeded
// into ctx by the run loop, instead of an empty session id.
func TestOnUserInputHooks_Elicitation_CarriesConversationID(t *testing.T) {
	t.Parallel()

	r, rb := runtimeWithRecordedUserInput(t)

	sinkCalled := make(chan Event, 1)
	r.OnElicitationRequest(func(ev Event) { sinkCalled <- ev })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx = genai.WithConversationID(ctx, "elicit-sess-1")

	done := make(chan error, 1)
	go func() {
		_, err := r.elicitationHandler(ctx, &mcp.ElicitParams{Message: "confirm?"})
		done <- err
	}()

	var ev *ElicitationRequestEvent
	select {
	case e := <-sinkCalled:
		ev = e.(*ElicitationRequestEvent)
	case <-time.After(time.Second):
		t.Fatal("elicitation request event not delivered to the sink")
	}
	require.NoError(t, r.ResumeElicitation(t.Context(), tools.ElicitationActionAccept, nil, ev.ElicitationID))
	require.NoError(t, <-done)

	got := rb.snapshot()
	require.Len(t, got, 1, "elicitation wait must fire on_user_input once")
	assert.Equal(t, "elicit-sess-1", got[0].SessionID,
		"on_user_input must carry the conversation id from ctx, not an empty session id")
}
