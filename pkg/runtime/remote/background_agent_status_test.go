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

func TestClientStreamBackgroundAgentStatus(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, running := range []bool{true, false} {
			data, err := json.Marshal(agentruntime.BackgroundAgentStatus("child", "worker", running))
			if err != nil {
				t.Error(err)
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}))
	t.Cleanup(srv.Close)

	events, err := newTestClient(t, srv.URL).StreamSessionEvents(t.Context(), "parent")
	require.NoError(t, err)
	var statuses []*agentruntime.BackgroundAgentStatusEvent
	for event := range events {
		status, ok := event.(*agentruntime.BackgroundAgentStatusEvent)
		require.True(t, ok, "unexpected event %T", event)
		assert.Equal(t, "child", status.GetSessionID())
		assert.Equal(t, "worker", status.GetAgentName())
		statuses = append(statuses, status)
	}
	require.Len(t, statuses, 2)
	assert.True(t, statuses[0].Running)
	assert.False(t, statuses[1].Running)
}
