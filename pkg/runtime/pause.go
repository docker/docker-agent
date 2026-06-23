package runtime

import "context"

// TogglePause toggles whether runStreamLoop pauses at iteration boundaries.
// Returns true if now paused. The pause takes effect as soon as the in-flight
// LLM request and its tool calls complete.
//
// The error return is reserved for runtimes that need to talk to a remote
// service to flip the flag; LocalRuntime always returns nil.
//
// Internally, pauseCh is non-nil and open while paused; closing it on resume
// wakes every goroutine waiting in waitIfPaused.
func (r *LocalRuntime) TogglePause(context.Context) (bool, error) {
	r.pauseMu.Lock()
	defer r.pauseMu.Unlock()
	if r.pauseCh != nil {
		close(r.pauseCh)
		r.pauseCh = nil
		return false, nil
	}
	r.pauseCh = make(chan struct{})
	return true, nil
}

// isPaused reports whether /pause is currently armed (the run loop will
// block at the next iteration boundary).
func (r *LocalRuntime) isPaused() bool {
	r.pauseMu.Lock()
	defer r.pauseMu.Unlock()
	return r.pauseCh != nil
}

// waitIfPaused blocks until the runtime is resumed or ctx is cancelled.
func (r *LocalRuntime) waitIfPaused(ctx context.Context) error {
	r.pauseMu.Lock()
	ch := r.pauseCh
	r.pauseMu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
