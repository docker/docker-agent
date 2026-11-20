package markdowntransform

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// RefreshInterval batches completions, such as a restored transcript being
// transformed, into at most one view refresh per interval.
const RefreshInterval = 50 * time.Millisecond

// StreamInterval is the minimum time between transforms of one streaming
// message. Each request carries the whole reply, so transforming every chunk
// would grow quadratically with its length; a reply's first result and its
// completed snapshot are not delayed.
const StreamInterval = 50 * time.Millisecond

// Surface is the transform state of one rendered message. The zero value is
// ready to use; a Surface must not be copied after first use.
//
// Invariant: whenever want differs from have, the surface is queued or
// running, so the latest wanted input always gets a result.
type Surface struct {
	want, have Input
	output     string
	version    uint64
	ready      bool
	running    bool
	started    time.Time
	// queued is this surface's queue node, or nil when it is not queued.
	queued *list.Element
}

// Display runs transformers on one background worker so rendering never waits
// for a process. Each view reads the latest result for its own Surface.
type Display struct {
	transformers   []Transformer
	streamInterval time.Duration

	mu sync.Mutex
	// queue holds *Surface values, newest at the back.
	queue   list.List
	wake    chan struct{}
	changed chan struct{}
}

// NewDisplay returns a display whose worker runs once Start is called.
func NewDisplay(transformers ...Transformer) *Display {
	return &Display{
		transformers:   transformers,
		streamInterval: StreamInterval,
		wake:           make(chan struct{}, 1),
		changed:        make(chan struct{}),
	}
}

// Start runs the transform worker until ctx is done.
func (d *Display) Start(ctx context.Context) {
	go d.run(ctx)
}

// Markdown returns the transformed Markdown for s and schedules a transform
// when in changed. It returns the source until a first result exists, then
// the latest result, so streamed text never flickers back to source.
func (d *Display) Markdown(s *Surface, in Input) string {
	if d == nil {
		return in.Markdown
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s.want = in
	d.schedule(s)
	if s.ready {
		return s.output
	}
	return in.Markdown
}

// Current reports whether the result for exactly in is ready. A frame that
// nothing will redraw, such as the last one before exit, should show the
// source instead of an older result.
func (d *Display) Current(s *Surface, in Input) bool {
	if d == nil {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return s.ready && s.have == in
}

// schedule queues s at the newest position unless its wanted input already
// has a result or is running; a queued surface rendered again moves up, so
// the queue follows the most recent render. The caller holds d.mu.
func (d *Display) schedule(s *Surface) {
	if s.running || (s.ready && s.have == s.want) {
		return
	}
	if s.queued != nil {
		d.queue.MoveToBack(s.queued)
	} else {
		s.queued = d.queue.PushBack(s)
	}
	// Wake even on promotion: a throttled surface whose input completed is
	// due now, while the worker may be waiting out the throttle.
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Version changes whenever the transformed Markdown for s changes.
func (d *Display) Version(s *Surface) uint64 {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return s.version
}

// Changed returns a channel closed when the next transform completes. Callers
// must take the channel before refreshing so no completion is missed, and
// should wait RefreshInterval before refreshing so bursts become one refresh.
func (d *Display) Changed() <-chan struct{} {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.changed
}

func (d *Display) run(ctx context.Context) {
	for {
		s, in, wait := d.next(time.Now())
		if s != nil {
			d.complete(s, in, apply(ctx, d.transformers, in))
			continue
		}
		var due <-chan time.Time
		if wait > 0 {
			due = time.After(wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		case <-due:
		}
	}
}

// next pops the most recently queued surface that is due: live replies and
// the bottom of a restored transcript go first, ahead of older history. When
// only throttled streaming surfaces are queued, it returns how long until the
// first is due, or zero when the queue is empty.
func (d *Display) next(now time.Time) (*Surface, Input, time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var wait time.Duration
	for e := d.queue.Back(); e != nil; e = e.Prev() {
		s := e.Value.(*Surface)
		if left := d.throttled(s, now); left > 0 {
			if wait == 0 || left < wait {
				wait = left
			}
			continue
		}
		d.queue.Remove(e)
		s.queued, s.running, s.started = nil, true, now
		return s, s.want, 0
	}
	return nil, Input{}, wait
}

// throttled returns how long s must still wait before its next transform.
func (d *Display) throttled(s *Surface, now time.Time) time.Duration {
	if !s.ready || !s.want.IsStreaming {
		return 0
	}
	return max(0, s.started.Add(d.streamInterval).Sub(now))
}

func (d *Display) complete(s *Surface, in Input, out string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !s.ready || s.output != out {
		s.version++
	}
	s.have, s.output, s.ready, s.running = in, out, true, false
	// The input may have changed while this one ran.
	d.schedule(s)
	close(d.changed)
	d.changed = make(chan struct{})
}
