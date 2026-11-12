package server

import (
	"context"
	"slices"
	"sync"
)

// defaultEventLogCapacity is how many recent events an [eventLog] keeps for
// replay. It bounds memory while comfortably covering the window between a
// client taking a snapshot and (re)connecting to the event stream.
const defaultEventLogCapacity = 1024

// seqEvent is an event tagged with its monotonic, per-session sequence number.
type seqEvent struct {
	seq   uint64
	event any
}

// gapEvent is sent to a subscriber whose requested resume point has already
// been evicted from the ring buffer. It is a per-connection control marker,
// not part of the session's sequenced stream, so it carries no sequence
// number. The client should re-snapshot (GET /snapshot) to resync, then
// continue tailing from the snapshot's sequence number.
type gapEvent struct {
	Type string `json:"type"` // always "gap"
}

// sessionExitedEvent is the terminal event appended to the log when the
// session's event source ends (the agent process exited or the session was
// deleted). It is part of the sequenced stream — it has a sequence number and
// is replayed — so a client can rely on it to tell apart two cases:
//
//   - it received session_exited: the session is gone for good; stop.
//   - the SSE connection closed WITHOUT session_exited: a transport drop or a
//     slow-client disconnect; reconnect with the last id (Last-Event-ID) to
//     replay and continue.
type sessionExitedEvent struct {
	Type   string `json:"type"` // always "session_exited"
	Reason string `json:"reason,omitempty"`
}

// eventLog buffers a session's events with monotonic sequence numbers and
// fans them out to live subscribers. A late or reconnecting subscriber can
// replay everything after a sequence number it already saw and then tail new
// events, so the stream is gapless across reconnects as long as the resume
// point is still within the buffer.
//
// One eventLog exists per attached session: a single pump goroutine feeds it
// from the underlying event source, while any number of SSE clients read from
// it via [eventLog.stream].
type eventLog struct {
	capacity int

	mu        sync.Mutex
	seq       uint64
	buf       []seqEvent // oldest first, len <= capacity
	listeners map[*eventListener]struct{}
	closed    bool
}

type eventListener struct {
	ch chan seqEvent
}

func newEventLog(capacity int) *eventLog {
	if capacity <= 0 {
		capacity = defaultEventLogCapacity
	}
	return &eventLog{
		capacity:  capacity,
		listeners: make(map[*eventListener]struct{}),
	}
}

// append records event, assigns it the next sequence number, stores it in the
// ring buffer, and delivers it to all live listeners. A listener whose buffer
// is full is dropped (its channel is closed): the pump must never block on a
// slow client. A dropped client's SSE stream ends; it reconnects with the
// last sequence number it saw and replays from the buffer.
func (l *eventLog) append(event any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.appendLocked(event)
}

// appendLocked is append's body; the caller must hold l.mu and must have
// checked that the log is not closed.
func (l *eventLog) appendLocked(event any) {
	l.seq++
	ev := seqEvent{seq: l.seq, event: event}

	l.buf = append(l.buf, ev)
	if len(l.buf) > l.capacity {
		// Drop the oldest. Copy down so the backing array doesn't grow
		// unbounded as we keep re-slicing.
		copy(l.buf, l.buf[len(l.buf)-l.capacity:])
		l.buf = l.buf[:l.capacity]
	}

	for ln := range l.listeners {
		select {
		case ln.ch <- ev:
		default:
			close(ln.ch)
			delete(l.listeners, ln)
		}
	}
}

// close appends a terminal session_exited event (so connected and replaying
// clients get a definitive end-of-session marker) and then disconnects every
// live listener. Buffered events, including session_exited, remain available
// for replay until the log is dropped. close is idempotent.
func (l *eventLog) close(reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.appendLocked(sessionExitedEvent{Type: "session_exited", Reason: reason})
	l.closed = true
	for ln := range l.listeners {
		close(ln.ch)
		delete(l.listeners, ln)
	}
}

// lastSeq returns the sequence number of the most recent event (0 if none).
func (l *eventLog) lastSeq() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

// stream delivers events to send until ctx is cancelled or the log is closed.
//
// When since is non-nil, buffered events with a sequence number greater than
// *since are replayed first; if *since predates the buffer (the resume point
// was evicted) a [gapEvent] is sent before the replay so the client knows to
// re-snapshot. When since is nil, the current buffer is replayed in full and
// then live events are tailed.
//
// The backlog snapshot and the live-listener registration happen under a
// single lock so that no event can be both missing from the backlog and not
// delivered live: every event is seen exactly once, in order.
func (l *eventLog) stream(ctx context.Context, since *uint64, send func(seq uint64, event any)) {
	l.mu.Lock()
	if l.closed {
		backlog := l.backlogLocked(since)
		l.mu.Unlock()
		l.replay(ctx, since, backlog, send)
		return
	}
	backlog := l.backlogLocked(since)
	ln := &eventListener{ch: make(chan seqEvent, l.capacity)}
	l.listeners[ln] = struct{}{}
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.listeners, ln)
	}()

	if !l.replay(ctx, since, backlog, send) {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ln.ch:
			if !ok {
				return // dropped (slow client) or log closed
			}
			send(ev.seq, ev.event)
		}
	}
}

// backlogLocked returns the buffered events that should be replayed for a
// subscriber resuming at since, plus whether a gap marker is needed. Caller
// must hold l.mu.
func (l *eventLog) backlogLocked(since *uint64) []seqEvent {
	if len(l.buf) == 0 {
		return nil
	}
	if since == nil {
		return slices.Clone(l.buf)
	}
	out := make([]seqEvent, 0, len(l.buf))
	for _, ev := range l.buf {
		if ev.seq > *since {
			out = append(out, ev)
		}
	}
	return out
}

// replay sends a gap marker (when the resume point was evicted) followed by
// the backlog. Returns false if ctx was cancelled mid-replay.
func (l *eventLog) replay(ctx context.Context, since *uint64, backlog []seqEvent, send func(seq uint64, event any)) bool {
	if since != nil && l.gapped(*since, backlog) {
		select {
		case <-ctx.Done():
			return false
		default:
			send(0, gapEvent{Type: "gap"})
		}
	}
	for _, ev := range backlog {
		select {
		case <-ctx.Done():
			return false
		default:
			send(ev.seq, ev.event)
		}
	}
	return true
}

// gapped reports whether resuming at since lost events: the next expected
// sequence number (since+1) is older than the oldest event we can still
// replay. An empty backlog with a non-zero since is not a gap on its own
// (the client is simply already caught up).
func (l *eventLog) gapped(since uint64, backlog []seqEvent) bool {
	if len(backlog) == 0 {
		return false
	}
	return backlog[0].seq > since+1
}
