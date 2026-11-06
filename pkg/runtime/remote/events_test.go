package remote

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
)

func awaitEvent[T agentruntime.Event](t *testing.T, ch <-chan agentruntime.Event, what string) T {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case event, ok := <-ch:
			require.True(t, ok, "event channel closed while waiting for %s", what)
			if typed, ok := event.(T); ok {
				return typed
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func drainEvents(ch <-chan agentruntime.Event) []agentruntime.Event {
	var events []agentruntime.Event
	for {
		select {
		case event, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, event)
		default:
			return events
		}
	}
}
