package eventmerge

import (
	"context"
	"time"

	"github.com/docker/docker-agent/pkg/runtime"
)

// DefaultWindow is the coalescing window used when none is configured. It
// matches the TUI's event throttle.
const DefaultWindow = 50 * time.Millisecond

// maxPendingBytes bounds the content held back in one window, so a burst of
// tool output is flushed early instead of building one oversized event.
const maxPendingBytes = 64 << 10

// Coalesce forwards the events of in, merging streaming deltas (see [Merge])
// so at most one batch of them is emitted per window.
//
//   - A delta arriving after a quiet spell of at least window is forwarded
//     immediately, so the first token is never delayed.
//   - Any other event first flushes the pending deltas, so the merged stream
//     keeps the original order.
//   - Pending deltas are flushed early once they exceed maxPendingBytes, and
//     when in is closed.
//
// A window <= 0 disables coalescing and returns in itself.
//
// The returned channel is closed once in is closed. Coalesce always drains
// in to the end, discarding events once ctx is done, so the producer never
// blocks on an abandoned consumer.
func Coalesce(ctx context.Context, in <-chan runtime.Event, window time.Duration) <-chan runtime.Event {
	if window <= 0 {
		return in
	}

	out := make(chan runtime.Event)
	c := &coalescer{done: ctx.Done(), out: out, window: window}
	go func() {
		defer close(out)
		if !c.run(in) {
			for range in {
			}
		}
	}()
	return out
}

type coalescer struct {
	done   <-chan struct{}
	out    chan<- runtime.Event
	window time.Duration

	pending      []runtime.Event
	pendingBytes int
	lastEmit     time.Time
	timer        *time.Timer
}

// run forwards in until it is closed. It returns false if done was closed
// first, leaving the rest of in to the caller.
func (c *coalescer) run(in <-chan runtime.Event) bool {
	for {
		var tick <-chan time.Time
		if c.timer != nil {
			tick = c.timer.C
		}

		select {
		case event, ok := <-in:
			if !ok {
				return c.flush()
			}
			if !c.accept(event) {
				return false
			}
		case <-tick:
			c.timer = nil
			if !c.flush() {
				return false
			}
		}
	}
}

func (c *coalescer) accept(event runtime.Event) bool {
	if !Mergeable(event) {
		return c.flush() && c.send(event)
	}

	if len(c.pending) == 0 && time.Since(c.lastEmit) >= c.window {
		c.lastEmit = time.Now()
		return c.send(event)
	}

	c.pending = append(c.pending, event)
	c.pendingBytes += deltaSize(event)
	if c.pendingBytes >= maxPendingBytes {
		return c.flush()
	}
	if c.timer == nil {
		c.timer = time.NewTimer(c.window - time.Since(c.lastEmit))
	}
	return true
}

func (c *coalescer) flush() bool {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if len(c.pending) == 0 {
		return true
	}

	for _, event := range Merge(c.pending) {
		if !c.send(event) {
			return false
		}
	}
	clear(c.pending)
	c.pending = c.pending[:0]
	c.pendingBytes = 0
	c.lastEmit = time.Now()
	return true
}

func (c *coalescer) send(event runtime.Event) bool {
	select {
	case c.out <- event:
		return true
	case <-c.done:
		return false
	}
}
