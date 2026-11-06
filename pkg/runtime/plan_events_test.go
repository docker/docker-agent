package runtime

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
)

func TestPlanChangedEventSerialization(t *testing.T) {
	t.Parallel()

	ev := PlanChanged("shared", "release", plan.ChangeActionWrite, 3, "architect")
	data, err := json.Marshal(ev)
	require.NoError(t, err)

	var decoded PlanChangedEvent
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, "plan_changed", decoded.Type)
	assert.Equal(t, "shared", decoded.Scope)
	assert.Equal(t, "release", decoded.Name)
	assert.Equal(t, "write", decoded.Action)
	assert.Equal(t, 3, decoded.Version)
	assert.Equal(t, "architect", decoded.GetAgentName())
	assert.NotContains(t, string(data), "content", "the event must never carry plan content")
}

// planToolHandler finds one of the plan toolset's tools by name. The write
// goes through the tool handler (not the storage) so the test exercises the
// same code path as an agent's tool call.
func planToolHandler(t *testing.T, ts tools.ToolSet, name string) tools.ToolHandler {
	t.Helper()
	all, err := ts.Tools(t.Context())
	require.NoError(t, err)
	for _, tl := range all {
		if tl.Name == name {
			return tl.Handler
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func planToolCall(t *testing.T, args any) tools.ToolCall {
	t.Helper()
	payload, err := json.Marshal(args)
	require.NoError(t, err)
	call := tools.ToolCall{}
	call.Function.Arguments = string(payload)
	return call
}

// newPlanRuntime builds a runtime whose only agent carries the given plan
// toolset, mirroring how agents share the process-wide plan singleton.
func newPlanRuntime(t *testing.T, planTool *plan.ToolSet) *LocalRuntime {
	t.Helper()
	prov := &mockProvider{id: "test/plan-model", stream: &mockStream{}}
	root := agent.New("root", "agent",
		agent.WithModel(prov),
		agent.WithToolSets(planTool),
	)
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("root"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	return rt
}

// TestSubscribePlanChanges_EmitsPlanChangeEvents verifies that a stream's
// plan subscription reaches the plan toolset through the agent's toolset
// wrappers (agent.WithToolSets wraps in a StartableToolSet) and that
// successful mutations emit PlanChangedEvent while failed ones emit nothing.
func TestSubscribePlanChanges_EmitsPlanChangeEvents(t *testing.T) {
	t.Parallel()

	planTool := plan.New(plan.WithStorage(plan.NewFilesystemStorage(t.TempDir())))
	rt := newPlanRuntime(t, planTool)

	events := make(chan Event, 8)
	release := rt.subscribePlanChanges(session.New(), NewChannelSink(events))
	t.Cleanup(release)

	write := planToolHandler(t, planTool, plan.ToolNameWritePlan)
	setStatus := planToolHandler(t, planTool, plan.ToolNameSetPlanStatus)
	del := planToolHandler(t, planTool, plan.ToolNameDeletePlan)

	// Successful write emits a versioned event without content. The agent
	// name is deliberately empty: the shared toolset mutates on behalf of
	// whichever session's agent calls it, so the subscriber cannot know the
	// mutator and must not claim one.
	result, err := write(t.Context(), planToolCall(t, plan.WritePlanArgs{Name: "release", Content: "v1"}), nil)
	require.NoError(t, err)
	require.False(t, result.IsError)
	ev := requirePlanChanged(t, events)
	assert.Equal(t, "shared", ev.Scope)
	assert.Equal(t, "release", ev.Name)
	assert.Equal(t, plan.ChangeActionWrite, ev.Action)
	assert.Equal(t, 1, ev.Version)
	assert.Empty(t, ev.GetAgentName(), "shared-plan events carry neutral attribution")

	// Successful status change.
	result, err = setStatus(t.Context(), planToolCall(t, plan.SetPlanStatusArgs{Name: "release", Status: "done"}), nil)
	require.NoError(t, err)
	require.False(t, result.IsError)
	ev = requirePlanChanged(t, events)
	assert.Equal(t, plan.ChangeActionStatus, ev.Action)
	assert.Equal(t, 2, ev.Version)

	// Failed write (version conflict) emits nothing.
	stale := 42
	result, err = write(t.Context(), planToolCall(t, plan.WritePlanArgs{Name: "release", Content: "v2", LastKnownRevision: &stale}), nil)
	require.NoError(t, err)
	require.True(t, result.IsError)
	requireNoEvent(t, events)

	// Successful guarded delete.
	rev := 2
	result, err = del(t.Context(), planToolCall(t, plan.DeletePlanArgs{Name: "release", LastKnownRevision: &rev}), nil)
	require.NoError(t, err)
	require.False(t, result.IsError)
	ev = requirePlanChanged(t, events)
	assert.Equal(t, plan.ChangeActionDelete, ev.Action)
	assert.Equal(t, 2, ev.Version)
}

// TestSubscribePlanChanges_FanOutAndReleaseIsolation proves the subscription
// semantics the single-callback slot could not provide: every subscribed
// stream receives every change exactly once, releasing one subscription
// neither disturbs the others nor fires into the released sink again, and
// release is idempotent.
func TestSubscribePlanChanges_FanOutAndReleaseIsolation(t *testing.T) {
	t.Parallel()

	planTool := plan.New(plan.WithStorage(plan.NewFilesystemStorage(t.TempDir())))
	rt := newPlanRuntime(t, planTool)
	write := planToolHandler(t, planTool, plan.ToolNameWritePlan)

	eventsA := make(chan Event, 8)
	eventsB := make(chan Event, 8)
	releaseA := rt.subscribePlanChanges(session.New(), NewChannelSink(eventsA))
	releaseB := rt.subscribePlanChanges(session.New(), NewChannelSink(eventsB))

	_, err := write(t.Context(), planToolCall(t, plan.WritePlanArgs{Name: "release", Content: "v1"}), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, requirePlanChanged(t, eventsA).Version, "subscriber A must receive the change")
	requireNoEvent(t, eventsA)
	assert.Equal(t, 1, requirePlanChanged(t, eventsB).Version, "subscriber B must receive the change")
	requireNoEvent(t, eventsB)

	releaseA()
	releaseA() // idempotent: a second release must not disturb subscriber B

	_, err = write(t.Context(), planToolCall(t, plan.WritePlanArgs{Name: "release", Content: "v2"}), nil)
	require.NoError(t, err)
	requireNoEvent(t, eventsA)
	assert.Equal(t, 2, requirePlanChanged(t, eventsB).Version, "releasing A must not silence B")

	releaseB()
	_, err = write(t.Context(), planToolCall(t, plan.WritePlanArgs{Name: "release", Content: "v3"}), nil)
	require.NoError(t, err)
	requireNoEvent(t, eventsA)
	requireNoEvent(t, eventsB)
}

// TestSubscribePlanChanges_DedupesSharedNotifierAcrossAgents pins the
// dedup invariant: the registry-created plan toolset is one process-wide
// singleton shared by every agent, so a stream whose team has several
// plan-capable agents must still receive one event per mutation.
func TestSubscribePlanChanges_DedupesSharedNotifierAcrossAgents(t *testing.T) {
	t.Parallel()

	planTool := plan.New(plan.WithStorage(plan.NewFilesystemStorage(t.TempDir())))
	a1 := agent.New("a1", "agent",
		agent.WithModel(&mockProvider{id: "test/plan-model", stream: &mockStream{}}),
		agent.WithToolSets(planTool),
	)
	a2 := agent.New("a2", "agent",
		agent.WithModel(&mockProvider{id: "test/plan-model", stream: &mockStream{}}),
		agent.WithToolSets(planTool),
	)
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a1, a2)), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	events := make(chan Event, 8)
	release := rt.subscribePlanChanges(session.New(), NewChannelSink(events))
	t.Cleanup(release)

	write := planToolHandler(t, planTool, plan.ToolNameWritePlan)
	_, err = write(t.Context(), planToolCall(t, plan.WritePlanArgs{Name: "release", Content: "v1"}), nil)
	require.NoError(t, err)

	requirePlanChanged(t, events)
	requireNoEvent(t, events)
}

// TestSubscribePlanChanges_IncludesSessionExtraToolSets covers skill
// sub-sessions: their assistive toolsets ride on the session rather than an
// agent, and a plan toolset among them must still notify the stream.
func TestSubscribePlanChanges_IncludesSessionExtraToolSets(t *testing.T) {
	t.Parallel()

	planTool := plan.New(plan.WithStorage(plan.NewFilesystemStorage(t.TempDir())))
	root := agent.New("root", "agent",
		agent.WithModel(&mockProvider{id: "test/plan-model", stream: &mockStream{}}),
	)
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New()
	sess.ExtraToolSets = []tools.ToolSet{planTool}

	events := make(chan Event, 8)
	release := rt.subscribePlanChanges(sess, NewChannelSink(events))
	t.Cleanup(release)

	write := planToolHandler(t, planTool, plan.ToolNameWritePlan)
	_, err = write(t.Context(), planToolCall(t, plan.WritePlanArgs{Name: "release", Content: "v1"}), nil)
	require.NoError(t, err)
	requirePlanChanged(t, events)
}

// funcSubscribeNotifier is a valid ChangeNotifier whose dynamic type is not
// comparable (the func field makes the value struct unhashable), so using it
// as a map key panics at runtime. Subscriptions delegate to a real registry
// so delivery can be asserted end to end.
type funcSubscribeNotifier struct {
	subscribe func(cb func(plan.Change)) func()
}

var (
	_ tools.ToolSet       = funcSubscribeNotifier{}
	_ plan.ChangeNotifier = funcSubscribeNotifier{}
)

func (f funcSubscribeNotifier) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

func (f funcSubscribeNotifier) SubscribeChanges(cb func(plan.Change)) func() {
	return f.subscribe(cb)
}

// TestSubscribePlanChanges_NonComparableNotifierDoesNotPanic pins the dedup
// bookkeeping against unhashable implementations: a value-type notifier
// reached through the agent's StartableToolSet wrapper must not panic the
// subscription loop — it skips dedup but still subscribes, delivers, and
// releases.
func TestSubscribePlanChanges_NonComparableNotifierDoesNotPanic(t *testing.T) {
	t.Parallel()

	planTool := plan.New(plan.WithStorage(plan.NewFilesystemStorage(t.TempDir())))
	root := agent.New("root", "agent",
		agent.WithModel(&mockProvider{id: "test/plan-model", stream: &mockStream{}}),
		agent.WithToolSets(funcSubscribeNotifier{subscribe: planTool.SubscribeChanges}),
	)
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	events := make(chan Event, 8)
	var release func()
	require.NotPanics(t, func() {
		release = rt.subscribePlanChanges(session.New(), NewChannelSink(events))
	})

	write := planToolHandler(t, planTool, plan.ToolNameWritePlan)
	_, err = write(t.Context(), planToolCall(t, plan.WritePlanArgs{Name: "release", Content: "v1"}), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, requirePlanChanged(t, events).Version, "a non-comparable notifier must still deliver changes")

	release()
	_, err = write(t.Context(), planToolCall(t, plan.WritePlanArgs{Name: "release", Content: "v2"}), nil)
	require.NoError(t, err)
	requireNoEvent(t, events)
}

// fakePlanNotifier is a toolset that records subscription lifecycle calls so
// stream tests can prove RunStream subscribes exactly once per stream and
// releases exactly once when the stream ends.
type fakePlanNotifier struct {
	toolsList []tools.Tool

	mu           sync.Mutex
	subs         map[int]func(plan.Change)
	nextID       int
	subscribed   int
	unsubscribed int
}

var _ plan.ChangeNotifier = (*fakePlanNotifier)(nil)

func (f *fakePlanNotifier) Tools(context.Context) ([]tools.Tool, error) { return f.toolsList, nil }

func (f *fakePlanNotifier) SubscribeChanges(cb func(plan.Change)) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subs == nil {
		f.subs = make(map[int]func(plan.Change))
	}
	id := f.nextID
	f.nextID++
	f.subs[id] = cb
	f.subscribed++
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.subs[id]; ok {
			delete(f.subs, id)
			f.unsubscribed++
		}
	}
}

