package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestOperationIdentity(t *testing.T) {
	t.Parallel()
	a := New(t.Context(), &mockRuntime{}, session.New())
	first := a.BeginOperation(t.Context())
	stop := runtime.StreamStopped(a.session.ID, "root", "canceled")
	tagged := operationEvent(context.WithoutCancel(first), stop)
	assert.True(t, a.IsCurrentOperation(tagged))
	second := a.BeginOperation(t.Context())
	assert.False(t, a.IsCurrentOperation(tagged))
	for _, route := range []runtime.Event{&runtime.RoutingDecisionEvent{}, &runtime.AgentRouteEvent{}} {
		assert.False(t, a.IsCurrentOperation(operationEvent(first, route)), "%T from the replaced run", route)
		assert.True(t, a.IsCurrentOperation(operationEvent(second, route)), "%T from the new run", route)
	}
	assert.True(t, a.IsCurrentOperation(operationEvent(second, runtime.StreamStarted(a.session.ID, "root"))))
	assert.Zero(t, stop.(*runtime.StreamStoppedEvent).OperationID, "do not mutate shared events")
	wire, err := json.Marshal(tagged)
	require.NoError(t, err)
	assert.NotContains(t, string(wire), "operation")
	assert.True(t, a.IsCurrentOperation(&runtime.ElicitationClosedEvent{ElicitationID: "job"}), "detached closures outlive the foreground operation")
}
