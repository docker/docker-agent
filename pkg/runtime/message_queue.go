package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/chat"
)

// Framing controls how a steered message is rendered for the model when it is
// drained into the conversation. It is orthogonal to scheduling (which drain
// site picks the message up): it only changes how the model is told to read
// the injected text.
type Framing string

const (
	// FramingPlain injects the message as a bare user turn — the historical
	// behavior. Use for programmatic callers (chains, channels, bridges) that
	// don't want any meta-instruction wrapped around the text.
	FramingPlain Framing = "plain"
	// FramingInstruction wraps the message in a <system-reminder> that tells the
	// model to finish its current task first, then address the message. This is
	// the default for steered messages and restores the "finish first" UX.
	FramingInstruction Framing = "instruction"
	// FramingReplacement wraps the message in a <system-reminder> that tells the
	// model to abandon its current task and address the message instead.
	FramingReplacement Framing = "replacement"
)

// Valid reports whether f is a recognized framing value. The empty string is
// valid: it represents "unspecified" and is resolved to a route-specific
// default at drain time (instruction for steered messages, plain for
// follow-ups).
func (f Framing) Valid() bool {
	switch f {
	case "", FramingPlain, FramingInstruction, FramingReplacement:
		return true
	}
	return false
}

// QueuedMessage is a user message waiting to be injected into the agent loop,
// either mid-turn (via the steer queue) or at end-of-turn (via the follow-up
// queue).
type QueuedMessage struct {
	Content      string
	MultiContent []chat.MessagePart
	// Framing selects how a steered message is rendered for the model. The zero
	// value ("") is resolved to FramingInstruction when the message is drained
	// from the steer queue. Follow-up messages ignore this field and are always
	// rendered plain.
	Framing Framing
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
