package rag

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/rag"
	"github.com/docker/docker-agent/pkg/rag/strategy"
	ragtypes "github.com/docker/docker-agent/pkg/rag/types"
)

// eventRecorder collects forwarded events and signals each arrival.
type eventRecorder struct {
	mu     sync.Mutex
	events []ragtypes.Event
	got    chan struct{}
}

func newEventRecorder() *eventRecorder {
	return &eventRecorder{got: make(chan struct{}, 16)}
}

func (r *eventRecorder) callback(ev ragtypes.Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	r.got <- struct{}{}
}

func (r *eventRecorder) wait(t *testing.T) {
	t.Helper()
	select {
	case <-r.got:
	case <-time.After(5 * time.Second):
		t.Fatal("event was never forwarded")
	}
}

func (r *eventRecorder) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, ev := range r.events {
		out = append(out, ev.Message)
	}
	return out
}

func newEventedRAGToolSet(t *testing.T) (*ToolSet, chan<- ragtypes.Event) {
	t.Helper()
	events := make(chan ragtypes.Event, 16)
	cfg := rag.Config{
		StrategyConfigs: []strategy.Config{{Name: "mock", Strategy: &mockStrategy{}}},
	}
	mgr, err := rag.New(t.Context(), "evented-rag", cfg, events)
	require.NoError(t, err)
	ts := New(mgr, "evented-rag")
	t.Cleanup(func() { _ = ts.Stop(t.Context()) })
	return ts, events
}

// TestSubscribeEvents_FanOut pins the multi-host contract: every subscriber
// and the legacy SetEventCallback slot receive every event, subscriptions
// added after Start still work, and unsubscribing only removes that one.
func TestSubscribeEvents_FanOut(t *testing.T) {
	t.Parallel()

	ts, events := newEventedRAGToolSet(t)

	before := newEventRecorder()
	unsubBefore := ts.SubscribeEvents(before.callback)
	legacy := newEventRecorder()
	ts.SetEventCallback(legacy.callback)

	require.NoError(t, ts.Start(t.Context()))

	after := newEventRecorder()
	ts.SubscribeEvents(after.callback)

	events <- ragtypes.Event{Type: ragtypes.EventTypeIndexingStarted, Message: "one"}
	for _, r := range []*eventRecorder{before, legacy, after} {
		r.wait(t)
	}

	unsubBefore()
	events <- ragtypes.Event{Type: ragtypes.EventTypeIndexingComplete, Message: "two"}
	legacy.wait(t)
	after.wait(t)

	assert.Equal(t, []string{"one"}, before.messages())
	assert.Equal(t, []string{"one", "two"}, legacy.messages())
	assert.Equal(t, []string{"one", "two"}, after.messages())
}

// TestSetEventCallback_ReplacesPrevious keeps the legacy single-slot
// semantics: a second SetEventCallback displaces the first, nil clears.
func TestSetEventCallback_ReplacesPrevious(t *testing.T) {
	t.Parallel()

	ts, events := newEventedRAGToolSet(t)
	first := newEventRecorder()
	second := newEventRecorder()
	ts.SetEventCallback(first.callback)
	ts.SetEventCallback(second.callback)
	require.NoError(t, ts.Start(t.Context()))

	events <- ragtypes.Event{Type: ragtypes.EventTypeIndexingStarted, Message: "one"}
	second.wait(t)

	ts.SetEventCallback(nil)
	watcher := newEventRecorder()
	ts.SubscribeEvents(watcher.callback)
	events <- ragtypes.Event{Type: ragtypes.EventTypeIndexingStarted, Message: "two"}
	watcher.wait(t)

	assert.Empty(t, first.messages())
	assert.Equal(t, []string{"one"}, second.messages())
}
