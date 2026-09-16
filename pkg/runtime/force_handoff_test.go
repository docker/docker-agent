package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// handoffRecordingProvider wraps mockProvider to count model invocations and
// capture the messages of the most recent call, so tests can assert
// whether and with what context a forced-handoff target was invoked.
type handoffRecordingProvider struct {
	mockProvider

	// newStream, when set, builds a fresh stream per invocation. A probe can be
	// reached more than once at a time — parallel delegation children each
	// routing to the same force_handoff target — and mockProvider hands out one
	// single-use mockStream whose cursor is not safe for concurrent Recv.
	newStream func() chat.MessageStream

	mu       sync.Mutex
	calls    int
	lastMsgs []chat.Message
}

func (p *handoffRecordingProvider) CreateChatCompletionStream(ctx context.Context, msgs []chat.Message, t []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	p.calls++
	p.lastMsgs = append([]chat.Message(nil), msgs...)
	p.mu.Unlock()
	if p.newStream != nil {
		return p.newStream(), nil
	}
	return p.mockProvider.CreateChatCompletionStream(ctx, msgs, t)
}

func (p *handoffRecordingProvider) handoffCallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *handoffRecordingProvider) lastMessages() []chat.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]chat.Message(nil), p.lastMsgs...)
}

// forceHandoffTeam builds a two-agent team where root force-hands off to
// summarizer, plus a runtime wired with the usual test doubles.
func forceHandoffTeam(t *testing.T, rootStream, summarizerStream *mockStream) (*LocalRuntime, *handoffRecordingProvider) {
	t.Helper()

	rootProv := &mockProvider{id: "test/mock-model", stream: rootStream}
	sumProv := &handoffRecordingProvider{mockProvider: mockProvider{id: "test/mock-model", stream: summarizerStream}}

	summarizer := agent.New("summarizer", "You summarize", agent.WithModel(sumProv))
	root := agent.New("root", "You extract", agent.WithModel(rootProv), agent.WithForceHandoff(summarizer))
	tm := team.New(team.WithAgents(root, summarizer))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	return rt, sumProv
}

// TestForceHandoff_RoutesToTargetOnNaturalStop pins the core contract of
// force_handoff: when the first agent produces a final response, the
// runtime deterministically routes the conversation to the configured
// target — no LLM tool call involved — and the target runs in the same
// session with the carried-over context.
func TestForceHandoff_RoutesToTargetOnNaturalStop(t *testing.T) {
	t.Parallel()

	rt, sumProv := forceHandoffTeam(t,
		newStreamBuilder().AddContent("extracted facts").AddStopWithUsage(10, 5).Build(),
		newStreamBuilder().AddContent("final summary").AddStopWithUsage(10, 5).Build(),
	)

	sess := session.New(session.WithUserMessage("Summarize this article"))
	sess.Title = "Unit Test"

	var events []Event
	for ev := range rt.RunStream(t.Context(), sess) {
		events = append(events, ev)
	}

	for _, ev := range events {
		errEv, isErr := ev.(*ErrorEvent)
		require.False(t, isErr, "unexpected error event: %+v", errEv)
	}

	assert.Equal(t, "summarizer", rt.CurrentAgentName(t.Context()), "current agent must be the force_handoff target after the run")
	assert.Equal(t, 1, sumProv.handoffCallCount(), "target agent's model must be invoked exactly once")
	assert.Equal(t, "final summary", sess.GetLastAssistantMessageContent())

	// The runtime injects an implicit user message so the target's model
	// call doesn't start on a dangling assistant message.
	var implicitHandoffMsg bool
	for _, m := range sess.GetAllMessages() {
		if m.Implicit && m.Message.Role == chat.MessageRoleUser && strings.Contains(m.Message.Content, "automatically handed off") {
			implicitHandoffMsg = true
		}
	}
	assert.True(t, implicitHandoffMsg, "implicit handoff user message must be recorded in the session")

	// The target agent must see the previous agent's output (carried-over
	// context) and the handoff notice in its prompt.
	prompt := sumProv.lastMessages()
	var sawExtracted, sawNotice bool
	for _, m := range prompt {
		if m.Role == chat.MessageRoleAssistant && strings.Contains(m.Content, "extracted facts") {
			sawExtracted = true
		}
		if m.Role == chat.MessageRoleUser && strings.Contains(m.Content, "automatically handed off") {
			sawNotice = true
		}
	}
	assert.True(t, sawExtracted, "target agent must see the previous agent's final response")
	assert.True(t, sawNotice, "target agent must see the handoff notice")
}

// TestForceHandoff_RoutesToTargetInPinnedSession pins force_handoff's
// documented contract — "unconditionally", "deterministic pipelines" — inside a
// pinned session (a background agent's session, or a child pinned by a parallel
// delegation batch). Routing there moves the session's own pin, so the next loop
// iteration resolves the target instead of the agent that just stopped; the
// shared current agent belongs to the concurrent foreground loop and stays put
// (#3886).
func TestForceHandoff_RoutesToTargetInPinnedSession(t *testing.T) {
	t.Parallel()

	rt, sumProv := forceHandoffTeam(t,
		newStreamBuilder().AddContent("done").AddStopWithUsage(10, 5).Build(),
		newStreamBuilder().AddContent("summarized").AddStopWithUsage(10, 5).Build(),
	)

	sess := session.New(session.WithUserMessage("hi"), session.WithAgentName("root"))
	sess.Title = "Unit Test"

	for range rt.RunStream(t.Context(), sess) {
	}

	assert.Equal(t, 1, sumProv.handoffCallCount(), "force_handoff must reach its target in a pinned session")
	assert.Equal(t, "summarizer", sess.AgentName, "routing moves the session's pin, not the shared current agent")
	assert.Equal(t, "root", rt.CurrentAgentName(t.Context()),
		"the shared current agent belongs to the foreground loop and must not be mutated (#3886)")
	assert.Equal(t, "summarized", sess.GetLastAssistantMessageContent())
}

// TestForceHandoff_PinnedChainTerminates guards the reason the pinned-session
// gate existed: honouring force_handoff without moving the pin re-resolved the
// agent that had just stopped, looping stop/handoff forever. Re-pinning walks
// the chain to its end exactly once — config validation rejects cycles, so the
// chain is always finite.
func TestForceHandoff_PinnedChainTerminates(t *testing.T) {
	t.Parallel()

	stream := func(content string) *handoffRecordingProvider {
		return &handoffRecordingProvider{mockProvider: mockProvider{
			id:     "test/mock-model",
			stream: newStreamBuilder().AddContent(content).AddStopWithUsage(10, 5).Build(),
		}}
	}

	lastProv := stream("last done")
	last := agent.New("last", "Last agent", agent.WithModel(lastProv))
	middle := agent.New("middle", "Middle agent", agent.WithModel(stream("middle done")),
		agent.WithForceHandoff(last))
	root := agent.New("root", "Root agent", agent.WithModel(stream("root done")),
		agent.WithForceHandoff(middle))

	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, middle, last)),
		WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })

	sess := session.New(session.WithUserMessage("hi"), session.WithAgentName("root"))
	sess.Title = "Unit Test"

	for range rt.RunStream(t.Context(), sess) {
	}

	assert.Equal(t, 1, lastProv.handoffCallCount(), "the chain must reach its tail exactly once")
	assert.Equal(t, "last", sess.AgentName)
	assert.Equal(t, "last done", sess.GetLastAssistantMessageContent())
}
