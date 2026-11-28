package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/session"
)

// EventObserver receives the runtime's event stream as it's produced.
// Implementations subscribe to lifecycle moments and act on them —
// persisting to a store, forwarding to a metrics pipeline, writing an
// audit transcript, etc.
//
// Concurrency: the runtime invokes observers synchronously from the
// goroutine that forwards events to the consumer's channel, in
// registration order. A slow observer therefore back-pressures both
// downstream observers and the consumer; long-running work (network
// I/O, file syncing) should fan out to a private goroutine.
//
// Errors: observers do not return errors. The runtime cannot recover
// from a misbehaving observer (it can't unregister it mid-stream and
// can't ask the consumer to retry), so an observer must log internally
// and never panic. The contract is "best-effort observation" rather
// than "all-or-nothing transactional".
//
// Observers see every event the runtime emits, including sub-session
// events (from delegated tasks via transfer_task) and
// [SessionScoped]-mismatch events. Filtering is the observer's
// responsibility; see [PersistenceObserver] for the canonical pattern.
type EventObserver interface {
	// OnRunStart fires once when [LocalRuntime.RunStream] begins, before
	// any event is dispatched. Use it for one-shot lifecycle work like
	// persisting initial session metadata.
	OnRunStart(ctx context.Context, sess *session.Session)
	// OnEvent fires once per event, after the runtime emits it but
	// before the consumer's channel receives it. Observers cannot
	// modify or suppress events (a future extension may relax this);
	// to drop an event from persistence, simply ignore it inside
	// OnEvent.
	OnEvent(ctx context.Context, sess *session.Session, event Event)
}

// WithEventObserver appends o to the runtime's observer chain.
// Observers are invoked in registration order, synchronously, on every
// event the runtime produces. Multiple calls are additive.
//
// The runtime auto-registers a [PersistenceObserver] for the configured
// session store; users do not need to wire persistence themselves.
// Custom observers (telemetry, audit, metrics, A2A forward) compose
// alongside that one.
func WithEventObserver(o EventObserver) Opt {
	return func(r *LocalRuntime) {
		if o == nil {
			return
		}
		if factory, ok := o.(interface{ NewRunObserver() EventObserver }); ok {
			r.observers = append(r.observers, factory.NewRunObserver)
		} else {
			r.observers = append(r.observers, func() EventObserver { return o })
		}
	}
}

// WithEventObserverFactory creates an independent observer for each RunStream.
// Factories run synchronously before OnRunStart; nil results are ignored.
func WithEventObserverFactory(factory func() EventObserver) Opt {
	return func(r *LocalRuntime) {
		if factory != nil {
			r.observers = append(r.observers, factory)
		}
	}
}

type observerBarrier struct {
	done chan struct{}
	// lost is why an observer that stores the transcript no longer holds
	// all of it, if one says so. Set before done is closed.
	lost error
}

func (*observerBarrier) GetAgentName() string { return "" }

type observerFlushKey struct{}

// observerFlush waits until the observers have seen every event emitted so
// far, and returns the barrier they passed.
type observerFlush func(ctx context.Context) (*observerBarrier, error)

// flushObservers orders direct store writes after previously emitted events.
func flushObservers(ctx context.Context) error {
	flush, _ := ctx.Value(observerFlushKey{}).(observerFlush)
	if flush == nil {
		return nil
	}
	_, err := flush(ctx)
	return err
}

// transcriptStored waits like flushObservers, and also reports the error
// that kept part of what was emitted out of the store, if one did.
func (flush observerFlush) transcriptStored(ctx context.Context) error {
	barrier, err := flush(ctx)
	if err != nil {
		return err
	}
	return barrier.lost
}

func newObserverFlush(events chan<- Event) observerFlush {
	return func(ctx context.Context) (*observerBarrier, error) {
		barrier := &observerBarrier{done: make(chan struct{})}
		select {
		case events <- barrier:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		select {
		case <-barrier.done:
			return barrier, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func withObserverFlush(ctx context.Context, events chan<- Event) context.Context {
	return context.WithValue(ctx, observerFlushKey{}, newObserverFlush(events))
}

// observe wraps inner with the runtime's observer chain: each
// observer sees [EventObserver.OnRunStart] before the first event,
// then every event drained from inner is dispatched to each observer
// in registration order before being forwarded to the returned
// channel. Observers run synchronously, so a slow observer
// back-pressures the consumer.
func (r *LocalRuntime) observe(ctx context.Context, sess *session.Session, inner <-chan Event) <-chan Event {
	observers := make([]EventObserver, 0, len(r.observers))
	for _, factory := range r.observers {
		if obs := factory(); obs != nil {
			observers = append(observers, obs)
		}
	}
	for _, obs := range observers {
		obs.OnRunStart(ctx, sess)
	}
	out := make(chan Event, cap(inner))
	go func() {
		defer close(out)
		for event := range inner {
			if barrier, ok := event.(*observerBarrier); ok {
				for _, obs := range observers {
					if keeper, ok := obs.(interface{ Err() error }); ok && barrier.lost == nil {
						barrier.lost = keeper.Err()
					}
				}
				close(barrier.done)
				continue
			}
			for _, obs := range observers {
				obs.OnEvent(ctx, sess, event)
			}
			out <- event
		}
	}()
	return out
}
