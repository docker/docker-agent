package tabstate

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

func TestAttentionQueue(t *testing.T) {
	t.Parallel()
	state := New("tab", "")
	first := &runtime.ElicitationRequestEvent{ElicitationID: "first"}
	second := &runtime.ElicitationRequestEvent{ElicitationID: "second"}
	for _, event := range []tea.Msg{first, second} {
		changed, bell := state.Apply(event, false)
		assert.True(t, changed)
		assert.True(t, bell)
	}
	_, _, attention := state.Snapshot()
	assert.True(t, attention)
	state.Acknowledge()
	stashed := &runtime.ElicitationRequestEvent{ElicitationID: "stashed"}
	state.Prepend(stashed)
	_, _, attention = state.Snapshot()
	assert.False(t, attention, "restashing a seen prompt must not raise attention")
	assert.Same(t, stashed, state.Consume())
	assert.Same(t, first, state.Consume())
	assert.Same(t, second, state.Consume())
	assert.Nil(t, state.Consume())
}

func TestActiveAttentionUsesSameQueueWithoutBell(t *testing.T) {
	t.Parallel()
	for _, event := range []tea.Msg{
		&runtime.ToolCallConfirmationEvent{},
		&runtime.MaxIterationsReachedEvent{},
		&runtime.ElicitationRequestEvent{},
	} {
		state := New("tab", "")
		changed, bell := state.Apply(event, true)
		assert.True(t, changed)
		assert.False(t, bell)
		assert.Same(t, event, state.Consume())
		_, _, attention := state.Snapshot()
		assert.False(t, attention)
	}
}

func TestStreamBoundaries(t *testing.T) {
	t.Parallel()
	for _, started := range []bool{false, true} {
		name := "stopped"
		if started {
			name = "started"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, id := range []string{"tab", "", "child"} {
				t.Run(id, func(t *testing.T) {
					t.Parallel()
					state := New("tab", "")
					state.Apply(&runtime.StreamStartedEvent{SessionID: "tab"}, false)
					foreground := []tea.Msg{
						&runtime.ToolCallConfirmationEvent{},
						&runtime.MaxIterationsReachedEvent{},
						&runtime.ElicitationRequestEvent{SessionID: "tab"},
						&runtime.ElicitationRequestEvent{},
					}
					detached := &runtime.ElicitationRequestEvent{SessionID: "background-job"}
					for _, event := range append(foreground, detached) {
						state.Apply(event, false)
					}
					var event tea.Msg = &runtime.StreamStoppedEvent{SessionID: id}
					if started {
						event = &runtime.StreamStartedEvent{SessionID: id}
					}
					changed, bell := state.Apply(event, false)
					assert.False(t, bell)
					_, running, attention := state.Snapshot()
					assert.True(t, attention, "a detached job's prompt survives its parent's turn")
					if id == "child" {
						assert.False(t, changed)
						assert.True(t, running)
						for _, event := range foreground {
							assert.Same(t, event, state.Consume())
						}
					} else {
						assert.True(t, changed)
						assert.Equal(t, started, running)
					}
					assert.Same(t, detached, state.Consume())
					assert.Nil(t, state.Consume())
				})
			}
		})
	}
}

func TestStreamBoundaryClearsForegroundAttention(t *testing.T) {
	t.Parallel()
	for _, event := range []tea.Msg{
		&runtime.StreamStartedEvent{SessionID: "tab"},
		&runtime.StreamStoppedEvent{SessionID: "tab"},
	} {
		state := New("tab", "")
		state.Apply(&runtime.ElicitationRequestEvent{}, false)
		state.Apply(event, false)
		assert.Nil(t, state.Consume())
		_, _, attention := state.Snapshot()
		assert.False(t, attention)
	}
}

func TestTitleAndUnrelatedEvents(t *testing.T) {
	t.Parallel()
	state := New("tab", "initial")
	title, _, _ := state.Snapshot()
	assert.Equal(t, "initial", title)
	state.SetTitle("restored")
	title, _, _ = state.Snapshot()
	assert.Equal(t, "restored", title)
	changed, bell := state.Apply(&runtime.SessionTitleEvent{Title: "generated"}, true)
	require.True(t, changed)
	assert.False(t, bell)
	title, _, _ = state.Snapshot()
	assert.Equal(t, "generated", title)
	changed, bell = state.Apply(struct{}{}, false)
	assert.False(t, changed)
	assert.False(t, bell)
}

func TestReplaceSessionRetainsSameConversation(t *testing.T) {
	t.Parallel()
	state := New("conversation", "title")
	event := &runtime.ElicitationRequestEvent{SessionID: "child"}
	state.Apply(&runtime.StreamStartedEvent{SessionID: "conversation"}, false)
	state.Apply(event, false)
	state.ReplaceSession("conversation")
	title, running, attention := state.Snapshot()
	assert.Equal(t, "title", title)
	assert.True(t, running)
	assert.True(t, attention)
	assert.Same(t, event, state.Consume())
	state.ReplaceSession("replacement")
	assert.Equal(t, "replacement", state.SessionID())
	_, running, attention = state.Snapshot()
	assert.False(t, running)
	assert.False(t, attention)
}