func (f *fakePlanNotifier) counts() (subscribed, unsubscribed, live int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subscribed, f.unsubscribed, len(f.subs)
}

// TestRunStream_PlanSubscriptionOncePerStream drives a stream through two
// loop iterations (a tool-call turn, then a stop turn) and proves the plan
// subscription is a stream-lifecycle concern: registered exactly once — not
// re-registered per iteration — and released exactly once when the events
// channel closes.
func TestRunStream_PlanSubscriptionOncePerStream(t *testing.T) {
	t.Parallel()

	noop := tools.Tool{
		Name:       "noop",
		Parameters: map[string]any{},
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("ok"), nil
		},
	}
	fake := &fakePlanNotifier{toolsList: []tools.Tool{noop}}

	prov := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{
		newStreamBuilder().
			AddToolCallName("call_1", "noop").
			AddToolCallArguments("call_1", `{}`).
			AddToolCallStopWithUsage(2, 2).
			Build(),
		newStreamBuilder().AddStopWithUsage(1, 1).Build(),
	}}
	root := agent.New("root", "test agent",
		agent.WithModel(prov),
		agent.WithToolSets(fake),
	)
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("do the thing"), session.WithToolsApproved(true))
	for range rt.RunStream(t.Context(), sess) {
	}

	subscribed, unsubscribed, live := fake.counts()
	assert.Equal(t, 1, subscribed, "subscription must happen once per stream, not per iteration")
	assert.Equal(t, 1, unsubscribed, "the stream must release its subscription when it ends")
	assert.Zero(t, live, "no subscription may leak past the stream")
}

