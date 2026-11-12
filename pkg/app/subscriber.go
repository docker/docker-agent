package app

import (
	"context"
	"sync"

	tea "charm.land/bubbletea/v2"
)

// eventSubscriber buffers reliable deliveries independently of slow consumers.
// Best-effort subscribers keep their bounded channel and overflow policy.
type eventSubscriber struct {
	ch                chan tea.Msg
	queue             *eventQueue
	prepare           func(tea.Msg) tea.Msg
	registered        func()
	prepareGeneration func(tea.Msg, uint64) tea.Msg
}

// SubscribeOption configures reliable event delivery.
type SubscribeOption func(*eventSubscriber)

// WithEventMapper stamps routing metadata before an event is queued.
// The mapper runs on the fan-out goroutine and must not block; nil skips delivery.
func WithEventMapper(mapper func(tea.Msg) tea.Msg) SubscribeOption {
	return func(sub *eventSubscriber) {
		sub.prepare = mapper
	}
}

// WithGenerationEventMapper preserves producer identity through routing queues.
func WithGenerationEventMapper(mapper func(tea.Msg, uint64) tea.Msg) SubscribeOption {
	return func(sub *eventSubscriber) { sub.prepareGeneration = mapper }
}

// WithSubscriptionReady signals after registration, before any events are delivered.
func WithSubscriptionReady(ready func()) SubscribeOption {
	return func(sub *eventSubscriber) { sub.registered = ready }
}

type queuedEvent struct {
	generation uint64
	msg        tea.Msg
}

type eventQueue struct {
	mu      sync.Mutex
	pending []queuedEvent
	ready   chan struct{}
	closed  bool
}

func newEventQueue() *eventQueue {
	return &eventQueue{ready: make(chan struct{}, 1)}
}

func (q *eventQueue) pushGeneration(msg tea.Msg, generation uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.pending = append(q.pending, queuedEvent{generation: generation, msg: msg})
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *eventQueue) next(ctx context.Context, done <-chan struct{}) (tea.Msg, bool) {
	for {
		select {
		case <-ctx.Done():
			return nil, false
		case <-done:
			return nil, false
		default:
		}

		q.mu.Lock()
		if len(q.pending) > 0 {
			msg := q.pending[0].msg
			q.pending[0] = queuedEvent{}
			q.pending = q.pending[1:]
			if len(q.pending) == 0 {
				q.pending = nil
			}
			q.mu.Unlock()
			return msg, true
		}
		q.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, false
		case <-done:
			return nil, false
		case <-q.ready:
		}
	}
}

func (q *eventQueue) retire(generation uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	kept := q.pending[:0]
	for _, event := range q.pending {
		if event.generation >= generation {
			kept = append(kept, event)
		}
	}
	clear(q.pending[len(kept):])
	q.pending = kept
	if len(kept) == 0 {
		q.pending = nil
	}
}

func (q *eventQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.pending = nil
}
