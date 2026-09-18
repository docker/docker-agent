package runtime

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	ragtypes "github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// fakeRAGToolSet is a RAG-like toolset that records its event subscribers
// so tests can fire events as the RAG manager would.
type fakeRAGToolSet struct {
	name string
	subs tools.Subscribers[ragtypes.Event]
}

var (
	_ tools.ToolSet            = (*fakeRAGToolSet)(nil)
	_ ragtypes.EventSubscriber = (*fakeRAGToolSet)(nil)
)

func (f *fakeRAGToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (f *fakeRAGToolSet) Name() string                                { return f.name }
func (f *fakeRAGToolSet) SubscribeEvents(cb ragtypes.EventCallback) func() {
	return f.subs.Subscribe(cb)
}

func (f *fakeRAGToolSet) fireProgress(current int) {
	f.subs.Notify(ragtypes.Event{
		Type:         ragtypes.EventTypeIndexingProgress,
		StrategyName: "vector",
		Progress:     &ragtypes.Progress{Current: current, Total: 10},
	})
}

func newRAGAgent(name string, toolsets ...tools.ToolSet) *agent.Agent {
	return agent.New(name, "agent",
		agent.WithModel(&mockProvider{id: "test/rag-model", stream: &mockStream{}}),
		agent.WithToolSets(toolsets...),
	)
}

func requireRAGProgress(t *testing.T, events chan Event) *RAGIndexingProgressEvent {
	t.Helper()
	select {
	case ev := <-events:
		progress, ok := ev.(*RAGIndexingProgressEvent)
		require.True(t, ok, "expected *RAGIndexingProgressEvent, got %T", ev)
		return progress
	default:
		t.Fatal("expected a RAGIndexingProgressEvent, channel is empty")
		return nil
	}
}

// TestSubscribeToolsetEvents_StableAgentAttribution verifies that RAG events
// reach the stream through the agent's toolset wrapper, are attributed to
// the agent owning the toolset rather than the runtime's current agent, and
// stop after release.
func TestSubscribeToolsetEvents_StableAgentAttribution(t *testing.T) {
	t.Parallel()

	rootRAG := &fakeRAGToolSet{name: "docs"}
	helperRAG := &fakeRAGToolSet{name: "specs"}
	tm := team.New(team.WithAgents(newRAGAgent("root", rootRAG), newRAGAgent("helper", helperRAG)))
	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("root"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	events := make(chan Event, 8)
	release := rt.subscribeToolsetEvents(session.New(), NewChannelSink(events))

	rt.setCurrentAgent("helper")
	rootRAG.fireProgress(1)
	got := requireRAGProgress(t, events)
	assert.Equal(t, "docs", got.RAGName)
	assert.Equal(t, "root", got.GetAgentName(), "attribution must follow the toolset owner, not the current agent")

	helperRAG.fireProgress(2)
	assert.Equal(t, "helper", requireRAGProgress(t, events).GetAgentName())

	release()
	rootRAG.fireProgress(3)
	requireNoEvent(t, events)
}

// TestSubscribeToolsetEvents_ExtraToolSetsAndDedup covers skill-provided
// session toolsets (attributed to the session's agent) and the one-
// subscription-per-instance invariant for a toolset shared by two agents.
func TestSubscribeToolsetEvents_ExtraToolSetsAndDedup(t *testing.T) {
	t.Parallel()

	shared := &fakeRAGToolSet{name: "shared"}
	extra := &fakeRAGToolSet{name: "skill-docs"}
	tm := team.New(team.WithAgents(newRAGAgent("a1", shared), newRAGAgent("a2", shared)))
	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("a1"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New()
	sess.AgentName = "a2"
	sess.ExtraToolSets = []tools.ToolSet{extra}

	events := make(chan Event, 8)
	release := rt.subscribeToolsetEvents(sess, NewChannelSink(events))
	t.Cleanup(release)

	shared.fireProgress(1)
	requireRAGProgress(t, events)
	requireNoEvent(t, events)

	extra.fireProgress(1)
	assert.Equal(t, "a2", requireRAGProgress(t, events).GetAgentName())
}

// TestSubscribeToolsetEvents_FanOutAcrossStreams pins the motivating case:
// two streams subscribed to the same toolset both receive its events, and
// releasing one leaves the other intact.
func TestSubscribeToolsetEvents_FanOutAcrossStreams(t *testing.T) {
	t.Parallel()

	ragTool := &fakeRAGToolSet{name: "docs"}
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(newRAGAgent("root", ragTool))), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	eventsA := make(chan Event, 8)
	eventsB := make(chan Event, 8)
	releaseA := rt.subscribeToolsetEvents(session.New(), NewChannelSink(eventsA))
	releaseB := rt.subscribeToolsetEvents(session.New(), NewChannelSink(eventsB))
	t.Cleanup(releaseB)

	ragTool.fireProgress(1)
	requireRAGProgress(t, eventsA)
	requireRAGProgress(t, eventsB)

	releaseA()
	ragTool.fireProgress(2)
	requireNoEvent(t, eventsA)
	requireRAGProgress(t, eventsB)
}

// subscribableToolSet implements both tools-changed capabilities and counts
// live subscriptions so tests can assert on leaks.
type subscribableToolSet struct {
	subs   tools.ChangeSubscribers
	live   atomic.Int32
	legacy atomic.Pointer[func()]
}

var (
	_ tools.ToolSet          = (*subscribableToolSet)(nil)
	_ tools.ChangeNotifier   = (*subscribableToolSet)(nil)
	_ tools.ChangeSubscriber = (*subscribableToolSet)(nil)
)

func (s *subscribableToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (s *subscribableToolSet) SetToolsChangedHandler(handler func())       { s.legacy.Store(&handler) }

func (s *subscribableToolSet) SubscribeToolsChanged(handler func()) func() {
	s.live.Add(1)
	unsub := s.subs.Subscribe(handler)
	var once atomic.Bool
	return func() {
		if once.CompareAndSwap(false, true) {
			s.live.Add(-1)
		}
		unsub()
	}
}

func (s *subscribableToolSet) fire() { s.subs.Notify() }

func newToolsChangedRuntime(t *testing.T, ts tools.ToolSet) *LocalRuntime {
	t.Helper()
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(newRAGAgent("root", ts))), WithCurrentAgent("root"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	return rt
}

// TestOnToolsChanged_SubscribesPerRuntime verifies that runtimes sharing a
// toolset each hold their own subscription: both are notified, re-
// registering replaces only the caller's subscription, and Close releases it
// without disturbing the other runtime. The legacy single slot stays unused.
func TestOnToolsChanged_SubscribesPerRuntime(t *testing.T) {
	t.Parallel()

	ts := &subscribableToolSet{}
	rtA := newToolsChangedRuntime(t, ts)
	rtB := newToolsChangedRuntime(t, ts)

	var callsA, callsB atomic.Int32
	rtA.OnToolsChanged(func(Event) { callsA.Add(1) })
	rtA.OnToolsChanged(func(Event) { callsA.Add(1) })
	rtB.OnToolsChanged(func(Event) { callsB.Add(1) })
	assert.Equal(t, int32(2), ts.live.Load(), "one live subscription per runtime")
	assert.Nil(t, ts.legacy.Load(), "subscribable toolsets must not have their legacy slot overwritten")

	ts.fire()
	assert.Equal(t, int32(1), callsA.Load())
	assert.Equal(t, int32(1), callsB.Load())

	require.NoError(t, rtA.Close())
	assert.Equal(t, int32(1), ts.live.Load(), "Close must release the runtime's subscription")

	ts.fire()
	assert.Equal(t, int32(1), callsA.Load(), "closed runtime must not be notified")
	assert.Equal(t, int32(2), callsB.Load())

	rtB.OnToolsChanged(nil)
	assert.Zero(t, ts.live.Load(), "nil handler unsubscribes")
}

// legacyNotifierToolSet only implements the single-slot ChangeNotifier.
type legacyNotifierToolSet struct {
	handler atomic.Pointer[func()]
}

func (l *legacyNotifierToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (l *legacyNotifierToolSet) SetToolsChangedHandler(handler func())       { l.handler.Store(&handler) }

// TestOnToolsChanged_LegacyNotifierFallback keeps toolsets that predate
// ChangeSubscriber working through the single-slot setter.
func TestOnToolsChanged_LegacyNotifierFallback(t *testing.T) {
	t.Parallel()

	ts := &legacyNotifierToolSet{}
	rt := newToolsChangedRuntime(t, ts)

	var calls atomic.Int32
	rt.OnToolsChanged(func(Event) { calls.Add(1) })
	handler := ts.handler.Load()
	require.NotNil(t, handler)
	(*handler)()
	assert.Equal(t, int32(1), calls.Load())
}
