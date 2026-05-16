package runtime

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
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
