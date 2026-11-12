package runtime

import (
	"context"
	"sync"

	"github.com/docker/docker-agent/pkg/chat"
)

// QueuedMessage is a user message waiting to be injected into the agent loop,
// either mid-turn (via the steer queue) or at end-of-turn (via the follow-up
// queue).
type QueuedMessage struct {
	ID           string
	Content      string
	MultiContent []chat.MessagePart
}

// PendingMessageCanceler is implemented by runtimes that can withdraw queued
// messages before the agent loop consumes them.
type PendingMessageCanceler interface {
	CancelSteer(ctx context.Context, id string) bool
	CancelFollowUp(ctx context.Context, id string) bool
}

// RecallHandler delivers a tool-produced message through an embedder-owned
// wake-up path. Embedders use it to resume an idle UI/session when background
// work completes after the active RunStream has stopped.
type RecallHandler func(ctx context.Context, msg QueuedMessage) bool

// MessageQueue is the interface for storing messages that are injected into
// the agent loop. Implementations must be safe for concurrent use: Enqueue
// is called from API handlers while Dequeue/Drain are called from the agent
// loop goroutine.
//
// The default implementation is NewInMemoryMessageQueue. Callers that need
// durable or distributed storage can provide their own implementation
// via the WithSteerQueue or WithFollowUpQueue options.
type MessageQueue interface {
	// Enqueue adds a message to the queue. Returns false if the queue is
	// full or the context is cancelled.
	Enqueue(ctx context.Context, msg QueuedMessage) bool
	// Dequeue removes and returns the next message from the queue.
	// Returns the message and true, or a zero value and false if the
	// queue is empty. Must not block.
	Dequeue(ctx context.Context) (QueuedMessage, bool)
	// Drain returns all pending messages and removes them from the queue.
	// Must not block — if the queue is empty it returns nil.
	Drain(ctx context.Context) []QueuedMessage
}

type cancelableMessageQueue interface {
	MessageQueue
	Cancel(id string) bool
}

// inMemoryMessageQueue is the default MessageQueue.
type inMemoryMessageQueue struct {
	mu       sync.Mutex
	messages []QueuedMessage
	capacity int
}

const (
	// defaultSteerQueueCapacity is the buffer size for the default in-memory steer queue.
	defaultSteerQueueCapacity = 5
	// defaultFollowUpQueueCapacity is the buffer size for the default in-memory follow-up queue.
	// Higher than steer because follow-ups accumulate while waiting for the turn to end.
	defaultFollowUpQueueCapacity = 20
)

// NewInMemoryMessageQueue creates an in-memory FIFO queue with the given capacity.
func NewInMemoryMessageQueue(capacity int) MessageQueue {
	return &inMemoryMessageQueue{capacity: capacity}
}

func (q *inMemoryMessageQueue) Enqueue(ctx context.Context, msg QueuedMessage) bool {
	if ctx.Err() != nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.messages) >= q.capacity {
		return false
	}
	q.messages = append(q.messages, msg)
	return true
}

func (q *inMemoryMessageQueue) Dequeue(_ context.Context) (QueuedMessage, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.messages) == 0 {
		return QueuedMessage{}, false
	}
	msg := q.messages[0]
	q.messages[0] = QueuedMessage{}
	q.messages = q.messages[1:]
	return msg, true
}

func (q *inMemoryMessageQueue) Drain(_ context.Context) []QueuedMessage {
	q.mu.Lock()
	defer q.mu.Unlock()
	msgs := q.messages
	q.messages = nil
	return msgs
}

func (q *inMemoryMessageQueue) Cancel(id string) bool {
	if id == "" {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, msg := range q.messages {
		if msg.ID != id {
			continue
		}
		q.messages[i] = QueuedMessage{}
		q.messages = append(q.messages[:i], q.messages[i+1:]...)
		return true
	}
	return false
}

func (q *inMemoryMessageQueue) status() (depth, capacity int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.messages), q.capacity
}

// QueueStatus represents the current depth and capacity of message queues
type QueueStatus struct {
	SteerDepth       int
	SteerCapacity    int
	FollowupDepth    int
	FollowupCapacity int
}
