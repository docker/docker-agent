package remote

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
)

func TestExecutionCapabilitiesClientDecodesRoster(t *testing.T) {
	t.Parallel()
	roster := agentruntime.TeamInfo([]agentruntime.AgentDetails{
		{Name: "native", ExecutionCapabilities: nativeExecutionProfile()},
		{Name: "external", ExecutionCapabilities: harnessExecutionProfile()},
		{Name: "legacy"},
	}, "native")
	payload, err := json.Marshal(roster)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL)
	require.NoError(t, err)
	stream, err := client.RunAgent(t.Context(), "session", "team", nil, "")
	require.NoError(t, err)
	var decoded *agentruntime.TeamInfoEvent
	for event := range stream {
		if info, ok := event.(*agentruntime.TeamInfoEvent); ok {
			decoded = info
		}
	}
	require.NotNil(t, decoded)
	want := roster.(*agentruntime.TeamInfoEvent)
	assert.Equal(t, want.Type, decoded.Type)
	assert.Equal(t, want.CurrentAgent, decoded.CurrentAgent)
	assert.Equal(t, want.AvailableAgents, decoded.AvailableAgents)
	assert.True(t, want.Timestamp.Equal(decoded.Timestamp))
}
