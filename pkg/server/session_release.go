package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/team"
)

const (
	// releaseStreamWait bounds how long a deleted session's stream may keep
	// its runtime alive before the runtime is released anyway.
	releaseStreamWait = 5 * time.Minute
	// releaseCloseTimeout bounds runtime.Close, which waits on background
	// agents with no timeout of its own.
	releaseCloseTimeout = 10 * time.Second
	// releaseStopTimeout bounds stopping the team's toolsets. It is a separate
	// budget so a slow Close cannot starve it.
	releaseStopTimeout = 30 * time.Second
)

// awaitStreamIdle blocks until rs's stream has released the streaming lock or
// timeout elapses, and reports whether the stream ended.
func awaitStreamIdle(rs *activeRuntimes, timeout time.Duration) bool {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		if rs.streaming.TryLock() {
			rs.streaming.Unlock()
			return true
		}
		select {
		case <-deadline.C:
			return false
		case <-ticker.C:
		}
	}
}

// releaseRuntime closes a deleted session's server-owned runtime and disposes its
// team's toolsets. Nothing else does: LocalRuntime.Close does not stop the
// team's toolsets, so without this every deleted session leaves its MCP
// clients, supervisors and connections running until the process exits.
// The caller has already waited for the stream to end.
func releaseRuntime(ctx context.Context, sessionID string, rs *activeRuntimes) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "Panic releasing runtime of deleted session", "session_id", sessionID, "panic", r)
		}
	}()
	releaseRuntimeResources(ctx, sessionID, rs.runtime, rs.team)
}

func releaseRuntimeResources(ctx context.Context, sessionID string, rt runtime.Runtime, t *team.Team) {
	releaseRuntimeResourcesWithin(ctx, sessionID, rt, t, releaseCloseTimeout)
}

// releaseRuntimeResourcesWithin closes rt, then disposes t's toolsets. A Close
// that fails, panics or outlives closeTimeout never skips toolset disposal,
// since that is what frees the MCP clients. t is nil when the caller owns the team.
func releaseRuntimeResourcesWithin(ctx context.Context, sessionID string, rt runtime.Runtime, t *team.Team, closeTimeout time.Duration) {
	closed := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				closed <- fmt.Errorf("panic: %v", r)
			}
		}()
		closed <- rt.Close()
	}()
	timeout := time.NewTimer(closeTimeout)
	defer timeout.Stop()
	select {
	case err := <-closed:
		if err != nil {
			slog.ErrorContext(ctx, "Failed to close runtime", "session_id", sessionID, "error", err)
		}
	case <-timeout.C:
		slog.ErrorContext(ctx, "Timed out closing runtime; disposing its toolsets anyway", "session_id", sessionID, "timeout", closeTimeout)
	}
	if t == nil {
		return
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseStopTimeout)
	defer cancel()
	if err := t.Dispose(stopCtx); err != nil {
		slog.ErrorContext(ctx, "Failed to dispose toolsets", "session_id", sessionID, "error", err)
	}
}
