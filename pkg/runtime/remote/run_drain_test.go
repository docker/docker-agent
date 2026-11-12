package remote

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config/latest"
	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

// stallingObserver stands in for teardown work (e.g. persistence)
// stallingRemoteClient serves a stream that fails, waits for the caller to
// cancel, then floods more events than the runtime buffer holds. Only a
// consumer that cancels and drains lets the producer finish.
type stallingRemoteClient struct {
	stubBackend

	done chan struct{}
}

func (c *stallingRemoteClient) RunAgent(ctx context.Context, _, _ string, _ []api.Message, _ string) (<-chan agentruntime.Event, error) {
	ch := make(chan agentruntime.Event)
	go func() {
		defer close(ch)
		defer close(c.done)
		ch <- agentruntime.Error("remote failure")
		<-ctx.Done()
		for range 2 * defaultEventChannelCapacity {
			ch <- agentruntime.Warning("trailing", "test")
		}
	}()
	return ch, nil
}

func (c *stallingRemoteClient) RunAgentWithAgentName(ctx context.Context, sessionID, agentFile, _ string, msgs []api.Message, model string) (<-chan agentruntime.Event, error) {
	return c.RunAgent(ctx, sessionID, agentFile, msgs, model)
}

func TestRemoteRuntime_RunCancelsAndDrainsStreamOnError(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		client := &stallingRemoteClient{
			stubBackend: stubBackend{cfg: &latest.Config{Agents: latest.Agents{{Name: "test"}}}},
			done:        make(chan struct{}),
		}
		rt, err := NewRuntime(client)
		require.NoError(t, err)

		_, err = rt.Run(t.Context(), session.New(session.WithUserMessage("hi")))
		require.EqualError(t, err, "remote failure")

		select {
		case <-client.done:
		default:
			t.Fatal("Run returned before cancelling and draining its stream")
		}
	})
}
