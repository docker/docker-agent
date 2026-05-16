package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/chat"
)

// QueuedMessage is a user message waiting to be injected into the agent loop,
// either mid-turn (via the steer queue) or at end-of-turn (via the follow-up
// queue).
type QueuedMessage struct {
	Content      string
	MultiContent []chat.MessagePart
}

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

// inMemoryMessageQueue is the default MessageQueue backed by a buffered channel.
type inMemoryMessageQueue struct {
	ch chan QueuedMessage
}

const (
	// defaultSteerQueueCapacity is the buffer size for the default in-memory steer queue.
	defaultSteerQueueCapacity = 5
	// defaultFollowUpQueueCapacity is the buffer size for the default in-memory follow-up queue.
	// Higher than steer because follow-ups accumulate while waiting for the turn to end.
	defaultFollowUpQueueCapacity = 20
)

// NewInMemoryMessageQueue creates a MessageQueue backed by a buffered channel
// with the given capacity.
func NewInMemoryMessageQueue(capacity int) MessageQueue {
	return &inMemoryMessageQueue{ch: make(chan QueuedMessage, capacity)}
}

func (q *inMemoryMessageQueue) Enqueue(_ context.Context, msg QueuedMessage) bool {
	select {
	case q.ch <- msg:
		return true
	default:
		return false
	}
}

func (q *inMemoryMessageQueue) Dequeue(_ context.Context) (QueuedMessage, bool) {
	select {
	case m := <-q.ch:
		return m, true
	default:
		return QueuedMessage{}, false
	}
}

func (q *inMemoryMessageQueue) Drain(_ context.Context) []QueuedMessage {
	var msgs []QueuedMessage
	for {
		select {
		case m := <-q.ch:
			msgs = append(msgs, m)
		default:
			return msgs
		}
	}
}

// QueueStatus represents the current depth and capacity of message queues
type QueueStatus struct {
	SteerDepth       int
	SteerCapacity    int
	FollowupDepth    int
	FollowupCapacity int
}