// TestRunStream_PlanSubscriptionReleasedOnCancel proves the release also runs
// on the cancellation path, so an interrupted stream cannot leak its
// subscription into the shared toolset.
func TestRunStream_PlanSubscriptionReleasedOnCancel(t *testing.T) {
	t.Parallel()

	fake := &fakePlanNotifier{}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	root := agent.New("root", "test agent",
		agent.WithModel(&activeRootBlockingProvider{id: "test/mock-model", release: release}),
		agent.WithToolSets(fake),
	)
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	events := rt.RunStream(ctx, session.New(session.WithUserMessage("block")))
	awaitEvent[*StreamStartedEvent](t, events, "stream start")
	cancel()
	for range events {
	}

	subscribed, unsubscribed, live := fake.counts()
	assert.Equal(t, 1, subscribed)
	assert.Equal(t, 1, unsubscribed, "cancellation must still release the subscription")
	assert.Zero(t, live)
}

// writePlanStream scripts a model turn that calls write_plan once.
func writePlanStream(t *testing.T, callID, name, content string) *mockStream {
	t.Helper()
	args, err := json.Marshal(plan.WritePlanArgs{Name: name, Content: content})
	require.NoError(t, err)
	return newStreamBuilder().
		AddToolCallName(callID, plan.ToolNameWritePlan).
		AddToolCallArguments(callID, string(args)).
		AddToolCallStopWithUsage(2, 2).
		Build()
}

