package remote

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config/latest"
	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/inspection"
	"github.com/docker/docker-agent/pkg/session"
)

type capabilityRemoteClient struct {
	stubBackend

	events []agentruntime.Event
}

func (c *capabilityRemoteClient) RunAgent(context.Context, string, string, []api.Message, string) (<-chan agentruntime.Event, error) {
	out := make(chan agentruntime.Event, len(c.events))
	for _, event := range c.events {
		out <- event
	}
	close(out)
	return out, nil
}

func TestExecutionCapabilitiesRemoteDoesNotInferSupport(t *testing.T) {
	t.Parallel()
	for _, capabilities := range []*inspection.ExecutionCapabilities{nil, harnessExecutionProfile()} {
		t.Run(fmt.Sprintf("known=%t", capabilities != nil), func(t *testing.T) {
			t.Parallel()
			roster := agentruntime.TeamInfo([]agentruntime.AgentDetails{{Name: "root", ExecutionCapabilities: capabilities}}, "root")
			client := &capabilityRemoteClient{
				stubBackend: stubBackend{cfg: &latest.Config{Agents: latest.Agents{{Name: "root", Harness: &latest.HarnessConfig{Type: "codex"}}}}},
				events:      []agentruntime.Event{roster, agentruntime.StreamStopped("session", "root", "completed")},
			}
			rt, err := NewRuntime(client)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rt.Close()) })
			rt.EmitAgentInfo(t.Context(), agentruntime.EventSinkFunc(func(event agentruntime.Event) {
				switch info := event.(type) {
				case *agentruntime.AgentInfoEvent:
					assert.Nil(t, info.ExecutionCapabilities)
				case *agentruntime.TeamInfoEvent:
					assert.Nil(t, info.AvailableAgents[0].ExecutionCapabilities)
				}
			}))
			var forwarded *agentruntime.TeamInfoEvent
			for event := range rt.RunStream(t.Context(), session.New(session.WithID("session"))) {
				if info, ok := event.(*agentruntime.TeamInfoEvent); ok {
					forwarded = info
				}
			}
			require.NotNil(t, forwarded)
			assert.Same(t, roster, forwarded)
			assert.Equal(t, capabilities, forwarded.AvailableAgents[0].ExecutionCapabilities)
		})
	}
}
