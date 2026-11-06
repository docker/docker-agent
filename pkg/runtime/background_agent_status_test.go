package runtime

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
)

func TestBackgroundAgentStatusConcurrentCancellation(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		worker := agent.New("worker", "test", agent.WithModel(&activeRootBlockingProvider{id: "test/mock-model", release: make(chan struct{})}))
		root := agent.New("root", "test", agent.WithSubAgents(worker), agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}))
		rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, rt.Close()) })

		var mu sync.Mutex
		var statuses []*BackgroundAgentStatusEvent
		var contextErrors []error
		rt.OnBackgroundEventWithContext(func(ctx context.Context, event Event) {
			if status, ok := event.(*BackgroundAgentStatusEvent); ok {
				mu.Lock()
				defer mu.Unlock()
				statuses = append(statuses, status)
				contextErrors = append(contextErrors, ctx.Err())
			}
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var wg sync.WaitGroup
		for range 2 {
			wg.Go(func() {
				rt.RunAgent(ctx, agenttool.RunParams{AgentName: "worker", Task: "wait", ParentSession: session.New()})
			})
		}
		synctest.Wait()
		require.Len(t, statuses, 2)
		assert.True(t, statuses[0].Running)
		assert.True(t, statuses[1].Running)
		assert.NotEqual(t, statuses[0].SessionID, statuses[1].SessionID)
		cancel()
		wg.Wait()
		require.Len(t, statuses, 4)
		for _, err := range contextErrors {
			require.NoError(t, err, "terminal status is delivered even after cancellation")
		}
		active := make(map[string]bool)
		for _, status := range statuses {
			assert.Equal(t, "worker", status.AgentName)
			require.NotEmpty(t, status.SessionID)
			if status.Running {
				active[status.SessionID] = true
			} else {
				require.True(t, active[status.SessionID])
				delete(active, status.SessionID)
			}
		}
		assert.Empty(t, active)
	})
}
