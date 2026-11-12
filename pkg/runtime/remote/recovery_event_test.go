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

func TestSessionRecoveredEventContract(t *testing.T) {
	t.Parallel()
	event := agentruntime.SessionRecovered("root")
	data, err := json.Marshal(event)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(data, &wire))
	assert.Equal(t, "session_recovered", wire["type"])
	assert.Equal(t, "root", wire["session_id"])
	assert.Equal(t, "root", event.(agentruntime.SessionScoped).GetSessionID())
	assert.NotContains(t, wire, "reason", "reset is not a stop or a completed turn")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", data)
	}))
	t.Cleanup(srv.Close)
	client := newTestClient(t, srv.URL)
	events, err := client.StreamSessionEvents(t.Context(), "root")
	require.NoError(t, err)
	var got []agentruntime.Event
	for event := range events {
		got = append(got, event)
	}
	require.Len(t, got, 1)
	reset, ok := got[0].(*agentruntime.SessionRecoveredEvent)
	require.True(t, ok)
	assert.Equal(t, "root", reset.SessionID)
}
