package mcp

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

func TestInstructions_Concurrent(t *testing.T) {
	t.Parallel()

	ts := newTestToolset("test", "test", &mockMCPClient{})
	ts.markStartedForTesting()
	ts.instructions = "initial"

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			// Simulate a concurrent writer (the supervisor's Connect updates
			// instructions under ts.mu after a successful Initialize).
			ts.mu.Lock()
			ts.instructions = "updated"
			ts.mu.Unlock()
		}()
		go func() {
			defer wg.Done()
			_ = ts.Instructions()
		}()
	}
	wg.Wait()
}

// TestSupervisorRespectsContextCancellation verifies that a supervisor's
// restart loop returns promptly when ctx is cancelled instead of sleeping
// through the full backoff. This used to be a Toolset.tryRestart test;
// after the supervisor extraction it lives at the supervisor layer.
func TestSupervisorRespectsContextCancellation(t *testing.T) {
	t.Parallel()

	// A connector that always fails: the supervisor will spin in its
	// restart loop until ctx is cancelled.
	failing := failingConnector{err: context.DeadlineExceeded}

	policy := lifecycle.Policy{
		MaxAttempts: 100, // large, so cancellation must be the exit reason
		Backoff:     lifecycle.Backoff{Initial: 5 * time.Second},
	}
	s := lifecycle.New("ctx-test", &failing, policy)

	// Drive the supervisor manually: simulate a session failure that the
	// watcher would react to, by starting then forcing a reconnect under
	// our cancellable ctx.
	ctx, cancel := context.WithCancel(t.Context())

	// Start fails immediately because the connector errors.
	err := s.Start(ctx)
	require.Error(t, err, "Start must propagate connector error")

	// Now exercise RestartAndWait + cancel: it should return promptly.
	done := make(chan error, 1)
	go func() { done <- s.RestartAndWait(ctx, 10*time.Second) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case got := <-done:
		require.Error(t, got, "RestartAndWait should return after cancel")
	case <-time.After(2 * time.Second):
		t.Fatal("RestartAndWait did not return promptly after context cancellation")
	}
}

type failingConnector struct{ err error }

func (f *failingConnector) Connect(context.Context) (lifecycle.Session, error) {
	return nil, f.err
}
