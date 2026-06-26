package runtime

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// TestUserPromptSubmitFiresOncePerTopLevelTurn pins the contract that
// user_prompt_submit fires exactly once per real user message in a
// top-level session: not once per LLM call, not once per turn, but
// once per submission. The runtime gates the dispatch in
// [LocalRuntime.RunStream].
func TestUserPromptSubmitFiresOncePerTopLevelTurn(t *testing.T) {
	t.Parallel()

	calls, rt, sess := setupUserPromptSubmitCounter(t,
		session.WithUserMessage("hi"),
	)

	for range rt.RunStream(t.Context(), sess) {
	}

	assert.Equal(t, int32(1), calls.Load(),
		"user_prompt_submit must fire exactly once for a top-level user submission")
}

// TestUserPromptSubmitSkippedForSubSessions pins the design choice
// that user_prompt_submit fires for *human* prompts only. Sub-sessions
// (transferred tasks, background agents, skill sub-sessions) carry a
// runtime-synthesised "Please proceed." kick-off message that no human
// authored, so firing the hook there would be noise. The runtime gates
// the dispatch on [session.Session.SendUserMessage], which is exactly
// the same flag the runtime uses to decide whether to emit a
// [UserMessageEvent] \u2014 a sub-session sets it to false.
func TestUserPromptSubmitSkippedForSubSessions(t *testing.T) {
	t.Parallel()

	calls, rt, sess := setupUserPromptSubmitCounter(t,
		session.WithUserMessage("synthesised kick-off"),
		session.WithSendUserMessage(false),
	)

	for range rt.RunStream(t.Context(), sess) {
	}

	assert.Equal(t, int32(0), calls.Load(),
		"user_prompt_submit must NOT fire for sub-sessions (SendUserMessage=false): "+
			"their kick-off message is synthesised by the runtime, not authored by a human")
}

// TestUserSteeringMessagesSubmitFiresOnDrain pins the contract that
// user_steering_messages_submit fires when the runtime drains the
// steering queue, and that the hook receives the drained messages via
// Input.SteeringMessages. Enqueuing before RunStream exercises the
// idle/first-turn drain site, which is the one user_prompt_submit does
// NOT cover.
func TestUserSteeringMessagesSubmitFiresOnDrain(t *testing.T) {
	t.Parallel()

	const counterName = "test-user-steering-submit-counter"
	var calls atomic.Int32
	var seen atomic.Value

	stream := newStreamBuilder().
		AddContent("ok").
		AddStopWithUsage(3, 2).
		Build()
	prov := &mockProvider{id: "test/mock-model", stream: stream}

	root := agent.New("root", "test agent",
		agent.WithModel(prov),
		agent.WithHooks(&latest.HooksConfig{
			UserSteeringMessagesSubmit: []latest.HookDefinition{
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

	require.NoError(t, rt.hooksRegistry.RegisterBuiltin(
		counterName,
		func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
			calls.Add(1)
			seen.Store(append([]string(nil), in.SteeringMessages...))
			return nil, nil
		},
	))

	// Enqueue before RunStream so the messages are drained at the
	// idle/first-turn drain site at the top of the run loop.
	require.NoError(t, rt.Steer(QueuedMessage{Content: "steer one"}))
	require.NoError(t, rt.Steer(QueuedMessage{Content: "steer two"}))

	sess := session.New()
	sess.Title = "Unit Test"

	for range rt.RunStream(t.Context(), sess) {
	}

	assert.Equal(t, int32(1), calls.Load(),
		"user_steering_messages_submit must fire once for the drained batch")
	got, _ := seen.Load().([]string)
	assert.Equal(t, []string{"steer one", "steer two"}, got,
		"hook must receive the drained messages via Input.SteeringMessages, in order")

	// The hook saw the raw text above, but with the default framing
	// (instruction) the persisted messages must carry the <system-reminder>
	// envelope — proving the hook-raw / model-framed split holds in one run.
	var steered []string
	for _, item := range sess.Messages {
		if item.IsMessage() && item.Message.Message.Role == chat.MessageRoleUser {
			steered = append(steered, item.Message.Message.Content)
		}
	}
	require.Len(t, steered, 2)
	for _, content := range steered {
		assert.Contains(t, content, "<system-reminder>",
			"default-framed steer messages must be wrapped for the model")
		assert.Contains(t, content, "finish your current task first")
	}
}

// setupUserPromptSubmitCounter wires up a single-turn mock runtime with
// a builtin user_prompt_submit hook that atomically increments the
// returned counter on every dispatch. Both tests above share this
// scaffolding so the only thing that varies between them is the
// session's [session.WithSendUserMessage] flag.
func setupUserPromptSubmitCounter(t *testing.T, opts ...session.Opt) (*atomic.Int32, *LocalRuntime, *session.Session) {
	t.Helper()

	const counterName = "test-user-prompt-submit-counter"
	var calls atomic.Int32

	stream := newStreamBuilder().
		AddContent("ok").
		AddStopWithUsage(3, 2).
		Build()
	prov := &mockProvider{id: "test/mock-model", stream: stream}

	root := agent.New("root", "test agent",
		agent.WithModel(prov),
		agent.WithHooks(&latest.HooksConfig{
			UserPromptSubmit: []latest.HookDefinition{
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

	require.NoError(t, rt.hooksRegistry.RegisterBuiltin(
		counterName,
		func(_ context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
			calls.Add(1)
			return nil, nil
		},
	))

	sess := session.New(opts...)
	sess.Title = "Unit Test"

	return &calls, rt, sess
}

// TestUserFollowupSubmitFiresOnDequeue pins the contract that
// user_followup_submit fires when the runtime dequeues a follow-up
// message at the end of a turn, and that the hook receives the
// follow-up text via Input.Prompt. A follow-up enqueued before
// RunStream is dequeued when the first turn stops, starting a fresh
// turn — the path user_prompt_submit never covers.
func TestUserFollowupSubmitFiresOnDequeue(t *testing.T) {
	t.Parallel()

	const counterName = "test-user-followup-submit-counter"
	var calls atomic.Int32
	var seen atomic.Value

	// Two turns: the first stops, the runtime dequeues the follow-up and
	// runs a second turn which also stops (queue now empty).
	newStopStream := func() *mockStream {
		return newStreamBuilder().
			AddContent("ok").
			AddStopWithUsage(3, 2).
			Build()
	}
	prov := &queueProvider{
		id:      "test/mock-model",
		streams: []chat.MessageStream{newStopStream(), newStopStream()},
	}

	root := agent.New("root", "test agent",
		agent.WithModel(prov),
		agent.WithHooks(&latest.HooksConfig{
			UserFollowupSubmit: []latest.HookDefinition{
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

	require.NoError(t, rt.hooksRegistry.RegisterBuiltin(
		counterName,
		func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
			calls.Add(1)
			seen.Store(in.Prompt)
			return nil, nil
		},
	))

	// Enqueue before RunStream so the follow-up is dequeued when the
	// first turn stops.
	require.NoError(t, rt.FollowUp(QueuedMessage{Content: "please also do this"}))

	sess := session.New(session.WithUserMessage("hi"))
	sess.Title = "Unit Test"

	for range rt.RunStream(t.Context(), sess) {
	}

	assert.Equal(t, int32(1), calls.Load(),
		"user_followup_submit must fire once for the dequeued follow-up")
	got, _ := seen.Load().(string)
	assert.Equal(t, "please also do this", got,
		"hook must receive the follow-up text via Input.Prompt")
}
