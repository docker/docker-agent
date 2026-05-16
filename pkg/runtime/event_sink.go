package runtime

// EventSink is the write side of the runtime's event stream. Methods
// that produce events accept an EventSink instead of a raw chan Event,
// decoupling event producers from the channel implementation.
//
// Implementations must be safe for concurrent use: multiple goroutines
// may call Emit simultaneously (e.g. tool-call handlers running in
// parallel).
type EventSink interface {
	// Emit delivers an event to the sink. The default implementation
	// preserves back-pressure: if the consumer is slow, Emit blocks
	// until the event can be delivered. Implementations must still be
	// panic-safe so a closed channel does not crash the caller.
	Emit(event Event)
}

// channelSink adapts a chan Event into an [EventSink]. Sends are
// blocking so back-pressure is preserved between producers and the
// consumer. Send-on-closed-channel panics are recovered and the event
// is dropped, since a closed channel signals that the consumer has
// gone away.
type channelSink struct {
	ch chan Event
}

// NewChannelSink returns an [EventSink] that writes to ch. This is the
// standard bridge between the runtime's EventSink-based internals and
// external callers that create raw event channels (e.g. app.go,
// tests).
func NewChannelSink(ch chan Event) EventSink {
	return &channelSink{ch: ch}
}

func (s *channelSink) Emit(e Event) {
	defer func() { recover() }() //nolint:errcheck // swallow send-on-closed-channel panic
	s.ch <- e
}

// nonBlockingChannelSink wraps an event channel with non-blocking
// semantics: if the buffer is full, the event is dropped instead of
// blocking the producer. Use this only from long-lived goroutines
// that may outlive the event channel (e.g. the RAG file watcher);
// regular runtime code should use the blocking [channelSink] so
// back-pressure is preserved.
type nonBlockingChannelSink struct {
	ch chan Event
}

// nonBlocking returns a non-blocking sink for sink. If sink wraps a
// channel directly, the result writes to that channel with a
// select-default; otherwise, sink is returned unchanged because
// non-channel sinks (notably [EventSinkFunc] used in tests) do not
// have an underlying buffer that can fill up.
func nonBlocking(sink EventSink) EventSink {
	if cs, ok := sink.(*channelSink); ok {
		return nonBlockingChannelSink{ch: cs.ch}
	}
	return sink
}

func (s nonBlockingChannelSink) Emit(e Event) {
	defer func() { recover() }() //nolint:errcheck // swallow send-on-closed-channel panic
	select {
	case s.ch <- e:
	default:
	}
}

// EventSinkFunc adapts a plain function into an [EventSink], following
// the same adapter pattern as http.HandlerFunc. This is convenient for
// tests and one-off callbacks that don't need a full struct.
type EventSinkFunc func(Event)

func (f EventSinkFunc) Emit(e Event) { f(e) }
