package tools

import "context"

// toolsetRun bridges blocking workers to the host's Start/Stop lifecycle.
// Only the canonical wrapper owns cancellation and joining; workers keep
// their resources local to Run.
type toolsetRun struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

func startToolsetRun(ctx context.Context, runnable Runnable) *toolsetRun {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		runnable.Run(ctx)
	}()
	return &toolsetRun{cancel: cancel, done: done}
}

func (r *toolsetRun) finished() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

func (r *toolsetRun) stop() {
	r.cancel()
	<-r.done
}

// Blocking reporters must settle inner lifecycle operations before recovery.
func (s *StartableToolSet) isStartedLocked(wait bool) bool {
	if !s.started || (s.run != nil && s.run.finished()) {
		return false
	}
	if owner, ok := As[*StartableToolSet](s.ToolSet); ok {
		if wait {
			return owner.IsStarted()
		}
		return owner.TryIsStarted()
	}
	return true
}

func nonblockingReporterStarted(reporter StartReporter) bool {
	if owner, ok := reporter.(*StartableToolSet); ok {
		return owner.TryIsHealthy()
	}
	return reporter.IsStarted()
}

// Recovery waits for an inner owner and then checks health, not just its latch.
func blockingReporterStarted(reporter StartReporter) bool {
	owner, ok := reporter.(*StartableToolSet)
	if !ok {
		return reporter.IsStarted()
	}
	owner.mu.Lock()
	defer owner.unlock()
	if owner.disposed || !owner.isStartedLocked(true) {
		return false
	}
	if inner, ok := As[StartReporter](owner.ToolSet); ok {
		return blockingReporterStarted(inner)
	}
	return true
}
