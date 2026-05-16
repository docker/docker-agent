package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// recordingObserver captures every lifecycle hook the runtime invokes,
// in order. Concurrency-safe because OnEvent fires from the runtime's
// forwarding goroutine while OnRunStart fires from the caller's
// goroutine; production observers don't need this, but the test does
// to assert on ordering deterministically.
type recordingObserver struct {
	mu          sync.Mutex
	events      []Event
	startCalls  atomic.Int64
	startSessID string
}

func (o *recordingObserver) OnRunStart(_ context.Context, sess *session.Session) {
	o.startCalls.Add(1)
	o.mu.Lock()
	o.startSessID = sess.ID
	o.mu.Unlock()
}

func (o *recordingObserver) OnEvent(_ context.Context, _ *session.Session, e Event) {
	o.mu.Lock()
	o.events = append(o.events, e)
	o.mu.Unlock()
}

func (o *recordingObserver) snapshot() []Event {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Event, len(o.events))
	copy(out, o.events)
	return out
}

// runtimeWithObserver builds a minimal runtime that runs through one
// stream tick. The mock provider returns "done" + a stop event, so
// the loop runs to completion quickly without external side effects.
func runtimeWithObserver(t *testing.T, obs EventObserver) (*LocalRuntime, *session.Session) {
	t.Helper()

	prov := &mockProvider{
		id:     "test/mock-model",
		stream: newStreamBuilder().AddContent("done").AddStopWithUsage(10, 5).Build(),
	}
	a := agent.New("root", "instructions", agent.WithModel(prov))
	tm := team.New(team.WithAgents(a))

	r, err := NewLocalRuntime(tm,
		WithModelStore(mockModelStore{}),
		WithSessionCompaction(false),
		WithEventObserver(obs),
	)
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("hi"))
	return r, sess
}

// TestObserver_OnRunStartFiresExactlyOnce pins the lifecycle contract:
// each registered observer sees exactly one OnRunStart call per
// RunStream invocation, regardless of how many events flow afterwards.
func TestObserver_OnRunStartFiresExactlyOnce(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	r, sess := runtimeWithObserver(t, obs)

	for range r.RunStream(t.Context(), sess) {
		// drain
	}

	assert.Equal(t, int64(1), obs.startCalls.Load(), "OnRunStart must fire exactly once")
	assert.Equal(t, sess.ID, obs.startSessID)
}

// TestObserver_SeesEveryEventBeforeCaller verifies that OnEvent
// receives the same events the consumer's channel does, and in the
// same order. The observer chain is synchronous in the forwarding
// goroutine, so by the time the consumer reads event N, the observer
// has already processed it.
func TestObserver_SeesEveryEventBeforeCaller(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	r, sess := runtimeWithObserver(t, obs)

	var consumed []Event
	for event := range r.RunStream(t.Context(), sess) {
		consumed = append(consumed, event)
	}

	got := obs.snapshot()
	require.Len(t, got, len(consumed), "observer count must match consumer count")
	for i := range consumed {
		assert.Same(t, consumed[i], got[i],
			"observer event %d must be the same pointer the consumer saw", i)
	}
}

// TestObserver_MultipleObserversFireInRegistrationOrder pins the
// ordering contract: observers are invoked in the order they were
// registered. This matters for chained pipelines where a downstream
// observer relies on an upstream one's side effects (e.g. the stock
// PersistenceObserver writes to the store first, then a mirror
// observer reads what was just written).
func TestObserver_MultipleObserversFireInRegistrationOrder(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		order []string
	)
	tag := func(name string) EventObserver {
		return &fnObserver{
			onEvent: func(_ context.Context, _ *session.Session, _ Event) {
				mu.Lock()
				order = append(order, name)
				mu.Unlock()
			},
		}
	}

	prov := &mockProvider{
		id:     "test/mock-model",
		stream: newStreamBuilder().AddContent("hi").AddStopWithUsage(1, 1).Build(),
	}
	a := agent.New("root", "instructions", agent.WithModel(prov))
	tm := team.New(team.WithAgents(a))

	r, err := NewLocalRuntime(tm,
		WithModelStore(mockModelStore{}),
		WithSessionCompaction(false),
		WithEventObserver(tag("first")),
		WithEventObserver(tag("second")),
		WithEventObserver(tag("third")),
	)
	require.NoError(t, err)

	for range r.RunStream(t.Context(), session.New(session.WithUserMessage("hi"))) {
	}

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, order)
	// Each event triples the order slice with one entry per observer in
	// registration order, so the first three entries must always be
	// "first", "second", "third".
	assert.Equal(t, "first", order[0])
	assert.Equal(t, "second", order[1])
	assert.Equal(t, "third", order[2])
}

// TestObserver_NilOptIsIgnored documents the safety property: passing
// a nil EventObserver to WithEventObserver does not register a nil
// entry that would later panic on dispatch.
func TestObserver_NilOptIsIgnored(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	a := agent.New("root", "instructions", agent.WithModel(prov))
	tm := team.New(team.WithAgents(a))

	r, err := NewLocalRuntime(tm,
		WithModelStore(mockModelStore{}),
		WithEventObserver(nil),
	)
	require.NoError(t, err)

	// The auto-registered PersistenceObserver counts as one entry, so
	// observers must contain exactly one element (not two).
	assert.Len(t, r.observers, 1, "nil observer must not be appended")
}

// fnObserver adapts function values to the [EventObserver] interface
// for tests that only need to observe a subset of the lifecycle hooks.
type fnObserver struct {
	onStart func(context.Context, *session.Session)
	onEvent func(context.Context, *session.Session, Event)
}

func (f *fnObserver) OnRunStart(ctx context.Context, sess *session.Session) {
	if f.onStart != nil {
		f.onStart(ctx, sess)
	}
}

func (f *fnObserver) OnEvent(ctx context.Context, sess *session.Session, e Event) {
	if f.onEvent != nil {
		f.onEvent(ctx, sess, e)
	}
}
