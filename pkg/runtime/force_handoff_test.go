package runtime

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// handoffRecordingProvider wraps mockProvider to count model invocations and
// capture the messages of the most recent call, so tests can assert
// whether and with what context a forced-handoff target was invoked.
type handoffRecordingProvider struct {
	mockProvider

	mu       sync.Mutex
	calls    int
	lastMsgs []chat.Message
}

func (p *handoffRecordingProvider) CreateChatCompletionStream(ctx context.Context, msgs []chat.Message, t []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	p.calls++
	p.lastMsgs = slices.Clone(msgs)
	if len(p.lastMsgs) == 0 {
		p.lastMsgs = nil
	}
	p.mu.Unlock()
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
	return slices.Clone(p.lastMsgs)
}

// forceHandoffTeam builds a two-agent team where root force-hands off to
// summarizer, plus a runtime wired with the usual test doubles.
func forceHandoffTeam(t *testing.T, rootStream, summarizerStream *mockStream, opts ...Opt) (*LocalRuntime, *handoffRecordingProvider) {
	t.Helper()

	rootProv := &mockProvider{id: "test/mock-model", stream: rootStream}
	sumProv := &handoffRecordingProvider{mockProvider: mockProvider{id: "test/mock-model", stream: summarizerStream}}

	summarizer := agent.New("summarizer", "You summarize", agent.WithModel(sumProv))
	root := agent.New("root", "You extract", agent.WithModel(rootProv), agent.WithForceHandoff(summarizer))
	tm := team.New(team.WithAgents(root, summarizer))

	rt, err := NewLocalRuntime(t.Context(), tm, append([]Opt{WithSessionCompaction(false), WithModelStore(mockModelStore{})}, opts...)...)
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

	events := runRouted(t, rt, sess)
	require.Empty(t, routedErrors(events))
	decisions := sess.RoutingDecisionHistory()
	require.Len(t, decisions, 1)
	d := decisions[0]
	assert.Equal(t, agentSwitchKindForceHandoff, d.Action)
	assert.Equal(t, "root", d.FromAgent)
	assert.Equal(t, "summarizer", d.ToAgent)
	emitted := routingDecisionEvents(events)
	require.Len(t, emitted, 1)
	assert.Equal(t, d, emitted[0].Decision)
	routes := routeEvents(events)
	require.Len(t, routes, 1)
	assert.Equal(t, agentSwitchKindForceHandoff, routes[0].Phase)
	turn := slices.IndexFunc(events, func(e Event) bool {
		info, ok := e.(*AgentInfoEvent)
		return ok && info.SessionID == sess.ID && info.AgentName == "summarizer"
	})
	assert.Less(t, slices.Index(events, Event(routes[0])), turn, "announce the switch before the target turn")

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

// TestForceHandoff_NoteIsStored pins that a reloaded conversation reads as the
// live one did: the note that hands it over sits between the two agents'
// answers, so the target's answer never follows another assistant message.
func TestForceHandoff_NoteIsStored(t *testing.T) {
	t.Parallel()

	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "session.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	rt, _ := forceHandoffTeam(t,
		newStreamBuilder().AddContent("extracted facts").AddStopWithUsage(10, 5).Build(),
		newStreamBuilder().AddContent("final summary").AddStopWithUsage(10, 5).Build(),
		WithSessionStore(store),
	)

	sess := session.New(session.WithUserMessage("Summarize this article"))
	require.Empty(t, routedErrors(runRouted(t, rt, sess)))

	stored, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	var roles []string
	for _, m := range stored.GetAllMessages() {
		role := string(m.Message.Role)
		if m.Implicit {
			role = "implicit " + role
		}
		roles = append(roles, role)
	}
	assert.Equal(t, []string{"user", "assistant", "implicit user", "assistant"}, roles)
}

// TestForceHandoff_SkippedForPinnedSession documents the guard for pinned
// sessions (background agents): resolveSessionAgent always returns the
// pinned agent, so honouring force_handoff there would loop forever. The
// runtime must finish the run on the pinned agent without ever invoking
// the target.
func TestForceHandoff_SkippedForPinnedSession(t *testing.T) {
	t.Parallel()

	rt, sumProv := forceHandoffTeam(t,
		newStreamBuilder().AddContent("done").AddStopWithUsage(10, 5).Build(),
		newStreamBuilder().AddContent("should never run").AddStopWithUsage(10, 5).Build(),
	)

	sess := session.New(session.WithUserMessage("hi"), session.WithAgentName("root"))
	sess.Title = "Unit Test"

	for range rt.RunStream(t.Context(), sess) {
	}

	assert.Equal(t, 0, sumProv.handoffCallCount(), "force_handoff target must not run for pinned sessions")
	assert.Equal(t, "root", rt.CurrentAgentName(t.Context()))
	assert.Equal(t, "done", sess.GetLastAssistantMessageContent())
}
