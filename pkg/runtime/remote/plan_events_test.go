package remote

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
)

// TestClient_DecodesPlanChangedEvent verifies the event is registered in the
// client's event registry, so remote runtimes deliver it typed rather than
// dropping it.
func TestClient_DecodesPlanChangedEvent(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"type\":\"plan_changed\",\"scope\":\"shared\",\"name\":\"release\",\"action\":\"status\",\"version\":4}\n\n")
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL)

	ch, err := c.StreamSessionEvents(t.Context(), "s")
	require.NoError(t, err)

	var got *agentruntime.PlanChangedEvent
	for ev := range ch {
		if planEv, ok := ev.(*agentruntime.PlanChangedEvent); ok {
			got = planEv
			break
		}
	}
	require.NotNil(t, got, "plan_changed must decode to *PlanChangedEvent")
	assert.Equal(t, "release", got.Name)
	assert.Equal(t, "status", got.Action)
	assert.Equal(t, 4, got.Version)
	assert.Equal(t, "shared", got.Scope)
}
