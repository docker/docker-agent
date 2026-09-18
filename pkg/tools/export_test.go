package tools

import (
	"context"
	"time"
)

// ExportedPublishStopRequest exposes the request-publication half of
// StopIfStarted for tests in the _test package: it leaves a pending stop
// request behind exactly as a requester that lost the lifecycle-lock race
// (e.g. to a concurrent TryIsStarted) with an already-done context would.
func (s *StartableToolSet) ExportedPublishStopRequest(ctx context.Context) {
	s.stopRequestMu.Lock()
	defer s.stopRequestMu.Unlock()
	s.stopRequested = true
	s.stopRequestCtx = ctx
}

// ExportedComputeStartBackoff exposes computeStartBackoff for unit tests
// that verify the bounds and cap behaviour without going through a full
// Start cycle.
func ExportedComputeStartBackoff(attempt int, jitterFn func(time.Duration) time.Duration) time.Duration {
	return computeStartBackoff(attempt, jitterFn)
}

// Exported backoff bound constants for test assertions.
const (
	ExportedStartBackoffBase = startBackoffBase
	ExportedStartBackoffMax  = startBackoffMax
)