// TestRunStream_TwoActiveStreams_BothReceivePlanChanges is the end-to-end
// fan-out proof: two live streams share one plan toolset (the singleton
// arrangement), a mutation made by one agent's tool call reaches both
// streams' sinks, and a stream that has ended neither receives further
// events nor silences the survivors. Under the previous single-slot
// callback the writer's registration overwrote the waiter's, so the waiter
// never saw any event and this test would fail.
func TestRunStream_TwoActiveStreams_BothReceivePlanChanges(t *testing.T) {
	t.Parallel()

	planTool := plan.New(plan.WithStorage(plan.NewFilesystemStorage(t.TempDir())))

	waiterRelease := make(chan struct{})
	waiter := agent.New("waiter", "parks in its model call",
		agent.WithModel(&activeRootBlockingProvider{id: "test/mock-model", release: waiterRelease}),
		agent.WithToolSets(planTool),
	)

	// Each writer run consumes two streams: a write_plan tool-call turn,
	// then a clean stop.
	writerProv := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{
		writePlanStream(t, "call_1", "release", "v1"),
		newStreamBuilder().AddStopWithUsage(1, 1).Build(),
		writePlanStream(t, "call_2", "release", "v2"),
		newStreamBuilder().AddStopWithUsage(1, 1).Build(),
	}}
	writer := agent.New("writer", "writes shared plans",
		agent.WithModel(writerProv),
		agent.WithToolSets(planTool),
	)

	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(waiter, writer)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	// Stream A subscribes at stream start, then parks inside its model call.
	// StreamStarted is emitted after the subscription is in place.
	eventsA := rt.RunStream(t.Context(), session.New(session.WithUserMessage("wait"), session.WithAgentName("waiter")))
	awaitEvent[*StreamStartedEvent](t, eventsA, "waiter stream start")

	runWriter := func(label string) {
		t.Helper()
		sess := session.New(
			session.WithUserMessage("write the plan"),
			session.WithAgentName("writer"),
			session.WithToolsApproved(true),
		)
		got := 0
		for ev := range rt.RunStream(t.Context(), sess) {
			if planEv, ok := ev.(*PlanChangedEvent); ok {
				got++
				assert.Empty(t, planEv.GetAgentName(), "%s: shared-plan events carry neutral attribution", label)
			}
		}
		assert.Equal(t, 1, got, "%s: the mutating stream must receive its own plan change exactly once", label)
	}

	// First writer stream: both the writer and the parked waiter see the write.
	runWriter("writer run 1")
	ev := awaitEvent[*PlanChangedEvent](t, eventsA, "plan change from writer run 1")
	assert.Equal(t, 1, ev.Version)
	assert.Empty(t, ev.GetAgentName())

	// The first writer stream has ended (its channel closed and its
	// subscription released); a second writer stream must still fan out to
	// the waiter, proving one stream ending does not silence another.
	runWriter("writer run 2")
	ev = awaitEvent[*PlanChangedEvent](t, eventsA, "plan change from writer run 2")
	assert.Equal(t, 2, ev.Version)

	// Unblock the waiter and drain: no further plan events may arrive.
	close(waiterRelease)
	for evA := range eventsA {
		if _, isPlan := evA.(*PlanChangedEvent); isPlan {
			t.Fatalf("waiter received an unexpected extra plan change")
		}
	}
}

// awaitEvent reads from ch until an event of type T arrives, failing the
// test if the channel closes or ten seconds pass first.
func awaitEvent[T Event](t *testing.T, ch <-chan Event, what string) T {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			require.True(t, ok, "event channel closed while waiting for %s", what)
			if typed, isT := ev.(T); isT {
				return typed
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func requirePlanChanged(t *testing.T, events <-chan Event) *PlanChangedEvent {
	t.Helper()
	select {
	case ev := <-events:
		planEv, ok := ev.(*PlanChangedEvent)
		require.True(t, ok, "expected *PlanChangedEvent, got %T", ev)
		return planEv
	default:
		t.Fatal("expected a PlanChangedEvent, channel is empty")
		return nil
	}
}

func requireNoEvent(t *testing.T, events <-chan Event) {
	t.Helper()
	select {
	case ev := <-events:
		t.Fatalf("expected no event, got %T", ev)
	default:
	}
}
