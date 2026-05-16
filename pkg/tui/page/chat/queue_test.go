package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// newTestChatPage creates a minimal chatPage for testing queue behavior.
// Note: This only initializes fields needed for queue testing.
// processMessage cannot be called without full initialization.
func newTestChatPage(t *testing.T) *chatPage {
	t.Helper()
	sessionState := &service.SessionState{}

	return &chatPage{
		sidebar:      sidebar.New(sessionState),
		sessionState: sessionState,
		working:      true, // Start busy so messages get queued
	}
}

func TestQueueFlow_BusyAgent_QueuesMessage(t *testing.T) {
	t.Parallel()

	p := newTestChatPage(t)
	// newTestChatPage already sets working=true

	// Send first message while busy
	msg1 := messages.SendMsg{Content: "first message"}
	_, cmd := p.handleSendMsg(msg1)

	// Should be queued
	require.Len(t, p.messageQueue, 1)
	assert.Equal(t, "first message", p.messageQueue[0].content)
	// Command should be a notification (not processMessage)
	assert.NotNil(t, cmd)

	// Send second message while still busy
	msg2 := messages.SendMsg{Content: "second message"}
	_, _ = p.handleSendMsg(msg2)

	require.Len(t, p.messageQueue, 2)
	assert.Equal(t, "first message", p.messageQueue[0].content)
	assert.Equal(t, "second message", p.messageQueue[1].content)

	// Send third message
	msg3 := messages.SendMsg{Content: "third message"}
	_, _ = p.handleSendMsg(msg3)

	require.Len(t, p.messageQueue, 3)
}

func TestQueueFlow_QueueFull_RejectsMessage(t *testing.T) {
	t.Parallel()

	p := newTestChatPage(t)
	// newTestChatPage sets working=true

	// Fill the queue to max
	for i := range maxQueuedMessages {
		msg := messages.SendMsg{Content: "message"}
		_, _ = p.handleSendMsg(msg)
		assert.Len(t, p.messageQueue, i+1)
	}

	require.Len(t, p.messageQueue, maxQueuedMessages)

	// Try to add one more - should be rejected
	msg := messages.SendMsg{Content: "overflow message"}
	_, cmd := p.handleSendMsg(msg)

	// Queue size should not change
	assert.Len(t, p.messageQueue, maxQueuedMessages)
	// Should return a warning notification command
	assert.NotNil(t, cmd)
}

func TestQueueFlow_PopFromQueue(t *testing.T) {
	t.Parallel()

	p := newTestChatPage(t)

	// Queue some messages
	p.handleSendMsg(messages.SendMsg{Content: "first"})
	p.handleSendMsg(messages.SendMsg{Content: "second"})
	p.handleSendMsg(messages.SendMsg{Content: "third"})

	require.Len(t, p.messageQueue, 3)

	// Manually pop messages (simulating what processNextQueuedMessage does internally)
	// Pop first
	popped := p.messageQueue[0]
	p.messageQueue = p.messageQueue[1:]
	p.syncQueueToSidebar()

	assert.Equal(t, "first", popped.content)
	require.Len(t, p.messageQueue, 2)
	assert.Equal(t, "second", p.messageQueue[0].content)
	assert.Equal(t, "third", p.messageQueue[1].content)

	// Pop second
	popped = p.messageQueue[0]
	p.messageQueue = p.messageQueue[1:]

	assert.Equal(t, "second", popped.content)
	require.Len(t, p.messageQueue, 1)
	assert.Equal(t, "third", p.messageQueue[0].content)

	// Pop last
	popped = p.messageQueue[0]
	p.messageQueue = p.messageQueue[1:]

	assert.Equal(t, "third", popped.content)
	assert.Empty(t, p.messageQueue)
}

func TestQueueFlow_ClearQueue(t *testing.T) {
	t.Parallel()

	p := newTestChatPage(t)
	// newTestChatPage sets working=true

	// Queue some messages
	p.handleSendMsg(messages.SendMsg{Content: "first"})
	p.handleSendMsg(messages.SendMsg{Content: "second"})
	p.handleSendMsg(messages.SendMsg{Content: "third"})

	require.Len(t, p.messageQueue, 3)

	// Clear the queue
	_, cmd := p.handleClearQueue()

	assert.Empty(t, p.messageQueue)
	assert.NotNil(t, cmd) // Success notification

	// Clearing empty queue
	_, cmd = p.handleClearQueue()
	assert.Empty(t, p.messageQueue)
	assert.NotNil(t, cmd) // Info notification
}
